"""JobQueue tests with a fake yt-dlp process (no network, no real subprocesses)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, cast

import pytest

from conftest import FakePopen, FakeProcess, fake_kill, wait_for
from jobs import JobError, JobQueue, sanitise_folder_name
from models import JobOptions

VIDEOS = ["jNQXAC9IVRw", "dQw4w9WgXcQ", "9bZkp7q19f0", "kJQP7kiw5Fk"]


def url(video_id: str) -> str:
    return f"https://www.youtube.com/watch?v={video_id}"


def make_queue(tmp_path: Path, popen: FakePopen, **kwargs: Any) -> JobQueue:
    kwargs.setdefault("concurrency", 2)
    return JobQueue(
        downloads_dir=lambda: tmp_path,
        ffmpeg=lambda: True,
        # Fakes duck-type Popen; cast so type checkers accept them in place of the real thing
        popen=cast(Any, popen),
        kill=cast(Any, fake_kill),
        **kwargs,
    )


def status(queue: JobQueue, job_id: str) -> str:
    return queue.snapshot(job_id)["status"]


def test_concurrency_limit_and_fifo_order(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen)
    ids = [queue.create(url(v), JobOptions())[0]["id"] for v in VIDEOS[:3]]

    wait_for(lambda: len(blocking_popen.processes) == 2)
    assert [status(queue, i) for i in ids] == ["downloading", "downloading", "queued"]
    assert blocking_popen.processes[0].args[-1] == url(VIDEOS[0])
    assert blocking_popen.processes[1].args[-1] == url(VIDEOS[1])

    blocking_popen.processes[0].finish(0)
    wait_for(lambda: len(blocking_popen.processes) == 3)
    assert status(queue, ids[0]) == "complete"
    assert status(queue, ids[2]) == "downloading"
    assert blocking_popen.processes[2].args[-1] == url(VIDEOS[2])

    for proc in blocking_popen.processes[1:]:
        proc.finish(0)
    assert queue.wait_idle()
    assert not queue.busy()


def test_raising_concurrency_starts_waiting_jobs(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=1)
    for v in VIDEOS[:3]:
        queue.create(url(v), JobOptions())
    wait_for(lambda: len(blocking_popen.processes) == 1)
    queue.apply_settings(concurrency=3, fragments=8, cookies=True)
    wait_for(lambda: len(blocking_popen.processes) == 3)
    assert "--cookies-from-browser" in blocking_popen.processes[2].args
    assert blocking_popen.processes[2].args[blocking_popen.processes[2].args.index("--concurrent-fragments") + 1] == "8"
    queue.shutdown()


def test_successful_run_records_progress_title_and_file(tmp_path: Path) -> None:
    final = str(tmp_path / "Me at the zoo [jNQXAC9IVRw].mp4")
    lines = [
        "YTDLP_TITLE Me at the zoo",
        f"[download] Destination: {tmp_path / 'Me at the zoo [jNQXAC9IVRw].f134.mp4'}",
        "YTDLP_PROGRESS " + json.dumps({"status": "downloading", "_percent": 40.0, "speed": 1048576, "eta": 9}),
        f'[Merger] Merging formats into "{final}"',
        f"YTDLP_FILE {final}",
    ]
    popen = FakePopen(lambda: FakeProcess(lines, returncode=0))
    queue = make_queue(tmp_path, popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]

    wait_for(lambda: status(queue, job_id) == "complete")
    job = queue.snapshot(job_id)
    assert job["title"] == "Me at the zoo"
    assert job["percent"] == 100.0
    assert job["filepath"] == final
    assert job["filename"] == "Me at the zoo [jNQXAC9IVRw].mp4"
    assert job["error"] is None and job["speed"] is None
    proc = popen.processes[0]
    assert proc.kwargs["stdout"] is not None and proc.kwargs["text"] is True
    assert "creationflags" in proc.kwargs or "preexec_fn" in proc.kwargs


def test_processing_state_while_merging(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: len(blocking_popen.processes) == 1)
    blocking_popen.processes[0].emit("[Merger] Merging formats into \"x [jNQXAC9IVRw].mp4\"")
    wait_for(lambda: status(queue, job_id) == "processing")
    blocking_popen.processes[0].finish(0)
    wait_for(lambda: status(queue, job_id) == "complete")


def test_failed_run_records_error_and_detail(tmp_path: Path) -> None:
    lines = ["[youtube] Extracting URL", "ERROR: [youtube] jNQXAC9IVRw: Video unavailable"]
    popen = FakePopen(lambda: FakeProcess(lines, returncode=1))
    queue = make_queue(tmp_path, popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: status(queue, job_id) == "error")
    job = queue.snapshot(job_id)
    assert job["error"] == "Video unavailable"
    assert "Extracting URL" in job["detail"]


def test_start_failure_becomes_error(tmp_path: Path) -> None:
    def broken(args: list[str], **kwargs: Any) -> FakeProcess:
        raise FileNotFoundError("python missing")

    queue = make_queue(tmp_path, broken)  # type: ignore[arg-type]
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: status(queue, job_id) == "error")
    assert queue.snapshot(job_id)["error"] == "Could not start yt-dlp"


def test_pause_resume_keeps_continue(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: len(blocking_popen.processes) == 1)
    blocking_popen.processes[0].emit("YTDLP_PROGRESS " + json.dumps({"status": "downloading", "_percent": 50.0}))
    wait_for(lambda: queue.snapshot(job_id)["percent"] == 45.0)

    assert queue.pause(job_id)["status"] == "paused"
    assert blocking_popen.processes[0].killed
    assert queue.wait_idle()
    assert status(queue, job_id) == "paused"
    assert queue.snapshot(job_id)["percent"] == 45.0

    assert queue.resume(job_id)["status"] == "queued"
    wait_for(lambda: len(blocking_popen.processes) == 2)
    assert status(queue, job_id) == "downloading"
    assert "--continue" in blocking_popen.processes[1].args
    blocking_popen.processes[1].finish(0)
    wait_for(lambda: status(queue, job_id) == "complete")


def test_pause_queued_job_and_frees_slot(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=1)
    first = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    second = queue.create(url(VIDEOS[1]), JobOptions())[0]["id"]
    third = queue.create(url(VIDEOS[2]), JobOptions())[0]["id"]
    wait_for(lambda: len(blocking_popen.processes) == 1)

    assert queue.pause(second)["status"] == "paused"
    queue.pause(first)
    wait_for(lambda: len(blocking_popen.processes) == 2)
    assert blocking_popen.processes[1].args[-1] == url(VIDEOS[2])
    assert status(queue, third) == "downloading"
    queue.shutdown()


def test_resume_waits_for_old_process_to_exit(tmp_path: Path) -> None:
    """A resumed job must not start while the paused process still holds the .part file."""
    popen = FakePopen(lambda: FakeProcess(block=True))
    killed: list[FakeProcess] = []
    queue = JobQueue(
        downloads_dir=lambda: tmp_path,
        ffmpeg=lambda: True,
        popen=cast(Any, popen),
        kill=cast(Any, killed.append),  # record the kill but let the process linger
    )
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: len(popen.processes) == 1)
    queue.pause(job_id)
    queue.resume(job_id)
    assert status(queue, job_id) == "queued"
    assert len(popen.processes) == 1
    killed[0].kill()
    wait_for(lambda: len(popen.processes) == 2)
    assert status(queue, job_id) == "downloading"
    queue.shutdown()


def test_cancel_running_job_removes_partials(tmp_path: Path, blocking_popen: FakePopen) -> None:
    vid = VIDEOS[0]
    partials = [f"T [{vid}].f134.mp4.part", f"T [{vid}].f134.mp4", f"T [{vid}].mp4.ytdl", f"T [{vid}].f140.m4a.part-Frag3"]
    keep = [f"Other [{VIDEOS[1]}].mp4.part", f"Done [{vid}].mp3"]
    for name in partials + keep:
        (tmp_path / name).write_text("x")

    queue = make_queue(tmp_path, blocking_popen)
    job_id = queue.create(url(vid), JobOptions())[0]["id"]
    wait_for(lambda: len(blocking_popen.processes) == 1)
    assert queue.cancel(job_id)["status"] == "cancelled"
    assert queue.wait_idle()
    assert status(queue, job_id) == "cancelled"
    remaining = sorted(p.name for p in tmp_path.iterdir())
    assert remaining == sorted(keep)


def test_cancel_queued_and_paused_jobs(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=1)
    first = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    second = queue.create(url(VIDEOS[1]), JobOptions())[0]["id"]
    wait_for(lambda: len(blocking_popen.processes) == 1)
    assert queue.cancel(second)["status"] == "cancelled"
    queue.pause(first)
    assert queue.wait_idle()
    assert queue.cancel(first)["status"] == "cancelled"
    assert len(blocking_popen.processes) == 1


def test_retry_failed_job(tmp_path: Path) -> None:
    outcomes = iter([1, 0])
    popen = FakePopen(lambda: FakeProcess(["ERROR: boom"], returncode=next(outcomes)))
    queue = make_queue(tmp_path, popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: status(queue, job_id) == "error")
    retried = queue.retry(job_id)
    assert retried["error"] is None and retried["percent"] == 0.0
    wait_for(lambda: status(queue, job_id) == "complete")
    assert len(popen.processes) == 2


@pytest.mark.parametrize(
    ("action", "state"),
    [("resume", "queued"), ("retry", "queued"), ("pause", "complete"), ("cancel", "complete"), ("retry", "complete")],
)
def test_invalid_transitions_raise_409(tmp_path: Path, action: str, state: str) -> None:
    popen = FakePopen(lambda: FakeProcess(block=True) if state == "queued" else FakeProcess())
    queue = make_queue(tmp_path, popen, concurrency=1 if state == "complete" else 0)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    if state == "complete":
        wait_for(lambda: status(queue, job_id) == "complete")
    with pytest.raises(JobError) as info:
        getattr(queue, action)(job_id)
    assert info.value.status == 409


def test_unknown_job_raises_404(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen)
    for action in ("pause", "resume", "cancel", "retry", "finished_file", "snapshot"):
        with pytest.raises(JobError) as info:
            getattr(queue, action)("nope")
        assert info.value.status == 404


def test_finished_file_requires_existing_file(tmp_path: Path) -> None:
    final = tmp_path / "a [jNQXAC9IVRw].mp4"
    popen = FakePopen(lambda: FakeProcess([f"YTDLP_FILE {final}"]))
    queue = make_queue(tmp_path, popen)
    job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: status(queue, job_id) == "complete")
    with pytest.raises(JobError) as info:
        queue.finished_file(job_id)
    assert info.value.status == 409
    final.write_text("x")
    assert queue.finished_file(job_id) == str(final)


def test_clear_finished_keeps_active(tmp_path: Path) -> None:
    popen = FakePopen(lambda: FakeProcess())
    queue = make_queue(tmp_path, popen, concurrency=1)
    done = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    wait_for(lambda: status(queue, done) == "complete")
    queue.concurrency = 0
    waiting = queue.create(url(VIDEOS[1]), JobOptions())[0]["id"]
    assert queue.clear_finished() == 1
    assert [j["id"] for j in queue.list_jobs()] == [waiting]


def test_playlist_expansion_with_range(tmp_path: Path, blocking_popen: FakePopen) -> None:
    calls: list[str] = []

    def fetcher(canonical: str) -> dict[str, Any]:
        calls.append(canonical)
        return {
            "title": 'My: "List"?',
            "entries": [{"id": v, "title": f"Video {i}"} for i, v in enumerate(VIDEOS, start=1)]
            + [{"id": "bad id!", "title": "skip"}],
        }

    queue = make_queue(tmp_path, blocking_popen, concurrency=0, info_fetcher=fetcher)
    jobs = queue.create("https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE", JobOptions(kind="audio"), [(2, 3)])
    assert calls == ["https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"]
    assert [j["url"] for j in jobs] == [url(VIDEOS[1]), url(VIDEOS[2])]
    assert [j["title"] for j in jobs] == ["Video 2", "Video 3"]
    assert len({j["group_id"] for j in jobs}) == 1 and jobs[0]["group_id"]
    assert all(j["group_title"] == 'My: "List"?' for j in jobs)
    assert all(j["kind"] == "audio" for j in jobs)

    queue.apply_settings(concurrency=1)
    wait_for(lambda: len(blocking_popen.processes) == 1)
    out = blocking_popen.processes[0].args[blocking_popen.processes[0].args.index("-o") + 1]
    assert out.startswith(str(tmp_path / "My_ _List__"))
    queue.shutdown()


def test_same_video_in_same_folder_runs_one_at_a_time(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=3)
    first = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    second = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
    other = queue.create(url(VIDEOS[1]), JobOptions())[0]["id"]

    wait_for(lambda: len(blocking_popen.processes) == 2)
    assert [status(queue, i) for i in (first, second, other)] == ["downloading", "queued", "downloading"]

    blocking_popen.processes[0].finish(0)
    wait_for(lambda: len(blocking_popen.processes) == 3)
    assert status(queue, second) == "downloading"
    assert blocking_popen.processes[2].args[-1] == url(VIDEOS[0])
    queue.shutdown()


def test_channel_folder_drops_tab_suffix(tmp_path: Path, blocking_popen: FakePopen) -> None:
    def fetcher(canonical: str) -> dict[str, Any]:
        return {"title": "Jawed - Videos", "entries": [{"id": VIDEOS[0], "title": "a"}]}

    queue = make_queue(tmp_path, blocking_popen, concurrency=1, info_fetcher=fetcher)
    jobs = queue.create("https://www.youtube.com/@jawed", JobOptions())
    assert jobs[0]["group_title"] == "Jawed"
    wait_for(lambda: len(blocking_popen.processes) == 1)
    out = blocking_popen.processes[0].args[blocking_popen.processes[0].args.index("-o") + 1]
    assert out.startswith(str(tmp_path / "Jawed"))
    assert "Videos" not in out
    queue.shutdown()


def test_playlist_empty_range_and_cap(tmp_path: Path, blocking_popen: FakePopen) -> None:
    entries = [{"id": VIDEOS[0], "title": "a"}]
    queue = make_queue(tmp_path, blocking_popen, info_fetcher=lambda u: {"title": "t", "entries": entries})
    with pytest.raises(JobError) as info:
        queue.create("https://www.youtube.com/@jawed", JobOptions(), [(5, 9)])
    assert info.value.status == 400

    many = [{"id": f"{i:011d}", "title": str(i)} for i in range(1001)]
    queue = make_queue(tmp_path, blocking_popen, info_fetcher=lambda u: {"title": "t", "entries": many})
    with pytest.raises(JobError) as info:
        queue.create("https://www.youtube.com/@jawed", JobOptions())
    assert info.value.status == 400
    assert queue.list_jobs() == []


def test_video_title_from_lookup(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=0, title_lookup=lambda u: "Known title")
    assert queue.create(url(VIDEOS[0]), JobOptions())[0]["title"] == "Known title"


def test_job_shape(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen, concurrency=0)
    job = queue.create(url(VIDEOS[0]), JobOptions(quality="1080"))[0]
    assert set(job) == {
        "id", "group_id", "group_title", "url", "title", "kind", "status", "percent", "speed",
        "eta", "filename", "filepath", "error", "detail", "options", "created_at",
    }
    assert job["options"]["quality"] == "1080"
    assert job["status"] == "queued"


@pytest.mark.parametrize(
    ("title", "expected"),
    [
        ("Normal title", "Normal title"),
        ("a/b\\c:d*e?f\"g<h>i|j%k", "a_b_c_d_e_f_g_h_i_j_k"),
        ("trailing dots...  ", "trailing dots"),
        ("CON", "_CON"),
        ("", "Playlist"),
        (None, "Playlist"),
        ("///", "Playlist"),
        ("x" * 300, "x" * 100),
    ],
)
def test_sanitise_folder_name(title: str | None, expected: str) -> None:
    assert sanitise_folder_name(title) == expected


def test_hold_blocks_dispatch_until_released(tmp_path: Path, blocking_popen: FakePopen) -> None:
    queue = make_queue(tmp_path, blocking_popen)
    with queue.hold():
        job_id = queue.create(url(VIDEOS[0]), JobOptions())[0]["id"]
        assert status(queue, job_id) == "queued"
        assert blocking_popen.processes == []
    wait_for(lambda: len(blocking_popen.processes) == 1)
    assert status(queue, job_id) == "downloading"
    queue.shutdown()

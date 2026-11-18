"""HTTP API tests: status codes, CSRF/CORS guard, ffmpeg rule, settings and update."""

from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Any, cast

import pytest
from fastapi.testclient import TestClient

import platform_utils
from conftest import FakePopen, FakeProcess, fake_kill, wait_for
from jobs import JobQueue
from server import create_app
from updater import Updater
from validation import classify_url
from ytdlp import YtDlpError, YtDlpTimeout

HEADERS = {"X-YTDL-Client": "1"}
VIDEO = "https://www.youtube.com/watch?v=jNQXAC9IVRw"
PLAYLIST = "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"
EXT_ORIGIN = "moz-extension://0d6c8f43-1f39-4a1b-9e1c-0f6a1d0b2e3c"

VIDEO_RAW = {
    "id": "jNQXAC9IVRw",
    "title": "Me at the zoo",
    "uploader": "jawed",
    "duration": 19,
    "formats": [{"vcodec": "avc1", "acodec": "none", "height": 360, "filesize": 10}],
}
PLAYLIST_RAW = {
    "id": "PLBCF2DAC6FFB574DE",
    "title": "Stories",
    "entries": [{"id": "jNQXAC9IVRw", "title": "A"}, {"id": "dQw4w9WgXcQ", "title": "B"}, {"id": "9bZkp7q19f0", "title": "C"}],
}


class FakeFetcher:
    """fetch_raw_info replacement that counts calls and can fail on demand."""

    def __init__(self) -> None:
        self.calls: list[tuple[str, bool]] = []
        self.error: Exception | None = None

    def __call__(self, url: str, *, flat: bool, cookies: bool, timeout: float) -> dict[str, Any]:
        self.calls.append((url, flat))
        if self.error:
            raise self.error
        return PLAYLIST_RAW if flat else VIDEO_RAW


class Env:
    """Bundle of the client and its fakes."""

    def __init__(self, tmp_path: Path, ffmpeg: bool = True, procs: str = "block") -> None:
        self.ffmpeg = ffmpeg
        self.fetcher = FakeFetcher()
        self.popen = FakePopen(lambda: FakeProcess(block=procs == "block"))
        self.pip_result = subprocess.CompletedProcess([], 0, stdout="", stderr="")
        self.installed = "2026.03.03"
        self.updater = Updater(
            tmp_path / "update_state.json",
            latest_fetcher=lambda: "2026.11.01",
            pip_runner=self.run_pip,
            version_reader=lambda: self.installed,
        )
        self.app = create_app(
            info_fetcher=self.fetcher,
            ffmpeg=lambda: self.ffmpeg,
            updater=self.updater,
            settings_path=tmp_path / "settings.json",
        )
        backend = self.app.state.backend
        # Swap in a queue that never touches real processes or the real Downloads folder
        backend.queue = JobQueue(
            downloads_dir=lambda: tmp_path,
            ffmpeg=lambda: self.ffmpeg,
            # Fakes duck-type Popen; cast so type checkers accept them in place of the real thing
            popen=cast(Any, self.popen),
            kill=cast(Any, fake_kill),
            info_fetcher=lambda u: backend.info(classify_url(u)),
            title_lookup=backend.cached_title,
        )
        self.queue = backend.queue
        self.client = TestClient(self.app, base_url="http://127.0.0.1:18956", headers=HEADERS)

    def run_pip(self) -> subprocess.CompletedProcess:
        """Fake pip: a successful run installs the newer version."""
        if self.pip_result.returncode == 0:
            self.installed = "2026.11.01"
        return self.pip_result


@pytest.fixture
def env(tmp_path: Path) -> Env:
    return Env(tmp_path)


# Guard


def test_health_requires_client_header(env: Env) -> None:
    response = env.client.get("/health", headers={"X-YTDL-Client": ""})
    assert response.status_code == 403
    assert "error" in response.json()


def test_foreign_origin_rejected(env: Env) -> None:
    response = env.client.get("/health", headers={"Origin": "https://evil.example"})
    assert response.status_code == 403
    assert env.client.get("/health", headers={"Origin": "null"}).status_code == 403


def test_extension_origin_allowed_with_cors(env: Env) -> None:
    response = env.client.get("/health", headers={"Origin": EXT_ORIGIN})
    assert response.status_code == 200
    assert response.headers["access-control-allow-origin"] == EXT_ORIGIN


def test_preflight(env: Env) -> None:
    preflight = {
        "Origin": EXT_ORIGIN,
        "Access-Control-Request-Method": "POST",
        "Access-Control-Request-Headers": "x-ytdl-client,content-type",
    }
    ok = env.client.options("/jobs", headers={**preflight, "X-YTDL-Client": ""})
    assert ok.status_code == 200
    assert ok.headers["access-control-allow-origin"] == EXT_ORIGIN
    bad = env.client.options("/jobs", headers={**preflight, "Origin": "https://evil.example"})
    assert bad.status_code == 403


def test_foreign_host_rejected(env: Env) -> None:
    assert env.client.get("/health", headers={"Host": "evil.example:18956"}).status_code == 403
    assert env.client.get("/health", headers={"Host": "localhost:18956"}).status_code == 200


# Health and info


def test_health_body(env: Env) -> None:
    env.updater.check(force=True)
    body = env.client.get("/health").json()
    assert body["status"] == "ok"
    assert body["version"] == "2.0.0"
    assert body["ffmpeg"] is True
    assert body["yt_dlp_version"] == "2026.03.03"
    assert body["yt_dlp_latest"] == "2026.11.01"
    assert body["update_available"] is True
    assert body["concurrency"] == 2 and body["fragments"] == 4
    assert body["cookies"] is False and body["auto_update"] is True


def test_info_video_and_cache(env: Env) -> None:
    first = env.client.get("/info", params={"url": "https://youtu.be/jNQXAC9IVRw"})
    assert first.status_code == 200
    body = first.json()
    assert body["kind"] == "video" and body["title"] == "Me at the zoo"
    assert body["qualities"][0]["height"] == 360
    env.client.get("/info", params={"url": VIDEO})
    assert env.fetcher.calls == [(VIDEO, False)]


def test_info_playlist(env: Env) -> None:
    body = env.client.get("/info", params={"url": PLAYLIST}).json()
    assert body["kind"] == "playlist"
    assert body["entry_count"] == 3
    assert env.fetcher.calls == [(PLAYLIST, True)]


def test_info_errors(env: Env) -> None:
    assert env.client.get("/info", params={"url": "https://example.com"}).status_code == 400
    assert env.client.get("/info").status_code == 400
    env.fetcher.error = YtDlpError("Private video", "ERROR: Private video")
    response = env.client.get("/info", params={"url": VIDEO})
    assert response.status_code == 502
    assert response.json() == {"error": "Private video", "detail": "ERROR: Private video"}
    env.fetcher.error = YtDlpTimeout("YouTube took too long to answer")
    assert env.client.get("/info", params={"url": "https://youtu.be/dQw4w9WgXcQ"}).status_code == 504


# Jobs


def test_create_video_job(env: Env) -> None:
    env.client.get("/info", params={"url": VIDEO})
    response = env.client.post("/jobs", json={"url": VIDEO, "options": {"kind": "video", "quality": "1080"}})
    assert response.status_code == 202
    jobs = response.json()["jobs"]
    assert len(jobs) == 1
    assert jobs[0]["title"] == "Me at the zoo"
    assert jobs[0]["options"]["quality"] == "1080"
    listed = env.client.get("/jobs").json()["jobs"]
    assert [j["id"] for j in listed] == [jobs[0]["id"]]
    env.queue.shutdown()


def test_create_playlist_jobs_with_items(env: Env) -> None:
    response = env.client.post("/jobs", json={"url": PLAYLIST, "options": {"kind": "audio"}, "items": "1,3"})
    assert response.status_code == 202
    jobs = response.json()["jobs"]
    assert [j["title"] for j in jobs] == ["A", "C"]
    assert jobs[0]["group_title"] == "Stories"
    env.queue.shutdown()


@pytest.mark.parametrize(
    "body",
    [
        {"url": "https://example.com/x"},
        {"url": PLAYLIST, "items": "3-1"},
        {"url": VIDEO, "options": {"quality": "999"}},
        {"url": VIDEO, "options": {"kind": "gif"}},
        {"url": VIDEO, "options": {"audio_bitrate": "64"}},
        {"options": {}},
    ],
)
def test_create_jobs_rejects_bad_input(env: Env, body: dict[str, Any]) -> None:
    response = env.client.post("/jobs", json=body)
    assert response.status_code == 400
    assert "error" in response.json()


def test_create_jobs_list_failure(env: Env) -> None:
    env.fetcher.error = YtDlpError("The playlist does not exist")
    assert env.client.post("/jobs", json={"url": PLAYLIST}).status_code == 502
    env.fetcher.error = None
    assert env.client.post("/jobs", json={"url": PLAYLIST, "items": "10-20"}).status_code == 400


@pytest.mark.parametrize(
    ("options", "status"),
    [
        ({"kind": "audio", "audio_format": "mp3"}, 422),
        ({"kind": "video", "quality": "best"}, 422),
        ({"kind": "video", "quality": "1080"}, 422),
        ({"kind": "video", "quality": "720"}, 202),
        ({"kind": "audio", "audio_format": "m4a"}, 202),
    ],
)
def test_ffmpeg_requirement(tmp_path: Path, options: dict[str, str], status: int) -> None:
    env = Env(tmp_path, ffmpeg=False)
    response = env.client.post("/jobs", json={"url": VIDEO, "options": options})
    assert response.status_code == status
    if status == 422:
        assert response.json()["error"] == "This download needs ffmpeg"
    env.queue.shutdown()


def test_job_actions(env: Env) -> None:
    job_id = env.client.post("/jobs", json={"url": VIDEO, "options": {"quality": "720"}}).json()["jobs"][0]["id"]
    wait_for(lambda: len(env.popen.processes) == 1)

    paused = env.client.post(f"/jobs/{job_id}/pause")
    assert paused.status_code == 200 and paused.json()["job"]["status"] == "paused"
    assert env.client.post(f"/jobs/{job_id}/pause").status_code == 409
    assert env.queue.wait_idle()
    assert env.client.post(f"/jobs/{job_id}/resume").json()["job"]["status"] in ("queued", "downloading")
    assert env.client.post(f"/jobs/{job_id}/open").status_code == 409
    assert env.client.post(f"/jobs/{job_id}/cancel").json()["job"]["status"] == "cancelled"
    assert env.client.post(f"/jobs/{job_id}/retry").status_code == 200
    assert env.client.post("/jobs/missing/pause").status_code == 404
    assert env.client.post(f"/jobs/{job_id}/explode").status_code == 404
    env.queue.shutdown()


def test_open_and_reveal_finished_file(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    env = Env(tmp_path, procs="finish")
    final = tmp_path / "Me [jNQXAC9IVRw].mp4"
    final.write_text("x")
    env.popen.factory = lambda: FakeProcess([f"YTDLP_FILE {final}"])
    opened: list[tuple[str, str]] = []
    monkeypatch.setattr(platform_utils, "open_file", lambda p: opened.append(("open", p)))
    monkeypatch.setattr(platform_utils, "reveal", lambda p: opened.append(("reveal", p)))

    job_id = env.client.post("/jobs", json={"url": VIDEO}).json()["jobs"][0]["id"]
    wait_for(lambda: env.queue.snapshot(job_id)["status"] == "complete")
    assert env.client.post(f"/jobs/{job_id}/open").status_code == 200
    assert env.client.post(f"/jobs/{job_id}/reveal").json()["job"]["filepath"] == str(final)
    assert opened == [("open", str(final)), ("reveal", str(final))]
    final.unlink()
    assert env.client.post(f"/jobs/{job_id}/open").status_code == 409


def test_clear_finished(tmp_path: Path) -> None:
    env = Env(tmp_path, procs="finish")
    job_id = env.client.post("/jobs", json={"url": VIDEO}).json()["jobs"][0]["id"]
    wait_for(lambda: env.queue.snapshot(job_id)["status"] == "complete")
    assert env.client.delete("/jobs", params={"finished": "true"}).json() == {"removed": 1}
    assert env.client.get("/jobs").json() == {"jobs": []}
    assert env.client.delete("/jobs").status_code == 400


# Settings and update


def test_put_settings_partial_and_persisted(env: Env, tmp_path: Path) -> None:
    response = env.client.put("/settings", json={"concurrency": 4, "cookies": True})
    assert response.status_code == 200
    assert response.json() == {"concurrency": 4, "fragments": 4, "cookies": True, "auto_update": True}
    assert env.queue.concurrency == 4 and env.queue.cookies is True
    assert env.client.get("/health").json()["concurrency"] == 4
    assert '"concurrency":4' in (tmp_path / "settings.json").read_text()

    reloaded = create_app(updater=env.updater, settings_path=tmp_path / "settings.json")
    assert reloaded.state.backend.settings.current.concurrency == 4


@pytest.mark.parametrize("body", [{"concurrency": 0}, {"concurrency": 6}, {"fragments": 9}, {"cookies": "maybe"}])
def test_put_settings_bounds(env: Env, body: dict[str, Any]) -> None:
    assert env.client.put("/settings", json=body).status_code == 400


def test_update_blocked_while_downloading(env: Env) -> None:
    env.client.post("/jobs", json={"url": VIDEO})
    wait_for(lambda: len(env.popen.processes) == 1)
    assert env.client.post("/update").status_code == 409
    env.queue.shutdown()


def test_update_runs_pip(env: Env) -> None:
    response = env.client.post("/update")
    assert response.status_code == 200
    assert response.json() == {"updated": True, "version": "2026.11.01"}


def test_update_failure(env: Env) -> None:
    env.pip_result = subprocess.CompletedProcess([], 1, stdout="", stderr="ERROR: no network")
    response = env.client.post("/update")
    assert response.status_code == 502
    assert "no network" in response.json()["detail"]

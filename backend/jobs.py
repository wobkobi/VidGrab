"""In-memory download queue running yt-dlp processes with a concurrency limit."""

from __future__ import annotations

import logging
import os
import re
import subprocess
import threading
import time
import uuid
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterator

import platform_utils
from models import JobOptions
from validation import VIDEO_ID_RE, classify_url, select_items, watch_url
from ytdlp import OutputParser, build_args, child_env, expected_streams

logger = logging.getLogger("jobs")

RUNNING_STATUSES = ("downloading", "processing")
FINISHED_STATUSES = ("complete", "error", "cancelled")
MAX_GROUP_ITEMS = 1000

# Leftovers of an interrupted download: .part/.ytdl files and per-format intermediates such as
# "Title [id].f137.mp4" that only exist until the merge finishes
_PARTIAL_RE = re.compile(r"(\.part(-Frag\d+)?|\.ytdl|\.f\d+[\w-]*\.\w+|\.temp\.\w+)$")
# yt-dlp titles a channel tab listing "<Channel> - Videos" (or Shorts/Live); the folder uses the
# channel name alone
_CHANNEL_TAB_SUFFIX_RE = re.compile(r"\s+-\s+(Videos|Shorts|Live)$")


class JobError(Exception):
    """A job action that cannot be applied; status is the HTTP code to answer with."""

    def __init__(self, status: int, message: str, detail: str | None = None) -> None:
        super().__init__(message)
        self.status = status
        self.message = message
        self.detail = detail


@dataclass
class Job:
    """One download; public fields mirror the API's Job object."""

    id: str
    url: str
    kind: str
    options: JobOptions
    out_dir: str
    video_id: str
    group_id: str | None = None
    group_title: str | None = None
    title: str | None = None
    status: str = "queued"
    percent: float = 0.0
    speed: str | None = None
    eta: str | None = None
    filename: str | None = None
    filepath: str | None = None
    error: str | None = None
    detail: str | None = None
    created_at: float = field(default_factory=time.time)
    # Runner bookkeeping; never serialised
    process: subprocess.Popen | None = None
    running: bool = False
    run_id: int = 0
    stopped_run: int = -1

    def to_dict(self) -> dict[str, Any]:
        """Return the API representation."""
        return {
            "id": self.id,
            "group_id": self.group_id,
            "group_title": self.group_title,
            "url": self.url,
            "title": self.title,
            "kind": self.kind,
            "status": self.status,
            "percent": self.percent,
            "speed": self.speed,
            "eta": self.eta,
            "filename": self.filename,
            "filepath": self.filepath,
            "error": self.error,
            "detail": self.detail,
            "options": self.options.model_dump(),
            "created_at": self.created_at,
        }


_UNSAFE_NAME_RE = re.compile(r'[<>:"/\\|?*%\x00-\x1f]')
_RESERVED_NAMES = {"CON", "PRN", "AUX", "NUL"} | {f"{p}{i}" for p in ("COM", "LPT") for i in range(1, 10)}


def sanitise_folder_name(title: str | None, fallback: str = "Playlist") -> str:
    """
    Turn a playlist or channel title into a safe single folder name.

    "%" is removed too because the folder becomes part of yt-dlp's output template.

    Args:
        title: The raw title.
        fallback: Name used when nothing usable remains.

    Returns:
        A folder name without separators, reserved names or trailing dots/spaces.
    """
    name = _UNSAFE_NAME_RE.sub("_", title or "").strip().rstrip(". ")
    name = re.sub(r"\s+", " ", name)[:100].rstrip(". ")
    if not name or set(name) <= {"_", "."}:
        return fallback
    if name.split(".")[0].upper() in _RESERVED_NAMES:
        name = f"_{name}"
    return name


class JobQueue:
    """
    Thread-safe download queue.

    A dispatcher starts one runner thread per job while fewer than `concurrency` jobs are
    running, oldest queued job first, so the limit can change at runtime. One lock guards all
    job state. Pausing or cancelling kills the yt-dlp process tree and leaves .part files for
    --continue; a job is only restarted once its previous process has fully exited, since
    Windows keeps the .part file locked until then.
    """

    def __init__(
        self,
        *,
        concurrency: int = 2,
        fragments: int = 4,
        cookies: bool = False,
        downloads_dir: Callable[[], Path] = platform_utils.downloads_dir,
        ffmpeg: Callable[[], bool] = platform_utils.ffmpeg_available,
        popen: Callable[..., subprocess.Popen] = subprocess.Popen,
        kill: Callable[[subprocess.Popen], None] = platform_utils.kill_tree,
        info_fetcher: Callable[[str], dict[str, Any]] | None = None,
        title_lookup: Callable[[str], str | None] | None = None,
    ) -> None:
        self.concurrency = concurrency
        self.fragments = fragments
        self.cookies = cookies
        self._downloads_dir = downloads_dir
        self._ffmpeg = ffmpeg
        self._popen = popen
        self._kill = kill
        self._info_fetcher = info_fetcher
        self._title_lookup = title_lookup
        self._jobs: dict[str, Job] = {}
        self._lock = threading.RLock()
        self._threads: list[threading.Thread] = []
        self._closed = False
        self._holds = 0

    # Queries

    def list_jobs(self) -> list[dict[str, Any]]:
        """Return every job, oldest first."""
        with self._lock:
            return [job.to_dict() for job in self._jobs.values()]

    def get(self, job_id: str) -> Job:
        """
        Return a job by id.

        Args:
            job_id: The job id.

        Returns:
            The job.

        Raises:
            JobError: 404 when the id is unknown.
        """
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                raise JobError(404, "Download not found")
            return job

    def snapshot(self, job_id: str) -> dict[str, Any]:
        """Return the API representation of one job."""
        with self._lock:
            return self.get(job_id).to_dict()

    def busy(self) -> bool:
        """Return True while any job is queued or running (paused jobs do not count)."""
        with self._lock:
            return any(
                j.status == "queued" or j.status in RUNNING_STATUSES or j.running
                for j in self._jobs.values()
            )

    def running(self) -> bool:
        """Return True while any yt-dlp process is alive."""
        with self._lock:
            return any(j.running for j in self._jobs.values())

    # Creation

    def create(
        self, url: str, options: JobOptions, ranges: list[tuple[int, int]] | None = None
    ) -> list[dict[str, Any]]:
        """
        Queue a video, or one child job per selected entry of a playlist or channel.

        Args:
            url: The URL from the extension; validated again here.
            options: Download options.
            ranges: Parsed item ranges for playlists/channels, or None for all items.

        Returns:
            The created jobs in API form.

        Raises:
            InvalidUrl: If the URL is not accepted.
            JobError: 400 when a list has no entries in range or too many.
            YtDlpError: If listing a playlist or channel fails.
        """
        parsed = classify_url(url)
        root = self._downloads_dir()
        if parsed.kind == "video":
            title = self._title_lookup(parsed.canonical) if self._title_lookup else None
            jobs = [self._new_job(parsed.canonical, parsed.id, options, str(root), title=title)]
        else:
            if self._info_fetcher is None:
                raise JobError(500, "Playlist downloads are not available")
            info = self._info_fetcher(parsed.canonical)
            entries = [
                e for e in info.get("entries") or [] if VIDEO_ID_RE.match(str(e.get("id") or ""))
            ]
            selected = select_items(entries, ranges)
            if not selected:
                raise JobError(400, "No videos in that range")
            if len(selected) > MAX_GROUP_ITEMS:
                raise JobError(
                    400,
                    f"Too many videos ({len(selected)}); choose a range of at most {MAX_GROUP_ITEMS}",
                )
            group_title = info.get("title") or parsed.id
            if parsed.kind == "channel":
                group_title = _CHANNEL_TAB_SUFFIX_RE.sub("", group_title) or group_title
            out_dir = str(root / sanitise_folder_name(group_title))
            group_id = uuid.uuid4().hex
            jobs = [
                self._new_job(
                    watch_url(e["id"]),
                    e["id"],
                    options,
                    out_dir,
                    title=e.get("title"),
                    group_id=group_id,
                    group_title=group_title,
                )
                for e in selected
            ]

        with self._lock:
            for job in jobs:
                self._jobs[job.id] = job
            logger.info(
                "Queued %d job(s) for %s %s (%s)",
                len(jobs),
                parsed.kind,
                parsed.id,
                options.kind,
            )
            result = [job.to_dict() for job in jobs]
        self._schedule()
        return result

    @staticmethod
    def _new_job(
        url: str,
        video_id: str,
        options: JobOptions,
        out_dir: str,
        *,
        title: str | None = None,
        group_id: str | None = None,
        group_title: str | None = None,
    ) -> Job:
        """Build a queued job with a fresh id."""
        return Job(
            id=uuid.uuid4().hex,
            url=url,
            kind=options.kind,
            options=options.model_copy(),
            out_dir=out_dir,
            video_id=video_id,
            title=title,
            group_id=group_id,
            group_title=group_title,
        )

    # Actions

    def pause(self, job_id: str) -> dict[str, Any]:
        """
        Pause a queued or running job, keeping its partial files.

        Args:
            job_id: The job id.

        Returns:
            The updated job.

        Raises:
            JobError: 404 for an unknown id, 409 when the job cannot be paused.
        """
        with self._lock:
            job = self.get(job_id)
            if job.status not in ("queued", *RUNNING_STATUSES):
                raise JobError(409, f"Cannot pause a {job.status} download")
            proc = self._stop(job, "paused")
            result = job.to_dict()
        if proc is not None:
            self._kill(proc)
        return result

    def resume(self, job_id: str) -> dict[str, Any]:
        """
        Re-queue a paused job; yt-dlp picks up its .part files via --continue.

        Args:
            job_id: The job id.

        Returns:
            The updated job.

        Raises:
            JobError: 404 for an unknown id, 409 when the job is not paused.
        """
        with self._lock:
            job = self.get(job_id)
            if job.status != "paused":
                raise JobError(409, f"Cannot resume a {job.status} download")
            job.status = "queued"
            result = job.to_dict()
        self._schedule()
        return result

    def cancel(self, job_id: str) -> dict[str, Any]:
        """
        Cancel a job and remove its partial files.

        Args:
            job_id: The job id.

        Returns:
            The updated job.

        Raises:
            JobError: 404 for an unknown id, 409 when the job already finished.
        """
        with self._lock:
            job = self.get(job_id)
            if job.status in FINISHED_STATUSES:
                raise JobError(409, f"Cannot cancel a {job.status} download")
            proc = self._stop(job, "cancelled")
            cleanup_now = not job.running
            result = job.to_dict()
        if proc is not None:
            self._kill(proc)
        if cleanup_now:
            self._remove_partials(job)
        return result

    def retry(self, job_id: str) -> dict[str, Any]:
        """
        Re-queue a failed or cancelled job from scratch.

        Args:
            job_id: The job id.

        Returns:
            The updated job.

        Raises:
            JobError: 404 for an unknown id, 409 when the job is not failed or cancelled.
        """
        with self._lock:
            job = self.get(job_id)
            if job.status not in ("error", "cancelled"):
                raise JobError(409, f"Cannot retry a {job.status} download")
            job.status = "queued"
            job.percent = 0.0
            job.speed = job.eta = job.error = job.detail = None
            result = job.to_dict()
        self._schedule()
        return result

    def finished_file(self, job_id: str) -> str:
        """
        Return the recorded file of a completed job, for open/reveal.

        Args:
            job_id: The job id.

        Returns:
            The absolute file path.

        Raises:
            JobError: 404 for an unknown id, 409 when the job is not complete or the file is gone.
        """
        with self._lock:
            job = self.get(job_id)
            if job.status != "complete" or not job.filepath:
                raise JobError(409, "The download has not finished")
            path = job.filepath
        if not os.path.isfile(path):
            raise JobError(409, "The file was moved or deleted")
        return path

    def clear_finished(self) -> int:
        """Remove complete, failed and cancelled jobs (files are untouched) and return the count."""
        with self._lock:
            ids = [
                j.id for j in self._jobs.values() if j.status in FINISHED_STATUSES and not j.running
            ]
            for job_id in ids:
                del self._jobs[job_id]
            return len(ids)

    def apply_settings(
        self, *, concurrency: int | None = None, fragments: int | None = None, cookies: bool | None = None
    ) -> None:
        """
        Update queue settings; a higher concurrency starts waiting jobs straight away.

        Args:
            concurrency: Maximum parallel downloads.
            fragments: Concurrent fragments for jobs started from now on.
            cookies: Whether jobs started from now on use Firefox cookies.
        """
        with self._lock:
            if concurrency is not None:
                self.concurrency = concurrency
            if fragments is not None:
                self.fragments = fragments
            if cookies is not None:
                self.cookies = cookies
        self._schedule()

    @contextmanager
    def hold(self) -> Iterator[None]:
        """Keep queued jobs from starting inside the block (used while yt-dlp is replaced)."""
        with self._lock:
            self._holds += 1
        try:
            yield
        finally:
            with self._lock:
                self._holds -= 1
            self._schedule()

    def shutdown(self) -> None:
        """Kill every running process; used when the server stops."""
        with self._lock:
            self._closed = True
            procs = [j.process for j in self._jobs.values() if j.process is not None]
            for job in self._jobs.values():
                if job.running:
                    job.stopped_run = job.run_id
        for proc in procs:
            self._kill(proc)

    def wait_idle(self, timeout: float = 5.0) -> bool:
        """Wait until no runner thread is alive (used by tests and shutdown)."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self._lock:
                threads = [t for t in self._threads if t.is_alive()]
                self._threads = threads
            if not threads:
                return True
            threads[0].join(timeout=max(0.0, deadline - time.monotonic()))
        return False

    # Internals

    def _stop(self, job: Job, status: str) -> subprocess.Popen | None:
        """Mark a job paused/cancelled and return the process to kill (caller holds the lock)."""
        job.status = status
        job.speed = job.eta = None
        if job.running:
            job.stopped_run = job.run_id
            return job.process
        return None

    def _schedule(self) -> None:
        """Start queued jobs, oldest first, while below the concurrency limit."""
        with self._lock:
            if self._closed or self._holds:
                return
            running = [j for j in self._jobs.values() if j.running]
            active = len(running)
            # Two runs of one video into one folder would write the same .part and .fNNN files,
            # so a duplicate waits in the queue until the earlier run has exited
            busy_targets = {(j.video_id, j.out_dir) for j in running}
            for job in self._jobs.values():
                if active >= self.concurrency:
                    break
                if job.status != "queued" or job.running:
                    continue
                if (job.video_id, job.out_dir) in busy_targets:
                    continue
                busy_targets.add((job.video_id, job.out_dir))
                job.running = True
                job.run_id += 1
                job.status = "downloading"
                active += 1
                thread = threading.Thread(
                    target=self._run, args=(job, job.run_id), name=f"job-{job.id[:8]}", daemon=True
                )
                self._threads.append(thread)
                thread.start()

    def _run(self, job: Job, run_id: int) -> None:
        """Run yt-dlp for one job and record the outcome (runs on its own thread)."""
        ffmpeg = self._ffmpeg()
        with self._lock:
            options = job.options
            args = build_args(
                options,
                job.url,
                job.out_dir,
                self.cookies,
                fragments=self.fragments,
                ffmpeg=ffmpeg,
            )
        parser = OutputParser(expected_streams=expected_streams(options, ffmpeg))
        proc: subprocess.Popen | None = None
        try:
            Path(job.out_dir).mkdir(parents=True, exist_ok=True)
            proc = self._popen(
                args,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL,
                text=True,
                encoding="utf-8",
                errors="replace",
                bufsize=1,
                env=child_env(),
                **platform_utils.low_priority_popen_kwargs(),
            )
        except OSError as exc:
            logger.error("Could not start yt-dlp for %s: %s", job.video_id, exc)
            self._finish(job, run_id, None, parser, start_error=str(exc))
            return

        kill_now = False
        with self._lock:
            job.process = proc
            kill_now = job.stopped_run == run_id
        if kill_now:
            self._kill(proc)

        logger.info("Started %s (%s)", job.video_id, options.kind)
        assert proc.stdout is not None
        for line in proc.stdout:
            update = parser.feed(line)
            if update is None:
                continue
            with self._lock:
                if job.stopped_run == run_id:
                    continue
                if update.percent is not None:
                    job.percent = max(job.percent, update.percent)
                if update.speed is not None or update.eta is not None or update.percent is not None:
                    job.speed = update.speed
                    job.eta = update.eta
                if update.processing and job.status == "downloading":
                    job.status = "processing"
                    job.speed = job.eta = None
                if update.title and not job.title:
                    job.title = update.title
                if update.filepath:
                    job.filepath = update.filepath
                    job.filename = os.path.basename(update.filepath)
        returncode = proc.wait()
        self._finish(job, run_id, returncode, parser)

    def _finish(
        self,
        job: Job,
        run_id: int,
        returncode: int | None,
        parser: OutputParser,
        start_error: str | None = None,
    ) -> None:
        """Record a finished run and start the next queued job."""
        cleanup = False
        with self._lock:
            job.running = False
            job.process = None
            if job.stopped_run == run_id:
                # Paused or cancelled while running; the action already set the status
                cleanup = job.status == "cancelled"
                logger.info("Stopped %s (%s)", job.video_id, job.status)
            elif start_error is not None:
                job.status = "error"
                job.error = "Could not start yt-dlp"
                job.detail = start_error
            elif returncode == 0:
                job.status = "complete"
                job.percent = 100.0
                job.speed = job.eta = None
                if parser.filepath:
                    job.filepath = parser.filepath
                    job.filename = os.path.basename(parser.filepath)
                logger.info("Completed %s", job.video_id)
            else:
                job.status = "error"
                job.speed = job.eta = None
                job.error = parser.last_error or f"yt-dlp exited with code {returncode}"
                job.detail = parser.detail() or None
                logger.warning("Failed %s: %s", job.video_id, job.error)
        if cleanup:
            self._remove_partials(job)
        self._schedule()

    def _remove_partials(self, job: Job) -> None:
        """
        Delete a cancelled job's leftover partial files.

        Only files carrying the job's "[video id]" in its output folder are touched, and nothing
        is removed while another live job downloads the same video into the same folder.
        """
        with self._lock:
            if any(
                other is not job
                and other.video_id == job.video_id
                and other.out_dir == job.out_dir
                and (other.running or other.status in ("queued", "paused"))
                for other in self._jobs.values()
            ):
                return
        marker = f"[{job.video_id}]"
        try:
            names = os.listdir(job.out_dir)
        except OSError:
            return
        for name in names:
            if marker not in name or not _PARTIAL_RE.search(name):
                continue
            path = os.path.join(job.out_dir, name)
            # Windows can hold the handle for a moment after the kill
            for _ in range(5):
                try:
                    os.remove(path)
                    break
                except FileNotFoundError:
                    break
                except OSError:
                    time.sleep(0.2)

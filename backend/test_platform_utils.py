"""Platform helper tests: low-priority Popen kwargs and process-tree killing."""

from __future__ import annotations

import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import pytest

import platform_utils


def test_windows_popen_kwargs() -> None:
    kwargs = platform_utils.low_priority_popen_kwargs("win32")
    flags = kwargs["creationflags"]
    assert flags & 0x00004000  # BELOW_NORMAL_PRIORITY_CLASS
    assert flags & 0x00000200  # CREATE_NEW_PROCESS_GROUP
    assert flags & 0x08000000  # CREATE_NO_WINDOW
    assert "preexec_fn" not in kwargs


@pytest.mark.parametrize("platform", ["linux", "darwin"])
def test_posix_popen_kwargs(platform: str) -> None:
    kwargs = platform_utils.low_priority_popen_kwargs(platform)
    assert kwargs["start_new_session"] is True
    assert callable(kwargs["preexec_fn"])
    assert "creationflags" not in kwargs


def test_kill_tree_uses_taskkill_on_windows(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[list[str]] = []

    class Proc:
        pid = 1234
        alive = True

        def poll(self) -> int | None:
            return None if self.alive else 1

        def kill(self) -> None:
            self.alive = False

        def wait(self, timeout: float | None = None) -> int:
            return 1

    def fake_run(args: list[str], **kwargs: Any) -> subprocess.CompletedProcess:
        calls.append(args)
        proc.alive = False
        return subprocess.CompletedProcess(args, 0)

    proc = Proc()
    monkeypatch.setattr(platform_utils, "is_windows", lambda: True)
    monkeypatch.setattr(platform_utils.subprocess, "run", fake_run)
    platform_utils.kill_tree(proc)  # type: ignore[arg-type]
    assert calls == [["taskkill", "/T", "/F", "/PID", "1234"]]


def test_kill_tree_ignores_finished_process(monkeypatch: pytest.MonkeyPatch) -> None:
    proc = subprocess.Popen([sys.executable, "-c", "pass"])
    proc.wait()
    monkeypatch.setattr(platform_utils.subprocess, "run", lambda *a, **k: pytest.fail("should not run"))
    platform_utils.kill_tree(proc)


def _alive(pid: int) -> bool:
    """Return True if a process id is still running (without signalling it)."""
    if sys.platform == "win32":
        out = subprocess.run(
            ["tasklist", "/FI", f"PID eq {pid}", "/NH"], capture_output=True, text=True, check=False
        ).stdout
        return str(pid) in out
    try:
        import os

        os.kill(pid, 0)
        return True
    except OSError:
        return False


def test_kill_tree_kills_grandchildren(tmp_path: Path) -> None:
    """A real parent > child process pair: both must die, as ffmpeg must die with yt-dlp."""
    pid_file = tmp_path / "child.pid"
    child = "import time; time.sleep(60)"
    parent = (
        "import subprocess, sys, time\n"
        f"p = subprocess.Popen([sys.executable, '-c', {child!r}])\n"
        f"open({str(pid_file)!r}, 'w').write(str(p.pid))\n"
        "time.sleep(60)\n"
    )
    proc = subprocess.Popen([sys.executable, "-c", parent], **platform_utils.low_priority_popen_kwargs())
    deadline = time.monotonic() + 15
    while not (pid_file.exists() and pid_file.read_text()) and time.monotonic() < deadline:
        time.sleep(0.05)
    child_pid = int(pid_file.read_text())
    assert _alive(child_pid)

    platform_utils.kill_tree(proc)
    assert proc.poll() is not None
    deadline = time.monotonic() + 5
    while _alive(child_pid) and time.monotonic() < deadline:
        time.sleep(0.1)
    assert not _alive(child_pid)


def test_downloads_dir_is_a_path() -> None:
    assert isinstance(platform_utils.downloads_dir(), Path)

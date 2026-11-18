"""OS-specific helpers: download paths, ffmpeg detection, process control, file opening."""

from __future__ import annotations

import logging
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

logger = logging.getLogger("platform")

BACKEND_DIR = Path(__file__).resolve().parent
DATA_DIR = BACKEND_DIR / "data"

# Windows process creation flags, spelt out so the module imports on every platform
BELOW_NORMAL_PRIORITY_CLASS = 0x00004000
CREATE_NEW_PROCESS_GROUP = 0x00000200
CREATE_NO_WINDOW = 0x08000000
DETACHED_PROCESS = 0x00000008
CREATE_BREAKAWAY_FROM_JOB = 0x01000000

# Known-folder GUID for the user's Downloads folder (may be redirected away from ~/Downloads)
_DOWNLOADS_GUID = "{374DE290-123F-4565-9164-39C4925E467B}"


def is_windows() -> bool:
    """Return True when running on Windows."""
    return sys.platform == "win32"


def data_dir() -> Path:
    """Return the runtime data directory, creating it on first use."""
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    return DATA_DIR


def downloads_dir() -> Path:
    """
    Return the user's Downloads folder.

    On Windows the shell-folders registry key is read first so a Downloads folder moved to
    another drive is respected; everything else falls back to ~/Downloads.
    """
    if is_windows():
        try:
            import winreg

            key_path = r"Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders"
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, key_path) as key:
                value, _ = winreg.QueryValueEx(key, _DOWNLOADS_GUID)
            path = Path(os.path.expandvars(value))
            if path.is_dir():
                return path
        except OSError:
            pass
    return Path.home() / "Downloads"


def ffmpeg_available() -> bool:
    """Return True when an ffmpeg binary is on PATH."""
    return shutil.which("ffmpeg") is not None


def low_priority_popen_kwargs(platform: str | None = None) -> dict[str, Any]:
    """
    Return Popen kwargs that start a child at low priority in its own process group.

    The group lets kill_tree take down yt-dlp together with the ffmpeg it spawns,
    and ffmpeg inherits the lowered priority. CREATE_NO_WINDOW stops a console window
    flashing up when the backend itself runs detached without a console.

    Args:
        platform: Platform name to build for; defaults to sys.platform (used by tests).

    Returns:
        Keyword arguments for subprocess.Popen.
    """
    platform = platform or sys.platform
    if platform == "win32":
        return {
            "creationflags": BELOW_NORMAL_PRIORITY_CLASS
            | CREATE_NEW_PROCESS_GROUP
            | CREATE_NO_WINDOW
        }
    return {"preexec_fn": _lower_priority, "start_new_session": True}


def _lower_priority() -> None:
    """Lower the current process priority (runs in the child before exec on POSIX)."""
    # Direct sys.platform check so type checkers know os.nice exists on this branch
    if sys.platform == "win32":
        return
    try:
        os.nice(10)
    except OSError:
        pass


def hidden_popen_kwargs() -> dict[str, Any]:
    """Return Popen kwargs that keep short helper commands from opening a console window."""
    return {"creationflags": CREATE_NO_WINDOW} if is_windows() else {}


def kill_tree(proc: subprocess.Popen, timeout: float = 5.0) -> None:
    """
    Terminate a process and every child it spawned.

    Windows uses taskkill /T so ffmpeg children die with yt-dlp; POSIX signals the whole
    process group created by low_priority_popen_kwargs.

    Args:
        proc: The process to kill; a process that already exited is ignored.
        timeout: Seconds to wait for the process to exit before giving up.
    """
    if proc.poll() is not None:
        return
    try:
        # Direct sys.platform check so type checkers know the POSIX-only os/signal calls exist
        if sys.platform == "win32":
            subprocess.run(
                ["taskkill", "/T", "/F", "/PID", str(proc.pid)],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=timeout,
                check=False,
                **hidden_popen_kwargs(),
            )
        else:
            pgid = os.getpgid(proc.pid)
            os.killpg(pgid, signal.SIGTERM)
            deadline = time.monotonic() + timeout / 2
            while proc.poll() is None and time.monotonic() < deadline:
                time.sleep(0.05)
            if proc.poll() is None:
                os.killpg(pgid, signal.SIGKILL)
    except (OSError, subprocess.SubprocessError) as exc:
        logger.warning("Process tree kill failed for pid %s: %s", proc.pid, exc)
    # Last resort: at least the direct child dies even if the tree kill failed
    if proc.poll() is None:
        try:
            proc.kill()
        except OSError:
            pass
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        logger.warning("Process %s did not exit after kill", proc.pid)


def open_file(path: str) -> None:
    """
    Open a file with the system's default application.

    Args:
        path: Absolute path of an existing file.
    """
    if is_windows():
        os.startfile(path)  # type: ignore[attr-defined]  # noqa: S606 - Windows only
    elif sys.platform == "darwin":
        subprocess.Popen(["open", path])
    else:
        subprocess.Popen(["xdg-open", path])


def reveal(path: str) -> None:
    """
    Show a file selected in the system file manager.

    Args:
        path: Absolute path of an existing file.
    """
    if is_windows():
        # Explorer parses its own command line and needs the path quoted after "/select,",
        # which list-form quoting would wrap around the whole argument instead. Windows
        # paths cannot contain double quotes, so the f-string cannot be escaped out of.
        subprocess.Popen(f'explorer /select,"{path}"')
    elif sys.platform == "darwin":
        subprocess.Popen(["open", "-R", path])
    else:
        subprocess.Popen(["xdg-open", str(Path(path).parent)])

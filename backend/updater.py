"""yt-dlp version check against PyPI and pip-based self-update."""

from __future__ import annotations

import json
import logging
import re
import subprocess
import sys
import threading
import time
import urllib.request
from importlib import metadata
from pathlib import Path
from typing import Any, Callable

from platform_utils import hidden_popen_kwargs

logger = logging.getLogger("updater")

PYPI_URL = "https://pypi.org/pypi/yt-dlp/json"
CHECK_INTERVAL = 24 * 60 * 60
PIP_TIMEOUT = 300


class UpdateError(RuntimeError):
    """pip failed to update yt-dlp; detail holds the tail of its output."""

    def __init__(self, message: str, detail: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.detail = detail


class UpdateBusy(UpdateError):
    """Downloads are running, so yt-dlp must not be replaced right now."""


def installed_version() -> str:
    """
    Return the installed yt-dlp version.

    Package metadata is read fresh on every call, so a pip upgrade shows up without a restart.
    """
    try:
        return metadata.version("yt-dlp")
    except metadata.PackageNotFoundError:
        return "unknown"


def version_key(version: str | None) -> tuple[int, ...]:
    """Turn "2026.03.03" or "2026.03.03.1" into a comparable tuple; unknown sorts lowest."""
    if not version:
        return ()
    return tuple(int(part) for part in re.findall(r"\d+", version))


def fetch_latest(timeout: float = 5.0) -> str | None:
    """
    Ask PyPI for the newest yt-dlp release.

    Args:
        timeout: Network timeout in seconds.

    Returns:
        The version string, or None when PyPI cannot be reached.
    """
    request = urllib.request.Request(PYPI_URL, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - fixed https URL
            data = json.load(response)
        version = data.get("info", {}).get("version")
        return version if isinstance(version, str) else None
    except (OSError, ValueError) as exc:
        logger.info("PyPI version check failed: %s", exc)
        return None


def run_pip_upgrade() -> subprocess.CompletedProcess:
    """Run pip install -U yt-dlp with the interpreter running the backend."""
    return subprocess.run(
        [sys.executable, "-m", "pip", "install", "--upgrade", "--disable-pip-version-check", "yt-dlp"],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=PIP_TIMEOUT,
        stdin=subprocess.DEVNULL,
        check=False,
        **hidden_popen_kwargs(),
    )


class Updater:
    """
    Tracks the latest yt-dlp release (checked at most once a day) and installs updates.

    The last check is stored in data/update_state.json so restarts do not hit PyPI each time.
    """

    def __init__(
        self,
        state_path: Path,
        *,
        latest_fetcher: Callable[[], str | None] = fetch_latest,
        pip_runner: Callable[[], subprocess.CompletedProcess] = run_pip_upgrade,
        version_reader: Callable[[], str] = installed_version,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self._state_path = state_path
        self._fetch_latest = latest_fetcher
        self._run_pip = pip_runner
        self._installed = version_reader
        self._clock = clock
        self._lock = threading.Lock()
        self._update_lock = threading.Lock()
        self._state: dict[str, Any] = self._load_state()

    @property
    def latest(self) -> str | None:
        """Return the newest known release, or None if never checked."""
        with self._lock:
            value = self._state.get("latest")
            return value if isinstance(value, str) else None

    def installed(self) -> str:
        """Return the installed yt-dlp version."""
        return self._installed()

    def update_available(self) -> bool:
        """Return True when the newest known release is newer than the installed one."""
        latest = self.latest
        return latest is not None and version_key(latest) > version_key(self.installed())

    def check(self, force: bool = False) -> str | None:
        """
        Refresh the latest version from PyPI unless checked within the last day.

        Args:
            force: Check even if a check ran recently.

        Returns:
            The newest known release.
        """
        with self._lock:
            last = self._state.get("last_check", 0)
            due = force or not isinstance(last, (int, float)) or self._clock() - last >= CHECK_INTERVAL
        if not due:
            return self.latest
        latest = self._fetch_latest()
        with self._lock:
            self._state["last_check"] = self._clock()
            if latest:
                self._state["latest"] = latest
            self._save_state()
        return self.latest

    def update(self) -> dict[str, Any]:
        """
        Install the newest yt-dlp with pip.

        Returns:
            {updated, version}: whether the installed version changed, and the version now installed.

        Raises:
            UpdateError: If pip fails or times out.
        """
        with self._update_lock:
            before = self.installed()
            try:
                result = self._run_pip()
            except (OSError, subprocess.SubprocessError) as exc:
                raise UpdateError("Could not run pip", str(exc)) from exc
            if result.returncode != 0:
                output = (result.stderr or "") + (result.stdout or "")
                raise UpdateError("pip could not update yt-dlp", "\n".join(output.strip().splitlines()[-20:]))
            after = self.installed()
            with self._lock:
                if version_key(after) > version_key(self._state.get("latest")):
                    self._state["latest"] = after
                self._state["attempted"] = self._state.get("latest")
                self._save_state()
            updated = after != before
            logger.info("yt-dlp update: %s > %s", before, after)
            return {"updated": updated, "version": after}

    def auto_update(
        self, enabled: Callable[[], bool], run_update: Callable[[], dict[str, Any]] | None = None
    ) -> None:
        """
        Run the daily check and, when allowed, install an available update.

        Each release is tried once: if pip cannot reach it (for example a release that needs a
        newer Python), later startups do not re-run pip for the same version.

        Args:
            enabled: Returns the current auto-update setting.
            run_update: Performs the update; the server passes one that holds the job queue and
                raises UpdateBusy while downloads run. Defaults to Updater.update.
        """
        try:
            latest = self.check()
            with self._lock:
                attempted = self._state.get("attempted")
            if enabled() and self.update_available() and attempted != latest:
                (run_update or self.update)()
        except UpdateBusy:
            logger.info("Automatic yt-dlp update postponed: downloads are running")
        except UpdateError as exc:
            logger.warning("Automatic yt-dlp update failed: %s", exc.message)
        except Exception:  # noqa: BLE001 - a failed background update must not crash the server
            logger.exception("Automatic yt-dlp update failed")

    def _load_state(self) -> dict[str, Any]:
        """Read the saved check state, ignoring a missing or corrupt file."""
        try:
            data = json.loads(self._state_path.read_text(encoding="utf-8"))
            return data if isinstance(data, dict) else {}
        except (OSError, ValueError):
            return {}

    def _save_state(self) -> None:
        """Write the check state (caller holds the lock)."""
        try:
            self._state_path.parent.mkdir(parents=True, exist_ok=True)
            self._state_path.write_text(json.dumps(self._state), encoding="utf-8")
        except OSError as exc:
            logger.warning("Could not save update state: %s", exc)

"""Shared pytest fixtures: a scriptable fake yt-dlp process and polling helpers."""

from __future__ import annotations

import itertools
import queue
import sys
import threading
import time
from pathlib import Path
from typing import Any, Callable

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))

_pids = itertools.count(40000)


class FakeProcess:
    """Stands in for subprocess.Popen: yields scripted lines, optionally blocking until finished."""

    def __init__(self, lines: list[str] | None = None, returncode: int = 0, block: bool = False):
        self.pid = next(_pids)
        self.args: list[str] = []
        self.kwargs: dict[str, Any] = {}
        self.killed = False
        self.returncode: int | None = None
        self._final = returncode
        self._lines: queue.Queue[str | None] = queue.Queue()
        self._done = threading.Event()
        for line in lines or []:
            self._lines.put(line if line.endswith("\n") else line + "\n")
        if not block:
            self._lines.put(None)
        self.stdout = self

    def __iter__(self):
        while True:
            item = self._lines.get()
            if item is None:
                self._done.set()
                return
            yield item

    def emit(self, line: str) -> None:
        """Send another output line."""
        self._lines.put(line + "\n")

    def finish(self, returncode: int = 0) -> None:
        """End the output with the given exit code."""
        self._final = returncode
        self._lines.put(None)

    def kill(self) -> None:
        """Simulate the process tree being killed."""
        self.killed = True
        self._final = 1
        self._lines.put(None)

    def poll(self) -> int | None:
        return self.returncode

    def wait(self, timeout: float | None = None) -> int:
        self._done.wait(timeout)
        self.returncode = self._final
        return self._final


class FakePopen:
    """Popen replacement handing out FakeProcess objects made by a factory."""

    def __init__(self, factory: Callable[[], FakeProcess]):
        self.factory = factory
        self.processes: list[FakeProcess] = []

    def __call__(self, args: list[str], **kwargs: Any) -> FakeProcess:
        proc = self.factory()
        proc.args = list(args)
        proc.kwargs = kwargs
        self.processes.append(proc)
        return proc


def wait_for(predicate: Callable[[], bool], timeout: float = 3.0) -> None:
    """Poll until predicate() is true or fail the test."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("condition not met in time")


@pytest.fixture
def blocking_popen() -> FakePopen:
    """Popen whose processes run until finish() or kill()."""
    return FakePopen(lambda: FakeProcess(block=True))


def fake_kill(proc: FakeProcess) -> None:
    """kill_tree replacement for FakeProcess."""
    proc.kill()

"""Native messaging framing, start/already-running paths, and installer output."""

from __future__ import annotations

import io
import json
import struct
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest

import auth
import install_native_host
import native_messaging
from native_messaging import ensure_backend, read_message, write_message


def frame(message: Any) -> bytes:
    body = json.dumps(message).encode("utf-8")
    return struct.pack("=I", len(body)) + body


def test_round_trip() -> None:
    out = io.BytesIO()
    write_message(out, {"status": "running", "detail": "héllo"})
    out.seek(0)
    assert read_message(out) == {"status": "running", "detail": "héllo"}


def test_end_of_input_returns_none() -> None:
    assert read_message(io.BytesIO(b"")) is None
    assert read_message(io.BytesIO(b"\x01\x00")) is None


def test_truncated_and_oversized_messages() -> None:
    with pytest.raises(ValueError):
        read_message(io.BytesIO(struct.pack("=I", 10) + b"{}"))
    with pytest.raises(ValueError):
        read_message(io.BytesIO(struct.pack("=I", 50 * 1024 * 1024)))


def test_already_running_does_not_spawn() -> None:
    spawned: list[bool] = []
    reply = ensure_backend(health=lambda: {"version": "2.0.0"}, spawn=lambda: spawned.append(True))
    assert reply["status"] == "running"
    assert not spawned


def test_starts_backend_when_down() -> None:
    answers = iter([None, None, {"version": "2.0.0"}])
    spawned: list[bool] = []
    reply = ensure_backend(
        health=lambda: next(answers),
        spawn=lambda: spawned.append(True),
        sleep=lambda s: None,
    )
    assert reply == {"status": "started", "detail": "Backend 2.0.0 started"}
    assert spawned == [True]


def test_start_that_never_answers() -> None:
    reply = ensure_backend(health=lambda: None, spawn=lambda: None, wait=0.05, sleep=lambda s: None)
    assert reply["status"] == "starting"


def test_spawn_failure() -> None:
    def fail() -> None:
        raise OSError("no python")

    reply = ensure_backend(health=lambda: None, spawn=fail)
    assert reply["status"] == "error"
    assert "no python" in reply["detail"]


def test_foreign_backend_is_an_error() -> None:
    spawned: list[bool] = []

    def impostor() -> None:
        raise auth.ForeignBackend("Port 18956 is used by another program, not the yt-dl backend")

    reply = ensure_backend(health=impostor, spawn=lambda: spawned.append(True))
    assert reply["status"] == "error"
    assert "another program" in reply["detail"]
    assert not spawned


def run_main(monkeypatch: pytest.MonkeyPatch, payload: bytes) -> tuple[int, Any]:
    monkeypatch.setattr(native_messaging, "auth", _FakeAuth)
    stdin = SimpleNamespace(buffer=io.BytesIO(payload))
    stdout = SimpleNamespace(buffer=io.BytesIO())
    monkeypatch.setattr(sys, "stdin", stdin)
    monkeypatch.setattr(sys, "stdout", stdout)
    code = native_messaging.main()
    stdout.buffer.seek(0)
    return code, read_message(stdout.buffer)


class _FakeAuth:
    """auth stand-in so main() never touches the real data/token file."""

    TOKEN_FILE = auth.TOKEN_FILE
    ForeignBackend = auth.ForeignBackend

    @staticmethod
    def load_token(path: Path) -> str:
        return "t" * 43


def test_main_start_command(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(native_messaging, "ensure_backend", lambda health: {"status": "started", "detail": "ok"})
    code, reply = run_main(monkeypatch, frame({"command": "start"}))
    assert code == 0
    assert reply == {"status": "started", "detail": "ok", "token": "t" * 43}


def test_main_ping(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(native_messaging, "check_health", lambda token: None)
    _, reply = run_main(monkeypatch, frame({"command": "ping"}))
    assert reply["status"] == "stopped"
    assert reply["token"] == "t" * 43


def test_main_ping_reports_foreign_backend(monkeypatch: pytest.MonkeyPatch) -> None:
    def impostor(token: str) -> None:
        raise auth.ForeignBackend("Port 18956 is used by another program, not the yt-dl backend")

    monkeypatch.setattr(native_messaging, "check_health", impostor)
    _, reply = run_main(monkeypatch, frame({"command": "ping"}))
    assert reply["status"] == "error"


def test_main_bad_frame(monkeypatch: pytest.MonkeyPatch) -> None:
    code, reply = run_main(monkeypatch, struct.pack("=I", 5) + b"{")
    assert code == 1
    assert reply["status"] == "error"


def test_manifest_shape(tmp_path: Path) -> None:
    manifest = install_native_host.build_manifest(tmp_path / "launcher.bat")
    assert manifest == {
        "name": "yt_dl_backend",
        "description": "Starts the yt-dl-firefox download backend",
        "path": str(tmp_path / "launcher.bat"),
        "type": "stdio",
        "allowed_extensions": ["yt-dl-firefox@local"],
    }


def test_launcher_pins_interpreter(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(install_native_host, "BAT_PATH", tmp_path / "native_messaging.bat")
    monkeypatch.setattr(install_native_host, "SH_PATH", tmp_path / "native_messaging.sh")
    launcher = install_native_host.write_launcher(r"C:\Python\python.exe")
    text = launcher.read_bytes().decode("utf-8")
    assert r'"C:\Python\python.exe"' in text
    assert "native_messaging.py" in text
    if sys.platform == "win32":
        assert text.startswith("@echo off")
        assert "\r\n" in text

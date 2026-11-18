"""Server process tests: port-in-use exit, idle timeout, and a real-network smoke test."""

from __future__ import annotations

import socket
import threading
import time
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

import server
from updater import Updater


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def test_bind_socket_refuses_taken_port() -> None:
    port = free_port()
    first = server.bind_socket("127.0.0.1", port)
    assert first is not None
    try:
        first.listen()
        assert server.bind_socket("127.0.0.1", port) is None
    finally:
        first.close()


def test_main_exits_cleanly_when_port_in_use(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(server.platform_utils, "data_dir", lambda: tmp_path)
    monkeypatch.setattr(server.auth, "probe", lambda port, token, timeout=1.5: None)
    port = free_port()
    holder = server.bind_socket("127.0.0.1", port)
    assert holder is not None
    holder.listen()
    try:
        assert server.main(["--port", str(port)]) == 0
    finally:
        holder.close()


@pytest.mark.parametrize(
    ("last", "now", "timeout", "busy", "expected"),
    [
        (0, 100, 300, False, False),
        (0, 300, 300, False, True),
        (0, 1000, 300, True, False),
        (0, 1000, 0, False, False),
    ],
)
def test_idle_expired(last: float, now: float, timeout: float, busy: bool, expected: bool) -> None:
    assert server.idle_expired(last, now, timeout, busy) is expected


def test_watchdog_stops_idle_server() -> None:
    fake_server = SimpleNamespace(should_exit=False)
    busy = {"value": True}
    backend = SimpleNamespace(
        last_request=time.monotonic() - 10, queue=SimpleNamespace(busy=lambda: busy["value"])
    )
    thread = threading.Thread(target=server._watch_idle, args=(fake_server, backend, 0.2), daemon=True)
    thread.start()
    time.sleep(0.3)
    assert fake_server.should_exit is False
    busy["value"] = False
    thread.join(timeout=3)
    assert fake_server.should_exit is True


def test_requests_reset_idle_clock(tmp_path: Path) -> None:
    app = server.create_app(
        info_fetcher=lambda *a, **k: {},
        ffmpeg=lambda: True,
        updater=Updater(tmp_path / "state.json", latest_fetcher=lambda: None),
    )
    backend = app.state.backend
    backend.last_request = 0.0
    client = TestClient(app, base_url="http://127.0.0.1:18956")
    client.get("/health")  # rejected: no header, must not count as activity
    assert backend.last_request == 0.0
    client.get("/health", headers={"X-YTDL-Client": "1"})
    assert backend.last_request > 0


@pytest.mark.slow
def test_real_info_smoke(tmp_path: Path) -> None:
    """Fetch real metadata for the first YouTube video (needs network)."""
    app = server.create_app(
        ffmpeg=lambda: True, updater=Updater(tmp_path / "state.json", latest_fetcher=lambda: None)
    )
    client = TestClient(app, base_url="http://127.0.0.1:18956", headers={"X-YTDL-Client": "1"})
    response = client.get("/info", params={"url": "https://www.youtube.com/watch?v=jNQXAC9IVRw"})
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["id"] == "jNQXAC9IVRw"
    assert body["qualities"]

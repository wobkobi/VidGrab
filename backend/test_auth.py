"""Install token: file handling, constant-time check, /health proof and the port probe."""

from __future__ import annotations

import json
import socket
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Callable, Iterator
from urllib.parse import parse_qs, urlsplit

import pytest
from fastapi.testclient import TestClient

import auth
import server
from updater import Updater

TOKEN = "k" * 43


def test_load_token_creates_once_and_reuses(tmp_path: Path) -> None:
    path = tmp_path / "data" / "token"
    first = auth.load_token(path)
    assert len(first) >= 32
    assert auth.load_token(path) == first
    assert path.read_text(encoding="utf-8") == first


def test_load_token_replaces_garbage(tmp_path: Path) -> None:
    path = tmp_path / "token"
    path.write_text("not a token", encoding="utf-8")
    token = auth.load_token(path)
    assert token != "not a token"
    assert auth.load_token(path) == token


def test_token_matches() -> None:
    assert auth.token_matches(TOKEN, TOKEN)
    assert not auth.token_matches(TOKEN, None)
    assert not auth.token_matches(TOKEN, "")
    assert not auth.token_matches(TOKEN, TOKEN[:-1] + "x")


def make_client(tmp_path: Path, token: str | None = TOKEN) -> TestClient:
    app = server.create_app(
        info_fetcher=lambda *a, **k: {},
        ffmpeg=lambda: True,
        updater=Updater(tmp_path / "state.json", latest_fetcher=lambda: None),
        token=token,
    )
    return TestClient(app, base_url="http://127.0.0.1:18956", headers={"X-YTDL-Client": "1"})


def test_api_requires_token(tmp_path: Path) -> None:
    client = make_client(tmp_path)
    assert client.get("/jobs").status_code == 401
    assert client.get("/jobs", headers={auth.TOKEN_HEADER: "wrong"}).status_code == 401
    assert client.put("/settings", json={"cookies": True}).status_code == 401
    assert client.get("/jobs", headers={auth.TOKEN_HEADER: TOKEN}).status_code == 200


def test_health_is_open_and_proves_token(tmp_path: Path) -> None:
    client = make_client(tmp_path)
    plain = client.get("/health")
    assert plain.status_code == 200
    assert "proof" not in plain.json()
    body = client.get("/health", params={"nonce": "abc123"}).json()
    assert body["proof"] == auth.proof(TOKEN, "abc123")
    assert client.get("/health", params={"nonce": "n" * 200}).status_code == 400


def test_cors_allows_token_header(tmp_path: Path) -> None:
    client = make_client(tmp_path)
    response = client.options(
        "/jobs",
        headers={
            "Origin": "moz-extension://0d6c8f43-1f39-4a1b-9e1c-0f6a1d0b2e3c",
            "Access-Control-Request-Method": "GET",
            "Access-Control-Request-Headers": "x-ytdl-client, x-ytdl-token",
        },
    )
    assert response.status_code == 200


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.fixture
def health_server() -> Iterator[Callable[[Callable[[str], dict]], int]]:
    """Start a throwaway HTTP server whose /health body is built from the request nonce."""
    servers: list[HTTPServer] = []

    def start(build: Callable[[str], dict]) -> int:
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - http.server naming
                nonce = parse_qs(urlsplit(self.path).query).get("nonce", [""])[0]
                payload = json.dumps(build(nonce)).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, format: str, *args: object) -> None:  # noqa: A002 - base class name
                return

        httpd = HTTPServer(("127.0.0.1", 0), Handler)
        servers.append(httpd)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        return httpd.server_address[1]

    yield start
    for httpd in servers:
        httpd.shutdown()
        httpd.server_close()


def test_probe_accepts_valid_proof(health_server: Callable) -> None:
    port = health_server(lambda nonce: {"status": "ok", "proof": auth.proof(TOKEN, nonce)})
    result = auth.probe(port, TOKEN)
    assert result is not None and result["status"] == "ok"


def test_probe_rejects_impostor(health_server: Callable) -> None:
    port = health_server(lambda nonce: {"status": "ok", "version": "2.0.0"})
    with pytest.raises(auth.ForeignBackend):
        auth.probe(port, TOKEN)
    port = health_server(lambda nonce: {"status": "ok", "proof": auth.proof("other" * 10, nonce)})
    with pytest.raises(auth.ForeignBackend):
        auth.probe(port, TOKEN)


def test_probe_nothing_listening() -> None:
    assert auth.probe(free_port(), TOKEN, timeout=0.5) is None

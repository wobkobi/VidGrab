"""Firefox native messaging host that starts the backend server when it is not running."""

from __future__ import annotations

import json
import os
import struct
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, BinaryIO, Callable

BACKEND_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(BACKEND_DIR))

import auth  # noqa: E402
import platform_utils  # noqa: E402

PORT = 18956
IDLE_TIMEOUT = 300
START_WAIT = 10.0
MAX_MESSAGE_BYTES = 1024 * 1024


def read_message(stream: BinaryIO) -> Any:
    """
    Read one length-prefixed JSON message (4-byte native-order length, then UTF-8 JSON).

    Args:
        stream: Binary input, normally stdin.

    Returns:
        The decoded message, or None at end of input.

    Raises:
        ValueError: If the length is oversized or the body is truncated or not JSON.
    """
    header = stream.read(4)
    if len(header) < 4:
        return None
    (length,) = struct.unpack("=I", header)
    if length > MAX_MESSAGE_BYTES:
        raise ValueError("Message too large")
    body = stream.read(length)
    if len(body) < length:
        raise ValueError("Truncated message")
    return json.loads(body.decode("utf-8"))


def write_message(stream: BinaryIO, message: Any) -> None:
    """
    Write one length-prefixed JSON message and flush.

    Args:
        stream: Binary output, normally stdout.
        message: JSON-serialisable value.
    """
    body = json.dumps(message).encode("utf-8")
    stream.write(struct.pack("=I", len(body)))
    stream.write(body)
    stream.flush()


def check_health(token: str, timeout: float = 1.5) -> dict[str, Any] | None:
    """
    Return the /health body when the real backend answers, else None.

    Args:
        token: The install token the backend must prove it knows.
        timeout: Seconds to wait for an answer.

    Returns:
        The /health body, or None when nothing answered.

    Raises:
        auth.ForeignBackend: If another program holds the port.
    """
    return auth.probe(PORT, token, timeout)


def spawn_server() -> subprocess.Popen:
    """
    Start server.py detached with an idle timeout, logging to data/server.log.

    Firefox runs native hosts inside a Windows job object that is killed when the host exits,
    so the server must break away from it. If the job forbids breakaway, the start is retried
    without that flag (the server then lives only as long as this host process tree allows).

    Returns:
        The started process.
    """
    log_path = platform_utils.data_dir() / "server.log"
    args = [sys.executable, str(BACKEND_DIR / "server.py"), "--idle-timeout", str(IDLE_TIMEOUT)]
    with open(log_path, "ab") as log:
        kwargs: dict[str, Any] = {
            "stdin": subprocess.DEVNULL,
            "stdout": log,
            "stderr": subprocess.STDOUT,
            "cwd": str(BACKEND_DIR),
            "close_fds": True,
        }
        if platform_utils.is_windows():
            base = platform_utils.DETACHED_PROCESS | platform_utils.CREATE_NEW_PROCESS_GROUP
            try:
                return subprocess.Popen(
                    args, creationflags=base | platform_utils.CREATE_BREAKAWAY_FROM_JOB, **kwargs
                )
            except OSError:
                return subprocess.Popen(args, creationflags=base, **kwargs)
        return subprocess.Popen(args, start_new_session=True, **kwargs)


def ensure_backend(
    health: Callable[[], dict[str, Any] | None],
    spawn: Callable[[], Any] = spawn_server,
    wait: float = START_WAIT,
    sleep: Callable[[float], None] = time.sleep,
) -> dict[str, Any]:
    """
    Make sure the backend is running, starting it when needed.

    Args:
        health: Health probe, normally check_health bound to the token.
        spawn: Starts the server process.
        wait: Seconds to wait for a freshly started server to answer.
        sleep: time.sleep replacement for tests.

    Returns:
        {status, detail}: status is "running", "started", "starting" or "error". A program
        that answers on the port without proving it knows the token is reported as an error
        rather than trusted.
    """
    try:
        return _ensure_backend(health, spawn, wait, sleep)
    except auth.ForeignBackend as exc:
        return {"status": "error", "detail": str(exc)}


def _ensure_backend(
    health: Callable[[], dict[str, Any] | None],
    spawn: Callable[[], Any],
    wait: float,
    sleep: Callable[[float], None],
) -> dict[str, Any]:
    """Body of ensure_backend; ForeignBackend from the probe propagates to it."""
    current = health()
    if current is not None:
        return {"status": "running", "detail": f"Backend {current.get('version', '')} is running"}
    try:
        spawn()
    except OSError as exc:
        return {"status": "error", "detail": f"Could not start the backend: {exc}"}
    deadline = time.monotonic() + wait
    while time.monotonic() < deadline:
        sleep(0.5)
        current = health()
        if current is not None:
            return {"status": "started", "detail": f"Backend {current.get('version', '')} started"}
    return {"status": "starting", "detail": "Backend was launched but has not answered yet"}


def main() -> int:
    """
    Handle one message from Firefox, reply, and exit.

    Every reply carries the install token, since this host is the only channel that hands it
    to the extension (the host manifest's allowed_extensions limits who can call it).
    """
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    try:
        message = read_message(stdin)
    except ValueError as exc:
        write_message(stdout, {"status": "error", "detail": str(exc)})
        return 1
    try:
        token = auth.load_token(platform_utils.data_dir() / auth.TOKEN_FILE)
    except OSError as exc:
        write_message(stdout, {"status": "error", "detail": f"Could not read the backend key: {exc}"})
        return 1
    command = message.get("command") if isinstance(message, dict) else None
    if command == "ping":
        try:
            running = check_health(token) is not None
            reply: dict[str, Any] = (
                {"status": "running", "detail": "Backend is running"}
                if running
                else {"status": "stopped", "detail": "Backend is not running"}
            )
        except auth.ForeignBackend as exc:
            reply = {"status": "error", "detail": str(exc)}
    else:
        reply = ensure_backend(health=lambda: check_health(token))
    reply["token"] = token
    write_message(stdout, reply)
    # Messages to stderr show in the Browser Console, which helps when debugging startup
    print(f"yt_dl_backend: {reply['status']} (pid {os.getpid()})", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""Per-install shared secret: the extension sends it on every call and the backend proves it."""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import re
import secrets
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

TOKEN_HEADER = "X-YTDL-Token"
TOKEN_FILE = "token"
MAX_NONCE_LENGTH = 128
_TOKEN_RE = re.compile(r"^[A-Za-z0-9_-]{32,128}$")


class ForeignBackend(RuntimeError):
    """Something answered on the backend port but could not prove it knows the token."""


def load_token(path: Path) -> str:
    """
    Read the install token, creating it on first use.

    The file is created exclusively with owner-only permissions (on Windows the user profile's
    ACL does that job), so the server and the native host racing to create it end up sharing
    whichever token was written first. A file holding anything but a token is replaced.

    Args:
        path: Location of the token file, normally data/token.

    Returns:
        The token.

    Raises:
        OSError: If the file can be neither read nor created.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    for _ in range(20):
        try:
            existing = path.read_text(encoding="utf-8").strip()
        except FileNotFoundError:
            existing = None
        if existing and _TOKEN_RE.match(existing):
            return existing
        if existing is None:
            try:
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            except FileExistsError:
                continue
            token = secrets.token_urlsafe(32)
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                handle.write(token)
            return token
        # Empty or partial: another process may be halfway through writing it
        time.sleep(0.05)
    token = secrets.token_urlsafe(32)
    path.write_text(token, encoding="utf-8")
    os.chmod(path, 0o600)
    return token


def token_matches(token: str, supplied: str | None) -> bool:
    """Compare a supplied token with the real one in constant time."""
    if not supplied:
        return False
    return hmac.compare_digest(token.encode("utf-8"), supplied.encode("utf-8"))


def proof(token: str, nonce: str) -> str:
    """Return the hex HMAC-SHA256 of a nonce under the token, as /health reports it."""
    return hmac.new(token.encode("utf-8"), nonce.encode("utf-8"), hashlib.sha256).hexdigest()


def probe(port: int, token: str, timeout: float = 1.5) -> dict[str, Any] | None:
    """
    Ask whatever listens on the backend port for /health and check its proof of the token.

    Args:
        port: Backend port on 127.0.0.1.
        token: The install token.
        timeout: Seconds to wait for an answer.

    Returns:
        The /health body when the real backend answered, or None when nothing answered.

    Raises:
        ForeignBackend: If something answered without a valid proof.
    """
    nonce = secrets.token_hex(16)
    url = f"http://127.0.0.1:{port}/health?{urllib.parse.urlencode({'nonce': nonce})}"
    request = urllib.request.Request(url, headers={"X-YTDL-Client": "1"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - fixed loopback URL
            data = json.load(response)
    except urllib.error.HTTPError as exc:
        raise ForeignBackend(f"Port {port} answered with HTTP {exc.code}") from exc
    except ValueError as exc:
        raise ForeignBackend(f"Port {port} answered with something that is not the backend") from exc
    except OSError:
        return None
    reported = data.get("proof") if isinstance(data, dict) else None
    if not isinstance(reported, str) or not hmac.compare_digest(reported, proof(token, nonce)):
        raise ForeignBackend(f"Port {port} is used by another program, not the yt-dl backend")
    return data

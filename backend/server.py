"""Local FastAPI backend for the yt-dl-firefox extension (127.0.0.1:18956)."""

from __future__ import annotations

import sys
from pathlib import Path

# Make sibling modules importable however the script is launched (any cwd, native host, tests)
sys.path.insert(0, str(Path(__file__).resolve().parent))

import argparse  # noqa: E402
import json  # noqa: E402
import logging  # noqa: E402
import socket  # noqa: E402
import threading  # noqa: E402
import time  # noqa: E402
from contextlib import asynccontextmanager  # noqa: E402
from typing import Any, AsyncIterator, Callable  # noqa: E402

import uvicorn  # noqa: E402
from fastapi import FastAPI, Query, Request  # noqa: E402
from fastapi.exceptions import RequestValidationError  # noqa: E402
from fastapi.middleware.cors import CORSMiddleware  # noqa: E402
from fastapi.responses import JSONResponse  # noqa: E402
from starlette.exceptions import HTTPException as StarletteHTTPException  # noqa: E402

import auth  # noqa: E402
import platform_utils  # noqa: E402
from jobs import JobError, JobQueue  # noqa: E402
from models import JobRequest, Settings, SettingsUpdate, needs_ffmpeg  # noqa: E402
from updater import UpdateBusy, UpdateError, Updater  # noqa: E402
from validation import InvalidItems, InvalidUrl, ParsedUrl, classify_url, parse_items  # noqa: E402
from ytdlp import YtDlpError, YtDlpTimeout, fetch_raw_info, summarise_info  # noqa: E402

VERSION = "2.0.0"
HOST = "127.0.0.1"
PORT = 18956
CLIENT_HEADER = "x-ytdl-client"
ALLOWED_HOSTS = {"127.0.0.1", "localhost"}
INFO_CACHE_TTL = 600.0
INFO_CACHE_MAX = 200
INFO_TIMEOUT = 20.0
# Listing a large channel when queueing it can take far longer than the panel's /info call
EXPAND_TIMEOUT = 120.0

logger = logging.getLogger("server")


class ApiError(Exception):
    """An error answered as {"error", "detail"} with the given status."""

    def __init__(self, status: int, error: str, detail: str | None = None) -> None:
        super().__init__(error)
        self.status = status
        self.error = error
        self.detail = detail


class SettingsStore:
    """Backend settings persisted as JSON so they survive restarts before the extension pushes them."""

    def __init__(self, path: Path | None) -> None:
        self._path = path
        self._lock = threading.Lock()
        self.current = self._load()

    def update(self, changes: SettingsUpdate) -> Settings:
        """
        Merge the fields present in a PUT /settings body and persist the result.

        Args:
            changes: The partial update.

        Returns:
            The full settings after the change.
        """
        with self._lock:
            merged = self.current.model_dump() | changes.model_dump(exclude_none=True)
            self.current = Settings(**merged)
            self._save()
            return self.current

    def _load(self) -> Settings:
        """Read saved settings, falling back to defaults when missing or invalid."""
        if self._path is None:
            return Settings()
        try:
            return Settings(**json.loads(self._path.read_text(encoding="utf-8")))
        except (OSError, ValueError, TypeError):
            return Settings()

    def _save(self) -> None:
        """Write settings to disk (caller holds the lock)."""
        if self._path is None:
            return
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            self._path.write_text(self.current.model_dump_json(), encoding="utf-8")
        except OSError as exc:
            logger.warning("Could not save settings: %s", exc)


class Backend:
    """Everything the routes share: queue, settings, updater, /info cache and request clock."""

    def __init__(
        self,
        *,
        queue: JobQueue | None,
        updater: Updater,
        settings: SettingsStore,
        info_fetcher: Callable[..., dict[str, Any]],
        ffmpeg: Callable[[], bool],
    ) -> None:
        self.settings = settings
        self.updater = updater
        self.ffmpeg = ffmpeg
        self._fetch = info_fetcher
        self._cache: dict[tuple[str, bool], tuple[float, dict[str, Any]]] = {}
        self._cache_lock = threading.Lock()
        self.last_request = time.monotonic()
        current = settings.current
        self.queue = queue or JobQueue(
            concurrency=current.concurrency,
            fragments=current.fragments,
            cookies=current.cookies,
            ffmpeg=ffmpeg,
            info_fetcher=lambda url: self.info(classify_url(url), timeout=EXPAND_TIMEOUT),
            title_lookup=self.cached_title,
        )

    def info(self, parsed: ParsedUrl, timeout: float = INFO_TIMEOUT) -> dict[str, Any]:
        """
        Return the /info summary for a URL, from a 10-minute cache when possible.

        Args:
            parsed: The classified URL.
            timeout: Seconds to allow yt-dlp.

        Returns:
            The summary dict.

        Raises:
            YtDlpError: If yt-dlp fails or times out.
        """
        cookies = self.settings.current.cookies
        key = (parsed.canonical, cookies)
        now = time.monotonic()
        with self._cache_lock:
            hit = self._cache.get(key)
            if hit and now - hit[0] < INFO_CACHE_TTL:
                return hit[1]
        raw = self._fetch(
            parsed.canonical, flat=parsed.kind != "video", cookies=cookies, timeout=timeout
        )
        summary = summarise_info(raw, parsed.kind)
        with self._cache_lock:
            if len(self._cache) >= INFO_CACHE_MAX:
                expired = [k for k, (t, _) in self._cache.items() if now - t >= INFO_CACHE_TTL]
                for k in expired or [min(self._cache, key=lambda k: self._cache[k][0])]:
                    self._cache.pop(k, None)
            self._cache[key] = (now, summary)
        return summary

    def cached_title(self, canonical: str) -> str | None:
        """Return a video title already known from /info, without calling yt-dlp."""
        with self._cache_lock:
            hit = self._cache.get((canonical, self.settings.current.cookies))
        return hit[1].get("title") if hit else None

    def run_update(self) -> dict[str, Any]:
        """
        Update yt-dlp with the queue held, so no download starts against a half-installed package.

        Returns:
            {updated, version} from Updater.update.

        Raises:
            UpdateBusy: If a download is running.
            UpdateError: If pip fails.
        """
        with self.queue.hold():
            if self.queue.running():
                raise UpdateBusy("Wait for downloads to finish before updating yt-dlp")
            result = self.updater.update()
        with self._cache_lock:
            self._cache.clear()
        return result

    def health(self) -> dict[str, Any]:
        """Return the GET /health body."""
        settings = self.settings.current
        return {
            "status": "ok",
            "version": VERSION,
            "ffmpeg": self.ffmpeg(),
            "yt_dlp_version": self.updater.installed(),
            "yt_dlp_latest": self.updater.latest,
            "update_available": self.updater.update_available(),
            "concurrency": settings.concurrency,
            "fragments": settings.fragments,
            "cookies": settings.cookies,
            "auto_update": settings.auto_update,
        }


def _error(status: int, error: str, detail: str | None = None) -> JSONResponse:
    """Build the standard error response."""
    body: dict[str, Any] = {"error": error}
    if detail:
        body["detail"] = detail
    return JSONResponse(status_code=status, content=body)


def _host_allowed(host_header: str) -> bool:
    """Accept only loopback Host headers, which blocks DNS-rebinding pages."""
    host = host_header.rsplit(":", 1)[0] if not host_header.startswith("[") else host_header
    return host.lower() in ALLOWED_HOSTS


def create_app(
    *,
    queue: JobQueue | None = None,
    updater: Updater | None = None,
    info_fetcher: Callable[..., dict[str, Any]] = fetch_raw_info,
    ffmpeg: Callable[[], bool] = platform_utils.ffmpeg_available,
    settings_path: Path | None = None,
    background_tasks: bool = False,
    token: str | None = None,
) -> FastAPI:
    """
    Build the FastAPI app.

    Args:
        queue: Job queue to use; one is built from the saved settings when omitted.
        updater: yt-dlp updater; defaults to one storing state in data/update_state.json.
        info_fetcher: fetch_raw_info replacement for tests.
        ffmpeg: Returns whether ffmpeg is available.
        settings_path: Where settings persist; None keeps them in memory only.
        background_tasks: Run the startup update check (off in tests).
        token: Install token every call except /health must carry; None turns the check off
            (tests only).

    Returns:
        The configured app; its Backend is on app.state.backend.
    """
    if updater is None:
        updater = Updater(platform_utils.data_dir() / "update_state.json")
    backend = Backend(
        queue=queue,
        updater=updater,
        settings=SettingsStore(settings_path),
        info_fetcher=info_fetcher,
        ffmpeg=ffmpeg,
    )

    @asynccontextmanager
    async def lifespan(_app: FastAPI) -> AsyncIterator[None]:
        if background_tasks:
            threading.Thread(
                target=backend.updater.auto_update,
                args=(lambda: backend.settings.current.auto_update, backend.run_update),
                name="auto-update",
                daemon=True,
            ).start()
        yield
        backend.queue.shutdown()

    app = FastAPI(title="yt-dl-firefox backend", version=VERSION, lifespan=lifespan)
    app.state.backend = backend
    app.add_middleware(
        CORSMiddleware,
        allow_origin_regex=r"^moz-extension://[0-9a-fA-F-]+$",
        allow_methods=["GET", "POST", "PUT", "DELETE"],
        allow_headers=["X-YTDL-Client", auth.TOKEN_HEADER, "Content-Type"],
    )

    @app.middleware("http")
    async def guard(request: Request, call_next: Callable) -> Any:
        """
        Reject requests that could come from a web page.

        A custom header forces a CORS preflight that only moz-extension:// origins pass, and any
        other Origin is refused outright. Preflights themselves carry no custom header, so they
        go through to the CORS middleware once the Origin has been checked.

        Every other extension shares the moz-extension:// scheme, and local programs send no
        Origin at all, so the install token is what limits the API to this extension. /health is
        exempt: the extension checks the backend's proof there before it sends the token.
        """
        if not _host_allowed(request.headers.get("host", "")):
            return _error(403, "Forbidden host")
        origin = request.headers.get("origin")
        if origin is not None and not origin.startswith("moz-extension://"):
            return _error(403, "Forbidden origin")
        if request.method != "OPTIONS" and request.headers.get(CLIENT_HEADER) != "1":
            return _error(403, "Missing X-YTDL-Client header")
        if (
            token is not None
            and request.method != "OPTIONS"
            and request.url.path != "/health"
            and not auth.token_matches(token, request.headers.get(auth.TOKEN_HEADER))
        ):
            return _error(401, "Backend key rejected", "Reload the extension so it fetches the key again.")
        backend.last_request = time.monotonic()
        return await call_next(request)

    @app.exception_handler(ApiError)
    async def api_error_handler(_request: Request, exc: ApiError) -> JSONResponse:
        return _error(exc.status, exc.error, exc.detail)

    @app.exception_handler(StarletteHTTPException)
    async def http_error_handler(_request: Request, exc: StarletteHTTPException) -> JSONResponse:
        return _error(exc.status_code, str(exc.detail))

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        _request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        problems = "; ".join(
            f"{'.'.join(str(p) for p in err.get('loc', ()))}: {err.get('msg')}" for err in exc.errors()
        )
        return _error(400, "Invalid request", problems[:500])

    def classify(url: str) -> ParsedUrl:
        try:
            return classify_url(url)
        except InvalidUrl as exc:
            raise ApiError(400, str(exc)) from exc

    def yt_error(exc: YtDlpError) -> ApiError:
        if isinstance(exc, YtDlpTimeout):
            return ApiError(504, exc.message, exc.detail)
        return ApiError(502, exc.message, exc.detail)

    @app.get("/health")
    def health(nonce: str | None = Query(None, max_length=auth.MAX_NONCE_LENGTH)) -> dict[str, Any]:
        body = backend.health()
        if token is not None and nonce:
            body["proof"] = auth.proof(token, nonce)
        return body

    @app.get("/info")
    def info(url: str = Query(max_length=2048)) -> dict[str, Any]:
        parsed = classify(url)
        try:
            return backend.info(parsed)
        except YtDlpError as exc:
            raise yt_error(exc) from exc

    @app.post("/jobs", status_code=202)
    def create_jobs(body: JobRequest) -> dict[str, Any]:
        parsed = classify(body.url)
        if needs_ffmpeg(body.options) and not backend.ffmpeg():
            raise ApiError(
                422,
                "This download needs ffmpeg",
                "Install ffmpeg and add it to PATH for MP3 and video above 720p.",
            )
        try:
            ranges = parse_items(body.items) if parsed.kind != "video" else None
        except InvalidItems as exc:
            raise ApiError(400, str(exc)) from exc
        try:
            jobs = backend.queue.create(parsed.canonical, body.options, ranges)
        except InvalidUrl as exc:
            raise ApiError(400, str(exc)) from exc
        except JobError as exc:
            raise ApiError(exc.status, exc.message, exc.detail) from exc
        except YtDlpError as exc:
            raise yt_error(exc) from exc
        return {"jobs": jobs}

    @app.get("/jobs")
    def list_jobs() -> dict[str, Any]:
        return {"jobs": backend.queue.list_jobs()}

    @app.delete("/jobs")
    def clear_jobs(finished: bool = False) -> dict[str, Any]:
        if not finished:
            raise ApiError(400, "Only finished=true is supported")
        return {"removed": backend.queue.clear_finished()}

    @app.post("/jobs/{job_id}/{action}")
    def job_action(job_id: str, action: str) -> dict[str, Any]:
        queue = backend.queue
        actions: dict[str, Callable[[str], dict[str, Any]]] = {
            "pause": queue.pause,
            "resume": queue.resume,
            "cancel": queue.cancel,
            "retry": queue.retry,
        }
        try:
            if action in actions:
                return {"job": actions[action](job_id)}
            if action in ("open", "reveal"):
                path = queue.finished_file(job_id)
                try:
                    (platform_utils.open_file if action == "open" else platform_utils.reveal)(path)
                except OSError as exc:
                    raise ApiError(409, "Could not open the file", str(exc)) from exc
                return {"job": queue.snapshot(job_id)}
        except JobError as exc:
            raise ApiError(exc.status, exc.message, exc.detail) from exc
        raise ApiError(404, "Unknown action")

    @app.put("/settings")
    def put_settings(body: SettingsUpdate) -> dict[str, Any]:
        settings = backend.settings.update(body)
        backend.queue.apply_settings(
            concurrency=settings.concurrency,
            fragments=settings.fragments,
            cookies=settings.cookies,
        )
        return settings.model_dump()

    @app.post("/update")
    def update() -> dict[str, Any]:
        try:
            return backend.run_update()
        except UpdateBusy as exc:
            raise ApiError(409, exc.message) from exc
        except UpdateError as exc:
            raise ApiError(502, exc.message, exc.detail) from exc

    return app


def bind_socket(host: str = HOST, port: int = PORT) -> socket.socket | None:
    """
    Bind the listening socket, or return None if another instance holds the port.

    Binding before uvicorn starts turns "port in use" into a clean exit instead of a crash.
    SO_EXCLUSIVEADDRUSE on Windows stops another process binding the same port alongside.

    Args:
        host: Interface to bind.
        port: Port to bind.

    Returns:
        The bound socket, or None when the port is taken.
    """
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        if hasattr(socket, "SO_EXCLUSIVEADDRUSE"):
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
        sock.bind((host, port))
    except OSError:
        sock.close()
        return None
    sock.set_inheritable(False)
    return sock


def idle_expired(last_request: float, now: float, timeout: float, busy: bool) -> bool:
    """
    Return True when the server should exit for being idle.

    Args:
        last_request: Monotonic time of the last accepted request.
        now: Current monotonic time.
        timeout: Idle limit in seconds; 0 or less disables the check.
        busy: Whether any job is queued or running.

    Returns:
        Whether to shut down.
    """
    return timeout > 0 and not busy and now - last_request >= timeout


def _watch_idle(server: uvicorn.Server, backend: Backend, timeout: float) -> None:
    """Stop the server once no requests arrived for `timeout` seconds and nothing is downloading."""
    while not server.should_exit:
        time.sleep(min(5.0, max(timeout / 4, 0.1)))
        if idle_expired(backend.last_request, time.monotonic(), timeout, backend.queue.busy()):
            logger.info("No requests for %ss and no active downloads; exiting", timeout)
            server.should_exit = True


def main(argv: list[str] | None = None) -> int:
    """
    Run the server.

    Args:
        argv: Command-line arguments (defaults to sys.argv[1:]).

    Returns:
        The process exit code.
    """
    parser = argparse.ArgumentParser(description="yt-dl-firefox backend")
    parser.add_argument(
        "--idle-timeout",
        type=float,
        default=0,
        help="exit after this many seconds without requests or active downloads (0 = never)",
    )
    parser.add_argument("--port", type=int, default=PORT, help=argparse.SUPPRESS)
    args = parser.parse_args(argv)

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    try:
        token = auth.load_token(platform_utils.data_dir() / auth.TOKEN_FILE)
    except OSError as exc:
        logger.error("Could not read or create the backend key: %s", exc)
        return 1

    sock = bind_socket(HOST, args.port)
    if sock is None:
        try:
            running = auth.probe(args.port, token)
        except auth.ForeignBackend as exc:
            logger.warning("%s; the extension will refuse it until that program stops", exc)
            return 0
        if running is not None:
            logger.info("Port %s is already in use by another backend", args.port)
        else:
            logger.warning("Port %s is in use but nothing answered as the backend", args.port)
        return 0

    app = create_app(
        settings_path=platform_utils.data_dir() / "settings.json",
        background_tasks=True,
        token=token,
    )
    backend: Backend = app.state.backend
    config = uvicorn.Config(app, log_level="warning", access_log=False)
    server = uvicorn.Server(config)
    if args.idle_timeout > 0:
        threading.Thread(
            target=_watch_idle, args=(server, backend, args.idle_timeout), daemon=True
        ).start()
    logger.info(
        "Backend %s on http://%s:%s (ffmpeg: %s)", VERSION, HOST, args.port, backend.ffmpeg()
    )
    server.run(sockets=[sock])
    return 0


if __name__ == "__main__":
    sys.exit(main())

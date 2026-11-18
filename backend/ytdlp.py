"""yt-dlp wrapper: command building, metadata fetching and progress-output parsing."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Callable

from models import JobOptions
from platform_utils import hidden_popen_kwargs

PROGRESS_PREFIX = "YTDLP_PROGRESS "
FILE_PREFIX = "YTDLP_FILE "
TITLE_PREFIX = "YTDLP_TITLE "

OUTPUT_TEMPLATE = "%(title)s [%(id)s].%(ext)s"
# Written to the comment tag of video files; "|" fallbacks keep it readable when a field is missing
VIDEO_SUMMARY_TEMPLATE = "%(width|?)sx%(height|?)s, %(fps|?)s fps, %(vcodec|unknown)s"

# Fallback for plain "[download]  45.2% of 12.34MiB at 1.23MiB/s ETA 00:30" lines
PROGRESS_REGEX = re.compile(r"^\[download\]\s+(\d+(?:\.\d+)?)%")
DESTINATION_REGEX = re.compile(r"^\[download\] Destination: (.+)$")
# A resumed merge download prints this instead of a Destination line for a finished stream
ALREADY_DOWNLOADED_REGEX = re.compile(r"^\[download\] (.+) has already been downloaded")
MERGER_REGEX = re.compile(r'^\[Merger\] Merging formats into "(.+)"$')
PROCESSING_PREFIXES = ("[Merger]", "[ExtractAudio]", "[VideoRemuxer]", "[VideoConvertor]")

CODEC_SORT = {"h264": "vcodec:h264", "vp9": "vcodec:vp9", "av1": "vcodec:av01"}

INFO_TIMEOUT = 20.0
TAIL_LINES = 20


class YtDlpError(RuntimeError):
    """yt-dlp exited with an error; the message is the last ERROR: line."""

    def __init__(self, message: str, detail: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.detail = detail


class YtDlpTimeout(YtDlpError):
    """yt-dlp did not answer within the allowed time."""


def base_command() -> list[str]:
    """Return the interpreter prefix that runs the installed yt-dlp module."""
    return [sys.executable, "-m", "yt_dlp"]


def common_flags(cookies: bool) -> list[str]:
    """
    Return flags shared by downloads and metadata calls.

    --ignore-config keeps a user's own yt-dlp config from changing output the parser relies on.

    Args:
        cookies: Whether to read the Firefox cookie store.

    Returns:
        The flag list.
    """
    flags = ["--ignore-config", "--no-update", "--color", "never"]
    if cookies:
        flags += ["--cookies-from-browser", "firefox"]
    return flags


def build_args(
    options: JobOptions,
    url: str,
    out_dir: str,
    cookies: bool,
    *,
    fragments: int = 4,
    ffmpeg: bool = True,
) -> list[str]:
    """
    Build the full yt-dlp command line for one download.

    Video sorts by resolution first (capped at the chosen quality) and then by codec, so a codec
    preference never lowers the resolution. MP4 also prefers AAC audio, so the merge is a plain
    remux. Without ffmpeg only single-file formats are requested, since nothing could merge them.
    With ffmpeg every file also gets embedded tags (title, channel, date, description, URL), and
    videos get a "1920x1080, 30 fps, avc1" summary in the comment tag.

    Args:
        options: The validated download options.
        url: The canonical URL from validation.classify_url.
        out_dir: Directory the finished file goes into.
        cookies: Whether to pass the Firefox cookie store.
        fragments: Number of DASH/HLS fragments to fetch concurrently (1-8).
        ffmpeg: Whether ffmpeg is available for merging and conversion.

    Returns:
        The argument list, starting with the Python interpreter.
    """
    args = base_command() + common_flags(cookies)
    args += [
        "--no-playlist",
        "--windows-filenames",
        "--no-mtime",
        "--newline",
        "--continue",
        "--progress",
        "--no-quiet",
        "--progress-delta",
        "0.5",
        "--concurrent-fragments",
        str(fragments),
        "--progress-template",
        f"download:{PROGRESS_PREFIX}%(progress)j",
        "--print",
        f"before_dl:{TITLE_PREFIX}%(title)s",
        "--print",
        f"after_move:{FILE_PREFIX}%(filepath)s",
        "-o",
        os.path.join(out_dir, OUTPUT_TEMPLATE),
    ]

    if options.kind == "audio":
        if options.audio_format == "mp3":
            args += ["-f", "ba/b", "-x", "--audio-format", "mp3"]
            args += ["--audio-quality", f"{options.audio_bitrate}K"]
        elif ffmpeg:
            # No --audio-quality here: a quality setting would force a re-encode of m4a sources
            args += ["-f", "ba[ext=m4a]/ba/b", "-x", "--audio-format", "m4a"]
        else:
            args += ["-f", "ba[ext=m4a]"]
    else:
        sort = ["res" if options.quality == "best" else f"res:{options.quality}"]
        if options.codec in CODEC_SORT:
            sort.append(CODEC_SORT[options.codec])
        if options.container == "mp4":
            sort.append("acodec:aac")
        if ffmpeg:
            args += ["-f", "bv*+ba/b", "--merge-output-format", options.container]
        else:
            args += ["-f", "b"]
        args += ["-S", ",".join(sort)]
        if ffmpeg:
            # Readable stream summary in the comment tag, which Explorer's Details tab shows.
            # before_dl runs after format selection, so width/height/fps/vcodec are known; the
            # template must not contain ":" because yt-dlp splits WHEN:FROM:TO on it
            args += ["--parse-metadata", f"before_dl:{VIDEO_SUMMARY_TEMPLATE}:(?P<meta_comment>.+)"]

    if ffmpeg:
        # Title, channel (as artist), upload date, description and the video URL
        args += ["--embed-metadata"]

    args += ["--", url]
    return args


def fetch_raw_info(
    url: str,
    *,
    flat: bool,
    cookies: bool = False,
    timeout: float = INFO_TIMEOUT,
    runner: Callable[..., subprocess.CompletedProcess] = subprocess.run,
) -> dict[str, Any]:
    """
    Run yt-dlp -J and return the parsed JSON.

    Args:
        url: The canonical URL.
        flat: List playlist/channel entries without resolving each video.
        cookies: Whether to pass the Firefox cookie store.
        timeout: Seconds before giving up.
        runner: subprocess.run replacement for tests.

    Returns:
        The raw yt-dlp info dict.

    Raises:
        YtDlpTimeout: If yt-dlp takes longer than the timeout.
        YtDlpError: If yt-dlp fails or prints invalid JSON.
    """
    args = base_command() + common_flags(cookies) + ["-J", "--no-warnings"]
    args += ["--flat-playlist"] if flat else ["--no-playlist"]
    args += ["--", url]
    try:
        result = runner(
            args,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
            env=child_env(),
            stdin=subprocess.DEVNULL,
            **hidden_popen_kwargs(),
        )
    except subprocess.TimeoutExpired as exc:
        raise YtDlpTimeout("YouTube took too long to answer") from exc
    except OSError as exc:
        raise YtDlpError("Could not run yt-dlp", str(exc)) from exc
    if result.returncode != 0:
        stderr = result.stderr or ""
        raise YtDlpError(last_error(stderr) or "yt-dlp failed", tail(stderr))
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise YtDlpError("yt-dlp returned unreadable data") from exc
    if not isinstance(data, dict):
        raise YtDlpError("yt-dlp returned unreadable data")
    return data


def child_env() -> dict[str, str]:
    """Return the environment for yt-dlp children, forcing UTF-8 output so titles survive."""
    env = dict(os.environ)
    env["PYTHONIOENCODING"] = "utf-8"
    env["PYTHONUTF8"] = "1"
    env["PYTHONUNBUFFERED"] = "1"
    return env


def last_error(text: str) -> str | None:
    """
    Return the last "ERROR:" line from yt-dlp output, without the prefix.

    Args:
        text: Combined yt-dlp output.

    Returns:
        The message, or None when no error line exists.
    """
    for line in reversed(text.splitlines()):
        line = line.strip()
        if line.startswith("ERROR:"):
            return clean_error(line)
    return None


def clean_error(line: str) -> str:
    """Strip the "ERROR:" and "[extractor] id:" prefixes yt-dlp puts on error lines."""
    message = line[len("ERROR:"):].strip() if line.startswith("ERROR:") else line.strip()
    message = re.sub(r"^\[[\w:]+\]\s+[\w-]+:\s*", "", message)
    return message[:300]


def tail(text: str, lines: int = TAIL_LINES) -> str:
    """Return the last few lines of output as one string."""
    return "\n".join(text.strip().splitlines()[-lines:])


def _normalise_codec(vcodec: str) -> str:
    """Map yt-dlp codec strings (avc1.64001F, vp09.00..., av01...) to h264/vp9/av1."""
    codec = vcodec.lower()
    if codec.startswith(("avc", "h264")):
        return "h264"
    if codec.startswith(("vp9", "vp09")):
        return "vp9"
    if codec.startswith(("av01", "av1")):
        return "av1"
    return codec.split(".")[0]


def summarise_info(raw: dict[str, Any], kind: str) -> dict[str, Any]:
    """
    Reduce raw yt-dlp JSON to the /info response shape.

    Size estimates add the largest audio stream to each video height's largest stream, since a
    download at that height merges the two.

    Args:
        raw: Output of fetch_raw_info.
        kind: The URL kind from validation ("video", "playlist" or "channel").

    Returns:
        {kind, id, title, uploader, duration, qualities, entry_count, entries}.
    """
    summary: dict[str, Any] = {
        "kind": kind,
        "id": raw.get("id"),
        "title": raw.get("title"),
        "uploader": raw.get("uploader") or raw.get("channel"),
        "duration": raw.get("duration"),
        "qualities": [],
        "entry_count": None,
        "entries": None,
    }
    if kind == "video":
        summary["qualities"] = _qualities(raw.get("formats") or [])
        return summary

    entries = [
        {"id": e.get("id"), "title": e.get("title")}
        for e in raw.get("entries") or []
        if isinstance(e, dict) and e.get("id")
    ]
    summary["entries"] = entries
    summary["entry_count"] = len(entries)
    summary["duration"] = None
    return summary


def _qualities(formats: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """
    Group video formats by resolution into [{height, codecs, filesize_est}], highest first.

    The "height" key is the shorter side, the same measure yt-dlp's -S res:N sorts by, so a
    vertical 1080x1920 Short reports 1080 and matches the 1080p preset the download will use.
    """
    audio_size = max(
        (
            f.get("filesize") or f.get("filesize_approx") or 0
            for f in formats
            if f.get("vcodec") == "none" and f.get("acodec") not in (None, "none")
        ),
        default=0,
    )
    by_height: dict[int, dict[str, Any]] = {}
    for fmt in formats:
        vcodec = fmt.get("vcodec")
        height = fmt.get("height")
        if not vcodec or vcodec == "none" or not isinstance(height, int) or height <= 0:
            continue
        width = fmt.get("width")
        if isinstance(width, int) and width > 0:
            height = min(width, height)
        entry = by_height.setdefault(height, {"height": height, "codecs": [], "size": 0})
        codec = _normalise_codec(vcodec)
        if codec not in entry["codecs"]:
            entry["codecs"].append(codec)
        size = fmt.get("filesize") or fmt.get("filesize_approx") or 0
        # Progressive formats already include audio; video-only ones get the audio added
        if fmt.get("acodec") in (None, "none"):
            size = size + audio_size if size else 0
        entry["size"] = max(entry["size"], size)
    return [
        {"height": e["height"], "codecs": sorted(e["codecs"]), "filesize_est": e["size"] or None}
        for e in sorted(by_height.values(), key=lambda e: e["height"], reverse=True)
    ]


def format_speed(speed: float | None) -> str | None:
    """Format bytes/second as "2.5 MB/s"; None when unknown."""
    if not speed or speed <= 0:
        return None
    value = float(speed)
    for unit in ("B/s", "KB/s", "MB/s", "GB/s"):
        if value < 1024 or unit == "GB/s":
            return f"{value:.0f} {unit}" if unit == "B/s" else f"{value:.1f} {unit}"
        value /= 1024
    return None


def format_eta(eta: float | None) -> str | None:
    """Format seconds as "m:ss" or "h:mm:ss"; None when unknown."""
    if eta is None or eta < 0:
        return None
    seconds = int(eta)
    hours, rest = divmod(seconds, 3600)
    minutes, secs = divmod(rest, 60)
    return f"{hours}:{minutes:02d}:{secs:02d}" if hours else f"{minutes}:{secs:02d}"


@dataclass
class ParseUpdate:
    """Changes to apply to a job after one line of output."""

    percent: float | None = None
    speed: str | None = None
    eta: str | None = None
    processing: bool = False
    title: str | None = None
    filepath: str | None = None


@dataclass
class OutputParser:
    """
    Stateful parser for one yt-dlp run's combined stdout/stderr.

    Progress stays monotonic across separate streams: with expected_streams=2 (video plus audio
    to merge) the first stream fills 0-90% and the second 90-100%, matching their usual size
    ratio. A third stream (rare) is folded into the last slot, and percentages never go back.

    Attributes:
        expected_streams: How many files yt-dlp is expected to download before post-processing.
        percent: Overall progress so far, 0-100.
        filepath: Final path from the after_move print, or the merge/destination path as fallback.
        last_error: Last ERROR: message seen.
    """

    expected_streams: int = 1
    percent: float = 0.0
    filepath: str | None = None
    last_error: str | None = None
    destinations: list[str] = field(default_factory=list)
    _stream: int = -1
    _tail: deque = field(default_factory=lambda: deque(maxlen=TAIL_LINES))

    def detail(self) -> str:
        """Return the last 20 non-progress lines for error details."""
        return "\n".join(self._tail)

    def feed(self, raw_line: str) -> ParseUpdate | None:
        """
        Parse one output line.

        Args:
            raw_line: A line of yt-dlp output, with or without the newline.

        Returns:
            The changes it implies, or None when the line carries nothing useful.
        """
        line = raw_line.rstrip("\r\n")
        if not line.strip():
            return None
        if line.startswith(PROGRESS_PREFIX):
            return self._progress_json(line[len(PROGRESS_PREFIX):])

        self._tail.append(line)

        if line.startswith(FILE_PREFIX):
            self.filepath = line[len(FILE_PREFIX):].strip()
            return ParseUpdate(filepath=self.filepath)
        if line.startswith(TITLE_PREFIX):
            return ParseUpdate(title=line[len(TITLE_PREFIX):].strip() or None)
        if line.startswith("ERROR:"):
            self.last_error = clean_error(line)
            return None

        match = DESTINATION_REGEX.match(line)
        if match:
            self._stream += 1
            self.destinations.append(match.group(1).strip())
            if self.filepath is None:
                self.filepath = match.group(1).strip()
            return None

        match = ALREADY_DOWNLOADED_REGEX.match(line)
        if match:
            # Count the finished stream so the next one maps onto its own slot
            self._stream += 1
            self.destinations.append(match.group(1).strip())
            if self.filepath is None:
                self.filepath = match.group(1).strip()
            return ParseUpdate(percent=self._overall(100.0))

        match = MERGER_REGEX.match(line)
        if match:
            self.filepath = match.group(1).strip()
            return ParseUpdate(processing=True, filepath=self.filepath)

        if line.startswith(PROCESSING_PREFIXES):
            return ParseUpdate(processing=True)

        match = PROGRESS_REGEX.match(line)
        if match:
            return ParseUpdate(percent=self._overall(float(match.group(1))))
        return None

    def _progress_json(self, payload: str) -> ParseUpdate | None:
        """Turn one --progress-template JSON payload into an update."""
        try:
            data = json.loads(payload)
        except json.JSONDecodeError:
            return None
        if not isinstance(data, dict):
            return None
        stream_percent = data.get("_percent")
        if not isinstance(stream_percent, (int, float)):
            done = data.get("downloaded_bytes")
            total = data.get("total_bytes") or data.get("total_bytes_estimate")
            if isinstance(done, (int, float)) and isinstance(total, (int, float)) and total > 0:
                stream_percent = done / total * 100
            elif data.get("fragment_count"):
                stream_percent = (data.get("fragment_index") or 0) / data["fragment_count"] * 100
            else:
                stream_percent = None
        finished = data.get("status") == "finished"
        if finished:
            stream_percent = 100.0
        update = ParseUpdate(
            speed=None if finished else format_speed(data.get("speed")),
            eta=None if finished else format_eta(data.get("eta")),
        )
        if stream_percent is not None:
            update.percent = self._overall(float(stream_percent))
        return update

    def _overall(self, stream_percent: float) -> float:
        """Map one stream's percentage onto overall progress, never moving backwards."""
        stream_percent = min(max(stream_percent, 0.0), 100.0)
        index = max(self._stream, 0)
        if self.expected_streams >= 2:
            if index == 0:
                overall = stream_percent * 0.9
            else:
                overall = 90.0 + stream_percent * 0.1
        else:
            overall = stream_percent
        self.percent = round(max(self.percent, min(overall, 100.0)), 1)
        return self.percent


def expected_streams(options: JobOptions, ffmpeg: bool) -> int:
    """Return how many files a download fetches before post-processing (2 when merging)."""
    return 2 if options.kind == "video" and ffmpeg else 1

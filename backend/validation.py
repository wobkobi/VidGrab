"""YouTube URL allowlist/classification and playlist item-range parsing."""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from typing import Literal
from urllib.parse import parse_qs, quote, unquote, urlsplit

UrlKind = Literal["video", "playlist", "channel"]

VIDEO_ID_RE = re.compile(r"^[A-Za-z0-9_-]{11}$")
PLAYLIST_ID_RE = re.compile(r"^[A-Za-z0-9_-]{2,64}$")
CHANNEL_ID_RE = re.compile(r"^UC[A-Za-z0-9_-]{22}$")
# Handles are 3-30 chars in practice; the wider bound tolerates legacy names under /c/ and /user/
MAX_CHANNEL_NAME = 100
# Besides Unicode letters, marks and digits, handles may contain these (U+00B7 is the middle dot)
CHANNEL_NAME_PUNCTUATION = frozenset("._-·")

YOUTUBE_HOSTS = {"youtube.com", "www.youtube.com", "m.youtube.com"}
SHORT_HOSTS = {"youtu.be", "www.youtu.be"}

# Channel tabs kept as-is; the channel root and "featured" map to /videos so yt-dlp returns
# one flat list instead of a list of tabs
CHANNEL_TABS = {"videos", "shorts", "streams"}
CHANNEL_TABS_AS_VIDEOS = {"", "featured"}

MAX_URL_LENGTH = 2048
MAX_ITEMS_SPEC_LENGTH = 200
MAX_ITEM_INDEX = 100_000


class InvalidUrl(ValueError):
    """Raised when a URL is not an accepted YouTube video, playlist or channel URL."""


class InvalidItems(ValueError):
    """Raised when an item range such as "1-10,15" cannot be parsed."""


@dataclass(frozen=True)
class ParsedUrl:
    """A validated YouTube URL rebuilt from its checked parts."""

    kind: UrlKind
    id: str
    canonical: str


def watch_url(video_id: str) -> str:
    """
    Build the canonical watch URL for a video ID.

    Args:
        video_id: An 11-character YouTube video ID.

    Returns:
        The https://www.youtube.com/watch?v= URL.

    Raises:
        InvalidUrl: If the ID is not a valid video ID.
    """
    if not VIDEO_ID_RE.match(video_id or ""):
        raise InvalidUrl("Invalid video ID")
    return f"https://www.youtube.com/watch?v={video_id}"


def classify_url(url: str) -> ParsedUrl:
    """
    Validate a YouTube URL and classify it as a video, playlist or channel.

    The returned canonical URL is rebuilt from regex-checked parts, so nothing else from the
    input ever reaches yt-dlp.

    Args:
        url: The URL sent by the extension.

    Returns:
        The classification with the extracted ID and canonical URL.

    Raises:
        InvalidUrl: If the URL is not an accepted YouTube URL.
    """
    if not isinstance(url, str) or not url.strip():
        raise InvalidUrl("URL is required")
    url = url.strip()
    if len(url) > MAX_URL_LENGTH or any(ord(ch) < 0x21 or ch == "\x7f" for ch in url):
        raise InvalidUrl("Not a valid YouTube URL")
    try:
        parts = urlsplit(url)
    except ValueError as exc:
        raise InvalidUrl("Not a valid YouTube URL") from exc
    if parts.scheme not in ("http", "https"):
        raise InvalidUrl("Not a valid YouTube URL")
    if parts.username or parts.password or parts.port:
        raise InvalidUrl("Not a valid YouTube URL")
    host = (parts.hostname or "").lower()
    path = parts.path or "/"
    query = parse_qs(parts.query)

    if host in SHORT_HOSTS:
        segments = [s for s in path.split("/") if s]
        if len(segments) == 1 and VIDEO_ID_RE.match(segments[0]):
            return ParsedUrl("video", segments[0], watch_url(segments[0]))
        raise InvalidUrl("Not a valid YouTube URL")

    if host not in YOUTUBE_HOSTS:
        raise InvalidUrl("Only youtube.com and youtu.be URLs are supported")

    segments = [s for s in path.split("/") if s]

    if path in ("/watch", "/watch/"):
        video_id = _single(query, "v")
        if video_id is not None:
            if VIDEO_ID_RE.match(video_id):
                return ParsedUrl("video", video_id, watch_url(video_id))
            raise InvalidUrl("Invalid video ID")
        return _playlist(query)

    if path in ("/playlist", "/playlist/"):
        return _playlist(query)

    if len(segments) == 2 and segments[0] in ("shorts", "embed", "v", "live"):
        if VIDEO_ID_RE.match(segments[1]):
            return ParsedUrl("video", segments[1], watch_url(segments[1]))
        raise InvalidUrl("Invalid video ID")

    return _channel(segments)


def _single(query: dict[str, list[str]], key: str) -> str | None:
    """Return a query parameter that must appear at most once, or None when absent."""
    values = query.get(key)
    if not values:
        return None
    if len(values) > 1:
        raise InvalidUrl(f"Duplicate '{key}' parameter")
    return values[0]


def _playlist(query: dict[str, list[str]]) -> ParsedUrl:
    """Classify a /playlist or /watch URL that carries only a list= parameter."""
    list_id = _single(query, "list")
    if list_id is None or not PLAYLIST_ID_RE.match(list_id):
        raise InvalidUrl("Invalid playlist URL")
    return ParsedUrl("playlist", list_id, f"https://www.youtube.com/playlist?list={list_id}")


def _channel(segments: list[str]) -> ParsedUrl:
    """Classify /@handle, /channel/UC..., /c/name and /user/name URLs with an optional tab."""
    if not segments:
        raise InvalidUrl("Not a video, playlist or channel URL")

    first = segments[0]
    if first.startswith("@"):
        handle, encoded = _channel_name(first[1:], "Invalid channel handle")
        base, ident, rest = f"@{encoded}", f"@{handle}", segments[1:]
    elif first == "channel" and len(segments) >= 2:
        if not CHANNEL_ID_RE.match(segments[1]):
            raise InvalidUrl("Invalid channel ID")
        base, ident, rest = f"channel/{segments[1]}", segments[1], segments[2:]
    elif first in ("c", "user") and len(segments) >= 2:
        name, encoded = _channel_name(segments[1], "Invalid channel name")
        base, ident, rest = f"{first}/{encoded}", name, segments[2:]
    else:
        raise InvalidUrl("Not a video, playlist or channel URL")

    if len(rest) > 1:
        raise InvalidUrl("Unsupported channel page")
    tab = rest[0].lower() if rest else ""
    if tab in CHANNEL_TABS_AS_VIDEOS:
        tab = "videos"
    elif tab not in CHANNEL_TABS:
        raise InvalidUrl("Unsupported channel page")
    return ParsedUrl("channel", ident, f"https://www.youtube.com/{base}/{tab}")


def _channel_name(segment: str, message: str) -> tuple[str, str]:
    """
    Validate a channel handle or legacy name, which may be percent-encoded Unicode.

    The segment is decoded once and must then start with a letter, digit or underscore and hold
    only letters, combining marks, digits and CHANNEL_NAME_PUNCTUATION. That rules out "/", "%",
    "?" and dot-only names such as "..", so the re-encoded name cannot add path segments or
    dot-segments to the canonical URL.

    Args:
        segment: The raw path segment, without any leading "@".
        message: Error text for a rejected name.

    Returns:
        The decoded name and its percent-encoded form for the canonical URL.

    Raises:
        InvalidUrl: If the name is empty, too long, not UTF-8 or has a disallowed character.
    """
    try:
        name = unquote(segment, errors="strict")
    except UnicodeDecodeError as exc:
        raise InvalidUrl(message) from exc
    if not 0 < len(name) <= MAX_CHANNEL_NAME:
        raise InvalidUrl(message)
    if name[0] != "_" and unicodedata.category(name[0])[0] not in "LN":
        raise InvalidUrl(message)
    for ch in name:
        if ch not in CHANNEL_NAME_PUNCTUATION and unicodedata.category(ch)[0] not in "LMN":
            raise InvalidUrl(message)
    return name, quote(name, safe="")


_ITEM_PART_RE = re.compile(r"^(\d+)(?:\s*-\s*(\d+))?$")


def parse_items(spec: str | None) -> list[tuple[int, int]] | None:
    """
    Parse a 1-based item selection such as "1-10,15" into inclusive ranges.

    Args:
        spec: The selection string; None or blank means "all items".

    Returns:
        Sorted, merged (start, end) ranges, or None for "all items".

    Raises:
        InvalidItems: If the spec is malformed, reversed, zero-based or too large.
    """
    if spec is None or not spec.strip():
        return None
    if len(spec) > MAX_ITEMS_SPEC_LENGTH:
        raise InvalidItems("Item range is too long")
    ranges: list[tuple[int, int]] = []
    for raw in spec.split(","):
        part = raw.strip()
        if not part:
            continue
        match = _ITEM_PART_RE.match(part)
        if not match:
            raise InvalidItems(f"Invalid item range '{part}'")
        start = int(match.group(1))
        end = int(match.group(2)) if match.group(2) else start
        if start < 1 or end < start or end > MAX_ITEM_INDEX:
            raise InvalidItems(f"Invalid item range '{part}'")
        ranges.append((start, end))
    if not ranges:
        raise InvalidItems("Item range is empty")
    ranges.sort()
    merged: list[tuple[int, int]] = [ranges[0]]
    for start, end in ranges[1:]:
        last_start, last_end = merged[-1]
        if start <= last_end + 1:
            merged[-1] = (last_start, max(last_end, end))
        else:
            merged.append((start, end))
    return merged


def format_items(ranges: list[tuple[int, int]]) -> str:
    """
    Render parsed ranges back to the normalised "1-10,15" form.

    Args:
        ranges: Ranges from parse_items.

    Returns:
        The normalised selection string.
    """
    return ",".join(str(a) if a == b else f"{a}-{b}" for a, b in ranges)


def select_items(entries: list, ranges: list[tuple[int, int]] | None) -> list:
    """
    Pick the entries whose 1-based position falls inside any range.

    Args:
        entries: Playlist or channel entries in listing order.
        ranges: Ranges from parse_items, or None for all entries.

    Returns:
        The selected entries in their original order.
    """
    if ranges is None:
        return list(entries)
    return [e for i, e in enumerate(entries, start=1) if any(a <= i <= b for a, b in ranges)]

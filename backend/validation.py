"""
Input validation utilities for the backend
Security-critical: Prevents command injection via URL and format_id
"""

import re
from fastapi import HTTPException

# YouTube URL pattern - matches youtube.com/watch?v=... and youtu.be/...
# Video IDs are exactly 11 characters: alphanumeric, underscore, hyphen
YOUTUBE_URL_REGEX = re.compile(
    r"^https?://(www\.)?(youtube\.com/watch\?v=|youtu\.be/)([\w-]{11})(&.*)?$"
)

# Format ID pattern - alphanumeric plus underscore/hyphen only
# Prevents path traversal and shell injection
FORMAT_ID_REGEX = re.compile(r"^[a-zA-Z0-9_-]+$")


def validate_youtube_url(url: str) -> str:
    """
    Validate and extract video ID from YouTube URL.

    Args:
        url: YouTube video URL to validate

    Returns:
        video_id: 11-character YouTube video ID

    Raises:
        HTTPException: 400 if URL is invalid or not a YouTube URL

    Examples:
        >>> validate_youtube_url("https://www.youtube.com/watch?v=dQw4w9WgXcQ")
        'dQw4w9WgXcQ'
        >>> validate_youtube_url("https://youtu.be/dQw4w9WgXcQ")
        'dQw4w9WgXcQ'
        >>> validate_youtube_url("https://example.com/watch?v=abc")
        HTTPException: 400
    """
    if not url or not isinstance(url, str):
        raise HTTPException(
            status_code=400, detail="URL is required and must be a string"
        )

    # Strip whitespace to prevent bypass via trailing newlines
    url = url.strip()

    match = YOUTUBE_URL_REGEX.match(url)
    if not match:
        raise HTTPException(
            status_code=400,
            detail=f"Invalid YouTube URL. Must be youtube.com/watch?v=... or youtu.be/...",
        )

    video_id = match.group(3)
    return video_id


def validate_format_id(format_id: str) -> str:
    """
    Validate format_id to prevent command injection.

    Args:
        format_id: Format ID to validate (e.g., "137", "best_video", "mp3")

    Returns:
        format_id: Validated format ID

    Raises:
        HTTPException: 400 if format_id contains invalid characters

    Examples:
        >>> validate_format_id("137")
        '137'
        >>> validate_format_id("best_video")
        'best_video'
        >>> validate_format_id("../../etc/passwd")
        HTTPException: 400
    """
    if not format_id or not isinstance(format_id, str):
        raise HTTPException(
            status_code=400, detail="format_id is required and must be a string"
        )

    # Strip whitespace to prevent bypass via trailing newlines/spaces
    format_id = format_id.strip()

    if not format_id:  # Check again after stripping
        raise HTTPException(
            status_code=400, detail="format_id cannot be empty or whitespace-only"
        )

    if not FORMAT_ID_REGEX.match(format_id):
        raise HTTPException(
            status_code=400,
            detail=f"Invalid format_id. Only alphanumeric, underscore, and hyphen allowed.",
        )

    return format_id

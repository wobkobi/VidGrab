"""Pydantic request/settings models; every option is an enum or a bounded number."""

from __future__ import annotations

from typing import Literal, Optional

from pydantic import BaseModel, Field, field_validator

Kind = Literal["video", "audio"]
Quality = Literal["best", "2160", "1440", "1080", "720", "480", "360"]
Codec = Literal["any", "h264", "vp9", "av1"]
Container = Literal["mp4", "mkv"]
AudioFormat = Literal["mp3", "m4a"]
AudioBitrate = Literal["320", "192", "128"]

# Above this height YouTube only serves separate video/audio streams, so ffmpeg must merge
MAX_PROGRESSIVE_HEIGHT = 720


class JobOptions(BaseModel):
    """Download options chosen in the extension panel."""

    kind: Kind = "video"
    quality: Quality = "best"
    codec: Codec = "any"
    container: Container = "mp4"
    audio_format: AudioFormat = "mp3"
    audio_bitrate: AudioBitrate = "192"

    @field_validator("quality", "audio_bitrate", mode="before")
    @classmethod
    def _numbers_as_strings(cls, value: object) -> object:
        """Accept 1080 as well as "1080" since JSON callers may send either."""
        if isinstance(value, int) and not isinstance(value, bool):
            return str(value)
        return value


class JobRequest(BaseModel):
    """Body of POST /jobs."""

    url: str = Field(max_length=2048)
    options: JobOptions = Field(default_factory=JobOptions)
    items: Optional[str] = Field(default=None, max_length=200)


class Settings(BaseModel):
    """Backend settings pushed by the extension and persisted in data/settings.json."""

    concurrency: int = Field(default=2, ge=1, le=5)
    fragments: int = Field(default=4, ge=1, le=8)
    cookies: bool = False
    auto_update: bool = True


class SettingsUpdate(BaseModel):
    """Body of PUT /settings; only fields present are changed."""

    concurrency: Optional[int] = Field(default=None, ge=1, le=5)
    fragments: Optional[int] = Field(default=None, ge=1, le=8)
    cookies: Optional[bool] = None
    auto_update: Optional[bool] = None


def needs_ffmpeg(options: JobOptions) -> bool:
    """
    Return True when a download cannot work without ffmpeg.

    MP3 always needs conversion; video at "best" or above 720p needs separate streams merged.
    M4A falls back to a native m4a stream, and 720p and below fall back to a progressive format.

    Args:
        options: The requested download options.

    Returns:
        Whether ffmpeg is required.
    """
    if options.kind == "audio":
        return options.audio_format == "mp3"
    return options.quality == "best" or int(options.quality) > MAX_PROGRESSIVE_HEIGHT

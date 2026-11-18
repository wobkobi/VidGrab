"""yt-dlp command building, info summarising and output parsing tests."""

from __future__ import annotations

import itertools
import json
import os
import subprocess
import sys

import pytest

from models import JobOptions
from ytdlp import (
    OutputParser,
    YtDlpError,
    YtDlpTimeout,
    build_args,
    clean_error,
    expected_streams,
    fetch_raw_info,
    format_eta,
    format_speed,
    summarise_info,
)

URL = "https://www.youtube.com/watch?v=jNQXAC9IVRw"
OUT = os.path.join("C:\\Users\\me", "Downloads")


def percent_of(update: object) -> float:
    """Return the percent of a parser update, failing the test when the line produced none."""
    assert update is not None
    return getattr(update, "percent")


def value_after(args: list[str], flag: str) -> str:
    """Return the argument following a flag."""
    return args[args.index(flag) + 1]


VIDEO_COMBOS = list(
    itertools.product(
        ["best", "2160", "1440", "1080", "720", "480", "360"],
        ["any", "h264", "vp9", "av1"],
        ["mp4", "mkv"],
    )
)


@pytest.mark.parametrize(("quality", "codec", "container"), VIDEO_COMBOS)
def test_video_args_every_combination(quality: str, codec: str, container: str) -> None:
    options = JobOptions.model_validate({"kind": "video", "quality": quality, "codec": codec, "container": container})
    args = build_args(options, URL, OUT, cookies=False, fragments=6)

    assert args[:3] == [sys.executable, "-m", "yt_dlp"]
    assert args[-2:] == ["--", URL]
    assert value_after(args, "-f") == "bv*+ba/b"
    assert value_after(args, "--merge-output-format") == container
    assert value_after(args, "--concurrent-fragments") == "6"
    sort = value_after(args, "-S").split(",")
    assert sort[0] == ("res" if quality == "best" else f"res:{quality}")
    codec_field = {"any": None, "h264": "vcodec:h264", "vp9": "vcodec:vp9", "av1": "vcodec:av01"}[codec]
    if codec_field:
        assert sort[1] == codec_field
    else:
        assert not any(s.startswith("vcodec") for s in sort)
    assert ("acodec:aac" in sort) == (container == "mp4")
    assert "-x" not in args
    for flag in ("--no-playlist", "--windows-filenames", "--no-mtime", "--newline", "--continue", "--ignore-config"):
        assert flag in args
    assert "--cookies-from-browser" not in args


@pytest.mark.parametrize(("audio_format", "bitrate"), itertools.product(["mp3", "m4a"], ["320", "192", "128"]))
def test_audio_args_every_combination(audio_format: str, bitrate: str) -> None:
    options = JobOptions.model_validate({"kind": "audio", "audio_format": audio_format, "audio_bitrate": bitrate})
    args = build_args(options, URL, OUT, cookies=False)

    assert "-x" in args
    assert value_after(args, "--audio-format") == audio_format
    assert "--merge-output-format" not in args
    assert "-S" not in args
    if audio_format == "mp3":
        assert value_after(args, "-f") == "ba/b"
        assert value_after(args, "--audio-quality") == f"{bitrate}K"
    else:
        assert value_after(args, "-f") == "ba[ext=m4a]/ba/b"
        assert "--audio-quality" not in args


def test_progress_and_file_templates() -> None:
    args = build_args(JobOptions(), URL, OUT, cookies=False)
    assert value_after(args, "--progress-template") == "download:YTDLP_PROGRESS %(progress)j"
    prints = [args[i + 1] for i, a in enumerate(args) if a == "--print"]
    assert "after_move:YTDLP_FILE %(filepath)s" in prints
    assert "before_dl:YTDLP_TITLE %(title)s" in prints
    assert value_after(args, "-o") == os.path.join(OUT, "%(title)s [%(id)s].%(ext)s")
    assert "--progress" in args and "--no-quiet" in args


def test_cookies_flag() -> None:
    args = build_args(JobOptions(), URL, OUT, cookies=True)
    assert value_after(args, "--cookies-from-browser") == "firefox"


def test_without_ffmpeg_requests_single_file_formats() -> None:
    video = build_args(JobOptions(quality="720"), URL, OUT, cookies=False, ffmpeg=False)
    assert value_after(video, "-f") == "b"
    assert "--merge-output-format" not in video
    m4a = build_args(JobOptions(kind="audio", audio_format="m4a"), URL, OUT, cookies=False, ffmpeg=False)
    assert value_after(m4a, "-f") == "ba[ext=m4a]"
    assert "-x" not in m4a


def test_metadata_embedded_with_ffmpeg() -> None:
    video = build_args(JobOptions(), URL, OUT, cookies=False)
    assert "--embed-metadata" in video
    summary = value_after(video, "--parse-metadata")
    # before_dl runs after format selection, so the chosen stream's fields are filled in
    assert summary.startswith("before_dl:")
    assert summary.endswith(":(?P<meta_comment>.+)")
    assert ":" not in summary.removeprefix("before_dl:").removesuffix(":(?P<meta_comment>.+)")

    audio = build_args(JobOptions(kind="audio"), URL, OUT, cookies=False)
    assert "--embed-metadata" in audio
    assert "--parse-metadata" not in audio


def test_no_metadata_without_ffmpeg() -> None:
    for options in (JobOptions(quality="720"), JobOptions(kind="audio", audio_format="m4a")):
        args = build_args(options, URL, OUT, cookies=False, ffmpeg=False)
        assert "--embed-metadata" not in args
        assert "--parse-metadata" not in args


def test_url_is_last_after_separator() -> None:
    args = build_args(JobOptions(), "-o/evil", OUT, cookies=False)
    assert args[-2:] == ["--", "-o/evil"]


def test_expected_streams() -> None:
    assert expected_streams(JobOptions(kind="video"), ffmpeg=True) == 2
    assert expected_streams(JobOptions(kind="video"), ffmpeg=False) == 1
    assert expected_streams(JobOptions(kind="audio"), ffmpeg=True) == 1


def progress(**fields: object) -> str:
    """Build a YTDLP_PROGRESS line."""
    return "YTDLP_PROGRESS " + json.dumps(fields)


def test_parser_merge_run_is_monotonic() -> None:
    parser = OutputParser(expected_streams=2)
    lines = [
        "[youtube] Extracting URL: x",
        "YTDLP_TITLE Me at the zoo",
        "[download] Destination: C:\\D\\Me at the zoo [jNQXAC9IVRw].f134.mp4",
        progress(status="downloading", _percent=50.0, speed=2_621_440, eta=65),
        progress(status="finished", _percent=100.0),
        "[download] Destination: C:\\D\\Me at the zoo [jNQXAC9IVRw].f140.m4a",
        progress(status="downloading", _percent=10.0, speed=1024, eta=3),
        progress(status="downloading", downloaded_bytes=50, total_bytes=100),
        '[Merger] Merging formats into "C:\\D\\Me at the zoo [jNQXAC9IVRw].mp4"',
        "YTDLP_FILE C:\\D\\Me at the zoo [jNQXAC9IVRw].mp4",
    ]
    updates = [parser.feed(line) for line in lines]

    assert updates[1] is not None and updates[1].title == "Me at the zoo"
    first = updates[3]
    assert first is not None and first.percent == 45.0
    assert first.speed == "2.5 MB/s" and first.eta == "1:05"
    assert updates[4] is not None and updates[4].percent == 90.0 and updates[4].speed is None
    assert updates[6] is not None and updates[6].percent == 91.0
    assert updates[7] is not None and updates[7].percent == 95.0
    assert updates[8] is not None and updates[8].processing
    assert updates[9] is not None and updates[9].filepath == "C:\\D\\Me at the zoo [jNQXAC9IVRw].mp4"
    percents = [u.percent for u in updates if u is not None and u.percent is not None]
    assert percents == sorted(percents)
    assert parser.filepath == "C:\\D\\Me at the zoo [jNQXAC9IVRw].mp4"


def test_parser_resumed_merge_counts_finished_stream() -> None:
    parser = OutputParser(expected_streams=2)
    done = parser.feed("[download] C:\\D\\Clip [jNQXAC9IVRw].f137.mp4 has already been downloaded")
    assert done is not None and done.percent == 90.0
    parser.feed("[download] Destination: C:\\D\\Clip [jNQXAC9IVRw].f140.m4a")
    update = parser.feed(progress(status="downloading", _percent=50.0))
    assert update is not None and update.percent == 95.0


def test_parser_audio_extract_marks_processing() -> None:
    parser = OutputParser(expected_streams=1)
    parser.feed("[download] Destination: a [x].webm")
    assert percent_of(parser.feed(progress(status="downloading", _percent=40.0))) == 40.0
    update = parser.feed("[ExtractAudio] Destination: a [x].mp3")
    assert update is not None and update.processing


def test_parser_fallback_regex_and_fragments() -> None:
    parser = OutputParser()
    assert percent_of(parser.feed("[download]  12.5% of 10.00MiB at 1.00MiB/s ETA 00:09")) == 12.5
    assert percent_of(parser.feed(progress(status="downloading", fragment_index=3, fragment_count=4))) == 75.0
    # Lower values never move progress backwards
    assert percent_of(parser.feed("[download]   5.0% of 10.00MiB")) == 75.0


def test_parser_errors_and_detail() -> None:
    parser = OutputParser()
    for i in range(30):
        parser.feed(f"line {i}")
    parser.feed("ERROR: [youtube] jNQXAC9IVRw: Sign in to confirm your age")
    assert parser.last_error == "Sign in to confirm your age"
    detail = parser.detail().splitlines()
    assert len(detail) == 20
    assert detail[-1].startswith("ERROR:")


def test_parser_ignores_garbage_progress() -> None:
    parser = OutputParser()
    assert parser.feed("YTDLP_PROGRESS not json") is None
    assert parser.feed("YTDLP_PROGRESS [1, 2]") is None
    assert parser.feed("") is None


def test_clean_error() -> None:
    assert clean_error("ERROR: unable to download video data: HTTP Error 403: Forbidden") == (
        "unable to download video data: HTTP Error 403: Forbidden"
    )
    assert clean_error("ERROR: [youtube:tab] PLx: The playlist does not exist") == "The playlist does not exist"


@pytest.mark.parametrize(
    ("value", "expected"),
    [(None, None), (0, None), (512, "512 B/s"), (2048, "2.0 KB/s"), (5 * 1024**3, "5.0 GB/s")],
)
def test_format_speed(value: float | None, expected: str | None) -> None:
    assert format_speed(value) == expected


@pytest.mark.parametrize(("value", "expected"), [(None, None), (-1, None), (5, "0:05"), (3725, "1:02:05")])
def test_format_eta(value: float | None, expected: str | None) -> None:
    assert format_eta(value) == expected


VIDEO_RAW = {
    "id": "jNQXAC9IVRw",
    "title": "Me at the zoo",
    "uploader": "jawed",
    "duration": 19,
    "formats": [
        {"format_id": "sb0", "vcodec": "none", "acodec": "none", "ext": "mhtml"},
        {"format_id": "140", "vcodec": "none", "acodec": "mp4a.40.2", "filesize": 300},
        {"format_id": "251", "vcodec": "none", "acodec": "opus", "filesize_approx": 250},
        {"format_id": "18", "vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "height": 360, "filesize": 900},
        {"format_id": "134", "vcodec": "avc1.4d401e", "acodec": "none", "height": 360, "filesize": 700},
        {"format_id": "243", "vcodec": "vp9", "acodec": "none", "height": 360},
        {"format_id": "160", "vcodec": "avc1.4d400c", "acodec": "none", "height": 144, "filesize": 100},
        {"format_id": "394", "vcodec": "av01.0.00M.08", "acodec": "none", "height": 144, "filesize_approx": 80},
    ],
}


def test_summarise_video() -> None:
    summary = summarise_info(VIDEO_RAW, "video")
    assert summary["kind"] == "video"
    assert summary["title"] == "Me at the zoo"
    assert summary["entries"] is None and summary["entry_count"] is None
    assert summary["qualities"] == [
        {"height": 360, "codecs": ["h264", "vp9"], "filesize_est": 1000},
        {"height": 144, "codecs": ["av1", "h264"], "filesize_est": 400},
    ]


def test_summarise_vertical_video_uses_shorter_side() -> None:
    raw = {
        "id": "abcdefghijk",
        "formats": [
            {"vcodec": "avc1.640028", "acodec": "none", "width": 1080, "height": 1920},
            {"vcodec": "vp09.00.40.08", "acodec": "none", "width": 720, "height": 1280},
            {"vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "width": 360, "height": 640},
            {"vcodec": "avc1.4d401e", "acodec": "none", "height": 480},
        ],
    }
    heights = [q["height"] for q in summarise_info(raw, "video")["qualities"]]
    assert heights == [1080, 720, 480, 360]


def test_summarise_playlist() -> None:
    raw = {
        "id": "PL1",
        "title": "List",
        "channel": "Chan",
        "entries": [{"id": "jNQXAC9IVRw", "title": "A"}, {"id": "dQw4w9WgXcQ", "title": "B"}, None, {"title": "no id"}],
    }
    summary = summarise_info(raw, "playlist")
    assert summary["uploader"] == "Chan"
    assert summary["entry_count"] == 2
    assert summary["entries"] == [{"id": "jNQXAC9IVRw", "title": "A"}, {"id": "dQw4w9WgXcQ", "title": "B"}]
    assert summary["qualities"] == []


def test_fetch_raw_info_success_and_flags() -> None:
    calls: list[list[str]] = []

    def runner(args: list[str], **kwargs: object) -> subprocess.CompletedProcess:
        calls.append(args)
        return subprocess.CompletedProcess(args, 0, stdout=json.dumps({"id": "x"}), stderr="")

    assert fetch_raw_info(URL, flat=True, cookies=True, runner=runner) == {"id": "x"}
    assert "--flat-playlist" in calls[0] and "-J" in calls[0]
    assert "--cookies-from-browser" in calls[0]
    assert calls[0][-2:] == ["--", URL]
    fetch_raw_info(URL, flat=False, runner=runner)
    assert "--no-playlist" in calls[1]


def test_fetch_raw_info_errors() -> None:
    def failing(args: list[str], **kwargs: object) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args, 1, stdout="", stderr="WARNING: x\nERROR: [youtube] abc: Private video")

    with pytest.raises(YtDlpError) as info:
        fetch_raw_info(URL, flat=False, runner=failing)
    assert info.value.message == "Private video"

    def slow(args: list[str], **kwargs: object) -> subprocess.CompletedProcess:
        raise subprocess.TimeoutExpired(args, 20)

    with pytest.raises(YtDlpTimeout):
        fetch_raw_info(URL, flat=False, runner=slow)

    def garbage(args: list[str], **kwargs: object) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args, 0, stdout="not json", stderr="")

    with pytest.raises(YtDlpError):
        fetch_raw_info(URL, flat=False, runner=garbage)

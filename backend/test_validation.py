"""URL classification and item-range tests."""

from __future__ import annotations

import pytest

from validation import (
    InvalidItems,
    InvalidUrl,
    classify_url,
    format_items,
    parse_items,
    select_items,
    watch_url,
)

VID = "jNQXAC9IVRw"
WATCH = f"https://www.youtube.com/watch?v={VID}"


@pytest.mark.parametrize(
    "url",
    [
        WATCH,
        f"http://youtube.com/watch?v={VID}",
        f"https://m.youtube.com/watch?v={VID}&t=10s",
        f"https://www.youtube.com/watch?v={VID}&list=PLBCF2DAC6FFB574DE&index=2",
        f"https://www.youtube.com/shorts/{VID}",
        f"https://www.youtube.com/shorts/{VID}/",
        f"https://www.youtube.com/embed/{VID}",
        f"https://www.youtube.com/live/{VID}",
        f"https://youtu.be/{VID}",
        f"https://youtu.be/{VID}?si=abcdef",
        f"  {WATCH}  ",
        f"https://WWW.YouTube.com/watch?v={VID}",
    ],
)
def test_video_urls(url: str) -> None:
    parsed = classify_url(url)
    assert parsed.kind == "video"
    assert parsed.id == VID
    assert parsed.canonical == WATCH


@pytest.mark.parametrize(
    "url",
    [
        "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE",
        "https://youtube.com/playlist?list=PLBCF2DAC6FFB574DE&si=x",
        "https://www.youtube.com/watch?list=PLBCF2DAC6FFB574DE",
    ],
)
def test_playlist_urls(url: str) -> None:
    parsed = classify_url(url)
    assert parsed.kind == "playlist"
    assert parsed.id == "PLBCF2DAC6FFB574DE"
    assert parsed.canonical == "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"


@pytest.mark.parametrize(
    ("url", "canonical"),
    [
        ("https://www.youtube.com/@jawed", "https://www.youtube.com/@jawed/videos"),
        ("https://www.youtube.com/@jawed/videos", "https://www.youtube.com/@jawed/videos"),
        ("https://www.youtube.com/@jawed/shorts", "https://www.youtube.com/@jawed/shorts"),
        ("https://www.youtube.com/@jawed/featured", "https://www.youtube.com/@jawed/videos"),
        ("https://www.youtube.com/@some.name-x_y/streams", "https://www.youtube.com/@some.name-x_y/streams"),
        (
            "https://www.youtube.com/channel/UC4QobU6STFB0P71PMvOGN5A",
            "https://www.youtube.com/channel/UC4QobU6STFB0P71PMvOGN5A/videos",
        ),
        ("https://www.youtube.com/c/GoogleSearchStories/videos", "https://www.youtube.com/c/GoogleSearchStories/videos"),
        ("https://www.youtube.com/user/jawed", "https://www.youtube.com/user/jawed/videos"),
        (
            "https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/streams",
            "https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/streams",
        ),
        ("https://www.youtube.com/@日本語", "https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/videos"),
        ("https://www.youtube.com/@_under", "https://www.youtube.com/@_under/videos"),
    ],
)
def test_channel_urls(url: str, canonical: str) -> None:
    parsed = classify_url(url)
    assert parsed.kind == "channel"
    assert parsed.canonical == canonical


def test_unicode_handle_id_is_decoded() -> None:
    assert classify_url("https://www.youtube.com/@%D0%BA%D0%BE%D1%82").id == "@кот"


@pytest.mark.parametrize(
    "url",
    [
        "",
        "   ",
        "not a url",
        "ftp://www.youtube.com/watch?v=jNQXAC9IVRw",
        "file:///etc/passwd",
        "javascript:alert(1)",
        "https://example.com/watch?v=jNQXAC9IVRw",
        "https://youtube.com.evil.com/watch?v=jNQXAC9IVRw",
        "https://evil.com/?u=https://www.youtube.com/watch?v=jNQXAC9IVRw",
        "https://user:pass@www.youtube.com/watch?v=jNQXAC9IVRw",
        "https://www.youtube.com:8080/watch?v=jNQXAC9IVRw",
        "https://www.youtube.com/watch?v=short",
        "https://www.youtube.com/watch?v=jNQXAC9IVRw;rm -rf",
        "https://www.youtube.com/watch?v=jNQXAC9IVRw&v=dQw4w9WgXcQ",
        "https://www.youtube.com/watch?v=--exec=calc",
        "https://www.youtube.com/watch?v=jNQXAC9IVR$",
        "https://www.youtube.com/watch?v=jNQXAC9IVRw\n--exec calc",
        "https://www.youtube.com/watch?v=jNQXAC9IVRw\x00",
        "https://youtu.be/jNQXAC9IVRw/extra",
        "https://youtu.be/",
        "https://www.youtube.com/playlist?list=",
        "https://www.youtube.com/playlist?list=PL$(whoami)",
        "https://www.youtube.com/playlist?list=PL%0Aecho",
        "https://www.youtube.com/@",
        "https://www.youtube.com/@bad$handle",
        "https://www.youtube.com/@..",
        "https://www.youtube.com/@%2E%2E",
        "https://www.youtube.com/c/..",
        "https://www.youtube.com/user/./streams",
        "https://www.youtube.com/@a%2Fb",
        "https://www.youtube.com/@a%2541",
        "https://www.youtube.com/@%FF%FE",
        "https://www.youtube.com/@-dash",
        "https://www.youtube.com/@jawed/community",
        "https://www.youtube.com/@jawed/videos/extra",
        "https://www.youtube.com/channel/notachannelid",
        "https://www.youtube.com/results?search_query=x",
        "https://www.youtube.com/",
        "https://www.youtube.com/feed/subscriptions",
        "https://music.youtube.com/watch?v=jNQXAC9IVRw",
        "--output=/tmp/x",
        "https://www.youtube.com/watch?v=" + "a" * 3000,
    ],
)
def test_rejected_urls(url: str) -> None:
    with pytest.raises(InvalidUrl):
        classify_url(url)


def test_non_string_rejected() -> None:
    with pytest.raises(InvalidUrl):
        classify_url(None)  # type: ignore[arg-type]


def test_watch_url_validates_id() -> None:
    assert watch_url(VID) == WATCH
    with pytest.raises(InvalidUrl):
        watch_url("../../etc")


@pytest.mark.parametrize(
    ("spec", "expected"),
    [
        (None, None),
        ("", None),
        ("   ", None),
        ("1", [(1, 1)]),
        ("1-10,15", [(1, 10), (15, 15)]),
        (" 3 - 5 , 1 ", [(1, 1), (3, 5)]),
        ("1-5,4-8,9", [(1, 9)]),
        ("10,2,", [(2, 2), (10, 10)]),
    ],
)
def test_parse_items(spec: str | None, expected: list[tuple[int, int]] | None) -> None:
    assert parse_items(spec) == expected


@pytest.mark.parametrize("spec", ["0", "5-3", "a", "1-", "-3", "1--2", "1;2", ",", "1-999999", "1" * 300])
def test_parse_items_rejects(spec: str) -> None:
    with pytest.raises(InvalidItems):
        parse_items(spec)


def test_format_items_round_trip() -> None:
    assert format_items(parse_items("15, 1-10, 3") or []) == "1-10,15"


def test_select_items_uses_one_based_positions() -> None:
    entries = list("abcdefghij")
    assert select_items(entries, None) == entries
    assert select_items(entries, [(1, 2), (5, 5), (9, 20)]) == ["a", "b", "e", "i", "j"]

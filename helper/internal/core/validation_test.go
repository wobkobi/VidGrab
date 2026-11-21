package core

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	testVID   = "jNQXAC9IVRw"
	testWatch = "https://www.youtube.com/watch?v=" + testVID
)

func TestVideoUrls(t *testing.T) {
	urls := []string{
		testWatch,
		"http://youtube.com/watch?v=" + testVID,
		"https://m.youtube.com/watch?v=" + testVID + "&t=10s",
		"https://www.youtube.com/watch?v=" + testVID + "&list=PLBCF2DAC6FFB574DE&index=2",
		"https://www.youtube.com/shorts/" + testVID,
		"https://www.youtube.com/shorts/" + testVID + "/",
		"https://www.youtube.com/embed/" + testVID,
		"https://www.youtube.com/live/" + testVID,
		"https://youtu.be/" + testVID,
		"https://youtu.be/" + testVID + "?si=abcdef",
		"  " + testWatch + "  ",
		"https://WWW.YouTube.com/watch?v=" + testVID,
	}
	for _, url := range urls {
		parsed, err := Classify(url)
		if err != nil {
			t.Errorf("Classify(%q) error: %v", url, err)
			continue
		}
		want := ParsedUrl{Kind: KindVideo, ID: testVID, Canonical: testWatch}
		if parsed != want {
			t.Errorf("Classify(%q) = %+v, want %+v", url, parsed, want)
		}
	}
}

func TestPlaylistUrls(t *testing.T) {
	urls := []string{
		"https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE",
		"https://youtube.com/playlist?list=PLBCF2DAC6FFB574DE&si=x",
		"https://www.youtube.com/watch?list=PLBCF2DAC6FFB574DE",
	}
	want := ParsedUrl{Kind: KindPlaylist, ID: "PLBCF2DAC6FFB574DE", Canonical: "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"}
	for _, url := range urls {
		parsed, err := Classify(url)
		if err != nil || parsed != want {
			t.Errorf("Classify(%q) = %+v, %v; want %+v", url, parsed, err, want)
		}
	}
}

func TestChannelUrls(t *testing.T) {
	cases := []struct{ url, canonical string }{
		{"https://www.youtube.com/@jawed", "https://www.youtube.com/@jawed/videos"},
		{"https://www.youtube.com/@jawed/videos", "https://www.youtube.com/@jawed/videos"},
		{"https://www.youtube.com/@jawed/shorts", "https://www.youtube.com/@jawed/shorts"},
		{"https://www.youtube.com/@jawed/featured", "https://www.youtube.com/@jawed/videos"},
		{"https://www.youtube.com/@some.name-x_y/streams", "https://www.youtube.com/@some.name-x_y/streams"},
		{"https://www.youtube.com/channel/UC4QobU6STFB0P71PMvOGN5A", "https://www.youtube.com/channel/UC4QobU6STFB0P71PMvOGN5A/videos"},
		{"https://www.youtube.com/c/GoogleSearchStories/videos", "https://www.youtube.com/c/GoogleSearchStories/videos"},
		{"https://www.youtube.com/user/jawed", "https://www.youtube.com/user/jawed/videos"},
		{"https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/streams", "https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/streams"},
		{"https://www.youtube.com/@日本語", "https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E/videos"},
		{"https://www.youtube.com/@_under", "https://www.youtube.com/@_under/videos"},
	}
	for _, c := range cases {
		parsed, err := Classify(c.url)
		if err != nil || parsed.Kind != KindChannel || parsed.Canonical != c.canonical {
			t.Errorf("Classify(%q) = %+v, %v; want canonical %q", c.url, parsed, err, c.canonical)
		}
	}
}

func TestUnicodeHandleIDIsDecoded(t *testing.T) {
	parsed, err := Classify("https://www.youtube.com/@%D0%BA%D0%BE%D1%82")
	if err != nil || parsed.ID != "@кот" {
		t.Fatalf("got %+v, %v", parsed, err)
	}
}

func TestRejectedUrls(t *testing.T) {
	urls := []string{
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
		"https://www.youtube.com/watch?v=" + strings.Repeat("a", 3000),
		// Go-only cases: a trailing %0A on an ID, a non-numeric port and an invalid IPv6 host
		"https://www.youtube.com/watch?v=jNQXAC9IVRw%0A",
		"https://www.youtube.com:abc/watch?v=jNQXAC9IVRw",
		"https://[zz]/watch?v=jNQXAC9IVRw",
		"https://www.youtube.com/@%E6%97\xff",
	}
	for _, url := range urls {
		parsed, err := Classify(url)
		var invalid *InvalidUrl
		if !errors.As(err, &invalid) {
			t.Errorf("Classify(%q) = %+v, %v; want *InvalidUrl", url, parsed, err)
		}
	}
}

func TestRejectionMessages(t *testing.T) {
	cases := []struct{ url, msg string }{
		{"", "URL is required"},
		{"https://example.com/watch?v=" + testVID, "Only youtube.com and youtu.be URLs are supported"},
		{"https://www.youtube.com/watch?v=" + testVID + "&v=dQw4w9WgXcQ", "Duplicate 'v' parameter"},
		{"https://www.youtube.com/watch?v=short", "Invalid video ID"},
		{"https://www.youtube.com/playlist?list=", "Invalid playlist URL"},
		{"https://www.youtube.com/@..", "Invalid channel handle"},
		{"https://www.youtube.com/c/..", "Invalid channel name"},
		{"https://www.youtube.com/channel/x", "Invalid channel ID"},
		{"https://www.youtube.com/@jawed/community", "Unsupported channel page"},
		{"https://www.youtube.com/", "Not a video, playlist or channel URL"},
		{"https://user:pass@www.youtube.com/", "Not a valid YouTube URL"},
		{"https://www.youtube.com/watch?v=" + testVID + ";x", "Invalid video ID"},
		{"https://youtu.be/x", "Not a valid YouTube URL"},
		// An empty userinfo and port 0 count as absent, so they pass
		{"https://@www.youtube.com:0/watch?v=" + testVID, ""},
	}
	for _, c := range cases {
		_, err := Classify(c.url)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.msg {
			t.Errorf("Classify(%q) error %q, want %q", c.url, got, c.msg)
		}
	}
}

func TestWatchUrlValidatesID(t *testing.T) {
	if got, err := WatchUrl(testVID); err != nil || got != testWatch {
		t.Fatalf("WatchUrl = %q, %v", got, err)
	}
	if _, err := WatchUrl("../../etc"); err == nil {
		t.Fatal("WatchUrl accepted a bad ID")
	}
}

func TestParseItems(t *testing.T) {
	cases := []struct {
		spec string
		want []ItemRange
	}{
		// Empty or blank means "all items"
		{"", nil},
		{"   ", nil},
		{"1", []ItemRange{{1, 1}}},
		{"1-10,15", []ItemRange{{1, 10}, {15, 15}}},
		{" 3 - 5 , 1 ", []ItemRange{{1, 1}, {3, 5}}},
		{"1-5,4-8,9", []ItemRange{{1, 9}}},
		{"10,2,", []ItemRange{{2, 2}, {10, 10}}},
	}
	for _, c := range cases {
		got, err := ParseItems(c.spec)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseItems(%q) = %v, %v; want %v", c.spec, got, err, c.want)
		}
	}
}

func TestParseItemsRejects(t *testing.T) {
	specs := []string{"0", "5-3", "a", "1-", "-3", "1--2", "1;2", ",", "1-999999", strings.Repeat("1", 300), "99999999999999999999999"}
	for _, spec := range specs {
		_, err := ParseItems(spec)
		var invalid *InvalidItems
		if !errors.As(err, &invalid) {
			t.Errorf("ParseItems(%q) error = %v, want *InvalidItems", spec, err)
		}
	}
}

func TestFormatItemsRoundTrip(t *testing.T) {
	ranges, err := ParseItems("15, 1-10, 3")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatItems(ranges); got != "1-10,15" {
		t.Fatalf("FormatItems = %q", got)
	}
}

func TestSelectItemsUsesOneBasedPositions(t *testing.T) {
	entries := strings.Split("abcdefghij", "")
	if got := SelectItems(entries, nil); !reflect.DeepEqual(got, entries) {
		t.Fatalf("SelectItems(nil) = %v", got)
	}
	got := SelectItems(entries, []ItemRange{{1, 2}, {5, 5}, {9, 20}})
	if want := []string{"a", "b", "e", "i", "j"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectItems = %v, want %v", got, want)
	}
}

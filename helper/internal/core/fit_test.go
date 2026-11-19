package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Typical YouTube formats for a 3-minute video (sizes in bytes).
var (
	v1080h264 = FitFormat{ID: "137", VCodec: "h264", Ext: "mp4", Height: 1080, Size: 40e6}
	v1080vp9  = FitFormat{ID: "248", VCodec: "vp9", Ext: "webm", Height: 1080, Size: 25e6}
	v1080av1  = FitFormat{ID: "399", VCodec: "av1", Ext: "mp4", Height: 1080, Size: 9e6}
	v720h264  = FitFormat{ID: "136", VCodec: "h264", Ext: "mp4", Height: 720, Size: 8e6}
	v720vp9   = FitFormat{ID: "247", VCodec: "vp9", Ext: "webm", Height: 720, Size: 8e6}
	v480h264  = FitFormat{ID: "135", VCodec: "h264", Ext: "mp4", Height: 480, Size: 4e6}
	aAAC      = FitFormat{ID: "140", ACodec: "aac", Ext: "m4a", Size: 3e6}
	aOpus     = FitFormat{ID: "251", ACodec: "opus", Ext: "webm", Size: 3.2e6}
	aOpusLow  = FitFormat{ID: "249", ACodec: "opus", Ext: "webm", Size: 1e6}
	prog360   = FitFormat{ID: "18", VCodec: "h264", ACodec: "aac", Ext: "mp4", Height: 360, Size: 6e6}
)

func TestSelectFit(t *testing.T) {
	cases := []struct {
		name     string
		formats  []FitFormat
		cap      int
		limitMB  int
		want     string
		wantFits bool
	}{
		{"everything fits: tallest h264 with aac", []FitFormat{v1080h264, v1080vp9, v720h264, aAAC, aOpus}, 0, 100, "137+140", true},
		{"h264 too big: vp9 at the same height is never chosen", []FitFormat{v1080h264, v1080vp9, v720h264, aAAC}, 0, 30, "136+140", true},
		{"av1 never chosen, even taller", []FitFormat{v1080h264, v1080av1, v720h264, aAAC}, 0, 15, "136+140", true},
		{"aac chosen over a bigger opus", []FitFormat{v720h264, aAAC, aOpus}, 0, 100, "136+140", true},
		{"opus never chosen, even when aac would not fit", []FitFormat{v720h264, aAAC, aOpusLow}, 0, 10, "", false},
		{"only vp9 and opus: nothing fits as it is", []FitFormat{v720vp9, aOpus}, 0, 100, "", false},
		{"same height and codecs: larger total wins", []FitFormat{v720h264, {ID: "298", VCodec: "h264", Height: 720, Size: 9e6}, aAAC}, 0, 100, "298+140", true},
		{"quality cap respected", []FitFormat{v1080h264, v720h264, aAAC}, 720, 100, "136+140", true},
		{"progressive used when nothing split fits", []FitFormat{v720h264, aAAC, prog360}, 0, 7, "18", true},
		{"split 480 beats progressive 360", []FitFormat{v480h264, aAAC, prog360}, 0, 10, "135+140", true},
		{"97% headroom: 9.7 MB budget excludes 9.8 MB", []FitFormat{{ID: "a", VCodec: "h264", Height: 720, Size: 6.8e6}, aAAC}, 0, 10, "", false},
		{"97% headroom: 9.6 MB fits", []FitFormat{{ID: "a", VCodec: "h264", Height: 720, Size: 6.6e6}, aAAC}, 0, 10, "a+140", true},
		{"unknown sizes are skipped", []FitFormat{{ID: "x", VCodec: "h264", Height: 1080}, v480h264, aAAC}, 0, 100, "135+140", true},
		{"nothing fits", []FitFormat{v1080h264, v720h264, aAAC}, 0, 5, "", false},
		{"video without audio never chosen alone", []FitFormat{v480h264}, 0, 100, "", false},
		{"unsupported video codec skipped", []FitFormat{{ID: "v8", VCodec: "vp8", Height: 720, Size: 1e6}, aAAC}, 0, 100, "", false},
		{"progressive webm skipped", []FitFormat{{ID: "43", VCodec: "vp8", ACodec: "vorbis", Ext: "webm", Height: 360, Size: 1e6}}, 0, 100, "", false},
		{"no formats", nil, 0, 10, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SelectFit(c.formats, c.cap, LimitBytes(c.limitMB))
			if ok != c.wantFits || (ok && got.Selector() != c.want) {
				t.Fatalf("SelectFit = %q, %v; want %q, %v", got.Selector(), ok, c.want, c.wantFits)
			}
			if ok && got.Size > float64(LimitBytes(c.limitMB))*0.97 {
				t.Fatalf("chosen size %v over budget", got.Size)
			}
		})
	}
}

func TestFitFormatsFromRawJSON(t *testing.T) {
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(`{"duration": 212.5, "formats": [
		{"format_id": "sb0", "vcodec": "none", "acodec": "none", "ext": "mhtml"},
		{"format_id": "140", "vcodec": "none", "acodec": "mp4a.40.2", "ext": "m4a", "filesize": 3400000},
		{"format_id": "251", "vcodec": "none", "acodec": "opus", "ext": "webm", "filesize_approx": 3500000},
		{"format_id": "137", "vcodec": "avc1.640028", "acodec": "none", "ext": "mp4", "width": 1920, "height": 1080, "filesize": null, "filesize_approx": 41000000},
		{"format_id": "616", "vcodec": "vp09.00.40.08", "acodec": "none", "ext": "mp4", "width": 1080, "height": 1920},
		{"format_id": "18", "vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "ext": "mp4", "width": 640, "height": 360, "filesize": 6000000},
		{"format_id": "140-drc", "vcodec": "none", "acodec": "mp4a.40.2", "ext": "m4a", "filesize": 3400000, "language_preference": 10, "quality": 3},
		{"format_id": "140-1", "vcodec": "none", "acodec": "mp4a.40.2", "ext": "m4a", "filesize": 3400000, "language_preference": -1, "quality": 3},
		{"format_id": "139", "vcodec": "none", "acodec": "mp4a.40.5", "ext": "m4a", "filesize": 1200000, "language_preference": 10, "quality": 2, "format_note": "low, DRC"}
	]}`))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	got := FitFormats(raw)
	want := []FitFormat{
		{ID: "140", ACodec: "aac", Ext: "m4a", Size: 3.4e6, Audio: AudioRank{LangPref: -1}},
		{ID: "251", ACodec: "opus", Ext: "webm", Size: 3.5e6, Audio: AudioRank{LangPref: -1}},
		{ID: "137", VCodec: "h264", Ext: "mp4", Height: 1080, Size: 41e6},
		// Portrait: the shorter side is the height yt-dlp's res sort uses
		{ID: "616", VCodec: "vp9", Ext: "mp4", Height: 1080},
		{ID: "18", VCodec: "h264", ACodec: "aac", Ext: "mp4", Height: 360, Size: 6e6, Audio: AudioRank{LangPref: -1}},
		{ID: "140-drc", ACodec: "aac", Ext: "m4a", Size: 3.4e6, Audio: AudioRank{LangPref: 10, DRC: true, Quality: 3}},
		{ID: "140-1", ACodec: "aac", Ext: "m4a", Size: 3.4e6, Audio: AudioRank{LangPref: -1, Quality: 3}},
		{ID: "139", ACodec: "aac", Ext: "m4a", Size: 1.2e6, Audio: AudioRank{LangPref: 10, DRC: true, Quality: 2}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("format %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if d := InfoDuration(raw); d != 212.5 {
		t.Errorf("duration %v", d)
	}
	if d := InfoDuration(map[string]any{"duration": nil}); d != 0 {
		t.Errorf("missing duration %v", d)
	}
	if h := SourceHeight(got, 0); h != 1080 {
		t.Errorf("SourceHeight = %d", h)
	}
	if h := SourceHeight(got, 720); h != 360 {
		t.Errorf("capped SourceHeight = %d", h)
	}
}

func TestQualityCap(t *testing.T) {
	if QualityCap("best") != 0 || QualityCap("720") != 720 {
		t.Fatal("QualityCap")
	}
}

func TestPlanEncode(t *testing.T) {
	cases := []struct {
		name                 string
		limitMB              int
		duration             float64
		maxHeight            int
		video, audio, height int
		capFPS               bool
	}{
		// 10 MB * 8 * 0.95 = 76,000,000 bits; / 60 s / 1000 = 1266 kbps total
		{"10 MB, 1 min (1266 kbps)", 10, 60, 1080, 1266 - 96, 96, 360, true},
		{"10 MB, 45 s (1688 kbps)", 10, 45, 1080, 1688 - 96, 96, 480, true},
		// 76e6 / 300 / 1000 = 253 kbps total: below 400, so audio drops to 64
		{"10 MB, 5 min", 10, 300, 1080, 253 - 64, 64, 240, true},
		// 50 MB: 380e6 bits / 60 / 1000 = 6333 kbps
		{"50 MB, 1 min", 50, 60, 2160, 6333 - 96, 96, 1080, false},
		{"50 MB, 1 min, 720p source", 50, 60, 720, 6333 - 96, 96, 720, false},
		{"25 MB, 1 min (3166 kbps)", 25, 60, 0, 3166 - 96, 96, 720, false},
		{"10 MB, 2 min (633 kbps)", 10, 120, 0, 633 - 96, 96, 240, true},
		{"never upscales a 144p source", 100, 10, 144, 75904, 96, 144, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanEncode(c.limitMB, c.duration, c.maxHeight)
			if err != nil {
				t.Fatal(err)
			}
			want := EncodePlan{VideoKbps: c.video, AudioKbps: c.audio, Height: c.height, CapFPS: c.capFPS}
			if plan != want {
				t.Fatalf("plan %+v, want %+v", plan, want)
			}
		})
	}
}

func TestPlanEncodeTooLong(t *testing.T) {
	// 76e6 bits / 600 s / 1000 = 126 kbps total, 62 for video after 64 audio: under the floor
	_, err := PlanEncode(10, 600, 0)
	var tooLong *TooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "Too long to fit 10 MB" {
		t.Fatalf("message %q", err.Error())
	}
	// Longest that fits: 76e6 / ((100 + 64) * 1000) = 463.4 s
	if tooLong.MaxDuration < 463 || tooLong.MaxDuration > 464 {
		t.Fatalf("max duration %v", tooLong.MaxDuration)
	}
	if tooLong.Detail() != "At 10 MB the longest video that fits is about 7:43; this one is 10:00." {
		t.Fatalf("detail %q", tooLong.Detail())
	}
	// And the boundary: 463 s still plans at the floor
	plan, err := PlanEncode(10, 463, 0)
	if err != nil || plan.VideoKbps < MinVideoKbps || plan.AudioKbps != 64 {
		t.Fatalf("plan at the floor = %+v, %v", plan, err)
	}

	if _, err := PlanEncode(10, 0, 0); !errors.Is(err, ErrUnknownDuration) {
		t.Fatalf("zero duration err = %v", err)
	}
}

func TestEncodePlanScaled(t *testing.T) {
	plan := EncodePlan{VideoKbps: 1000, AudioKbps: 96, Height: 480}.Scaled(0.85)
	if plan.VideoKbps != 850 || plan.AudioKbps != 96 || plan.Height != 480 {
		t.Fatalf("scaled %+v", plan)
	}
}

func TestSelectFitAudioPreference(t *testing.T) {
	original := FitFormat{ID: "140-0", ACodec: "aac", Size: 3e6, Audio: AudioRank{LangPref: 10, Quality: 3}}
	originalDRC := FitFormat{ID: "140-0-drc", ACodec: "aac", Size: 3.1e6, Audio: AudioRank{LangPref: 10, DRC: true, Quality: 3}}
	dub := FitFormat{ID: "140-1", ACodec: "aac", Size: 3.2e6, Audio: AudioRank{LangPref: -1, Quality: 3}}
	low := FitFormat{ID: "139-0", ACodec: "aac", Size: 1e6, Audio: AudioRank{LangPref: 10, Quality: 2}}
	smallDub := FitFormat{ID: "139-1", ACodec: "aac", Size: 1e6, Audio: AudioRank{LangPref: -1, Quality: 2}}
	cases := []struct {
		name    string
		formats []FitFormat
		limitMB int
		want    string
	}{
		{"a bigger DRC copy loses to the plain track", []FitFormat{v720h264, original, originalDRC}, 100, "136+140-0"},
		{"a bigger dub loses to the original", []FitFormat{v720h264, original, dub}, 100, "136+140-0"},
		// 720p + original (11 MB) is over the 10.67 MB budget, 720p + the small dub (9 MB) is not
		{"the original at 480p beats a dub at 720p", []FitFormat{v720h264, v480h264, original, smallDub}, 11, "135+140-0"},
		{"higher quality wins over a larger total", []FitFormat{v720h264, low, original}, 100, "136+140-0"},
		{"a lower quality track is used when the better one does not fit", []FitFormat{v720h264, originalDRC, low}, 10, "136+139-0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SelectFit(c.formats, 0, LimitBytes(c.limitMB))
			if !ok || got.Selector() != c.want {
				t.Fatalf("SelectFit = %q, %v; want %q", got.Selector(), ok, c.want)
			}
		})
	}
}

func TestPlaysAsIs(t *testing.T) {
	cases := []struct {
		ext, vcodec, acodec string
		want                bool
	}{
		{".mp4", "avc1.4d401f", "mp4a.40.2", true},
		{"mp4", "h264", "aac", true},
		{".MP4", "avc1.64001F", "none", true},
		{".mp4", "vp09.00.40.08", "mp4a.40.2", false},
		{".mp4", "av01.0.08M.08", "mp4a.40.2", false},
		{".mp4", "avc1.4d401f", "opus", false},
		{".webm", "avc1.4d401f", "mp4a.40.2", false},
		{".mp4", "", "", false},
	}
	for _, c := range cases {
		if got := PlaysAsIs(c.ext, c.vcodec, c.acodec); got != c.want {
			t.Errorf("PlaysAsIs(%q, %q, %q) = %v", c.ext, c.vcodec, c.acodec, got)
		}
	}
}

func TestEncodePlanCappedToSource(t *testing.T) {
	plan := EncodePlan{VideoKbps: 5000, AudioKbps: 96, Height: 1080}
	// 15 MB over 60 s = 2000 kbps in total, so video gets at most 2000 - 96
	if got := plan.CappedToSource(15_000_000, 60); got.VideoKbps != 1904 || got.Height != 1080 {
		t.Fatalf("capped %+v", got)
	}
	// A bigger source leaves the plan alone
	if got := plan.CappedToSource(100_000_000, 60); got != plan {
		t.Fatalf("over-sized source changed the plan: %+v", got)
	}
	// Never below the floor, and unknown inputs change nothing
	if got := plan.CappedToSource(100_000, 60); got.VideoKbps != MinVideoKbps {
		t.Fatalf("tiny source %+v", got)
	}
	if plan.CappedToSource(0, 60) != plan || plan.CappedToSource(15_000_000, 0) != plan {
		t.Fatal("unknown size or duration changed the plan")
	}
}

func TestSelectFitSkipsFormatsWithoutID(t *testing.T) {
	formats := []FitFormat{{VCodec: "h264", Height: 720, Size: 1e6}, aAAC}
	if _, ok := SelectFit(formats, 0, LimitBytes(100)); ok {
		t.Fatal("selected a format with no id")
	}
	if SourceHeight(formats, 0) != 720 {
		t.Fatal("id-less format ignored by SourceHeight")
	}
}

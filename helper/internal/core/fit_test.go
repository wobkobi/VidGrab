package core

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
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
			got, ok := SelectFit(c.formats, c.cap, LimitBytes(c.limitMB), "")
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
		{"format_id": "137", "vcodec": "avc1.640028", "acodec": "none", "ext": "mp4", "width": 1920, "height": 1080, "fps": 60, "filesize": null, "filesize_approx": 41000000},
		{"format_id": "616", "vcodec": "vp09.00.40.08", "acodec": "none", "ext": "mp4", "width": 1080, "height": 1920},
		{"format_id": "18", "vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "ext": "mp4", "width": 640, "height": 360, "fps": 29.97, "filesize": 6000000},
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
		{ID: "137", VCodec: "h264", Ext: "mp4", Height: 1080, Aspect: 1920.0 / 1080, FPS: 60, Size: 41e6, AudioNone: true},
		// Portrait: the shorter side is the height yt-dlp's res sort uses
		{ID: "616", VCodec: "vp9", Ext: "mp4", Height: 1080, Aspect: 1920.0 / 1080, AudioNone: true},
		{ID: "18", VCodec: "h264", ACodec: "aac", Ext: "mp4", Height: 360, Aspect: 640.0 / 360, FPS: 29.97, Size: 6e6, Audio: AudioRank{LangPref: -1}},
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
	if s := SourceShape(got, 0); s != (VideoShape{Height: 1080, Aspect: 1920.0 / 1080, FPS: 60}) {
		t.Errorf("SourceShape = %+v", s)
	}
	// The cap leaves only the 360p stream, so its frame rate is the source's
	if s := SourceShape(got, 720); s != (VideoShape{Height: 360, Aspect: 640.0 / 360, FPS: 29.97}) {
		t.Errorf("capped SourceShape = %+v", s)
	}
}

func TestSourceShapeNoAudio(t *testing.T) {
	silent := []FitFormat{{ID: "a", VCodec: "h264", Height: 720, AudioNone: true}, {ID: "b", VCodec: "h264", Height: 360, AudioNone: true}}
	if !SourceShape(silent, 0).NoAudio {
		t.Error("every format says acodec none, so the source is silent")
	}
	// An unknown acodec, or any format with sound, counts as having sound
	unknown := append(slices.Clone(silent), FitFormat{ID: "c", VCodec: "h264", Height: 480})
	withSound := append(slices.Clone(silent), FitFormat{ID: "d", ACodec: "aac"})
	for name, formats := range map[string][]FitFormat{"unknown": unknown, "sound": withSound, "empty": nil} {
		if SourceShape(formats, 0).NoAudio {
			t.Errorf("%s: NoAudio set", name)
		}
	}
}

func TestPlanEncodeSourceAudio(t *testing.T) {
	// 20 MB over 182 s: 835 kbps total, a 96k stereo step
	shape := VideoShape{Height: 1080, Aspect: 16.0 / 9, FPS: 24}
	cases := []struct {
		name     string
		audio    SourceAudio
		kbps, ch int
	}{
		{"unknown", SourceAudio{}, 96, 2},
		{"richer source", SourceAudio{Kbps: 128, Channels: 2, Codec: "mp4a.40.2"}, 96, 2},
		{"48k AAC caps", SourceAudio{Kbps: 48, Channels: 2, Codec: "mp4a.40.2"}, 48, 2},
		{"32k HE-AAC gets 1.5x", SourceAudio{Kbps: 32, Channels: 2, Codec: "mp4a.40.5"}, 48, 2},
		{"50k Opus gets 1.5x", SourceAudio{Kbps: 50, Channels: 2, Codec: "opus"}, 75, 2},
		{"mono stays mono at half", SourceAudio{Kbps: 128, Channels: 1, Codec: "mp4a.40.2"}, 48, 1},
		{"tiny source floors at 16", SourceAudio{Kbps: 8, Channels: 1, Codec: "mp4a.40.2"}, 16, 1},
	}
	for _, c := range cases {
		s := shape
		s.Audio = c.audio
		plan, err := PlanEncode(20, 182, s, CodecH264)
		if err != nil || plan.AudioKbps != c.kbps || plan.Channels != c.ch || plan.VideoKbps != 835-c.kbps {
			t.Errorf("%s: plan %+v, %v", c.name, plan, err)
		}
	}
}

func TestPlanEncodeNoAudio(t *testing.T) {
	// 8 MB over 60 s: 8e6 * 8 * 0.95 / 60 / 1000 = 1013 kbps, all of it video; 720p30 gets
	// 1013000 / (1280*720*30) = 0.037
	plan, err := PlanEncode(8, 60, VideoShape{Height: 1080, Aspect: 16.0 / 9, FPS: 30, NoAudio: true}, CodecH264)
	if err != nil || plan != (EncodePlan{Codec: CodecH264, VideoKbps: 1013, Height: 720, FPS: 30}) {
		t.Fatalf("plan %+v, %v", plan, err)
	}
	// The floor is 50 kbps of video alone: 20 MB lasts 152e6 / 50000 = 3040 s
	_, err = PlanEncode(20, 5230, VideoShape{Height: 2160, Aspect: 16.0 / 9, FPS: 24, NoAudio: true}, CodecH264)
	var tooLong *TooLongError
	if !errors.As(err, &tooLong) || math.Round(tooLong.MaxDuration) != 3040 {
		t.Fatalf("err %v", err)
	}
}

func TestQualityCap(t *testing.T) {
	if QualityCap("best") != 0 || QualityCap("720") != 720 {
		t.Fatal("QualityCap")
	}
}

func TestPlanEncode(t *testing.T) {
	wide := 16.0 / 9
	cases := []struct {
		name     string
		limitMB  int
		duration float64
		source   VideoShape
		want     EncodePlan
	}{
		// 20 MB * 8 * 0.95 = 152,000,000 bits; / 1421 s / 1000 = 106 kbps total: under 200, so
		// 32k mono audio and 74 video, which gets 10 fps. 360p: 74000 / (640*360*10) = 0.032
		// bits per pixel, just enough; 480p would get 0.018
		{"20 MB, 24 min screen recording", 20, 1421, VideoShape{1080, wide, 30, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 74, AudioKbps: 32, Channels: 1, Height: 360, FPS: 10}},
		// 152e6 / 182 / 1000 = 835 kbps total: 96k stereo, 739 video at the source's 24 fps.
		// 720p: 739000 / (1280*720*24) = 0.033; 1080p would get 0.015
		{"20 MB, 3 min music video", 20, 182, VideoShape{1080, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 720, FPS: 24}},
		// The height is the shorter side, so 9:16 portrait plans like landscape
		{"portrait 9:16", 20, 182, VideoShape{1080, 1920.0 / 1080, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 720, FPS: 24}},
		// A taller phone frame has more pixels per line: 720*1560*24 gives 0.027, so 480p
		{"portrait 9:19.5", 20, 182, VideoShape{1080, 2340.0 / 1080, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 480, FPS: 24}},
		{"never above a 480p source", 20, 182, VideoShape{480, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 480, FPS: 24}},
		{"an odd source height is kept", 20, 182, VideoShape{400, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 400, FPS: 24}},
		// 50 MB: 380e6 bits / 60 / 1000 = 6333 kbps; a 60 fps source keeps every other frame
		{"60 fps source capped to 30", 50, 60, VideoShape{2160, wide, 60, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 6237, AudioKbps: 96, Channels: 2, Height: 1080, FPS: 30}},
		{"50 fps source halved to 25", 50, 60, VideoShape{1080, wide, 50, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 6237, AudioKbps: 96, Channels: 2, Height: 1080, FPS: 25}},
		// 10 MB: 76e6 / 300 / 1000 = 253 kbps total: 64k stereo, 189 video at 10 fps.
		// Nothing known about the source: 16:9, 10 fps, 480p gets 189000 / (853*480*10) = 0.046
		{"unknown source", 10, 300, VideoShape{},
			EncodePlan{Codec: CodecH264, VideoKbps: 189, AudioKbps: 64, Channels: 2, Height: 480, FPS: 10}},
		// Every third frame of 25 fps (8.3) plays more evenly than an uneven 10
		{"25 fps source at 10 fps", 10, 300, VideoShape{1080, wide, 25, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 189, AudioKbps: 64, Channels: 2, Height: 480, FPS: 25.0 / 3}},
		// 76e6 / 200 / 1000 = 380 kbps total: 316 video, so up to 15 fps; 24 fps halves to 12
		{"24 fps source at 15 fps", 10, 200, VideoShape{1080, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 316, AudioKbps: 64, Channels: 2, Height: 480, FPS: 12}},
		{"30 fps source at 15 fps", 10, 200, VideoShape{1080, wide, 30, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 316, AudioKbps: 64, Channels: 2, Height: 480, FPS: 15}},
		{"a slower source keeps its rate", 50, 60, VideoShape{1080, wide, 23.976, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 6237, AudioKbps: 96, Channels: 2, Height: 1080, FPS: 23.976}},
		{"never upscales a 144p source", 100, 10, VideoShape{144, wide, 30, false, SourceAudio{}},
			EncodePlan{Codec: CodecH264, VideoKbps: 75904, AudioKbps: 96, Channels: 2, Height: 144, FPS: 30}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanEncode(c.limitMB, c.duration, c.source, "")
			if err != nil {
				t.Fatal(err)
			}
			if plan != c.want {
				t.Fatalf("plan %+v, want %+v", plan, c.want)
			}
		})
	}
}

func TestPlanEncodeAudioSteps(t *testing.T) {
	// 76e6 bits / 380 s / 1000 = 200 kbps total exactly: not under 200, so still stereo.
	// 189 s gives 402 kbps and 191 s 397
	cases := []struct {
		duration        float64
		audio, channels int
	}{{379, 64, 2}, {380, 64, 2}, {381, 32, 1}, {189, 96, 2}, {191, 64, 2}}
	for _, c := range cases {
		plan, err := PlanEncode(10, c.duration, VideoShape{}, "")
		if err != nil || plan.AudioKbps != c.audio || plan.Channels != c.channels {
			t.Errorf("%v s: plan %+v, %v", c.duration, plan, err)
		}
	}
}

func TestEncodePlanPrefersX264(t *testing.T) {
	if !(EncodePlan{VideoKbps: 1499}).PrefersX264() || (EncodePlan{VideoKbps: 1500}).PrefersX264() {
		t.Fatal("libx264 must be preferred below 1500 kbps only")
	}
}

func TestPlanEncodeTooLong(t *testing.T) {
	// 76e6 bits / 1000 s / 1000 = 76 kbps total, 44 for video after 32 audio: under the floor
	_, err := PlanEncode(10, 1000, VideoShape{}, "")
	var tooLong *TooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "Too long to fit 10 MB" {
		t.Fatalf("message %q", err.Error())
	}
	// Longest that fits: 76e6 / ((50 + 32) * 1000) = 926.8 s
	if tooLong.MaxDuration < 926 || tooLong.MaxDuration > 927 {
		t.Fatalf("max duration %v", tooLong.MaxDuration)
	}
	if tooLong.Detail() != "At 10 MB the longest video that fits is about 15:26; this one is 16:40." {
		t.Fatalf("detail %q", tooLong.Detail())
	}
	// And the boundary: 926 s still plans at the floor, 10 fps 240p in mono
	plan, err := PlanEncode(10, 926, VideoShape{}, "")
	if want := (EncodePlan{VideoKbps: MinVideoKbps, AudioKbps: 32, Channels: 1, Height: 240, FPS: 10, Codec: CodecH264}); err != nil || plan != want {
		t.Fatalf("plan at the floor = %+v, %v; want %+v", plan, err, want)
	}

	if _, err := PlanEncode(10, 0, VideoShape{}, ""); !errors.Is(err, ErrUnknownDuration) {
		t.Fatalf("zero duration err = %v", err)
	}
}

func TestEncodePlanShrunk(t *testing.T) {
	base := EncodePlan{VideoKbps: 1000, AudioKbps: 96, Height: 480}
	// 3% over a 10 MB limit across 100 s: (10.3e6 - 9.7e6) * 8 / 1000 / 100 = 48 kbps comes off,
	// under the 10% minimum, so 900
	if plan := base.Shrunk(10_300_000, 10_000_000, 100); plan.VideoKbps != 900 || plan.AudioKbps != 96 || plan.Height != 480 {
		t.Fatalf("small overshoot %+v", plan)
	}
	// 30% over: (13e6 - 9.7e6) * 8 / 1000 / 100 = 264 kbps comes off
	if plan := base.Shrunk(13_000_000, 10_000_000, 100); plan.VideoKbps != 736 {
		t.Fatalf("large overshoot %+v", plan)
	}
	// Rounds down and never reaches 0
	if plan := (EncodePlan{VideoKbps: 4}).Shrunk(1e9, 1e6, 10); plan.VideoKbps != 1 {
		t.Fatalf("floor %+v", plan)
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
			got, ok := SelectFit(c.formats, 0, LimitBytes(c.limitMB), "")
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
		if got := PlaysAsIs(c.ext, c.vcodec, c.acodec, CodecH264); got != c.want {
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
	if _, ok := SelectFit(formats, 0, LimitBytes(100), ""); ok {
		t.Fatal("selected a format with no id")
	}
	if SourceShape(formats, 0).Height != 720 {
		t.Fatal("id-less format ignored by SourceShape")
	}
}

func TestPlaysAsIsAV1(t *testing.T) {
	if !PlaysAsIs(".mp4", "av01.0.08M.08", "mp4a.40.2", CodecAV1) {
		t.Error("an AV1 job should keep an AV1 + AAC MP4")
	}
	if !PlaysAsIs(".mp4", "avc1.4d401f", "mp4a.40.2", CodecAV1) {
		t.Error("an AV1 job should keep an H.264 + AAC MP4 that fits")
	}
	if PlaysAsIs(".mp4", "vp09.00.40.08", "mp4a.40.2", CodecAV1) {
		t.Error("VP9 is never kept")
	}
}

func TestSelectFitAV1(t *testing.T) {
	formats := []FitFormat{
		{ID: "134", VCodec: "h264", Ext: "mp4", Height: 360, Size: 4e6},
		{ID: "397", VCodec: "av1", Ext: "mp4", Height: 480, Size: 5e6},
		{ID: "140", ACodec: "aac", Ext: "m4a", Size: 3e6, Audio: AudioRank{LangPref: -1}},
	}
	if got, ok := SelectFit(formats, 0, LimitBytes(10), CodecH264); !ok || got.Selector() != "134+140" {
		t.Errorf("H.264 job picked %+v, %v", got, ok)
	}
	if got, ok := SelectFit(formats, 0, LimitBytes(10), CodecAV1); !ok || got.Selector() != "397+140" {
		t.Errorf("AV1 job picked %+v, %v", got, ok)
	}
}

func TestPlanEncodeAV1(t *testing.T) {
	wide := 16.0 / 9
	cases := []struct {
		name     string
		limitMB  int
		duration float64
		source   VideoShape
		want     EncodePlan
	}{
		// Same 74 kbps budget as the H.264 screen recording, but AV1 keeps 30 fps (half of 60)
		// and 360p: 74000 / (640*360*30) = 0.0107, over 0.010; 480p would get 0.0060
		{"20 MB, 24 min, 60 fps source", 20, 1421, VideoShape{1080, wide, 60, false, SourceAudio{}},
			EncodePlan{Codec: CodecAV1, VideoKbps: 74, AudioKbps: 32, Channels: 1, Height: 360, FPS: 30}},
		// At 24 fps 360p gets 0.0134, enough
		{"20 MB, 24 min, 24 fps source", 20, 1421, VideoShape{1080, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecAV1, VideoKbps: 74, AudioKbps: 32, Channels: 1, Height: 360, FPS: 24}},
		// 739 kbps at 24 fps: 1080p gets 739000 / (1920*1080*24) = 0.0148
		{"20 MB, 3 min music video", 20, 182, VideoShape{1080, wide, 24, false, SourceAudio{}},
			EncodePlan{Codec: CodecAV1, VideoKbps: 739, AudioKbps: 96, Channels: 2, Height: 1080, FPS: 24}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan, err := PlanEncode(c.limitMB, c.duration, c.source, CodecAV1)
			if err != nil || plan != c.want {
				t.Fatalf("plan %+v, %v; want %+v", plan, err, c.want)
			}
			if plan.PrefersX264() {
				t.Fatal("an AV1 plan never uses libx264")
			}
		})
	}
	if FitCodec("vp9") != CodecH264 || FitCodec("any") != CodecH264 || FitCodec("av1") != CodecAV1 {
		t.Fatal("FitCodec maps only av1 to AV1")
	}
}

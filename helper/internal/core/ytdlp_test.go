package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

const testURL = "https://www.youtube.com/watch?v=jNQXAC9IVRw"

var (
	testOut   = filepath.Join(`C:\Users\me`, "Downloads")
	testTools = Tools{YtDlp: "/opt/yt-dlp"}
)

func valueAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("%s missing from %v", flag, args)
	}
	return args[i+1]
}

func options(overrides map[string]string) JobOptions {
	opts := DefaultJobOptions()
	for key, value := range overrides {
		switch key {
		case "kind":
			opts.Kind = value
		case "quality":
			opts.Quality = value
		case "codec":
			opts.Codec = value
		case "container":
			opts.Container = value
		case "audio_format":
			opts.AudioFormat = value
		case "audio_bitrate":
			opts.AudioBitrate = value
		}
	}
	return opts
}

func TestVideoArgsEveryCombination(t *testing.T) {
	codecFields := map[string]string{"any": "", "h264": "vcodec:h264", "vp9": "vcodec:vp9", "av1": "vcodec:av01"}
	for _, quality := range Qualities {
		for _, codec := range Codecs {
			for _, container := range Containers {
				opts := options(map[string]string{"kind": "video", "quality": quality, "codec": codec, "container": container})
				args := BuildArgs(opts, testURL, testOut, false, 6, true, testTools)
				name := quality + "/" + codec + "/" + container

				if args[0] != testTools.YtDlp {
					t.Errorf("%s: argv[0] = %q", name, args[0])
				}
				if !reflect.DeepEqual(args[len(args)-2:], []string{"--", testURL}) {
					t.Errorf("%s: url not last", name)
				}
				if got := valueAfter(t, args, "-f"); got != "bv*+ba/b" {
					t.Errorf("%s: -f %q", name, got)
				}
				if got := valueAfter(t, args, "--merge-output-format"); got != container {
					t.Errorf("%s: merge format %q", name, got)
				}
				if got := valueAfter(t, args, "--concurrent-fragments"); got != "6" {
					t.Errorf("%s: fragments %q", name, got)
				}
				sortKeys := strings.Split(valueAfter(t, args, "-S"), ",")
				wantRes := "res:" + quality
				if quality == "best" {
					wantRes = "res"
				}
				if sortKeys[0] != wantRes {
					t.Errorf("%s: sort %v", name, sortKeys)
				}
				if field := codecFields[codec]; field != "" {
					if sortKeys[1] != field {
						t.Errorf("%s: sort %v", name, sortKeys)
					}
				} else if slices.ContainsFunc(sortKeys, func(s string) bool { return strings.HasPrefix(s, "vcodec") }) {
					t.Errorf("%s: unexpected codec sort %v", name, sortKeys)
				}
				if slices.Contains(sortKeys, "acodec:aac") != (container == "mp4") {
					t.Errorf("%s: acodec sort %v", name, sortKeys)
				}
				if slices.Contains(args, "-x") {
					t.Errorf("%s: -x present", name)
				}
				for _, flag := range []string{"--no-playlist", "--windows-filenames", "--no-mtime", "--newline", "--continue", "--ignore-config"} {
					if !slices.Contains(args, flag) {
						t.Errorf("%s: %s missing", name, flag)
					}
				}
				if slices.Contains(args, "--cookies-from-browser") || slices.Contains(args, "--ffmpeg-location") {
					t.Errorf("%s: unexpected cookies or ffmpeg-location", name)
				}
			}
		}
	}
}

func TestAudioArgsEveryCombination(t *testing.T) {
	for _, format := range AudioFormats {
		for _, bitrate := range AudioBitrates {
			opts := options(map[string]string{"kind": "audio", "audio_format": format, "audio_bitrate": bitrate})
			args := BuildArgs(opts, testURL, testOut, false, 4, true, testTools)
			name := format + "/" + bitrate
			if !slices.Contains(args, "-x") || valueAfter(t, args, "--audio-format") != format {
				t.Errorf("%s: extract args wrong: %v", name, args)
			}
			if slices.Contains(args, "--merge-output-format") || slices.Contains(args, "-S") {
				t.Errorf("%s: video-only flags present", name)
			}
			if format == "mp3" {
				if valueAfter(t, args, "-f") != "ba/b" || valueAfter(t, args, "--audio-quality") != bitrate+"K" {
					t.Errorf("%s: mp3 args wrong: %v", name, args)
				}
			} else if valueAfter(t, args, "-f") != "ba[ext=m4a]/ba/b" || slices.Contains(args, "--audio-quality") {
				t.Errorf("%s: m4a args wrong: %v", name, args)
			}
		}
	}
}

func printsOf(args []string) []string {
	var prints []string
	for i, a := range args {
		if a == "--print" {
			prints = append(prints, args[i+1])
		}
	}
	return prints
}

func TestProgressAndFileTemplates(t *testing.T) {
	args := BuildArgs(DefaultJobOptions(), testURL, testOut, false, 4, true, testTools)
	if got := valueAfter(t, args, "--progress-template"); got != "download:YTDLP_PROGRESS %(progress)j" {
		t.Errorf("progress template %q", got)
	}
	prints := printsOf(args)
	if !slices.Contains(prints, "after_move:YTDLP_FILE %(filepath)s") || !slices.Contains(prints, "before_dl:YTDLP_TITLE %(title)s") {
		t.Errorf("prints %v", prints)
	}
	if got := valueAfter(t, args, "-o"); got != filepath.Join(testOut, "%(title)s [%(id)s].%(ext)s") {
		t.Errorf("-o %q", got)
	}
	if !slices.Contains(args, "--progress") || !slices.Contains(args, "--no-quiet") {
		t.Error("--progress/--no-quiet missing")
	}
}

func TestCookiesFlag(t *testing.T) {
	args := BuildArgs(DefaultJobOptions(), testURL, testOut, true, 4, true, testTools)
	if got := valueAfter(t, args, "--cookies-from-browser"); got != "firefox" {
		t.Fatalf("cookies %q", got)
	}
}

func TestFfmpegLocation(t *testing.T) {
	tools := Tools{YtDlp: "yt-dlp", FfmpegDir: `C:\VidGrab\tools\ffmpeg`}
	args := BuildArgs(DefaultJobOptions(), testURL, testOut, false, 4, true, tools)
	if got := valueAfter(t, args, "--ffmpeg-location"); got != tools.FfmpegDir {
		t.Fatalf("ffmpeg-location %q", got)
	}
}

func TestWithoutFfmpegRequestsSingleFileFormats(t *testing.T) {
	video := BuildArgs(options(map[string]string{"quality": "720"}), testURL, testOut, false, 4, false, testTools)
	if valueAfter(t, video, "-f") != "b" || slices.Contains(video, "--merge-output-format") {
		t.Errorf("video args %v", video)
	}
	m4a := BuildArgs(options(map[string]string{"kind": "audio", "audio_format": "m4a"}), testURL, testOut, false, 4, false, testTools)
	if valueAfter(t, m4a, "-f") != "ba[ext=m4a]" || slices.Contains(m4a, "-x") {
		t.Errorf("m4a args %v", m4a)
	}
}

func TestMetadataEmbeddedWithFfmpeg(t *testing.T) {
	video := BuildArgs(DefaultJobOptions(), testURL, testOut, false, 4, true, testTools)
	if !slices.Contains(video, "--embed-metadata") {
		t.Error("--embed-metadata missing")
	}
	summary := valueAfter(t, video, "--parse-metadata")
	// before_dl runs after format selection, so the chosen stream's fields are filled in
	const suffix = ":(?P<meta_comment>.+)"
	if !strings.HasPrefix(summary, "before_dl:") || !strings.HasSuffix(summary, suffix) {
		t.Errorf("parse-metadata %q", summary)
	}
	if strings.Contains(strings.TrimSuffix(strings.TrimPrefix(summary, "before_dl:"), suffix), ":") {
		t.Errorf("summary template contains ':': %q", summary)
	}

	audio := BuildArgs(options(map[string]string{"kind": "audio"}), testURL, testOut, false, 4, true, testTools)
	if !slices.Contains(audio, "--embed-metadata") || slices.Contains(audio, "--parse-metadata") {
		t.Errorf("audio args %v", audio)
	}
}

func TestNoMetadataWithoutFfmpeg(t *testing.T) {
	for _, opts := range []JobOptions{options(map[string]string{"quality": "720"}), options(map[string]string{"kind": "audio", "audio_format": "m4a"})} {
		args := BuildArgs(opts, testURL, testOut, false, 4, false, testTools)
		if slices.Contains(args, "--embed-metadata") || slices.Contains(args, "--parse-metadata") {
			t.Errorf("%+v: metadata flags present", opts)
		}
	}
}

func TestUrlIsLastAfterSeparator(t *testing.T) {
	args := BuildArgs(DefaultJobOptions(), "-o/evil", testOut, false, 4, true, testTools)
	if !reflect.DeepEqual(args[len(args)-2:], []string{"--", "-o/evil"}) {
		t.Fatalf("tail %v", args[len(args)-2:])
	}
}

func TestExpectedStreams(t *testing.T) {
	cases := []struct {
		kind   string
		ffmpeg bool
		want   int
	}{{"video", true, 2}, {"video", false, 1}, {"audio", true, 1}}
	for _, c := range cases {
		if got := ExpectedStreams(options(map[string]string{"kind": c.kind}), c.ffmpeg); got != c.want {
			t.Errorf("ExpectedStreams(%s, %v) = %d", c.kind, c.ffmpeg, got)
		}
	}
}

func progress(fields map[string]any) string {
	data, _ := json.Marshal(fields)
	return "YTDLP_PROGRESS " + string(data)
}

func percentOf(t *testing.T, u *ParseUpdate) float64 {
	t.Helper()
	if u == nil || u.Percent == nil {
		t.Fatalf("update has no percent: %+v", u)
	}
	return *u.Percent
}

func TestParserMergeRunIsMonotonic(t *testing.T) {
	parser := NewOutputParser(2)
	lines := []string{
		"[youtube] Extracting URL: x",
		"YTDLP_TITLE Me at the zoo",
		`[download] Destination: C:\D\Me at the zoo [jNQXAC9IVRw].f134.mp4`,
		progress(map[string]any{"status": "downloading", "_percent": 50.0, "speed": 2_621_440, "eta": 65}),
		progress(map[string]any{"status": "finished", "_percent": 100.0}),
		`[download] Destination: C:\D\Me at the zoo [jNQXAC9IVRw].f140.m4a`,
		progress(map[string]any{"status": "downloading", "_percent": 10.0, "speed": 1024, "eta": 3}),
		progress(map[string]any{"status": "downloading", "downloaded_bytes": 50, "total_bytes": 100}),
		`[Merger] Merging formats into "C:\D\Me at the zoo [jNQXAC9IVRw].mp4"`,
		`YTDLP_FILE C:\D\Me at the zoo [jNQXAC9IVRw].mp4`,
	}
	updates := make([]*ParseUpdate, len(lines))
	for i, line := range lines {
		updates[i] = parser.Feed(line)
	}

	if updates[1] == nil || updates[1].Title != "Me at the zoo" {
		t.Errorf("title update %+v", updates[1])
	}
	if percentOf(t, updates[3]) != 45.0 || updates[3].Speed != "2.5 MB/s" || updates[3].Eta != "1:05" {
		t.Errorf("first progress %+v", updates[3])
	}
	if percentOf(t, updates[4]) != 90.0 || updates[4].Speed != "" {
		t.Errorf("finished %+v", updates[4])
	}
	if percentOf(t, updates[6]) != 91.0 {
		t.Errorf("second stream %v", *updates[6].Percent)
	}
	if percentOf(t, updates[7]) != 95.0 {
		t.Errorf("bytes progress %v", *updates[7].Percent)
	}
	if updates[8] == nil || !updates[8].Processing {
		t.Errorf("merger %+v", updates[8])
	}
	want := `C:\D\Me at the zoo [jNQXAC9IVRw].mp4`
	if updates[9] == nil || updates[9].Filepath != want {
		t.Errorf("file %+v", updates[9])
	}
	var percents []float64
	for _, u := range updates {
		if u != nil && u.Percent != nil {
			percents = append(percents, *u.Percent)
		}
	}
	if !sort.Float64sAreSorted(percents) {
		t.Errorf("percents not monotonic: %v", percents)
	}
	if parser.Filepath != want {
		t.Errorf("parser filepath %q", parser.Filepath)
	}
}

func TestParserResumedMergeCountsFinishedStream(t *testing.T) {
	parser := NewOutputParser(2)
	if got := percentOf(t, parser.Feed(`[download] C:\D\Clip [jNQXAC9IVRw].f137.mp4 has already been downloaded`)); got != 90.0 {
		t.Errorf("already downloaded = %v", got)
	}
	parser.Feed(`[download] Destination: C:\D\Clip [jNQXAC9IVRw].f140.m4a`)
	if got := percentOf(t, parser.Feed(progress(map[string]any{"status": "downloading", "_percent": 50.0}))); got != 95.0 {
		t.Errorf("second stream = %v", got)
	}
	// A finished stream reported as already downloaded is not the final file
	parser.Feed(`[Merger] Merging formats into "C:\D\Clip [jNQXAC9IVRw].mp4"`)
	parser.Feed(`YTDLP_FILE C:\D\Clip [jNQXAC9IVRw].mp4`)
	if parser.PreExisting() {
		t.Error("resumed merge reported as pre-existing")
	}
}

func TestParserPreExistingFinalFile(t *testing.T) {
	path := filepath.Join("D", "Clip [jNQXAC9IVRw].mp3")
	parser := NewOutputParser(1)
	parser.Feed("[download] " + path + " has already been downloaded")
	parser.Feed("YTDLP_FILE " + path)
	if !parser.PreExisting() {
		t.Error("already downloaded final file not reported")
	}
	if NewOutputParser(1).PreExisting() {
		t.Error("empty run reported as pre-existing")
	}
}

func TestStripID(t *testing.T) {
	dir := filepath.Join("out", "List")
	cases := []struct {
		base, want string
		ok         bool
	}{
		{"Clip [jNQXAC9IVRw]", "Clip", true},
		{"Song [Live] [jNQXAC9IVRw]", "Song [Live]", true},
		{"Echo [jNQXAC9IVRw] [jNQXAC9IVRw]", "Echo [jNQXAC9IVRw]", true},
		{"Clip [jNQXAC9IVRw] (20 MB)", "Clip [jNQXAC9IVRw] (20 MB)", false},
		{"Clip [dQw4w9WgXcQ]", "Clip [dQw4w9WgXcQ]", false},
		{"Clip[jNQXAC9IVRw]", "Clip[jNQXAC9IVRw]", false},
		{" [jNQXAC9IVRw]", " [jNQXAC9IVRw]", false},
		{". [jNQXAC9IVRw]", ". [jNQXAC9IVRw]", false},
	}
	for _, c := range cases {
		got, ok := StripID(filepath.Join(dir, c.base), "jNQXAC9IVRw")
		if got != filepath.Join(dir, c.want) || ok != c.ok {
			t.Errorf("%q: %q %v", c.base, got, ok)
		}
	}
}

func TestSamePath(t *testing.T) {
	a := filepath.Join("D", "Clip.mp4")
	if !SamePath(a, filepath.Join("D", ".", "Clip.mp4")) || SamePath(a, filepath.Join("D", "Other.mp4")) || SamePath("", "") {
		t.Error("SamePath")
	}
}

func TestParserAudioExtractMarksProcessing(t *testing.T) {
	parser := NewOutputParser(1)
	parser.Feed("[download] Destination: a [x].webm")
	if got := percentOf(t, parser.Feed(progress(map[string]any{"status": "downloading", "_percent": 40.0}))); got != 40.0 {
		t.Errorf("percent = %v", got)
	}
	if u := parser.Feed("[ExtractAudio] Destination: a [x].mp3"); u == nil || !u.Processing {
		t.Errorf("extract update %+v", u)
	}
}

func TestParserFallbackRegexAndFragments(t *testing.T) {
	var parser OutputParser
	if got := percentOf(t, parser.Feed("[download]  12.5% of 10.00MiB at 1.00MiB/s ETA 00:09")); got != 12.5 {
		t.Errorf("fallback = %v", got)
	}
	if got := percentOf(t, parser.Feed(progress(map[string]any{"status": "downloading", "fragment_index": 3, "fragment_count": 4}))); got != 75.0 {
		t.Errorf("fragments = %v", got)
	}
	// Lower values never move progress backwards
	if got := percentOf(t, parser.Feed("[download]   5.0% of 10.00MiB")); got != 75.0 {
		t.Errorf("backwards = %v", got)
	}
}

func TestParserErrorsAndDetail(t *testing.T) {
	var parser OutputParser
	for i := 0; i < 30; i++ {
		parser.Feed(fmt.Sprintf("line %d", i))
	}
	parser.Feed("ERROR: [youtube] jNQXAC9IVRw: Sign in to confirm your age")
	if parser.LastError != "Sign in to confirm your age" {
		t.Errorf("last error %q", parser.LastError)
	}
	detail := strings.Split(parser.Detail(), "\n")
	if len(detail) != 20 || !strings.HasPrefix(detail[len(detail)-1], "ERROR:") {
		t.Errorf("detail %v", detail)
	}
}

func TestParserIgnoresGarbageProgress(t *testing.T) {
	var parser OutputParser
	for _, line := range []string{"YTDLP_PROGRESS not json", "YTDLP_PROGRESS [1, 2]", "YTDLP_PROGRESS null", ""} {
		if u := parser.Feed(line); u != nil {
			t.Errorf("Feed(%q) = %+v, want nil", line, u)
		}
	}
}

func TestCleanError(t *testing.T) {
	cases := []struct{ line, want string }{
		{"ERROR: unable to download video data: HTTP Error 403: Forbidden", "unable to download video data: HTTP Error 403: Forbidden"},
		{"ERROR: [youtube:tab] PLx: The playlist does not exist", "The playlist does not exist"},
		{"ERROR: " + strings.Repeat("é", 400), strings.Repeat("é", 300)},
	}
	for _, c := range cases {
		if got := CleanError(c.line); got != c.want {
			t.Errorf("CleanError(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestLastErrorAndTail(t *testing.T) {
	text := "ERROR: first\r\nWARNING: x\r\nERROR: [youtube] abc: second\r\n"
	if got := LastError(text); got != "second" {
		t.Errorf("LastError = %q", got)
	}
	if got := LastError("all fine\n"); got != "" {
		t.Errorf("LastError = %q", got)
	}
	if got := Tail("\n a\nb\nc\n\n", 2); got != "b\nc" {
		t.Errorf("Tail = %q", got)
	}
}

func TestFormatSpeed(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{{nil, ""}, {0, ""}, {512, "512 B/s"}, {2048, "2.0 KB/s"}, {5 * 1024 * 1024 * 1024, "5.0 GB/s"}}
	for _, c := range cases {
		if got := formatSpeedValue(c.value); got != c.want {
			t.Errorf("format speed %v = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestFormatEta(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{{nil, ""}, {-1, ""}, {5, "0:05"}, {3725, "1:02:05"}}
	for _, c := range cases {
		if got := formatEtaValue(c.value); got != c.want {
			t.Errorf("format eta %v = %q, want %q", c.value, got, c.want)
		}
	}
}

// decodeRaw decodes a JSON fixture the way FetchRawInfo does.
func decodeRaw(t *testing.T, text string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

const videoRaw = `{
	"id": "jNQXAC9IVRw", "title": "Me at the zoo", "uploader": "jawed", "duration": 19,
	"formats": [
		{"format_id": "sb0", "vcodec": "none", "acodec": "none", "ext": "mhtml"},
		{"format_id": "140", "vcodec": "none", "acodec": "mp4a.40.2", "filesize": 300},
		{"format_id": "251", "vcodec": "none", "acodec": "opus", "filesize_approx": 250},
		{"format_id": "18", "vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "height": 360, "filesize": 900},
		{"format_id": "134", "vcodec": "avc1.4d401e", "acodec": "none", "height": 360, "filesize": 700},
		{"format_id": "243", "vcodec": "vp9", "acodec": "none", "height": 360},
		{"format_id": "160", "vcodec": "avc1.4d400c", "acodec": "none", "height": 144, "filesize": 100},
		{"format_id": "394", "vcodec": "av01.0.00M.08", "acodec": "none", "height": 144, "filesize_approx": 80}
	]
}`

func TestSummariseVideo(t *testing.T) {
	summary := SummariseInfo(decodeRaw(t, videoRaw), KindVideo)
	if summary.Kind != "video" || summary.Title != "Me at the zoo" || summary.Entries != nil || summary.EntryCount != nil {
		t.Errorf("summary %+v", summary)
	}
	data, _ := json.Marshal(summary.Qualities)
	want := `[{"height":360,"codecs":["h264","vp9"],"filesize_est":1000},{"height":144,"codecs":["av1","h264"],"filesize_est":400}]`
	if string(data) != want {
		t.Errorf("qualities %s\nwant %s", data, want)
	}
	full, _ := json.Marshal(summary)
	if !strings.Contains(string(full), `"duration":19,`) || !strings.Contains(string(full), `"entry_count":null,"entries":null`) {
		t.Errorf("json %s", full)
	}
}

func TestSummariseVerticalVideoUsesShorterSide(t *testing.T) {
	raw := decodeRaw(t, `{"id": "abcdefghijk", "formats": [
		{"vcodec": "avc1.640028", "acodec": "none", "width": 1080, "height": 1920},
		{"vcodec": "vp09.00.40.08", "acodec": "none", "width": 720, "height": 1280},
		{"vcodec": "avc1.42001E", "acodec": "mp4a.40.2", "width": 360, "height": 640},
		{"vcodec": "avc1.4d401e", "acodec": "none", "height": 480}
	]}`)
	var heights []int
	for _, q := range SummariseInfo(raw, KindVideo).Qualities {
		heights = append(heights, q.Height)
	}
	if !reflect.DeepEqual(heights, []int{1080, 720, 480, 360}) {
		t.Fatalf("heights %v", heights)
	}
}

func TestSummarisePlaylist(t *testing.T) {
	raw := decodeRaw(t, `{"id": "PL1", "title": "List", "channel": "Chan", "duration": 99,
		"entries": [{"id": "jNQXAC9IVRw", "title": "A"}, {"id": "dQw4w9WgXcQ", "title": "B"}, null, {"title": "no id"}]}`)
	summary := SummariseInfo(raw, KindPlaylist)
	if summary.Uploader != "Chan" || summary.EntryCount == nil || *summary.EntryCount != 2 || summary.Duration != nil {
		t.Errorf("summary %+v", summary)
	}
	want := []InfoEntry{{ID: "jNQXAC9IVRw", Title: "A"}, {ID: "dQw4w9WgXcQ", Title: "B"}}
	if !reflect.DeepEqual(summary.Entries, want) {
		t.Errorf("entries %+v", summary.Entries)
	}
	data, _ := json.Marshal(summary)
	if !strings.Contains(string(data), `"qualities":[]`) {
		t.Errorf("json %s", data)
	}
}

func TestFetchRawInfoSuccessAndFlags(t *testing.T) {
	var calls [][]string
	runner := func(_ context.Context, argv []string) (string, string, int, error) {
		calls = append(calls, argv)
		return `{"id": "x"}`, "", 0, nil
	}
	tools := Tools{YtDlp: "yt-dlp.exe", FfmpegDir: `C:\ff`}
	raw, err := FetchRawInfo(context.Background(), tools, testURL, true, true, runner)
	if err != nil || !reflect.DeepEqual(raw, map[string]any{"id": "x"}) {
		t.Fatalf("raw %v, %v", raw, err)
	}
	first := calls[0]
	if first[0] != "yt-dlp.exe" || !slices.Contains(first, "--flat-playlist") || !slices.Contains(first, "-J") || !slices.Contains(first, "--cookies-from-browser") {
		t.Errorf("argv %v", first)
	}
	if valueAfter(t, first, "--ffmpeg-location") != `C:\ff` || !reflect.DeepEqual(first[len(first)-2:], []string{"--", testURL}) {
		t.Errorf("argv %v", first)
	}
	if _, err := FetchRawInfo(context.Background(), testTools, testURL, false, false, runner); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls[1], "--no-playlist") || slices.Contains(calls[1], "--ffmpeg-location") {
		t.Errorf("argv %v", calls[1])
	}
}

func TestFetchRawInfoErrors(t *testing.T) {
	failing := func(context.Context, []string) (string, string, int, error) {
		return "", "WARNING: x\nERROR: [youtube] abc: Private video", 1, nil
	}
	_, err := FetchRawInfo(context.Background(), testTools, testURL, false, false, failing)
	var ytErr *YtDlpError
	var timeout *YtDlpTimeout
	if !errors.As(err, &ytErr) || ytErr.Message != "Private video" || ytErr.Detail != "WARNING: x\nERROR: [youtube] abc: Private video" || errors.As(err, &timeout) {
		t.Errorf("failing: %#v", err)
	}

	silent := func(context.Context, []string) (string, string, int, error) { return "", "", 2, nil }
	if _, err := FetchRawInfo(context.Background(), testTools, testURL, false, false, silent); err == nil || err.Error() != "yt-dlp failed" {
		t.Errorf("silent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	slow := func(ctx context.Context, _ []string) (string, string, int, error) {
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}
	_, err = FetchRawInfo(ctx, testTools, testURL, false, false, slow)
	if !errors.As(err, &timeout) || timeout.Message != "YouTube took too long to answer" || !errors.As(err, &ytErr) {
		t.Errorf("slow: %#v", err)
	}

	missing := func(context.Context, []string) (string, string, int, error) {
		return "", "", -1, errors.New("exec: not found")
	}
	if _, err := FetchRawInfo(context.Background(), testTools, testURL, false, false, missing); !errors.As(err, &ytErr) || ytErr.Message != "Could not run yt-dlp" || ytErr.Detail != "exec: not found" {
		t.Errorf("missing: %#v", err)
	}

	for _, out := range []string{"not json", "[1]", "null", `{"id": 1} trailing`} {
		garbage := func(context.Context, []string) (string, string, int, error) { return out, "", 0, nil }
		if _, err := FetchRawInfo(context.Background(), testTools, testURL, false, false, garbage); err == nil || err.Error() != "yt-dlp returned unreadable data" {
			t.Errorf("garbage %q: %v", out, err)
		}
	}
}

// TestHelperProcess is the child DefaultRunner starts in TestDefaultRunner; it does nothing in
// a normal test run.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("CORE_HELPER_MODE") {
	case "":
		return
	case "exit3":
		fmt.Fprint(os.Stdout, "out "+os.Getenv("PYTHONUTF8"))
		fmt.Fprint(os.Stderr, "ERROR: boom")
		os.Exit(3)
	case "hang":
		time.Sleep(time.Minute)
	}
}

func TestDefaultRunner(t *testing.T) {
	argv := []string{os.Args[0], "-test.run=^TestHelperProcess$"}
	t.Setenv("CORE_HELPER_MODE", "exit3")
	stdout, stderr, code, err := DefaultRunner(context.Background(), argv)
	if err != nil || code != 3 || stdout != "out 1" || stderr != "ERROR: boom" {
		t.Errorf("exit3: %q %q %d %v", stdout, stderr, code, err)
	}

	t.Setenv("CORE_HELPER_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, _, err := DefaultRunner(ctx, argv); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hang: %v", err)
	}

	if _, _, _, err := DefaultRunner(context.Background(), []string{filepath.Join(t.TempDir(), "missing-yt-dlp")}); err == nil {
		t.Error("missing binary: no error")
	}
}

func TestBuildArgsEscapesPercentInFolder(t *testing.T) {
	out := filepath.Join(t.TempDir(), "100% Hits")
	args := BuildArgs(DefaultJobOptions(), testURL, out, false, 4, true, testTools)
	want := filepath.Join(filepath.Dir(out), "100%% Hits", OutputTemplate)
	if got := valueAfter(t, args, "-o"); got != want {
		t.Fatalf("-o %q, want %q", got, want)
	}
}

func TestChildEnvForcesUTF8(t *testing.T) {
	t.Setenv("PYTHONUTF8", "0")
	env := ChildEnv()
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(strings.ToUpper(entry), "PYTHONUTF8=") {
			count++
			if entry != "PYTHONUTF8=1" {
				t.Errorf("entry %q", entry)
			}
		}
	}
	if count != 1 || !slices.Contains(env, "PYTHONIOENCODING=utf-8") || !slices.Contains(env, "PYTHONUNBUFFERED=1") {
		t.Errorf("env overrides wrong (count %d)", count)
	}
}

func TestSizedDownloadArgs(t *testing.T) {
	tools := Tools{YtDlp: "yt-dlp"}
	fit := BuildFitArgs("https://www.youtube.com/watch?v=jNQXAC9IVRw", "out", false, 4, tools, "1a2b3c4d", "133+140")
	source := BuildSourceArgs("https://www.youtube.com/watch?v=jNQXAC9IVRw", "out", false, 4, tools, "1a2b3c4d", 480)
	for _, args := range [][]string{fit, source} {
		out := args[slices.Index(args, "-o")+1]
		if out != filepath.Join("out", "%(title)s [%(id)s].1a2b3c4d.source.%(ext)s") {
			t.Errorf("output template %q", out)
		}
		if !slices.Contains(args, "after_move:YTDLP_CODECS %(vcodec)s %(acodec)s") {
			t.Errorf("no codecs print in %v", args)
		}
	}
	if fit[slices.Index(fit, "-f")+1] != "133+140" || source[slices.Index(source, "-S")+1] != "res:480,vcodec:h264,acodec:aac" {
		t.Errorf("format args %v / %v", fit, source)
	}
}

func TestParserCodecs(t *testing.T) {
	parser := NewOutputParser(2)
	if u := parser.Feed("YTDLP_CODECS avc1.4d401f mp4a.40.2"); u != nil {
		t.Errorf("codecs line gave an update %+v", u)
	}
	if parser.VCodec != "avc1.4d401f" || parser.ACodec != "mp4a.40.2" {
		t.Fatalf("codecs %q %q", parser.VCodec, parser.ACodec)
	}
}

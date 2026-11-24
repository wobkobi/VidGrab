// yt-dlp wrapper: command building, metadata fetching and progress-output parsing.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Line prefixes yt-dlp is told to print, so OutputParser can pick them out.
const (
	ProgressPrefix = "YTDLP_PROGRESS "
	FilePrefix     = "YTDLP_FILE "
	TitlePrefix    = "YTDLP_TITLE "
	ChannelPrefix  = "YTDLP_CHANNEL "
	CodecsPrefix   = "YTDLP_CODECS "
)

// OutputTemplate names downloaded files "Title [id].ext". The id keeps two videos with one
// title apart while yt-dlp works (and lets cleanup find a job's partial files by "[id]"); the
// queue moves the finished file to "Title.ext" afterwards (see StripID).
const OutputTemplate = "%(title)s [%(id)s].%(ext)s"

// outputTemplate returns OutputTemplate with a clip's label after the title, "Title
// (1m30s-4m05s) [id].ext", so a clip never collides with (or is skipped as "already
// downloaded" because of) the whole video or another clip of it.
func outputTemplate(opts JobOptions) string {
	return "%(title)s" + opts.ClipLabel() + " [%(id)s].%(ext)s"
}

// clipArgs returns the flags that cut a clip, or nil for the whole video. yt-dlp then fetches
// only that range through ffmpeg; --force-keyframes-at-cuts re-encodes the cut so it starts on
// the exact second rather than the keyframe before it, which can be several seconds early.
func clipArgs(opts JobOptions) []string {
	if !opts.Clipped() {
		return nil
	}
	end := "inf"
	if opts.ClipEnd > 0 {
		end = strconv.Itoa(opts.ClipEnd)
	}
	return []string{"--download-sections", "*" + strconv.Itoa(opts.ClipStart) + "-" + end, "--force-keyframes-at-cuts"}
}

// StripID returns base, a file path without its extension, minus the " [id]" that
// OutputTemplate and WorkTemplate put after the title, and whether that marker was there. Only
// the last " [id]" is removed, so a title with brackets of its own (even its own id in them)
// keeps them. ok is false, and base comes back unchanged, when the marker is missing or no
// usable title would be left.
//
// Trailing dots and spaces of the title go too: a title ending in "." would otherwise give
// "Title..mp4" once the extension follows, and Windows drops them from a name's end anyway.
func StripID(base, id string) (stem string, ok bool) {
	marker := " [" + id + "]"
	if id == "" || !strings.HasSuffix(base, marker) {
		return base, false
	}
	stem = strings.TrimSuffix(base, marker)
	// The name keeps at least one other character, so trimming never reaches the folder
	if _, name := filepath.Split(stem); strings.Trim(name, " .") == "" {
		return base, false
	}
	return strings.TrimRight(stem, " ."), true
}

// SamePath reports whether two paths name the same file as yt-dlp prints them: equal once
// cleaned, ignoring case because Windows file names are case-insensitive.
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// VideoSummaryTemplate is written to the comment tag of video files; "|" fallbacks keep it
// readable when a field is missing. It must not contain ":" (see BuildArgs).
const VideoSummaryTemplate = "%(width|?)sx%(height|?)s, %(fps|?)s fps, %(vcodec|unknown)s"

const (
	// InfoTimeout is how long callers let FetchRawInfo run.
	InfoTimeout = 20 * time.Second
	// TailLines is how many output lines are kept for error details.
	TailLines = 20
)

var (
	// Fallback for plain "[download]  45.2% of 12.34MiB at 1.23MiB/s ETA 00:30" lines
	progressRe    = regexp.MustCompile(`^\[download\]\s+(\d+(?:\.\d+)?)%`)
	destinationRe = regexp.MustCompile(`^\[download\] Destination: (.+)$`)
	// A resumed merge download prints this instead of a Destination line for a finished stream
	alreadyDownloadedRe = regexp.MustCompile(`^\[download\] (.+) has already been downloaded`)
	mergerRe            = regexp.MustCompile(`^\[Merger\] Merging formats into "(.+)"$`)
	// ffmpeg's status line while yt-dlp cuts a clip ("frame=  284 fps=175 ... time=00:00:13.63
	// bitrate=..."; audio-only lines start at "size="); yt-dlp itself reports no progress then
	ffmpegTimeRe = regexp.MustCompile(`^(?:frame|size)=.*\btime=(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	// Strips the "[youtube] abc123: " or "[youtube:tab] PLx: " extractor prefix; the classes
	// spell out a Unicode \w (letters, marks, digits, underscore)
	extractorPrefixRe  = regexp.MustCompile(`^\[[\p{L}\p{M}\p{N}_:]+\]\s+[\p{L}\p{M}\p{N}_-]+:\s*`)
	processingPrefixes = []string{"[Merger]", "[ExtractAudio]", "[VideoRemuxer]", "[VideoConvertor]"}
	codecSort          = map[string]string{"h264": "vcodec:h264", "vp9": "vcodec:vp9", "av1": "vcodec:av01"}
)

const errYtDlpUnreadable = "yt-dlp returned unreadable data"

// Tools locates the external programs. YtDlp is the yt-dlp executable; FfmpegDir is the folder
// holding ffmpeg and ffprobe, or "" to let yt-dlp find them on PATH.
type Tools struct {
	YtDlp     string
	FfmpegDir string
}

// YtDlpError means yt-dlp failed; Message is the last ERROR: line (or a fixed summary) and
// Detail, when not empty, is the output tail for the error's "detail" field.
type YtDlpError struct {
	Message string
	Detail  string
}

// Error returns Message.
func (e *YtDlpError) Error() string { return e.Message }

// YtDlpTimeout means yt-dlp did not answer in time. errors.As also matches it as *YtDlpError,
// so check for the timeout first.
type YtDlpTimeout struct{ YtDlpError }

// Unwrap exposes the embedded *YtDlpError to errors.As.
func (e *YtDlpTimeout) Unwrap() error { return &e.YtDlpError }

// Runner runs argv to completion and returns its output. A non-zero exit is reported through
// exitCode with a nil err; err is for failures to start or a cancelled ctx.
type Runner func(ctx context.Context, argv []string) (stdout, stderr string, exitCode int, err error)

// PrepareCommand is called on every yt-dlp command before it starts. The platform package sets
// it to hide console windows, lower priority and kill the whole process tree on cancel.
var PrepareCommand = func(cmd *exec.Cmd) {}

// AfterStartCommand runs once a yt-dlp command has started, and the func it returns runs after
// Wait returns (so after any ctx-driven cancel). The platform package sets it to hold the
// process id until the cancel can no longer fire.
var AfterStartCommand = func(cmd *exec.Cmd) (release func()) { return func() {} }

// DefaultRunner runs argv with no stdin and ChildEnv, capturing both output streams. When ctx
// ends first it returns ctx.Err().
func DefaultRunner(ctx context.Context, argv []string) (string, string, int, error) {
	if len(argv) == 0 {
		return "", "", -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Env = ChildEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A grandchild that inherited the pipes would otherwise keep Wait blocked after a kill
	cmd.WaitDelay = 2 * time.Second
	PrepareCommand(cmd)
	exited := TrackChild()
	err := cmd.Start()
	if err == nil {
		release := AfterStartCommand(cmd)
		err = cmd.Wait()
		release()
	}
	exited(ctx.Err() != nil)
	// Invalid UTF-8 in the output becomes U+FFFD instead of failing
	out := strings.ToValidUTF8(stdout.String(), "�")
	errOut := strings.ToValidUTF8(stderr.String(), "�")
	if ctx.Err() != nil {
		return out, errOut, -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, errOut, exitErr.ExitCode(), nil
	}
	if err != nil {
		return out, errOut, -1, err
	}
	return out, errOut, 0, nil
}

// CommonFlags returns the flags shared by downloads and metadata calls.
//
// --ignore-config keeps a user's own yt-dlp config from changing output the parser relies on.
// --encoding utf-8 is needed because the standalone yt-dlp.exe ignores PYTHONIOENCODING and
// prints in the console code page, which silently drops characters such as Korean or "⧸" from
// printed file paths, so the helper could not find the file it had just downloaded.
func CommonFlags(cookies bool, tools Tools) []string {
	flags := []string{"--ignore-config", "--no-update", "--color", "never", "--encoding", "utf-8"}
	if cookies {
		flags = append(flags, "--cookies-from-browser", "firefox")
	}
	if tools.FfmpegDir != "" {
		flags = append(flags, "--ffmpeg-location", tools.FfmpegDir)
	}
	return flags
}

// BuildArgs builds the full yt-dlp command line for one download, starting with tools.YtDlp and
// ending with "--" and the URL.
//
// Video sorts by resolution first (capped at the chosen quality) and then by codec, so a codec
// preference never lowers the resolution. MP4 also prefers AAC audio, so the merge is a plain
// remux. Without ffmpeg only single-file formats are requested, since nothing could merge them.
// With ffmpeg every file also gets embedded tags (title, channel, date, description, URL), and
// videos get a "1920x1080, 30 fps, avc1" summary in the comment tag. fragments is the number of
// DASH/HLS fragments fetched concurrently (1-8).
func BuildArgs(opts JobOptions, url, outDir string, cookies bool, fragments int, ffmpeg bool, tools Tools) []string {
	args := downloadPrefix(outDir, outputTemplate(opts), cookies, fragments, tools)
	args = append(args, clipArgs(opts)...)

	if opts.Kind == "audio" {
		switch {
		case opts.AudioFormat == "mp3":
			args = append(args, "-f", "ba/b", "-x", "--audio-format", "mp3", "--audio-quality", opts.AudioBitrate+"K")
		case ffmpeg:
			// No --audio-quality here: a quality setting would force a re-encode of m4a sources
			args = append(args, "-f", "ba[ext=m4a]/ba/b", "-x", "--audio-format", "m4a")
		default:
			args = append(args, "-f", "ba[ext=m4a]")
		}
	} else {
		sortKeys := []string{"res"}
		if opts.Quality != "best" {
			sortKeys[0] = "res:" + opts.Quality
		}
		if field, ok := codecSort[opts.Codec]; ok {
			sortKeys = append(sortKeys, field)
		}
		if opts.Container == "mp4" {
			sortKeys = append(sortKeys, "acodec:aac")
		}
		if ffmpeg {
			args = append(args, "-f", "bv*+ba/b", "--merge-output-format", opts.Container)
		} else {
			args = append(args, "-f", "b")
		}
		args = append(args, "-S", strings.Join(sortKeys, ","))
		if ffmpeg {
			args = append(args, summaryArgs()...)
		}
		if ffmpeg && opts.Container == "mp4" {
			// The sound's codec, so sound MP4 players handle badly can be re-encoded
			args = append(args, codecsPrint()...)
		}
	}

	if ffmpeg {
		// Title, channel (as artist), upload date, description and the video URL
		args = append(args, "--embed-metadata")
	}
	return append(args, "--", url)
}

// WorkTemplate names the file a size-limited job downloads, "Title [id].<tag>.source.<ext>",
// where tag is unique to the job and a clip's label goes after the title as in
// outputTemplate. The ".source" marker keeps it apart from finished downloads until the job
// has checked its size (and re-encoded it when needed), and the tag stops two jobs for one
// video sharing it.
func WorkTemplate(tag string, opts JobOptions) string {
	return "%(title)s" + opts.ClipLabel() + " [%(id)s]." + tag + ".source.%(ext)s"
}

// codecsPrint reports the codecs of the file yt-dlp moved into place, so a size-limited job
// can tell whether the download plays as it is.
func codecsPrint() []string {
	return []string{"--print", "after_move:" + CodecsPrefix + "%(vcodec)s %(acodec)s %(abr)s %(audio_channels)s"}
}

// BuildFitArgs builds the yt-dlp command for a size-limited video whose streams already fit:
// format is an exact selector from FitChoice.Selector, merged into MP4 without re-encoding
// under WorkTemplate. It is never used for a clip, whose size is unknown until it is cut.
// ffmpeg is required (NeedsFfmpeg), so the file gets the same tags as BuildArgs gives it.
func BuildFitArgs(url, outDir string, cookies bool, fragments int, tools Tools, tag, format string) []string {
	args := downloadPrefix(outDir, WorkTemplate(tag, JobOptions{}), cookies, fragments, tools)
	args = append(args, "-f", format, "--merge-output-format", "mp4")
	args = append(args, summaryArgs()...)
	args = append(args, codecsPrint()...)
	args = append(args, "--embed-metadata")
	return append(args, "--", url)
}

// BuildSourceArgs builds the yt-dlp command that fetches the source a size-limited job will
// re-encode: at most height (the shorter side, as -S res measures it), H.264 and AAC preferred
// so the encoder decodes the cheapest codec (and the download can be kept as it is if it
// fits), merged into MP4 under WorkTemplate. Only the clip is fetched when opts has one. The
// stream summary comment is left out because the re-encode changes the resolution it
// describes.
func BuildSourceArgs(opts JobOptions, url, outDir string, cookies bool, fragments int, tools Tools, tag string, height int) []string {
	args := downloadPrefix(outDir, WorkTemplate(tag, opts), cookies, fragments, tools)
	args = append(args, clipArgs(opts)...)
	args = append(args,
		"-f", "bv*+ba/b", "--merge-output-format", "mp4",
		"-S", "res:"+strconv.Itoa(height)+",vcodec:h264,acodec:aac",
	)
	args = append(args, codecsPrint()...)
	args = append(args, "--embed-metadata")
	return append(args, "--", url)
}

// summaryArgs writes a readable stream summary to the comment tag, which Explorer's Details tab
// shows. before_dl runs after format selection, so width/height/fps/vcodec are known. The
// argument is WHEN:FROM:TO and yt-dlp splits it on ":", so the FROM template must not contain
// one; the TO regex captures the whole formatted string as meta_comment.
func summaryArgs() []string {
	return []string{"--parse-metadata", "before_dl:" + VideoSummaryTemplate + ":(?P<meta_comment>.+)"}
}

// downloadPrefix returns the flags every download shares, up to and including the output
// template inside outDir.
func downloadPrefix(outDir, template string, cookies bool, fragments int, tools Tools) []string {
	args := append([]string{tools.YtDlp}, CommonFlags(cookies, tools)...)
	return append(args,
		"--no-playlist",
		"--windows-filenames",
		"--no-mtime",
		"--newline",
		"--continue",
		"--progress",
		"--no-quiet",
		"--progress-delta", "0.5",
		"--concurrent-fragments", strconv.Itoa(fragments),
		"--progress-template", "download:"+ProgressPrefix+"%(progress)j",
		"--print", "before_dl:"+TitlePrefix+"%(title)s",
		// The channel name, else the uploader; "|" makes a video with neither print nothing, not "NA"
		"--print", "before_dl:"+ChannelPrefix+"%(channel,uploader|)s",
		"--print", "after_move:"+FilePrefix+"%(filepath)s",
		// A "%" in the folder would otherwise start a template field
		"-o", filepath.Join(strings.ReplaceAll(outDir, "%", "%%"), template),
	)
}

// FetchRawInfo runs yt-dlp -J and returns the parsed JSON object. flat lists playlist/channel
// entries without resolving each video. Numbers are decoded as json.Number so integer fields
// stay distinguishable from floats.
//
// items, when not "", is a --playlist-items spec ("1-4,15", see FormatItems) for a flat
// listing: yt-dlp then pages through the list only as far as the highest item instead of
// listing all of it, which on a channel with thousands of videos is the difference between
// seconds and minutes. The JSON then holds only the entries in range, in list order, and
// "requested_entries" gives each one's 1-based position in the whole list (the flat entries
// carry no playlist_index of their own). An item past the end of the list is left out, so a
// spec entirely past it gives no entries.
//
// Callers bound ctx (normally with InfoTimeout). When ctx's deadline passes the error is a
// *YtDlpTimeout; every other failure is a *YtDlpError.
func FetchRawInfo(ctx context.Context, tools Tools, url string, flat bool, items string, cookies bool, run Runner) (map[string]any, error) {
	if run == nil {
		run = DefaultRunner
	}
	args := append([]string{tools.YtDlp}, CommonFlags(cookies, tools)...)
	args = append(args, "-J", "--no-warnings")
	if flat {
		args = append(args, "--flat-playlist")
		if items != "" {
			args = append(args, "--playlist-items", items)
		}
	} else {
		args = append(args, "--no-playlist")
	}
	args = append(args, "--", url)

	stdout, stderr, code, err := run(ctx, args)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, &YtDlpTimeout{YtDlpError{Message: "YouTube took too long to answer"}}
	}
	if err != nil {
		return nil, &YtDlpError{Message: "Could not run yt-dlp", Detail: err.Error()}
	}
	if code != 0 {
		message := LastError(stderr)
		if message == "" {
			message = "yt-dlp failed"
		}
		return nil, &YtDlpError{Message: message, Detail: Tail(stderr, TailLines)}
	}
	var data any
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil || dec.More() {
		return nil, &YtDlpError{Message: errYtDlpUnreadable}
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, &YtDlpError{Message: errYtDlpUnreadable}
	}
	return obj, nil
}

// ChildEnv returns the environment for yt-dlp children. The standalone yt-dlp build is a frozen
// Python program, so the PYTHON* variables still force UTF-8, unbuffered output and keep
// non-ASCII titles intact. Once SetChildTempDir has named a folder, TEMP and TMP point there,
// so leftovers of killed runs can be swept (see TrackChild).
func ChildEnv() []string {
	overrides := [][2]string{{"PYTHONIOENCODING", "utf-8"}, {"PYTHONUTF8", "1"}, {"PYTHONUNBUFFERED", "1"}}
	if dir := ChildTempDir(); dir != "" {
		overrides = append(overrides, [2]string{"TEMP", dir}, [2]string{"TMP", dir})
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for _, o := range overrides {
			// Windows environment names are case-insensitive
			if strings.EqualFold(key, o[0]) {
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	for _, o := range overrides {
		env = append(env, o[0]+"="+o[1])
	}
	return env
}

// splitLines breaks on \n, \r, \r\n and the other Unicode
// line boundaries, and drops the trailing empty piece.
func splitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, text[start:i])
			if r == '\r' && i+1 < len(text) && text[i+1] == '\n' {
				size++
			}
			i += size
			start = i
		default:
			i += size
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// LastError returns the last "ERROR:" line from yt-dlp output, cleaned by CleanError, or ""
// when there is none.
func LastError(text string) string {
	lines := splitLines(text)
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "ERROR:") {
			return CleanError(line)
		}
	}
	return ""
}

// CleanError strips the "ERROR:" and "[extractor] id:" prefixes yt-dlp puts on error lines and
// caps the message at 300 characters.
func CleanError(line string) string {
	var message string
	if strings.HasPrefix(line, "ERROR:") {
		message = strings.TrimSpace(line[len("ERROR:"):])
	} else {
		message = strings.TrimSpace(line)
	}
	message = extractorPrefixRe.ReplaceAllString(message, "")
	if runes := []rune(message); len(runes) > 300 {
		message = string(runes[:300])
	}
	return message
}

// Tail returns the last n lines of output as one string.
func Tail(text string, n int) string {
	lines := splitLines(strings.TrimSpace(text))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// normaliseCodec maps yt-dlp codec strings (avc1.64001F, vp09.00..., av01...) to h264/vp9/av1.
func normaliseCodec(vcodec string) string {
	codec := strings.ToLower(vcodec)
	switch {
	case strings.HasPrefix(codec, "avc"), strings.HasPrefix(codec, "h264"):
		return "h264"
	case strings.HasPrefix(codec, "vp9"), strings.HasPrefix(codec, "vp09"):
		return "vp9"
	case strings.HasPrefix(codec, "av01"), strings.HasPrefix(codec, "av1"):
		return "av1"
	}
	head, _, _ := strings.Cut(codec, ".")
	return head
}

// Quality is one row of /info's qualities list. Height is the shorter side of the frame.
type Quality struct {
	Height      int      `json:"height"`
	Codecs      []string `json:"codecs"`
	FilesizeEst *float64 `json:"filesize_est"`
}

// InfoEntry is one playlist or channel entry in /info.
type InfoEntry struct {
	ID    string `json:"id"`
	Title any    `json:"title"`
}

// Info is the /info response. ID, Title, Uploader and Duration pass yt-dlp's values through
// unchanged (nil marshals as null). EntryCount and Entries are nil for a video.
type Info struct {
	Kind       string      `json:"kind"`
	ID         any         `json:"id"`
	Title      any         `json:"title"`
	Uploader   any         `json:"uploader"`
	Duration   any         `json:"duration"`
	Qualities  []Quality   `json:"qualities"`
	EntryCount *int        `json:"entry_count"`
	Entries    []InfoEntry `json:"entries"`
}

// truthy reports whether a decoded JSON value is set: false, null, 0, "" and empty arrays or
// objects are not.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// asNumber reads a JSON number from a json.Number, float64 or int.
func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// asInt reads a JSON integer: a json.Number must be written without a fraction
// or exponent, while a float64 (from a plain json.Unmarshal) counts when it is whole.
func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	case float64:
		return int(x), x == math.Trunc(x) && !math.IsInf(x, 0)
	case int:
		return x, true
	case int64:
		return int(x), true
	}
	return 0, false
}

// sizeOf returns `fmt.get("filesize") or fmt.get("filesize_approx") or 0`.
func sizeOf(format map[string]any) float64 {
	for _, key := range []string{"filesize", "filesize_approx"} {
		if v := format[key]; truthy(v) {
			if n, ok := asNumber(v); ok {
				return n
			}
		}
	}
	return 0
}

// isNoneCodec reports whether a codec field is missing, null or "none".
func isNoneCodec(v any) bool { return v == nil || v == "none" }

// ChannelOf returns the channel name from yt-dlp JSON for a video or a flat playlist entry,
// falling back to the uploader, or "" when neither is a non-empty string.
func ChannelOf(raw map[string]any) string {
	for _, key := range []string{"channel", "uploader"} {
		if name, _ := raw[key].(string); strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// SummariseInfo reduces raw yt-dlp JSON from FetchRawInfo to the /info response. kind is the
// ParsedUrl kind.
//
// Size estimates add the largest audio stream to each video height's largest stream, since a
// download at that height merges the two.
func SummariseInfo(raw map[string]any, kind string) Info {
	uploader := raw["uploader"]
	if !truthy(uploader) {
		uploader = raw["channel"]
	}
	summary := Info{
		Kind:      kind,
		ID:        raw["id"],
		Title:     raw["title"],
		Uploader:  uploader,
		Duration:  raw["duration"],
		Qualities: []Quality{},
	}
	if kind == KindVideo {
		formats, _ := raw["formats"].([]any)
		summary.Qualities = qualities(formats)
		return summary
	}

	entries := []InfoEntry{}
	list, _ := raw["entries"].([]any)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok || !truthy(entry["id"]) {
			continue
		}
		id, ok := entry["id"].(string)
		if !ok {
			id = fmt.Sprint(entry["id"])
		}
		entries = append(entries, InfoEntry{ID: id, Title: entry["title"]})
	}
	count := len(entries)
	summary.Entries = entries
	summary.EntryCount = &count
	summary.Duration = nil
	return summary
}

// qualities groups video formats by resolution into Quality rows, highest first.
//
// The height is the shorter side, the same measure yt-dlp's -S res:N sorts by, so a vertical
// 1080x1920 Short reports 1080 and matches the 1080p preset the download will use.
func qualities(formats []any) []Quality {
	var maps []map[string]any
	for _, f := range formats {
		if m, ok := f.(map[string]any); ok {
			maps = append(maps, m)
		}
	}
	audioSize := 0.0
	for _, f := range maps {
		if f["vcodec"] == "none" && !isNoneCodec(f["acodec"]) {
			audioSize = math.Max(audioSize, sizeOf(f))
		}
	}
	type group struct {
		codecs []string
		size   float64
	}
	byHeight := map[int]*group{}
	for _, f := range maps {
		vcodec, _ := f["vcodec"].(string)
		height, ok := asInt(f["height"])
		if vcodec == "" || vcodec == "none" || !ok || height <= 0 {
			continue
		}
		if width, ok := asInt(f["width"]); ok && width > 0 {
			height = min(width, height)
		}
		g := byHeight[height]
		if g == nil {
			g = &group{}
			byHeight[height] = g
		}
		codec := normaliseCodec(vcodec)
		if !contains(g.codecs, codec) {
			g.codecs = append(g.codecs, codec)
		}
		size := sizeOf(f)
		// Progressive formats already include audio; video-only ones get the audio added
		if isNoneCodec(f["acodec"]) && size != 0 {
			size += audioSize
		}
		g.size = math.Max(g.size, size)
	}
	result := make([]Quality, 0, len(byHeight))
	for height, g := range byHeight {
		sort.Strings(g.codecs)
		q := Quality{Height: height, Codecs: g.codecs}
		if g.size != 0 {
			size := g.size
			q.FilesizeEst = &size
		}
		result = append(result, q)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Height > result[j].Height })
	return result
}

// FormatSpeed formats bytes/second as "2.5 MB/s"; it returns "" when the speed is unknown (0 or
// negative).
func FormatSpeed(speed float64) string {
	if !(speed > 0) {
		return ""
	}
	value := speed
	for _, unit := range []string{"B/s", "KB/s", "MB/s", "GB/s"} {
		if value < 1024 || unit == "GB/s" {
			if unit == "B/s" {
				return fmt.Sprintf("%.0f %s", value, unit)
			}
			return fmt.Sprintf("%.1f %s", value, unit)
		}
		value /= 1024
	}
	return ""
}

// FormatEta formats seconds as "m:ss" or "h:mm:ss"; it returns "" when eta is negative.
func FormatEta(eta float64) string {
	if eta < 0 || math.IsNaN(eta) {
		return ""
	}
	seconds := int(eta)
	hours, rest := seconds/3600, seconds%3600
	minutes, secs := rest/60, rest%60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%d:%02d", minutes, secs)
}

// formatSpeedValue and formatEtaValue take a raw JSON value, where null or a non-number means
// unknown.
func formatSpeedValue(v any) string {
	n, ok := asNumber(v)
	if !ok {
		return ""
	}
	return FormatSpeed(n)
}

func formatEtaValue(v any) string {
	n, ok := asNumber(v)
	if !ok {
		return ""
	}
	return FormatEta(n)
}

// ParseUpdate holds the changes to apply to a job after one line of output. Percent is nil when
// the line says nothing about progress; empty strings mean unknown or unchanged.
type ParseUpdate struct {
	Percent    *float64
	Speed      string
	Eta        string
	Processing bool
	Title      string
	Channel    string
	Filepath   string
}

// HasProgress reports whether the update carries percent, speed or ETA, in which case the job's
// speed and ETA are replaced (cleared when empty).
func (u *ParseUpdate) HasProgress() bool {
	return u.Percent != nil || u.Speed != "" || u.Eta != ""
}

// OutputParser is a stateful parser for one yt-dlp run's combined stdout/stderr. The zero value
// is ready to use for a single-stream download; it is not safe for concurrent use.
//
// Progress stays monotonic across separate streams: with ExpectedStreams=2 (video plus audio to
// merge) the first stream fills 0-90% and the second 90-100%, matching their usual size ratio.
// A third stream (rare) is folded into the last slot, and percentages never go back.
type OutputParser struct {
	// ExpectedStreams is how many files yt-dlp downloads before post-processing.
	ExpectedStreams int
	// ClipSeconds is the length of the clip being cut, so ffmpeg's time= position can be read
	// as progress; 0 when the download is not a clip or its length is unknown.
	ClipSeconds float64
	// Percent is overall progress so far, 0-100.
	Percent float64
	// Filepath is the final path from the after_move print, or the merge/destination path as
	// a fallback.
	Filepath string
	// LastError is the last ERROR: message seen.
	LastError string
	// VCodec and ACodec are the finished file's codecs as yt-dlp names them ("avc1.4d401f",
	// "mp4a.40.2", "none"), from the CodecsPrefix print; empty when it was not printed.
	VCodec string
	ACodec string
	// ABR and AudioChannels are the finished file's audio bitrate (kbps) and channel count
	// from the same print, 0 when yt-dlp does not know them ("NA").
	ABR           float64
	AudioChannels int
	// Destinations lists every stream file announced so far.
	Destinations []string

	streams  int      // Destination or "already downloaded" lines seen
	existing []string // paths reported as already downloaded
	tail     []string // last TailLines non-progress lines
}

// NewOutputParser returns a parser for a download of expectedStreams files.
func NewOutputParser(expectedStreams int) *OutputParser {
	return &OutputParser{ExpectedStreams: expectedStreams}
}

// PreExisting reports whether the final file was on disk before this run: yt-dlp said it "has
// already been downloaded" rather than writing it. A resumed merge reports its finished
// streams ("Title [id].f137.mp4") the same way, but those never match the final file.
func (p *OutputParser) PreExisting() bool {
	for _, path := range p.existing {
		if SamePath(path, p.Filepath) {
			return true
		}
	}
	return false
}

// Detail returns the last 20 non-progress lines for error details.
func (p *OutputParser) Detail() string { return strings.Join(p.tail, "\n") }

// Feed parses one output line, with or without its newline, and returns the changes it implies,
// or nil when the line carries nothing useful.
func (p *OutputParser) Feed(rawLine string) *ParseUpdate {
	line := strings.TrimRight(rawLine, "\r\n")
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if strings.HasPrefix(line, ProgressPrefix) {
		return p.progressJSON(line[len(ProgressPrefix):])
	}
	if m := ffmpegTimeRe.FindStringSubmatch(line); m != nil {
		if p.ClipSeconds <= 0 {
			return nil
		}
		h, _ := strconv.ParseFloat(m[1], 64)
		mins, _ := strconv.ParseFloat(m[2], 64)
		s, _ := strconv.ParseFloat(m[3], 64)
		return &ParseUpdate{Percent: p.overall((h*3600 + mins*60 + s) / p.ClipSeconds * 100)}
	}

	p.tail = append(p.tail, line)
	if len(p.tail) > TailLines {
		p.tail = p.tail[len(p.tail)-TailLines:]
	}

	if strings.HasPrefix(line, FilePrefix) {
		p.Filepath = strings.TrimSpace(line[len(FilePrefix):])
		return &ParseUpdate{Filepath: p.Filepath}
	}
	if strings.HasPrefix(line, CodecsPrefix) {
		fields := strings.Fields(line[len(CodecsPrefix):])
		for len(fields) < 4 {
			fields = append(fields, "")
		}
		p.VCodec, p.ACodec = fields[0], fields[1]
		p.ABR, _ = strconv.ParseFloat(fields[2], 64)
		p.AudioChannels, _ = strconv.Atoi(fields[3])
		if p.ABR < 0 || math.IsNaN(p.ABR) || math.IsInf(p.ABR, 0) {
			p.ABR = 0
		}
		return nil
	}
	if strings.HasPrefix(line, TitlePrefix) {
		return &ParseUpdate{Title: strings.TrimSpace(line[len(TitlePrefix):])}
	}
	if strings.HasPrefix(line, ChannelPrefix) {
		if channel := strings.TrimSpace(line[len(ChannelPrefix):]); channel != "" && channel != "NA" {
			return &ParseUpdate{Channel: channel}
		}
		return nil
	}
	if strings.HasPrefix(line, "ERROR:") {
		p.LastError = CleanError(line)
		return nil
	}
	if m := destinationRe.FindStringSubmatch(line); m != nil {
		p.addStream(m[1])
		return nil
	}
	if m := alreadyDownloadedRe.FindStringSubmatch(line); m != nil {
		// Count the finished stream so the next one maps onto its own slot
		p.addStream(m[1])
		p.existing = append(p.existing, strings.TrimSpace(m[1]))
		return &ParseUpdate{Percent: p.overall(100)}
	}
	if m := mergerRe.FindStringSubmatch(line); m != nil {
		p.Filepath = strings.TrimSpace(m[1])
		return &ParseUpdate{Processing: true, Filepath: p.Filepath}
	}
	for _, prefix := range processingPrefixes {
		if strings.HasPrefix(line, prefix) {
			return &ParseUpdate{Processing: true}
		}
	}
	if m := progressRe.FindStringSubmatch(line); m != nil {
		percent, _ := strconv.ParseFloat(m[1], 64)
		return &ParseUpdate{Percent: p.overall(percent)}
	}
	return nil
}

func (p *OutputParser) addStream(path string) {
	path = strings.TrimSpace(path)
	p.streams++
	p.Destinations = append(p.Destinations, path)
	if p.Filepath == "" {
		p.Filepath = path
	}
}

// progressJSON turns one --progress-template JSON payload into an update. The stream's percent
// comes from _percent, else downloaded/total bytes, else fragment index/count; a "finished"
// status forces 100 and clears speed and ETA.
func (p *OutputParser) progressJSON(payload string) *ParseUpdate {
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	if dec.Decode(&data) != nil || data == nil || dec.More() {
		return nil
	}
	streamPercent, known := asNumber(data["_percent"])
	if !known {
		done, doneOK := asNumber(data["downloaded_bytes"])
		totalRaw := data["total_bytes"]
		if !truthy(totalRaw) {
			totalRaw = data["total_bytes_estimate"]
		}
		total, totalOK := asNumber(totalRaw)
		count, countOK := asNumber(data["fragment_count"])
		switch {
		case doneOK && totalOK && total > 0:
			streamPercent, known = done/total*100, true
		case countOK && count != 0:
			index, _ := asNumber(data["fragment_index"])
			streamPercent, known = index/count*100, true
		}
	}
	finished := data["status"] == "finished"
	if finished {
		streamPercent, known = 100, true
	}
	update := &ParseUpdate{}
	if !finished {
		update.Speed = formatSpeedValue(data["speed"])
		update.Eta = formatEtaValue(data["eta"])
	}
	if known {
		update.Percent = p.overall(streamPercent)
	}
	return update
}

// overall maps one stream's percentage onto overall progress, never moving backwards.
//
// With two or more expected streams the first maps to 0-90 (percent * 0.9) and every later one
// to 90-100 (90 + percent * 0.1). The result is rounded to one decimal place with strconv's 'f'
// formatting (correctly rounded, ties to even).
func (p *OutputParser) overall(streamPercent float64) *float64 {
	streamPercent = math.Min(math.Max(streamPercent, 0), 100)
	index := max(p.streams-1, 0)
	overall := streamPercent
	if p.ExpectedStreams >= 2 {
		if index == 0 {
			overall = streamPercent * 0.9
		} else {
			overall = 90 + streamPercent*0.1
		}
	}
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(math.Max(p.Percent, math.Min(overall, 100)), 'f', 1, 64), 64)
	p.Percent = rounded
	percent := p.Percent
	return &percent
}

// ExpectedStreams returns how many files a download fetches before post-processing: 2 when a
// video is merged from separate streams, otherwise 1.
func ExpectedStreams(opts JobOptions, ffmpeg bool) int {
	// A clip's streams are cut and merged by one ffmpeg run that reports a single position
	if opts.Kind == "video" && ffmpeg && !opts.Clipped() {
		return 2
	}
	return 1
}

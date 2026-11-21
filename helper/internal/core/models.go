// Request and settings models; every option is an enum or a bounded number.
//
// The decoders are lax about the shapes the API takes: unknown fields are ignored, and every
// problem is collected with its location so the "Invalid request" detail lists them all.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxProgressiveHeight is the tallest height YouTube serves as one progressive stream; above
// it only separate video/audio streams exist, so ffmpeg must merge.
const MaxProgressiveHeight = 720

// "3.0" (a zero fractional part) also counts as an integer string
var intStringRe = regexp.MustCompile(`^[+-]?[0-9]+(\.0*)?$`)

// Enum values, in the order error messages list them.
var (
	Kinds         = []string{"video", "audio"}
	Qualities     = []string{"best", "2160", "1440", "1080", "720", "480", "360"}
	Codecs        = []string{"any", "h264", "vp9", "av1"}
	Containers    = []string{"mp4", "mkv"}
	AudioFormats  = []string{"mp3", "m4a"}
	AudioBitrates = []string{"320", "192", "128"}
)

// Bounds for Settings and SettingsUpdate.
const (
	MinConcurrency = 1
	MaxConcurrency = 5
	MinFragments   = 1
	MaxFragments   = 8
	// MaxSizeLimitMB bounds JobOptions.MaxSizeMB; 0 turns the size limit off.
	MaxSizeLimitMB = 4000
	// MaxClipSeconds bounds JobOptions.ClipStart and ClipEnd (100 hours, past any real video).
	MaxClipSeconds = 100 * 3600
	// MaxItemsLength bounds JobRequest.Items, in characters.
	MaxItemsLength = MaxItemsSpecLength
)

// JobOptions are the download options chosen in the extension panel.
type JobOptions struct {
	Kind         string `json:"kind"`
	Quality      string `json:"quality"`
	Codec        string `json:"codec"`
	Container    string `json:"container"`
	AudioFormat  string `json:"audio_format"`
	AudioBitrate string `json:"audio_bitrate"`
	// MaxSizeMB caps a video download at this many megabytes (1 MB = 1,000,000 bytes), for
	// upload limits such as Discord's; 0 means no limit. Audio jobs ignore it, and a capped
	// video is always MP4 whatever Container says.
	MaxSizeMB int `json:"max_size_mb"`
	// ClipStart and ClipEnd trim the download to that range, in whole seconds from the start
	// of the video. ClipEnd 0 means the end of the video; both 0 means the whole video.
	ClipStart int `json:"clip_start"`
	ClipEnd   int `json:"clip_end"`
}

// Clipped reports whether the job downloads only part of the video.
func (o JobOptions) Clipped() bool { return o.ClipStart > 0 || o.ClipEnd > 0 }

// ClipLabel returns the " (1m30s-4m05s)" that goes after the title of a clip's file name, or ""
// for the whole video. An open end reads "end".
func (o JobOptions) ClipLabel() string {
	if !o.Clipped() {
		return ""
	}
	end := "end"
	if o.ClipEnd > 0 {
		end = clipTime(o.ClipEnd)
	}
	return " (" + clipTime(o.ClipStart) + "-" + end + ")"
}

// clipTime formats seconds for a file name: "0m05s", "4m30s", "1h02m05s". Colons are not
// allowed in Windows file names, hence the letters.
func clipTime(seconds int) string {
	h, m, s := seconds/3600, seconds/60%60, seconds%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

// ErrClipAfterEnd means the clip starts at or after the end of the video.
var ErrClipAfterEnd = errors.New("The clip starts after the video ends")

// ClipLength returns how many seconds of a video of duration seconds (0 when unknown) the job
// keeps: the whole duration without a clip, else the clip with its end clamped to the video's.
// An open-ended clip of a video of unknown length has unknown length (0).
func (o JobOptions) ClipLength(duration float64) (float64, error) {
	if !o.Clipped() {
		return duration, nil
	}
	end := float64(o.ClipEnd)
	if duration > 0 && (end == 0 || end > duration) {
		end = duration
	}
	if end == 0 {
		return 0, nil
	}
	if end <= float64(o.ClipStart) {
		return 0, ErrClipAfterEnd
	}
	return end - float64(o.ClipStart), nil
}

// SizeLimited reports whether the job must fit MaxSizeMB.
func (o JobOptions) SizeLimited() bool {
	return o.Kind == "video" && o.MaxSizeMB > 0
}

// JobRequest is the body of POST /jobs. Items is nil when absent or null.
type JobRequest struct {
	URL     string     `json:"url"`
	Options JobOptions `json:"options"`
	Items   *string    `json:"items"`
}

// Settings are the backend settings pushed by the extension and persisted in data/settings.json.
type Settings struct {
	Concurrency int  `json:"concurrency"`
	Fragments   int  `json:"fragments"`
	Cookies     bool `json:"cookies"`
	AutoUpdate  bool `json:"auto_update"`
}

// SettingsUpdate is the body of PUT /settings; only non-nil fields are changed.
type SettingsUpdate struct {
	Concurrency *int  `json:"concurrency,omitempty"`
	Fragments   *int  `json:"fragments,omitempty"`
	Cookies     *bool `json:"cookies,omitempty"`
	AutoUpdate  *bool `json:"auto_update,omitempty"`
}

// DefaultJobOptions returns the options used for any field the request leaves out.
func DefaultJobOptions() JobOptions {
	return JobOptions{Kind: "video", Quality: "best", Codec: "any", Container: "mp4", AudioFormat: "mp3", AudioBitrate: "192"}
}

// DefaultSettings returns the settings used before the extension pushes any.
func DefaultSettings() Settings {
	return Settings{Concurrency: 2, Fragments: 4, Cookies: false, AutoUpdate: true}
}

// Apply returns s with every field present in u replaced.
func (s Settings) Apply(u SettingsUpdate) Settings {
	if u.Concurrency != nil {
		s.Concurrency = *u.Concurrency
	}
	if u.Fragments != nil {
		s.Fragments = *u.Fragments
	}
	if u.Cookies != nil {
		s.Cookies = *u.Cookies
	}
	if u.AutoUpdate != nil {
		s.AutoUpdate = *u.AutoUpdate
	}
	return s
}

// NeedsFfmpeg reports whether a download cannot work without ffmpeg.
//
// MP3 always needs conversion; video at "best" or above 720p needs separate streams merged, and
// a size-limited video may need a re-encode. Cutting a clip always needs it. M4A falls back to
// a native m4a stream, and 720p and below fall back to a progressive format.
func NeedsFfmpeg(opts JobOptions) bool {
	if opts.Clipped() {
		return true
	}
	if opts.Kind == "audio" {
		return opts.AudioFormat == "mp3"
	}
	if opts.SizeLimited() || opts.Quality == "best" {
		return true
	}
	height, err := strconv.Atoi(opts.Quality)
	return err == nil && height > MaxProgressiveHeight
}

// Problem is one validation failure: Loc is the path to the field (starting with "body" for a
// request body) and Msg is the message shown for it.
type Problem struct {
	Loc []string
	Msg string
}

// ValidationError collects every Problem found in one body, in field order.
type ValidationError struct{ Problems []Problem }

// Error joins the problems as "body.options.quality: Input should be ...; ...", the detail sent
// with 400 "Invalid request".
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = strings.Join(p.Loc, ".") + ": " + p.Msg
	}
	return strings.Join(parts, "; ")
}

// validator accumulates problems while fields are checked.
type validator struct{ problems []Problem }

func (v *validator) add(loc []string, msg string) {
	v.problems = append(v.problems, Problem{Loc: append([]string(nil), loc...), Msg: msg})
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: v.problems}
}

func at(loc []string, field string) []string {
	return append(append([]string(nil), loc...), field)
}

// literalMessage renders an enum error: "Input should be 'a', 'b' or 'c'".
func literalMessage(allowed []string) string {
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = "'" + a + "'"
	}
	if len(quoted) == 1 {
		return "Input should be " + quoted[0]
	}
	return "Input should be " + strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Validate checks every option against its enum and MaxSizeMB against its bounds; Problem
// locations are the bare field names.
func (o JobOptions) Validate() error {
	var v validator
	o.check(&v, nil)
	return v.err()
}

func (o JobOptions) check(v *validator, loc []string) {
	for _, f := range []struct {
		name, value string
		allowed     []string
	}{
		{"kind", o.Kind, Kinds},
		{"quality", o.Quality, Qualities},
		{"codec", o.Codec, Codecs},
		{"container", o.Container, Containers},
		{"audio_format", o.AudioFormat, AudioFormats},
		{"audio_bitrate", o.AudioBitrate, AudioBitrates},
	} {
		if !contains(f.allowed, f.value) {
			v.add(at(loc, f.name), literalMessage(f.allowed))
		}
	}
	checkBounds(v, at(loc, "max_size_mb"), float64(o.MaxSizeMB), 0, MaxSizeLimitMB)
	startOK := checkBounds(v, at(loc, "clip_start"), float64(o.ClipStart), 0, MaxClipSeconds)
	endOK := checkBounds(v, at(loc, "clip_end"), float64(o.ClipEnd), 0, MaxClipSeconds)
	if startOK && endOK {
		checkClipOrder(v, loc, o)
	}
}

// checkClipOrder rejects a clip whose end is not after its start.
func checkClipOrder(v *validator, loc []string, o JobOptions) {
	if o.ClipEnd > 0 && o.ClipEnd <= o.ClipStart {
		v.add(at(loc, "clip_end"), "Input should be greater than clip_start")
	}
}

// Validate checks the concurrency and fragments bounds.
func (s Settings) Validate() error {
	var v validator
	checkBounds(&v, []string{"concurrency"}, float64(s.Concurrency), MinConcurrency, MaxConcurrency)
	checkBounds(&v, []string{"fragments"}, float64(s.Fragments), MinFragments, MaxFragments)
	return v.err()
}

func checkBounds(v *validator, loc []string, value float64, lo, hi int) bool {
	if value < float64(lo) {
		v.add(loc, fmt.Sprintf("Input should be greater than or equal to %d", lo))
		return false
	}
	if value > float64(hi) {
		v.add(loc, fmt.Sprintf("Input should be less than or equal to %d", hi))
		return false
	}
	return true
}

// decodeObject parses a request body that must be a JSON object. Empty bodies and null count as
// missing.
func decodeObject(data []byte, v *validator) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		v.add([]string{"body"}, "Field required")
		return nil, false
	}
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	err := dec.Decode(&raw)
	if err == nil && dec.More() {
		err = errTrailingData
	}
	if err != nil {
		v.add([]string{"body", strconv.Itoa(jsonErrorPos(data, dec, err))}, "JSON decode error")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil || fields == nil {
		v.add([]string{"body"}, "Input should be a valid dictionary or object to extract fields from")
		return nil, false
	}
	return fields, true
}

var errTrailingData = errors.New("trailing data")

// jsonErrorPos gives the index of the offending character: Go's SyntaxError offset counts one
// past it, so step back one; trailing data points at its first byte.
func jsonErrorPos(data []byte, dec *json.Decoder, err error) int {
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		return max(int(syntax.Offset)-1, 0)
	case err == errTrailingData:
		rest := data[dec.InputOffset():]
		return int(dec.InputOffset()) + len(rest) - len(bytes.TrimLeft(rest, " \t\r\n"))
	}
	return len(data)
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// decodeLiteral reads an enum field. With intToString set, a JSON integer such as 1080 becomes
// "1080" first, so quality and audio_bitrate accept either form.
func decodeLiteral(v *validator, loc []string, raw json.RawMessage, allowed []string, intToString bool) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		text := string(bytes.TrimSpace(raw))
		if !intToString || json.Unmarshal(raw, &n) != nil || strings.ContainsAny(text, ".eE") {
			v.add(loc, literalMessage(allowed))
			return "", false
		}
		s = text
	}
	if !contains(allowed, s) {
		v.add(loc, literalMessage(allowed))
		return "", false
	}
	return s, true
}

// decodeString reads a str field with an optional max length in characters (0 means no limit).
func decodeString(v *validator, loc []string, raw json.RawMessage, maxLen int) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		v.add(loc, "Input should be a valid string")
		return "", false
	}
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		v.add(loc, fmt.Sprintf("String should have at most %d characters", maxLen))
		return "", false
	}
	return s, true
}

// decodeInt reads an int field leniently: integers, floats with no fractional part,
// booleans (as 0/1) and integer strings are accepted, then the bounds are checked.
func decodeInt(v *validator, loc []string, raw json.RawMessage, lo, hi int) (int, bool) {
	// Values are compared as float64 so a huge integer still fails the bound check instead of
	// overflowing; ParseFloat returns +-Inf for out-of-range input
	var value float64
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&decoded) != nil {
		v.add(loc, "Input should be a valid integer")
		return 0, false
	}
	switch x := decoded.(type) {
	case bool:
		if x {
			value = 1
		}
	case json.Number:
		f, _ := strconv.ParseFloat(x.String(), 64)
		if f != math.Trunc(f) {
			v.add(loc, "Input should be a valid integer, got a number with a fractional part")
			return 0, false
		}
		if math.Abs(f) > math.MaxInt64 {
			v.add(loc, "Unable to parse input string as an integer, exceeded maximum size")
			return 0, false
		}
		value = f
	case string:
		text := strings.TrimSpace(x)
		if !intStringRe.MatchString(text) {
			v.add(loc, "Input should be a valid integer, unable to parse string as an integer")
			return 0, false
		}
		value, _ = strconv.ParseFloat(text, 64)
	default:
		v.add(loc, "Input should be a valid integer")
		return 0, false
	}
	if !checkBounds(v, loc, value, lo, hi) {
		return 0, false
	}
	return int(value), true
}

// decodeBool reads a bool field leniently: booleans, the numbers 0 and 1, and the strings
// queryBool also takes ("true", "yes", "on", "1", "f", ...) are accepted.
func decodeBool(v *validator, loc []string, raw json.RawMessage) (bool, bool) {
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&decoded) != nil {
		v.add(loc, "Input should be a valid boolean")
		return false, false
	}
	switch x := decoded.(type) {
	case bool:
		return x, true
	case json.Number:
		f, _ := strconv.ParseFloat(x.String(), 64)
		if f == 0 || f == 1 {
			return f == 1, true
		}
		// The two cases are worded differently: an integer that could not map, a float never tried
		if strings.ContainsAny(x.String(), ".eE") {
			v.add(loc, "Input should be a valid boolean")
		} else {
			v.add(loc, "Input should be a valid boolean, unable to interpret input")
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		}
		v.add(loc, "Input should be a valid boolean, unable to interpret input")
	default:
		v.add(loc, "Input should be a valid boolean")
	}
	return false, false
}

func decodeJobOptions(v *validator, loc []string, raw json.RawMessage) JobOptions {
	opts := DefaultJobOptions()
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		v.add(loc, "Input should be a valid dictionary or object to extract fields from")
		return opts
	}
	for _, f := range []struct {
		name        string
		target      *string
		allowed     []string
		intToString bool
	}{
		{"kind", &opts.Kind, Kinds, false},
		{"quality", &opts.Quality, Qualities, true},
		{"codec", &opts.Codec, Codecs, false},
		{"container", &opts.Container, Containers, false},
		{"audio_format", &opts.AudioFormat, AudioFormats, false},
		{"audio_bitrate", &opts.AudioBitrate, AudioBitrates, true},
	} {
		if value, present := fields[f.name]; present {
			if s, ok := decodeLiteral(v, at(loc, f.name), value, f.allowed, f.intToString); ok {
				*f.target = s
			}
		}
	}
	// null counts as absent (no limit), as the nullable settings fields do
	if raw, present := fields["max_size_mb"]; present && !isNull(raw) {
		if n, ok := decodeInt(v, at(loc, "max_size_mb"), raw, 0, MaxSizeLimitMB); ok {
			opts.MaxSizeMB = n
		}
	}
	clipOK := true
	for _, f := range []struct {
		name   string
		target *int
	}{{"clip_start", &opts.ClipStart}, {"clip_end", &opts.ClipEnd}} {
		if raw, present := fields[f.name]; present && !isNull(raw) {
			n, ok := decodeInt(v, at(loc, f.name), raw, 0, MaxClipSeconds)
			if ok {
				*f.target = n
			}
			clipOK = clipOK && ok
		}
	}
	if clipOK {
		checkClipOrder(v, loc, opts)
	}
	return opts
}

// DecodeJobOptions validates a JSON options object; absent fields take their defaults. The
// error is a *ValidationError with locations under "options".
func DecodeJobOptions(data []byte) (JobOptions, error) {
	var v validator
	opts := decodeJobOptions(&v, []string{"options"}, data)
	if err := v.err(); err != nil {
		return JobOptions{}, err
	}
	return opts, nil
}

// DecodeJobRequest validates a POST /jobs body. The error is a *ValidationError.
func DecodeJobRequest(data []byte) (JobRequest, error) {
	var v validator
	req := JobRequest{Options: DefaultJobOptions()}
	fields, ok := decodeObject(data, &v)
	if !ok {
		return JobRequest{}, v.err()
	}
	if raw, present := fields["url"]; !present {
		v.add([]string{"body", "url"}, "Field required")
	} else if s, ok := decodeString(&v, []string{"body", "url"}, raw, MaxURLLength); ok {
		req.URL = s
	}
	if raw, present := fields["options"]; present {
		req.Options = decodeJobOptions(&v, []string{"body", "options"}, raw)
	}
	if raw, present := fields["items"]; present && !isNull(raw) {
		if s, ok := decodeString(&v, []string{"body", "items"}, raw, MaxItemsLength); ok {
			req.Items = &s
		}
	}
	if err := v.err(); err != nil {
		return JobRequest{}, err
	}
	return req, nil
}

// DecodeSettingsUpdate validates a PUT /settings body; null fields count as absent. The error is
// a *ValidationError.
func DecodeSettingsUpdate(data []byte) (SettingsUpdate, error) {
	var v validator
	var update SettingsUpdate
	fields, ok := decodeObject(data, &v)
	if !ok {
		return update, v.err()
	}
	if raw, present := fields["concurrency"]; present && !isNull(raw) {
		if n, ok := decodeInt(&v, []string{"body", "concurrency"}, raw, MinConcurrency, MaxConcurrency); ok {
			update.Concurrency = &n
		}
	}
	if raw, present := fields["fragments"]; present && !isNull(raw) {
		if n, ok := decodeInt(&v, []string{"body", "fragments"}, raw, MinFragments, MaxFragments); ok {
			update.Fragments = &n
		}
	}
	if raw, present := fields["cookies"]; present && !isNull(raw) {
		if b, ok := decodeBool(&v, []string{"body", "cookies"}, raw); ok {
			update.Cookies = &b
		}
	}
	if raw, present := fields["auto_update"]; present && !isNull(raw) {
		if b, ok := decodeBool(&v, []string{"body", "auto_update"}, raw); ok {
			update.AutoUpdate = &b
		}
	}
	if err := v.err(); err != nil {
		return SettingsUpdate{}, err
	}
	return update, nil
}

// DecodeSettings validates a settings.json file; absent fields take their defaults and null is
// rejected. Callers fall back to DefaultSettings on any error.
func DecodeSettings(data []byte) (Settings, error) {
	var v validator
	settings := DefaultSettings()
	fields, ok := decodeObject(data, &v)
	if !ok {
		return DefaultSettings(), v.err()
	}
	if raw, present := fields["concurrency"]; present {
		if n, ok := decodeInt(&v, []string{"concurrency"}, raw, MinConcurrency, MaxConcurrency); ok {
			settings.Concurrency = n
		}
	}
	if raw, present := fields["fragments"]; present {
		if n, ok := decodeInt(&v, []string{"fragments"}, raw, MinFragments, MaxFragments); ok {
			settings.Fragments = n
		}
	}
	if raw, present := fields["cookies"]; present {
		if b, ok := decodeBool(&v, []string{"cookies"}, raw); ok {
			settings.Cookies = b
		}
	}
	if raw, present := fields["auto_update"]; present {
		if b, ok := decodeBool(&v, []string{"auto_update"}, raw); ok {
			settings.AutoUpdate = b
		}
	}
	if err := v.err(); err != nil {
		return DefaultSettings(), err
	}
	return settings, nil
}

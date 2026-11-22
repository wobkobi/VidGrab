// ffmpeg re-encoding for size-limited jobs: picking an H.264 encoder that actually works on
// this machine (or SVT-AV1 for AV1), building the encode commands and parsing -progress
// output.

package core

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EncoderX264 is the software H.264 encoder, used when no hardware encoder passes the probe.
const EncoderX264 = "libx264"

// EncoderSvtAV1 is the software AV1 encoder used for every AV1 conversion: at the low
// bitrates size limits mean, it beats the hardware AV1 encoders the way libx264 beats
// hardware H.264.
const EncoderSvtAV1 = "libsvtav1"

// probeTimeout bounds one test encode; a hardware encoder that hangs initialising its driver
// counts as missing.
const probeTimeout = 15 * time.Second

// encoderCandidates lists the hardware H.264 encoders worth probing, best first: NVENC is the
// fastest and most consistent at a fixed bitrate, then Intel Quick Sync, then AMD AMF.
var encoderCandidates = []string{"h264_nvenc", "h264_qsv", "h264_amf"}

// FfmpegPath returns the ffmpeg executable from tools.FfmpegDir, or plain "ffmpeg.exe" for PATH.
func FfmpegPath(tools Tools) string {
	const name = "ffmpeg.exe"
	if tools.FfmpegDir == "" {
		return name
	}
	return filepath.Join(tools.FfmpegDir, name)
}

// EncoderProbe finds the best working H.264 encoder once per ffmpeg binary and remembers it
// for the life of the process. The zero value is not usable; call NewEncoderProbe.
type EncoderProbe struct {
	run Runner

	// lock is a one-slot semaphore rather than a mutex so a caller waiting for another job's
	// probe can give up when its context ends
	lock  chan struct{}
	cache map[string]string
}

// NewEncoderProbe returns a probe that runs test encodes through run (DefaultRunner when nil).
func NewEncoderProbe(run Runner) *EncoderProbe {
	if run == nil {
		run = DefaultRunner
	}
	return &EncoderProbe{run: run, lock: make(chan struct{}, 1), cache: map[string]string{}}
}

// Pick returns the first hardware encoder whose one-frame test encode succeeds, else
// EncoderX264.
//
// An encoder being listed by `ffmpeg -encoders` says nothing about the GPU or driver, so each
// candidate encodes one black 256x144 frame with the same rate options a real job uses. Only
// one probe runs at a time, so jobs starting together wait for one probe rather than each
// running their own. When ctx ends (the job was paused or cancelled) Pick returns EncoderX264
// straight away and caches nothing, so the next caller probes properly.
func (p *EncoderProbe) Pick(ctx context.Context, ffmpeg string) string {
	select {
	case p.lock <- struct{}{}:
	case <-ctx.Done():
		return EncoderX264
	}
	defer func() { <-p.lock }()
	if encoder, ok := p.cache[ffmpeg]; ok {
		return encoder
	}
	encoder := EncoderX264
	for _, candidate := range encoderCandidates {
		if p.works(ctx, ffmpeg, candidate) {
			encoder = candidate
			break
		}
	}
	if ctx.Err() != nil {
		return EncoderX264
	}
	p.cache[ffmpeg] = encoder
	return encoder
}

func (p *EncoderProbe) works(ctx context.Context, ffmpeg, encoder string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	argv := []string{ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x144:d=0.1", "-frames:v", "1"}
	argv = append(argv, videoCodecArgs(encoder, 500, 144, 0)...)
	argv = append(argv, "-f", "null", "-")
	_, _, code, err := p.run(ctx, argv)
	return err == nil && code == 0
}

// svtLongSeconds is the output length from which SVT-AV1 always runs preset 8: at 360p15
// preset 5 scored 2.4-2.5 VMAF over preset 8 but took 3 times as long, which is worth it for a
// short clip and not for a 30-minute video (preset 5 took 9 minutes for 30 minutes of 178p).
const svtLongSeconds = 20 * 60

// svtPreset returns the SVT-AV1 speed preset for an output height and length in seconds (0
// when unknown; lower presets are slower and better). At 360p preset 5 scored about 2 VMAF
// over preset 8 at 3 times the time; taller outputs come with comfortable bitrates where the
// faster presets lose little, and a 1080p encode at preset 5 would take longer than the video
// plays. Outputs of svtLongSeconds or more get preset 8 at any height.
func svtPreset(height int, seconds float64) string {
	switch {
	case seconds >= svtLongSeconds:
		return "8"
	case height <= 480:
		return "5"
	case height <= 720:
		return "7"
	}
	return "8"
}

// videoCodecArgs returns the encoder and rate-control options for kbps of video at height,
// seconds long (0 when unknown).
//
// SVT-AV1 runs one pass in its default variable-bitrate mode, aimed at the target with no
// maxrate (it only honours one in capped-CRF mode); the size check afterwards catches an
// overshoot. Everything below is about the H.264 encoders.
//
// maxrate equals the target and bufsize is two seconds of it, which keeps the rate tight
// enough that the output size tracks the target closely. Hardware encoders run one pass, so
// they are told to hold a constant bitrate; libx264's two passes spread bits where they help,
// and preset slower is worth its time because libx264 only runs at low bitrates (see
// HardwareMinKbps), where every bit counts.
// QSV wants NV12 input; the others take plain 4:2:0, which also turns 10-bit HDR sources into
// the 8-bit stream every player handles.
func videoCodecArgs(encoder string, kbps, height int, seconds float64) []string {
	rate := strconv.Itoa(kbps) + "k"
	buffer := strconv.Itoa(kbps*2) + "k"
	args := []string{"-c:v", encoder}
	if encoder == EncoderSvtAV1 {
		return append(args, "-preset", svtPreset(height, seconds), "-b:v", rate, "-pix_fmt", "yuv420p")
	}
	switch encoder {
	case "h264_nvenc", "h264_amf":
		args = append(args, "-rc", "cbr")
	case EncoderX264:
		args = append(args, "-preset", "slower")
	}
	args = append(args, "-b:v", rate, "-maxrate", rate, "-bufsize", buffer)
	if encoder == "h264_qsv" {
		return append(args, "-pix_fmt", "nv12")
	}
	return append(args, "-pix_fmt", "yuv420p")
}

// EncodeJob describes one re-encode of Source into Output.
type EncodeJob struct {
	Ffmpeg  string
	Encoder string
	Source  string
	Output  string
	// PassLog is the -passlogfile prefix for libx264's two passes.
	PassLog string
	Plan    EncodePlan
	// Seconds is the output's length, 0 when unknown; long outputs get a faster AV1 preset.
	Seconds float64
}

// scaleFilter sizes the shorter side to height without upscaling, so portrait Shorts get the
// same treatment as landscape video. -2 keeps the aspect ratio with an even dimension, and
// trunc(x/2)*2 makes the fixed side even too, as 4:2:0 H.264 requires. The quotes stop the
// filter parser splitting the expressions on their commas.
func scaleFilter(height int) string {
	h := strconv.Itoa(height)
	return fmt.Sprintf("scale='if(gte(iw,ih),-2,trunc(min(%[1]s,iw)/2)*2)':'if(gte(iw,ih),trunc(min(%[1]s,ih)/2)*2,-2)'", h)
}

// fpsArg formats a plan's frame rate for -fpsmax, which lowers only sources faster than it (a
// plain -r would duplicate frames of a slower source, and the downloaded stream can be slower
// than the one the plan was made from). The value is rounded up to 3 decimal places, so a
// 23.976 source halved to 11.988011 is not clamped a hair below that even rate; the 1e-6
// keeps float error from rounding an exact 29.97 up to 29.971.
func fpsArg(fps float64) string {
	return strconv.FormatFloat(math.Ceil(fps*1000-1e-6)/1000, 'f', -1, 64)
}

// Passes returns the ffmpeg command for each pass: two for libx264 (an analysis pass with no
// output, then the real encode) and one for SVT-AV1 or a hardware encoder.
//
// Every pass reports progress as key=value lines on stdout (-progress pipe:1) and logs only
// errors, so FfmpegProgress can tell the two apart. The final pass maps the first video and
// (when present) first audio stream, copies the source's tags with -map_metadata so the
// title, channel and URL yt-dlp embedded survive, and moves the index to the front
// (+faststart) so the video starts playing before it has fully loaded.
func (e EncodeJob) Passes() [][]string {
	head := func() []string {
		return []string{e.Ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-nostats",
			"-progress", "pipe:1", "-y", "-i", e.Source}
	}
	video := []string{"-vf", scaleFilter(e.Plan.Height)}
	if e.Plan.FPS > 0 {
		video = append(video, "-fpsmax", fpsArg(e.Plan.FPS))
	}
	video = append(video, videoCodecArgs(e.Encoder, e.Plan.VideoKbps, e.Plan.Height, e.Seconds)...)
	final := append([]string{"-map", "0:v:0", "-map", "0:a:0?"}, video...)
	channels := e.Plan.Channels
	if channels <= 0 {
		channels = 2
	}
	audio := []string{"-c:a", "aac", "-b:a", strconv.Itoa(e.Plan.AudioKbps) + "k", "-ac", strconv.Itoa(channels)}
	if e.Plan.AudioKbps <= 0 {
		audio = []string{"-an"}
	}
	tail := append(audio, "-map_metadata", "0", "-movflags", "+faststart", "-f", "mp4", e.Output)

	if e.Encoder != EncoderX264 {
		args := append(head(), final...)
		return [][]string{append(args, tail...)}
	}
	first := append(head(), "-map", "0:v:0")
	first = append(first, video...)
	first = append(first, "-pass", "1", "-passlogfile", e.PassLog, "-an", "-f", "null", os.DevNull)
	second := append(head(), final...)
	second = append(second, "-pass", "2", "-passlogfile", e.PassLog)
	second = append(second, tail...)
	return [][]string{first, second}
}

// RemovePassLogs deletes the files libx264 wrote under a -passlogfile prefix
// (prefix-0.log, prefix-0.log.mbtree and their .temp versions).
func RemovePassLogs(prefix string) {
	matches, _ := filepath.Glob(prefix + "-*.log*")
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

// FfmpegProgress parses the combined output of the passes of one encode. Duration is the
// source length in seconds; Pass (0-based) and Passes place the current pass on the overall
// 0-100 scale, so with two passes the first fills 0-50 and the second 50-100. Not safe for
// concurrent use.
type FfmpegProgress struct {
	Duration float64
	Pass     int
	Passes   int
	// Percent is overall progress so far; it never moves backwards.
	Percent float64

	outTime float64 // seconds encoded in the current pass
	speed   float64 // realtime multiple; 0 when unknown
	text    string  // speed as ffmpeg printed it, e.g. "2.4x"
	tail    []string
}

// Detail returns the last non-progress lines (ffmpeg's errors) for the job's detail field.
func (p *FfmpegProgress) Detail() string { return strings.Join(p.tail, "\n") }

// LastError returns the last error line ffmpeg printed, or "".
func (p *FfmpegProgress) LastError() string {
	if len(p.tail) == 0 {
		return ""
	}
	return p.tail[len(p.tail)-1]
}

// Feed takes one output line and returns an update at the end of each progress block
// ("progress=continue" or "progress=end"), or nil.
//
// out_time_us is the position in microseconds; out_time_ms carries the same microsecond value
// despite its name (a long-standing ffmpeg quirk), so either is read the same way. Early
// blocks report "N/A" or a negative time and are skipped.
func (p *FfmpegProgress) Feed(rawLine string) *ParseUpdate {
	line := strings.TrimSpace(rawLine)
	if line == "" {
		return nil
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok || strings.ContainsAny(key, " \t:[") {
		p.tail = append(p.tail, line)
		if len(p.tail) > TailLines {
			p.tail = p.tail[len(p.tail)-TailLines:]
		}
		return nil
	}
	value = strings.TrimSpace(value)
	switch key {
	case "out_time_us", "out_time_ms":
		if us, err := strconv.ParseFloat(value, 64); err == nil && us >= 0 {
			p.outTime = us / 1e6
		}
	case "speed":
		factor, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64)
		if err == nil && factor > 0 && !math.IsInf(factor, 0) {
			p.speed, p.text = factor, strconv.FormatFloat(factor, 'f', 1, 64)+"x"
		} else {
			p.speed, p.text = 0, ""
		}
	case "progress":
		return p.update(value == "end")
	}
	return nil
}

func (p *FfmpegProgress) update(end bool) *ParseUpdate {
	passes := max(p.Passes, 1)
	within := 0.0
	if p.Duration > 0 {
		within = math.Min(p.outTime/p.Duration, 1)
	}
	if end {
		within = 1
	}
	overall := (float64(p.Pass) + within) / float64(passes) * 100
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(math.Min(overall, 100), 'f', 1, 64), 64)
	p.Percent = math.Max(p.Percent, rounded)
	percent := p.Percent
	update := &ParseUpdate{Percent: &percent}
	if !end {
		update.Speed = p.text
		if p.speed > 0 && p.Duration > 0 {
			// Media seconds left in this pass and every later one, at the current speed
			left := p.Duration*float64(passes-p.Pass) - math.Min(p.outTime, p.Duration)
			update.Eta = FormatEta(left / p.speed)
		}
	}
	return update
}

// NextPass moves the parser to the next pass of the same encode.
func (p *FfmpegProgress) NextPass() {
	p.Pass++
	p.outTime, p.speed, p.text = 0, 0, ""
}

// mp4AudioCodecs are the audio codecs (yt-dlp's names, matched as prefixes) an MP4 carries
// that players handle well. Opus, Vorbis, FLAC and PCM also mux into MP4 but many players
// will not play them there. Refract uses the same table.
var mp4AudioCodecs = []string{"mp4a", "aac", "mp3", "ac-3", "ac3", "ec-3", "eac3", "alac"}

// MP4AudioNeedsFix reports whether an MP4 whose sound yt-dlp names acodec should have it
// re-encoded. No sound ("none") and an unknown codec ("" or "NA") are left alone.
func MP4AudioNeedsFix(acodec string) bool {
	codec := strings.ToLower(strings.TrimSpace(acodec))
	if codec == "" || codec == "none" || codec == "na" {
		return false
	}
	for _, ok := range mp4AudioCodecs {
		if strings.HasPrefix(codec, ok) {
			return false
		}
	}
	return true
}

// FixAudioKbps is the AAC bitrate the sound is re-encoded at when it has to be: 128k mono,
// 160k stereo (or unknown), 64k a channel for surround up to 512k. Sound outside the size
// tools is kept generous, since it is a small part of a video's size.
func FixAudioKbps(channels int) int {
	switch {
	case channels == 1:
		return 128
	case channels <= 2:
		return 160
	}
	return min(512, 64*channels)
}

// MP4AudioFixArgs returns the ffmpeg command that copies source to output with every stream
// kept as it is except the sound, which becomes AAC at FixAudioKbps(channels).
func MP4AudioFixArgs(ffmpeg, source, output string, channels int) []string {
	return []string{ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", source,
		"-map", "0", "-c", "copy", "-c:a", "aac", "-b:a", strconv.Itoa(FixAudioKbps(channels)) + "k",
		"-map_metadata", "0", "-movflags", "+faststart", "-f", "mp4", output}
}

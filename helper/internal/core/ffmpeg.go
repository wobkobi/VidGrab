// ffmpeg re-encoding for size-limited jobs: picking an H.264 encoder that actually works on
// this machine, building the encode commands and parsing -progress output.

package core

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// EncoderX264 is the software H.264 encoder, used when no hardware encoder passes the probe.
const EncoderX264 = "libx264"

// EncoderVaapi is the Linux VA-API H.264 encoder (AMD and Intel GPUs).
const EncoderVaapi = "h264_vaapi"

// VaapiDevice is the render node VA-API encodes on. The first render node is the GPU on
// nearly every single-GPU machine; machines where it is not simply fall back to libx264.
const VaapiDevice = "/dev/dri/renderD128"

// probeTimeout bounds one test encode; a hardware encoder that hangs initialising its driver
// counts as missing.
const probeTimeout = 15 * time.Second

// encoderCandidates lists the hardware H.264 encoders worth probing on goos, best first: NVENC
// is the fastest and most consistent at a fixed bitrate, then Intel Quick Sync, then AMD AMF
// (or VA-API on Linux, which covers both AMD and Intel). macOS has only VideoToolbox.
func encoderCandidates(goos string) []string {
	switch goos {
	case "windows":
		return []string{"h264_nvenc", "h264_qsv", "h264_amf"}
	case "darwin":
		return []string{"h264_videotoolbox"}
	case "linux":
		return []string{"h264_nvenc", "h264_qsv", EncoderVaapi}
	}
	return nil
}

// FfmpegPath returns the ffmpeg executable from tools.FfmpegDir, or plain "ffmpeg" for PATH.
func FfmpegPath(tools Tools) string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if tools.FfmpegDir == "" {
		return name
	}
	return filepath.Join(tools.FfmpegDir, name)
}

// EncoderProbe finds the best working H.264 encoder once per ffmpeg binary and remembers it
// for the life of the process. The zero value is not usable; call NewEncoderProbe.
type EncoderProbe struct {
	run  Runner
	goos string
	// exists reports whether a device path exists; VA-API is skipped without its render node
	exists func(path string) bool

	// lock is a one-slot semaphore rather than a mutex so a caller waiting for another job's
	// probe can give up when its context ends
	lock  chan struct{}
	cache map[string]string
}

// NewEncoderProbe returns a probe that runs test encodes through run (DefaultRunner when nil)
// and tries the hardware encoders for goos ("" means runtime.GOOS).
func NewEncoderProbe(run Runner, goos string) *EncoderProbe {
	if run == nil {
		run = DefaultRunner
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	return &EncoderProbe{
		run: run, goos: goos, exists: pathExists,
		lock: make(chan struct{}, 1), cache: map[string]string{},
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	for _, candidate := range encoderCandidates(p.goos) {
		if candidate == EncoderVaapi && !p.exists(VaapiDevice) {
			continue
		}
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
	argv := []string{ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error"}
	argv = append(argv, deviceArgs(encoder)...)
	argv = append(argv, "-f", "lavfi", "-i", "color=c=black:s=256x144:d=0.1", "-frames:v", "1")
	if encoder == EncoderVaapi {
		argv = append(argv, "-vf", vaapiUpload)
	}
	argv = append(argv, videoCodecArgs(encoder, 500)...)
	argv = append(argv, "-f", "null", "-")
	_, _, code, err := p.run(ctx, argv)
	return err == nil && code == 0
}

// vaapiUpload ends a VA-API filter chain: frames are converted to NV12 in system memory, then
// uploaded to the GPU surface the encoder reads. Scaling happens before it, on the CPU, so the
// same scale expression serves every encoder.
const vaapiUpload = "format=nv12,hwupload"

// deviceArgs returns the global options an encoder needs before the input: VA-API must be told
// which render node to open.
func deviceArgs(encoder string) []string {
	if encoder == EncoderVaapi {
		return []string{"-vaapi_device", VaapiDevice}
	}
	return nil
}

// videoCodecArgs returns the encoder and rate-control options for kbps of video.
//
// maxrate equals the target and bufsize is two seconds of it, which keeps the rate tight
// enough that the output size tracks the target closely. Hardware encoders run one pass, so
// they are told to hold a constant bitrate; libx264's two passes spread bits where they help.
// QSV wants NV12 input and VA-API takes the uploaded surface as it is; the others take plain
// 4:2:0, which also turns 10-bit HDR sources into the 8-bit stream every player handles.
func videoCodecArgs(encoder string, kbps int) []string {
	rate := strconv.Itoa(kbps) + "k"
	buffer := strconv.Itoa(kbps*2) + "k"
	args := []string{"-c:v", encoder}
	switch encoder {
	case "h264_nvenc", "h264_amf":
		args = append(args, "-rc", "cbr")
	case EncoderVaapi:
		args = append(args, "-rc_mode", "CBR")
	case EncoderX264:
		args = append(args, "-preset", "medium")
	}
	args = append(args, "-b:v", rate, "-maxrate", rate, "-bufsize", buffer)
	switch encoder {
	case "h264_qsv":
		return append(args, "-pix_fmt", "nv12")
	case EncoderVaapi:
		return args
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
}

// scaleFilter sizes the shorter side to height without upscaling, so portrait Shorts get the
// same treatment as landscape video. -2 keeps the aspect ratio with an even dimension, and
// trunc(x/2)*2 makes the fixed side even too, as 4:2:0 H.264 requires. The quotes stop the
// filter parser splitting the expressions on their commas.
func scaleFilter(height int) string {
	h := strconv.Itoa(height)
	return fmt.Sprintf("scale='if(gte(iw,ih),-2,trunc(min(%[1]s,iw)/2)*2)':'if(gte(iw,ih),trunc(min(%[1]s,ih)/2)*2,-2)'", h)
}

// Passes returns the ffmpeg command for each pass: two for libx264 (an analysis pass with no
// output, then the real encode) and one for a hardware encoder.
//
// Every pass reports progress as key=value lines on stdout (-progress pipe:1) and logs only
// errors, so FfmpegProgress can tell the two apart. The final pass maps the first video and
// (when present) first audio stream, copies the source's tags with -map_metadata so the
// title, channel and URL yt-dlp embedded survive, and moves the index to the front
// (+faststart) so the video starts playing before it has fully loaded. VA-API opens its
// render node before the input and uploads the scaled frames at the end of the filter chain.
func (e EncodeJob) Passes() [][]string {
	head := func() []string {
		args := []string{e.Ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-nostats",
			"-progress", "pipe:1", "-y"}
		args = append(args, deviceArgs(e.Encoder)...)
		return append(args, "-i", e.Source)
	}
	filter := scaleFilter(e.Plan.Height)
	if e.Encoder == EncoderVaapi {
		filter += "," + vaapiUpload
	}
	video := []string{"-vf", filter}
	if e.Plan.CapFPS {
		// -fpsmax lowers only sources above 30 fps; a plain -r 30 would duplicate frames of
		// a 24 fps film
		video = append(video, "-fpsmax", "30")
	}
	video = append(video, videoCodecArgs(e.Encoder, e.Plan.VideoKbps)...)
	final := append([]string{"-map", "0:v:0", "-map", "0:a:0?"}, video...)
	tail := []string{
		"-c:a", "aac", "-b:a", strconv.Itoa(e.Plan.AudioKbps) + "k", "-ac", "2",
		"-map_metadata", "0", "-movflags", "+faststart", "-f", "mp4", e.Output,
	}

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

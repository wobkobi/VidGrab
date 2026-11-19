package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeEncoderRunner succeeds only for the encoders in working and records every probe.
type fakeEncoderRunner struct {
	mu      sync.Mutex
	working map[string]bool
	probed  []string
}

func (f *fakeEncoderRunner) run(_ context.Context, argv []string) (string, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	encoder := argv[slices.Index(argv, "-c:v")+1]
	f.probed = append(f.probed, encoder)
	if f.working[encoder] {
		return "", "", 0, nil
	}
	return "", "Cannot load nvcuda.dll", 1, nil
}

func TestEncoderProbeOrderAndCache(t *testing.T) {
	cases := []struct {
		goos    string
		working []string
		want    string
		probed  []string
	}{
		{"windows", []string{"h264_nvenc", "h264_qsv"}, "h264_nvenc", []string{"h264_nvenc"}},
		{"windows", []string{"h264_qsv", "h264_amf"}, "h264_qsv", []string{"h264_nvenc", "h264_qsv"}},
		{"windows", []string{"h264_amf"}, "h264_amf", []string{"h264_nvenc", "h264_qsv", "h264_amf"}},
		{"windows", nil, EncoderX264, []string{"h264_nvenc", "h264_qsv", "h264_amf"}},
		{"darwin", []string{"h264_videotoolbox"}, "h264_videotoolbox", []string{"h264_videotoolbox"}},
		{"linux", nil, EncoderX264, []string{"h264_nvenc", "h264_qsv", "h264_vaapi"}},
		{"linux", []string{"h264_vaapi"}, "h264_vaapi", []string{"h264_nvenc", "h264_qsv", "h264_vaapi"}},
		{"freebsd", nil, EncoderX264, nil},
	}
	for _, c := range cases {
		fake := &fakeEncoderRunner{working: map[string]bool{}}
		for _, name := range c.working {
			fake.working[name] = true
		}
		probe := NewEncoderProbe(fake.run, c.goos)
		probe.exists = func(path string) bool { return path == VaapiDevice }
		if got := probe.Pick(bg, "ffmpeg"); got != c.want {
			t.Errorf("%s %v: picked %s, want %s", c.goos, c.working, got, c.want)
		}
		// The second call is served from the cache
		probe.Pick(bg, "ffmpeg")
		if !slices.Equal(fake.probed, c.probed) {
			t.Errorf("%s %v: probed %v, want %v", c.goos, c.working, fake.probed, c.probed)
		}
	}
}

func TestEncoderProbeTestEncode(t *testing.T) {
	var argv []string
	probe := NewEncoderProbe(func(_ context.Context, a []string) (string, string, int, error) {
		argv = a
		return "", "", 0, nil
	}, "darwin")
	probe.Pick(bg, "/opt/ffmpeg")
	joined := strings.Join(argv, " ")
	if argv[0] != "/opt/ffmpeg" || !strings.Contains(joined, "-f lavfi -i color=c=black:s=256x144:d=0.1 -frames:v 1") ||
		!strings.HasSuffix(joined, "-f null -") {
		t.Fatalf("probe argv %v", argv)
	}
	// A different binary is probed separately
	argv = nil
	probe.Pick(bg, "/opt/ffmpeg")
	if argv != nil {
		t.Fatal("same binary probed twice")
	}
	probe.Pick(bg, "/other/ffmpeg")
	if argv == nil {
		t.Fatal("new binary not probed")
	}
}

var bg = context.Background()

func TestEncoderProbeSkipsVaapiWithoutDevice(t *testing.T) {
	fake := &fakeEncoderRunner{working: map[string]bool{"h264_vaapi": true}}
	probe := NewEncoderProbe(fake.run, "linux")
	probe.exists = func(string) bool { return false }
	if got := probe.Pick(bg, "ffmpeg"); got != EncoderX264 || slices.Contains(fake.probed, "h264_vaapi") {
		t.Fatalf("picked %s after probing %v", got, fake.probed)
	}
}

func TestEncoderProbeVaapiTestEncode(t *testing.T) {
	var argv []string
	probe := NewEncoderProbe(func(_ context.Context, a []string) (string, string, int, error) {
		argv = a
		if after(a, "-c:v") == "h264_vaapi" {
			return "", "", 0, nil
		}
		return "", "", 1, nil
	}, "linux")
	probe.exists = func(string) bool { return true }
	if got := probe.Pick(bg, "ffmpeg"); got != "h264_vaapi" {
		t.Fatalf("picked %s", got)
	}
	// The device comes before the input and the upload after the test frame's input
	device, input := slices.Index(argv, "-vaapi_device"), slices.Index(argv, "-i")
	if device < 0 || device > input || after(argv, "-vaapi_device") != VaapiDevice ||
		after(argv, "-vf") != "format=nv12,hwupload" || after(argv, "-rc_mode") != "CBR" || slices.Contains(argv, "-pix_fmt") {
		t.Fatalf("vaapi probe argv %v", argv)
	}
}

func TestEncoderProbeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(bg)
	fake := &fakeEncoderRunner{working: map[string]bool{"h264_nvenc": true}}
	probe := NewEncoderProbe(func(c context.Context, a []string) (string, string, int, error) {
		cancel()
		return "", "", -1, c.Err()
	}, "windows")
	if got := probe.Pick(ctx, "ffmpeg"); got != EncoderX264 {
		t.Fatalf("cancelled probe picked %s", got)
	}
	// Nothing was cached, so a live caller probes properly
	probe.run = fake.run
	if got := probe.Pick(bg, "ffmpeg"); got != "h264_nvenc" {
		t.Fatalf("after a cancelled probe picked %s", got)
	}

	// A caller waiting for another probe gives up when its context ends
	probe.lock <- struct{}{}
	waiting, stop := context.WithCancel(bg)
	stop()
	if got := probe.Pick(waiting, "other"); got != EncoderX264 {
		t.Fatalf("waiting caller got %s", got)
	}
	<-probe.lock
}

func TestEncoderProbeTreatsStartErrorAsMissing(t *testing.T) {
	probe := NewEncoderProbe(func(context.Context, []string) (string, string, int, error) {
		return "", "", -1, context.DeadlineExceeded
	}, "windows")
	if got := probe.Pick(bg, "ffmpeg"); got != EncoderX264 {
		t.Fatalf("picked %s", got)
	}
}

func TestFfmpegPath(t *testing.T) {
	exe := "ffmpeg"
	if filepath.Separator == '\\' {
		exe = "ffmpeg.exe"
	}
	if got := FfmpegPath(Tools{}); got != exe {
		t.Errorf("PATH lookup = %q", got)
	}
	if got := FfmpegPath(Tools{FfmpegDir: filepath.Join("data", "tools", "ffmpeg")}); got != filepath.Join("data", "tools", "ffmpeg", exe) {
		t.Errorf("tools dir = %q", got)
	}
}

func after(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestEncodeJobPassesX264(t *testing.T) {
	job := EncodeJob{
		Ffmpeg: "ffmpeg", Encoder: EncoderX264, Source: "in.source.mp4", Output: "in.sized.mp4",
		PassLog: "tmp/vidgrab-pass-1", Plan: EncodePlan{VideoKbps: 1170, AudioKbps: 96, Height: 360, CapFPS: true},
	}
	passes := job.Passes()
	if len(passes) != 2 {
		t.Fatalf("%d passes", len(passes))
	}
	first, second := passes[0], passes[1]
	for _, args := range passes {
		if args[0] != "ffmpeg" || after(args, "-i") != "in.source.mp4" || after(args, "-progress") != "pipe:1" ||
			!slices.Contains(args, "-nostats") || after(args, "-b:v") != "1170k" || after(args, "-maxrate") != "1170k" ||
			after(args, "-bufsize") != "2340k" || after(args, "-preset") != "medium" || after(args, "-fpsmax") != "30" ||
			after(args, "-passlogfile") != "tmp/vidgrab-pass-1" || after(args, "-pix_fmt") != "yuv420p" ||
			!strings.Contains(after(args, "-vf"), "min(360,ih)") {
			t.Errorf("pass args %v", args)
		}
	}
	if after(first, "-pass") != "1" || !slices.Contains(first, "-an") || last(first) != os.DevNull || slices.Contains(first, "in.sized.mp4") {
		t.Errorf("first pass %v", first)
	}
	if after(second, "-pass") != "2" || after(second, "-c:a") != "aac" || after(second, "-b:a") != "96k" ||
		after(second, "-map_metadata") != "0" || after(second, "-movflags") != "+faststart" || last(second) != "in.sized.mp4" ||
		!strings.Contains(strings.Join(second, " "), "-map 0:v:0 -map 0:a:0?") {
		t.Errorf("second pass %v", second)
	}
}

func last(args []string) string { return args[len(args)-1] }

func TestEncodeJobPassesHardware(t *testing.T) {
	cases := []struct {
		encoder, rc, pixFmt string
	}{
		{"h264_nvenc", "cbr", "yuv420p"},
		{"h264_amf", "cbr", "yuv420p"},
		{"h264_qsv", "", "nv12"},
		{"h264_videotoolbox", "", "yuv420p"},
		{"h264_vaapi", "", ""},
	}
	for _, c := range cases {
		job := EncodeJob{Ffmpeg: "ffmpeg", Encoder: c.encoder, Source: "s.mp4", Output: "o.mp4",
			Plan: EncodePlan{VideoKbps: 3000, AudioKbps: 96, Height: 720}}
		passes := job.Passes()
		if len(passes) != 1 {
			t.Fatalf("%s: %d passes", c.encoder, len(passes))
		}
		args := passes[0]
		if after(args, "-c:v") != c.encoder || after(args, "-rc") != c.rc || after(args, "-pix_fmt") != c.pixFmt ||
			after(args, "-b:v") != "3000k" || after(args, "-maxrate") != "3000k" || after(args, "-bufsize") != "6000k" ||
			slices.Contains(args, "-pass") || slices.Contains(args, "-fpsmax") || last(args) != "o.mp4" {
			t.Errorf("%s args %v", c.encoder, args)
		}
		vaapi := c.encoder == "h264_vaapi"
		if (after(args, "-rc_mode") == "CBR") != vaapi || slices.Contains(args, "-vaapi_device") != vaapi ||
			strings.HasSuffix(after(args, "-vf"), ",format=nv12,hwupload") != vaapi {
			t.Errorf("%s vaapi options %v", c.encoder, args)
		}
	}
}

func TestEncodeJobPassesVaapiOrder(t *testing.T) {
	job := EncodeJob{Ffmpeg: "ffmpeg", Encoder: EncoderVaapi, Source: "s.mp4", Output: "o.mp4",
		Plan: EncodePlan{VideoKbps: 800, AudioKbps: 96, Height: 360, CapFPS: true}}
	args := job.Passes()[0]
	device, input := slices.Index(args, "-vaapi_device"), slices.Index(args, "-i")
	// The render node is a global option, so it must precede the input it decodes
	if device < 0 || device > input || after(args, "-vaapi_device") != "/dev/dri/renderD128" {
		t.Fatalf("device args %v", args)
	}
	// Scale on the CPU first, then convert and upload for the encoder
	if vf := after(args, "-vf"); !strings.HasPrefix(vf, "scale=") || !strings.HasSuffix(vf, "min(360,ih)/2)*2,-2)',format=nv12,hwupload") {
		t.Fatalf("filter %q", vf)
	}
	if after(args, "-c:v") != "h264_vaapi" || after(args, "-b:v") != "800k" || after(args, "-fpsmax") != "30" {
		t.Fatalf("args %v", args)
	}
}

func TestFfmpegProgress(t *testing.T) {
	block := func(us, speed, state string) []string {
		return []string{"frame=10", "fps=25.0", "bitrate=1000kbits/s", "total_size=1024",
			"out_time_us=" + us, "out_time_ms=" + us, "out_time=00:00:01.000000", "speed=" + speed, "progress=" + state}
	}
	feed := func(p *FfmpegProgress, lines []string) *ParseUpdate {
		var last *ParseUpdate
		for _, line := range lines {
			if u := p.Feed(line); u != nil {
				last = u
			}
		}
		return last
	}

	// Two passes over a 100 s source: pass 1 fills 0-50, pass 2 fills 50-100
	p := &FfmpegProgress{Duration: 100, Passes: 2}
	if u := feed(p, block("N/A", "N/A", "continue")); u == nil || *u.Percent != 0 || u.Speed != "" || u.Eta != "" {
		t.Fatalf("N/A block %+v", u)
	}
	u := feed(p, block("50000000", "4.02x", "continue"))
	// Half of pass 1 = 25%; 50 + 100 media seconds left at 4x = 37.5 s
	if *u.Percent != 25 || u.Speed != "4.0x" || u.Eta != "0:37" {
		t.Fatalf("pass 1 half %+v (%v)", u, *u.Percent)
	}
	u = feed(p, block("100000000", "4x", "end"))
	if *u.Percent != 50 || u.Speed != "" || u.Eta != "" {
		t.Fatalf("pass 1 end %+v", u)
	}
	p.NextPass()
	u = feed(p, block("25000000", "2.4x", "continue"))
	// 50 + 25/100 * 50 = 62.5%; 75 media seconds left at 2.4x = 31.25 s
	if *u.Percent != 62.5 || u.Speed != "2.4x" || u.Eta != "0:31" {
		t.Fatalf("pass 2 quarter %+v (%v)", u, *u.Percent)
	}
	// A time past the duration (audio tail) is clamped, and percent never goes back
	u = feed(p, block("-5", "2.4x", "continue"))
	if *u.Percent != 62.5 {
		t.Fatalf("backwards %v", *u.Percent)
	}
	u = feed(p, block("120000000", "2.4x", "continue"))
	if *u.Percent != 100 {
		t.Fatalf("clamp %v", *u.Percent)
	}

	// Error lines are kept for the detail and never parsed as progress
	q := &FfmpegProgress{Duration: 10, Passes: 1}
	if q.Feed("[libx264 @ 000001] bitrate=abc invalid") != nil || q.Feed("Conversion failed!") != nil {
		t.Fatal("error line parsed as progress")
	}
	if q.LastError() != "Conversion failed!" || !strings.Contains(q.Detail(), "libx264") {
		t.Fatalf("detail %q", q.Detail())
	}
	// out_time_ms alone (older builds) is read as microseconds too
	u = feed(q, []string{"out_time_ms=5000000", "speed=1x", "progress=continue"})
	if *u.Percent != 50 || u.Eta != "0:05" {
		t.Fatalf("out_time_ms %+v", u)
	}
}

// TestRealFfmpegFit encodes a generated clip to a 1 MB target with the real ffmpeg on PATH
// and the encoder this machine picks. Set VIDGRAB_TEST_FFMPEG=1 to run it.
func TestRealFfmpegFit(t *testing.T) {
	if os.Getenv("VIDGRAB_TEST_FFMPEG") != "1" {
		t.Skip("set VIDGRAB_TEST_FFMPEG=1 to run against the real ffmpeg")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	run := func(argv []string) string {
		t.Helper()
		out, errOut, code, err := DefaultRunner(context.Background(), argv)
		if err != nil || code != 0 {
			t.Fatalf("%v: code %d err %v\n%s%s", argv, code, err, out, errOut)
		}
		return out
	}

	picked := NewEncoderProbe(nil, "").Pick(context.Background(), ffmpeg)
	t.Logf("encoder picked: %s", picked)
	encoders := []string{picked}
	if picked != EncoderX264 {
		encoders = append(encoders, EncoderX264)
	}
	for _, encoder := range encoders {
		for _, shape := range []struct{ name, size string }{{"landscape", "1280x720"}, {"portrait", "720x1280"}} {
			t.Run(encoder+"/"+shape.name, func(t *testing.T) {
				// 20 s of busy test pattern at 60 fps plus a tone, tagged like a yt-dlp download
				source := filepath.Join(dir, encoder+" "+shape.name+" [abc].source.mp4")
				run([]string{ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
					"-f", "lavfi", "-i", "testsrc2=s=" + shape.size + ":r=60:d=20",
					"-f", "lavfi", "-i", "sine=f=440:d=20",
					"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-c:a", "aac",
					"-metadata", "title=Real ffmpeg test", "-shortest", source})

				plan, err := PlanEncode(1, 20, 720)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("plan: %+v", plan)
				limit := LimitBytes(1)
				output := filepath.Join(dir, encoder+" "+shape.name+" [abc].sized.mp4")
				job := EncodeJob{Ffmpeg: ffmpeg, Encoder: encoder, Source: source, Output: output,
					PassLog: filepath.Join(dir, "pass-"+encoder+shape.name), Plan: plan}
				var size int64
				for attempt := 0; attempt < 2; attempt++ {
					passes := job.Passes()
					progress := &FfmpegProgress{Duration: 20, Passes: len(passes)}
					for i, argv := range passes {
						if i > 0 {
							progress.NextPass()
						}
						for _, line := range strings.Split(run(argv), "\n") {
							progress.Feed(line)
						}
					}
					if progress.Percent != 100 {
						t.Errorf("progress ended at %v", progress.Percent)
					}
					info, err := os.Stat(output)
					if err != nil {
						t.Fatal(err)
					}
					size = info.Size()
					t.Logf("attempt %d: %d bytes at %d kbps", attempt+1, size, job.Plan.VideoKbps)
					if size <= limit {
						break
					}
					job.Plan = plan.Scaled(0.85)
				}
				RemovePassLogs(job.PassLog)
				if size > limit {
					t.Fatalf("output %d bytes is over the %d byte limit", size, limit)
				}
				if leftovers, _ := filepath.Glob(job.PassLog + "*"); len(leftovers) > 0 {
					t.Errorf("pass logs left: %v", leftovers)
				}
				probeOut := run([]string{ffmpeg, "-hide_banner", "-i", output, "-f", "ffmetadata", "-"})
				if !strings.Contains(probeOut, "title=Real ffmpeg test") {
					t.Errorf("title tag lost:\n%s", probeOut)
				}
				if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
					dims := strings.TrimSpace(run([]string{ffprobe, "-v", "error", "-select_streams", "v:0",
						"-show_entries", "stream=width,height,r_frame_rate,codec_name", "-of", "csv=p=0", output}))
					t.Logf("output stream: %s", dims)
					want := map[string]string{"landscape": "h264,426,240,30/1", "portrait": "h264,240,426,30/1"}[shape.name]
					if dims != want {
						t.Errorf("stream %q, want %q", dims, want)
					}
				}
			})
		}
	}
}

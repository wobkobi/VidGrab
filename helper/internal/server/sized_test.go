package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vidgrab/internal/core"
)

// rawInfo decodes yt-dlp -J output the way core.FetchRawInfo does (numbers as json.Number).
func rawInfo(t *testing.T, text string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// A 30-second clip whose 720p streams (8 MB + 3 MB) are far over 1 MB but whose 480p H.264
// plus AAC (0.5 MB + 0.4 MB) fit 1 MB as they are, as tall as a 1 MB conversion would be.
const clipInfo = `{"id": "jNQXAC9IVRw", "title": "Clip", "duration": 30, "formats": [
	{"format_id": "140", "vcodec": "none", "acodec": "mp4a.40.2", "ext": "m4a", "filesize": 400000},
	{"format_id": "251", "vcodec": "none", "acodec": "opus", "ext": "webm", "filesize": 380000},
	{"format_id": "135", "vcodec": "avc1.4d401e", "acodec": "none", "ext": "mp4", "width": 854, "height": 480, "filesize": 500000},
	{"format_id": "136", "vcodec": "avc1.4d401f", "acodec": "none", "ext": "mp4", "width": 1280, "height": 720, "filesize": 8000000}
]}`

// The same clip without the small stream, so nothing fits 1 MB without converting.
const bigClipInfo = `{"id": "jNQXAC9IVRw", "title": "Clip", "duration": 30, "formats": [
	{"format_id": "140", "vcodec": "none", "acodec": "mp4a.40.2", "ext": "m4a", "filesize": 400000},
	{"format_id": "136", "vcodec": "avc1.4d401f", "acodec": "none", "ext": "mp4", "width": 1280, "height": 720, "filesize": 8000000}
]}`

// overLimit is a downloaded source size over the 1 MB most tests use.
const overLimit = 2_000_000

// hardwareQueue is sizedQueue for a 10-second clip at a 3 MB limit, whose 2184 kbps of video
// is enough for the hardware encoder to be used. The 4 MB download is over the limit and
// carries more than the plan, so CappedToSource leaves it alone.
func hardwareQueue(t *testing.T, sizes ...int) (*JobQueue, *fakeStarter, *sizedFakes) {
	t.Helper()
	info := strings.Replace(bigClipInfo, `"duration": 30`, `"duration": 10`, 1)
	q, starter, fakes := sizedQueue(t, info, "h264_nvenc", sizes...)
	fakes.set(func(f *sizedFakes) { f.downloadSize = 4_000_000 })
	return q, starter, fakes
}

// sizedFakes plays yt-dlp and ffmpeg for size-limited jobs: yt-dlp writes a downloadSize file
// at the name its -o template gives and reports codecs, and each final ffmpeg pass writes its
// output at the next size in sizes.
type sizedFakes struct {
	t            *testing.T
	dir          string
	mu           sync.Mutex
	sizes        []int
	encodes      int
	downloadSize int
	codecs       string
	// failEncoders makes every ffmpeg run with these encoders exit 1 with the given error line
	failEncoders map[string]string
	// blockFfmpeg makes the next ffmpeg start block until finished or killed
	blockFfmpeg bool
	infoCalls   int
	// reportPath, when set, is the path yt-dlp reports instead of the file it wrote
	reportPath string
}

func (f *sizedFakes) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infoCalls
}

func (f *sizedFakes) set(fn func(f *sizedFakes)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func isFfmpeg(argv []string) bool { return strings.HasPrefix(filepath.Base(argv[0]), "ffmpeg") }

func argAfter(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

func (f *sizedFakes) start(argv []string) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	if isFfmpeg(argv) {
		if line, ok := f.failEncoders[argAfter(argv, "-c:v")]; ok {
			return newFakeProc([]string{line}, 1, false)
		}
		if f.blockFfmpeg {
			f.blockFfmpeg = false
			return newFakeProc(nil, 0, true)
		}
		lines := []string{"out_time_us=15000000", "speed=3.1x", "progress=continue", "out_time_us=30000000", "progress=end"}
		if !slices.Contains(argv, "-pass") || argAfter(argv, "-pass") == "2" {
			size := f.sizes[min(f.encodes, len(f.sizes)-1)]
			f.encodes++
			if err := os.WriteFile(argv[len(argv)-1], make([]byte, size), 0o644); err != nil {
				f.t.Error(err)
			}
		}
		return newFakeProc(lines, 0, false)
	}
	// yt-dlp: resolve the -o template for the clip and write the file
	template := argAfter(argv, "-o")
	path := strings.NewReplacer("%(title)s", "Clip", "%(id)s", "jNQXAC9IVRw", "%(ext)s", "mp4").Replace(template)
	if err := os.WriteFile(path, make([]byte, f.downloadSize), 0o644); err != nil {
		f.t.Error(err)
	}
	reported := path
	if f.reportPath != "" {
		reported = f.reportPath
	}
	return newFakeProc([]string{"YTDLP_TITLE Clip", "YTDLP_FILE " + reported, "YTDLP_CODECS " + f.codecs}, 0, false)
}

func sizedQueue(t *testing.T, info string, encoder string, sizes ...int) (*JobQueue, *fakeStarter, *sizedFakes) {
	t.Helper()
	dir := t.TempDir()
	fakes := &sizedFakes{t: t, dir: dir, sizes: sizes, downloadSize: overLimit, codecs: "avc1.4d401f mp4a.40.2"}
	starter := &fakeStarter{byArgs: fakes.start}
	q := makeQueue(t, dir, starter, func(c *QueueConfig) {
		c.VideoInfo = func(context.Context, string) (map[string]any, error) {
			fakes.mu.Lock()
			fakes.infoCalls++
			fakes.mu.Unlock()
			return rawInfo(t, info), nil
		}
		c.Encoder = func(context.Context, string) string { return encoder }
	})
	return q, starter, fakes
}

func sizedOptions(mb int) core.JobOptions {
	opts := defaults()
	opts.MaxSizeMB = mb
	opts.Container = "mkv" // ignored: a size-limited video is always MP4
	return opts
}

func (s *fakeStarter) all() []*fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeProc(nil), s.procs...)
}

// clipPaths returns the final 1 MB name and the working source and encode of job id.
func clipPaths(dir, id string) (final, source, sized string) {
	tag := id[:8]
	return filepath.Join(dir, "Clip (1 MB).mp4"),
		filepath.Join(dir, "Clip [jNQXAC9IVRw]."+tag+".source.mp4"),
		filepath.Join(dir, "Clip [jNQXAC9IVRw]."+tag+".sized.mp4")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// dirNames lists dir, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSizedJobThatFitsDownloadsExactFormats(t *testing.T) {
	q, starter, fakes := sizedQueue(t, clipInfo, "libx264")
	fakes.set(func(f *sizedFakes) { f.downloadSize = 900_000 })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })

	procs := starter.all()
	if len(procs) != 1 || isFfmpeg(procs[0].args) {
		t.Fatalf("expected one yt-dlp run, got %d", len(procs))
	}
	args := procs[0].args
	if procs[0].argAfter("-f") != "135+140" || procs[0].argAfter("--merge-output-format") != "mp4" ||
		slices.Contains(args, "-S") || !slices.Contains(args, "--embed-metadata") || last(args) != watchURL(videos[0]) ||
		!strings.HasSuffix(procs[0].argAfter("-o"), "."+id[:8]+".source.%(ext)s") {
		t.Fatalf("args %v", args)
	}
	final, source, _ := clipPaths(fakes.dir, id)
	job := snapshot(t, q, id)
	if str(job.Filepath) != final || str(job.Filename) != "Clip (1 MB).mp4" || job.Stage != "" ||
		job.Percent != 100 || str(job.Title) != "Clip" {
		t.Fatalf("job %+v", job)
	}
	if exists(source) || fileSize(t, final) != 900_000 {
		t.Fatal("working file not moved to the final name")
	}
}

func TestSizedJobNeverReplacesExistingFiles(t *testing.T) {
	q, _, fakes := sizedQueue(t, clipInfo, "libx264")
	fakes.set(func(f *sizedFakes) { f.downloadSize = 900_000 })
	// A normal download of the same video and an earlier 1 MB one
	normal := filepath.Join(fakes.dir, "Clip.mp4")
	earlier := filepath.Join(fakes.dir, "Clip (1 MB).mp4")
	for _, path := range []string{normal, earlier} {
		if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	want := filepath.Join(fakes.dir, "Clip (1 MB) (2).mp4")
	if got := str(snapshot(t, q, id).Filepath); got != want {
		t.Fatalf("saved as %s", got)
	}
	if fileSize(t, normal) != 7 || fileSize(t, earlier) != 7 || fileSize(t, want) != 900_000 {
		t.Fatal("an existing file was replaced")
	}
}

func TestSizedJobOversizedFitIsConverted(t *testing.T) {
	// yt-dlp's sizes said 0.9 MB, but the merged file came out at 2 MB
	q, starter, fakes := sizedQueue(t, clipInfo, "libx264", 800_000)
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 3 || procs[0].argAfter("-f") != "135+140" || !isFfmpeg(procs[1].args) {
		t.Fatalf("expected the fit download then one two-pass encode, got %d processes", len(procs))
	}
	final, source, sized := clipPaths(fakes.dir, id)
	if fileSize(t, final) != 800_000 || exists(source) || exists(sized) {
		t.Fatal("conversion result not committed")
	}
}

func TestSizedJobFittingSourceIsNotReencoded(t *testing.T) {
	// Nothing fits by yt-dlp's numbers, but the H.264 + AAC source came out at 0.6 MB
	q, starter, fakes := sizedQueue(t, bigClipInfo, "h264_nvenc", 999_000)
	fakes.set(func(f *sizedFakes) { f.downloadSize = 600_000 })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if starter.count() != 1 {
		t.Fatalf("a fitting source was re-encoded (%d processes)", starter.count())
	}
	final, _, _ := clipPaths(fakes.dir, id)
	if fileSize(t, final) != 600_000 {
		t.Fatal("source not kept as it is")
	}
}

func TestSizedJobSmallVP9SourceIsConvertedAtItsOwnBitrate(t *testing.T) {
	// 0.4 MB over 30 s is 106 kbps in total, so video is capped at 106 - 64 = 42, then
	// raised to the 50 kbps floor, instead of the 189 kbps a 1 MB limit would allow
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 300_000)
	fakes.set(func(f *sizedFakes) { f.downloadSize, f.codecs = 400_000, "vp09.00.40.08 opus" })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 3 || procs[2].argAfter("-b:v") != "50k" {
		t.Fatalf("processes %d, encode %v", len(procs), procs[len(procs)-1].args)
	}
}

func TestSizedJobConvertsWhenFitIsShorterThanPlan(t *testing.T) {
	// At 5 MB over 30 s the plan is 1170 kbps video at 720p, but the only fit is 480p
	q, starter, fakes := sizedQueue(t, clipInfo, "libx264", 2_500_000)
	fakes.set(func(f *sizedFakes) { f.downloadSize = 6_000_000 })
	id := create(t, q, watchURL(videos[0]), sizedOptions(5))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 3 || procs[0].argAfter("-S") != "res:720,vcodec:h264,acodec:aac" || procs[0].argAfter("-f") != "bv*+ba/b" {
		t.Fatalf("expected a capped source download, got %v", procs[0].args)
	}
	if !exists(filepath.Join(fakes.dir, "Clip (5 MB).mp4")) {
		t.Fatal("no 5 MB file")
	}
}

func TestSizedJobReplansForMonoSource(t *testing.T) {
	// 1 MB over 30 s: 253 kbps total, planned as 64k stereo and 189 video. The download turns
	// out to have mono sound, so it stays mono at max(32, 64/2) = 32k and video gets 221
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 900_000)
	fakes.set(func(f *sizedFakes) { f.codecs = "avc1.4d401f mp4a.40.2 128 1" })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 3 || procs[1].argAfter("-b:v") != "221k" || procs[2].argAfter("-b:a") != "32k" || procs[2].argAfter("-ac") != "1" {
		t.Fatalf("processes %d, final pass %v", len(procs), procs[len(procs)-1].args)
	}
}

func TestSizedJobConvertsAndRetriesSmaller(t *testing.T) {
	// The first encode comes out at 1.2 MB, over the 1 MB limit; the retry takes the overshoot
	// against 0.97 MB off the video, (1.2e6 - 0.97e6) * 8 / 1000 / 30 = 61 kbps, and fits
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 1_200_000, 900_000)
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })

	procs := starter.all()
	if len(procs) != 5 {
		t.Fatalf("expected yt-dlp then two 2-pass encodes, got %d processes", len(procs))
	}
	// 1 MB over 30 s: 7,600,000 bits / 30 / 1000 = 253 kbps, 64 audio, 189 video, 480p 10 fps
	download := procs[0]
	if isFfmpeg(download.args) || download.argAfter("-S") != "res:480,vcodec:h264,acodec:aac" ||
		download.argAfter("--merge-output-format") != "mp4" || !strings.HasSuffix(download.argAfter("-o"), ".source.%(ext)s") {
		t.Fatalf("source download args %v", download.args)
	}
	wantRates := []string{"189k", "189k", "127k", "127k"}
	for i, p := range procs[1:] {
		if !isFfmpeg(p.args) || p.argAfter("-b:v") != wantRates[i] || p.argAfter("-c:v") != "libx264" {
			t.Fatalf("encode %d args %v", i, p.args)
		}
	}
	final, source, sized := clipPaths(fakes.dir, id)
	if fileSize(t, final) != 900_000 {
		t.Fatal("final file is not the retry")
	}
	if exists(source) || exists(sized) {
		t.Fatal("source or encode left behind")
	}
	job := snapshot(t, q, id)
	if str(job.Filepath) != final || str(job.Filename) != "Clip (1 MB).mp4" || job.Stage != "" || job.Percent != 100 {
		t.Fatalf("job %+v", job)
	}
}

func TestSizedJobHardwareEncodeIsOnePass(t *testing.T) {
	q, starter, _ := hardwareQueue(t, 800_000)
	id := create(t, q, watchURL(videos[0]), sizedOptions(3))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 2 || procs[1].argAfter("-c:v") != "h264_nvenc" || procs[1].argAfter("-rc") != "cbr" ||
		procs[1].argAfter("-b:v") != "2184k" || slices.Contains(procs[1].args, "-pass") {
		t.Fatalf("processes %d, encode args %v", len(procs), procs[len(procs)-1].args)
	}
}

func TestSizedJobLowBitrateSkipsHardware(t *testing.T) {
	// 189 kbps of video is under 1500, so libx264 runs without probing for a GPU encoder
	q, starter, _ := sizedQueue(t, bigClipInfo, "h264_nvenc", 800_000)
	var probes atomic.Int32
	q.encoder = func(context.Context, string) string {
		probes.Add(1)
		return "h264_nvenc"
	}
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 3 || procs[1].argAfter("-c:v") != "libx264" || procs[2].argAfter("-pass") != "2" || probes.Load() != 0 {
		t.Fatalf("processes %d, probes %d, encode args %v", len(procs), probes.Load(), procs[len(procs)-1].args)
	}
}

func TestSizedJobRetryUnderHardwareThresholdUsesX264(t *testing.T) {
	// 3 MB over 14 s: 22,800,000 bits / 14 / 1000 = 1628 kbps, 1532 video on NVENC. That comes
	// out at 3.3 MB, so the retry takes (3.3e6 - 2.91e6) * 8 / 1000 / 14 = 222 kbps off, and
	// 1309 is under 1500, so it runs on libx264
	q, starter, fakes := hardwareQueue(t, 3_300_000, 900_000)
	info := strings.Replace(bigClipInfo, `"duration": 30`, `"duration": 14`, 1)
	q.videoInfo = func(context.Context, string) (map[string]any, error) { return rawInfo(t, info), nil }
	id := create(t, q, watchURL(videos[0]), sizedOptions(3))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	if len(procs) != 4 || procs[1].argAfter("-c:v") != "h264_nvenc" || procs[1].argAfter("-b:v") != "1532k" ||
		procs[2].argAfter("-c:v") != "libx264" || procs[3].argAfter("-b:v") != "1309k" || procs[3].argAfter("-pass") != "2" {
		t.Fatalf("processes %d", len(procs))
	}
	if !exists(filepath.Join(fakes.dir, "Clip (3 MB).mp4")) {
		t.Fatal("no 3 MB file")
	}
}

func TestSizedJobFallsBackToX264WhenHardwareFails(t *testing.T) {
	q, starter, fakes := hardwareQueue(t, 800_000)
	fakes.set(func(f *sizedFakes) {
		f.failEncoders = map[string]string{"h264_nvenc": "[h264_nvenc @ 0x1] OpenEncodeSessionEx failed: out of memory"}
	})
	id := create(t, q, watchURL(videos[0]), sizedOptions(3))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	// yt-dlp, the failed NVENC encode, then libx264's two passes
	if len(procs) != 4 || procs[1].argAfter("-c:v") != "h264_nvenc" ||
		procs[2].argAfter("-c:v") != "libx264" || procs[3].argAfter("-pass") != "2" {
		t.Fatalf("processes %d", len(procs))
	}

	// The next job goes straight to libx264
	id = create(t, q, watchURL(videos[1]), sizedOptions(3))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	for _, p := range starter.all()[4:] {
		if isFfmpeg(p.args) && p.argAfter("-c:v") != "libx264" {
			t.Fatalf("failed encoder used again: %v", p.args)
		}
	}
}

func TestSizedJobWithoutAnyH264Encoder(t *testing.T) {
	q, _, fakes := hardwareQueue(t, 800_000)
	fakes.set(func(f *sizedFakes) {
		f.failEncoders = map[string]string{
			"h264_nvenc": "[h264_nvenc @ 0x1] Cannot load nvcuda.dll",
			"libx264":    "[vost#0:0 @ 0x2] Unknown encoder 'libx264'",
		}
	})
	id := create(t, q, watchURL(videos[0]), sizedOptions(3))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "This ffmpeg cannot encode H.264" || !strings.Contains(str(job.Detail), "Unknown encoder 'libx264'") {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
	_, source, sized := clipPaths(fakes.dir, id)
	if exists(source) || exists(sized) || job.Filepath != nil {
		t.Fatal("failed job left files or a file name")
	}
}

func TestSizedJobFailsWhenStillTooBig(t *testing.T) {
	q, _, fakes := sizedQueue(t, bigClipInfo, "libx264", 1_100_000, 1_050_000)
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "Could not fit 1 MB" || str(job.Detail) != "The converted video came out at 1.10 MB, then 1.05 MB, then 1.05 MB, over the 1 MB limit." {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
	if job.Filepath != nil || job.Filename != nil {
		t.Fatalf("failed job shows a file: %+v", job)
	}
	final, source, sized := clipPaths(fakes.dir, id)
	if exists(final) || exists(source) || exists(sized) {
		t.Fatal("files left after a failed fit")
	}

	// Retry reruns the whole job: formats again, download, convert
	fakes.set(func(f *sizedFakes) { f.sizes, f.encodes = []int{500_000}, 0 })
	if _, err := q.Retry(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if fakes.calls() != 2 || !exists(final) {
		t.Fatalf("retry did not rerun (info calls %d)", fakes.calls())
	}
}

func TestSizedJobFailureRemovesWorkingFiles(t *testing.T) {
	// yt-dlp reporting a path that does not exist (a mangled non-ASCII title) fails the job
	// after the download is on disk; the working file must not be left in the folder
	q, _, fakes := sizedQueue(t, bigClipInfo, "libx264", 800_000)
	fakes.set(func(f *sizedFakes) { f.reportPath = filepath.Join(f.dir, "Mangled.mp4") })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	if job := snapshot(t, q, id); str(job.Error) != "Could not find the downloaded video" {
		t.Fatalf("error %q", str(job.Error))
	}
	_, source, _ := clipPaths(fakes.dir, id)
	waitFor(t, func() bool { return !exists(source) })
}

func TestSizedJobKeepsEncodeWhenFinalRenameFails(t *testing.T) {
	q, _, fakes := sizedQueue(t, bigClipInfo, "libx264", 800_000)
	var mu sync.Mutex
	tries := 0
	q.rename = func(string, string) error {
		mu.Lock()
		defer mu.Unlock()
		tries++
		return errors.New("The process cannot access the file because it is being used by another process.")
	}
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	_, source, sized := clipPaths(fakes.dir, id)
	if str(job.Error) != "Could not save the converted video" ||
		!strings.Contains(str(job.Detail), "kept as "+filepath.Base(sized)) {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
	mu.Lock()
	defer mu.Unlock()
	if tries != 5 || fileSize(t, sized) != 800_000 || exists(source) {
		t.Fatalf("tries %d; the good encode must be kept and the source removed", tries)
	}
}

func TestSizedJobRetriesALockedRename(t *testing.T) {
	q, _, fakes := sizedQueue(t, bigClipInfo, "libx264", 800_000)
	var mu sync.Mutex
	tries := 0
	q.rename = func(from, to string) error {
		mu.Lock()
		defer mu.Unlock()
		if tries++; tries < 3 {
			return errors.New("locked")
		}
		return os.Rename(from, to)
	}
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	final, _, _ := clipPaths(fakes.dir, id)
	if fileSize(t, final) != 800_000 {
		t.Fatal("rename not retried")
	}
}

func TestSizedCommitSkippedForStoppedRun(t *testing.T) {
	q, _, fakes := sizedQueue(t, bigClipInfo, "libx264")
	j := newJob(watchURL(videos[0]), videos[0], sizedOptions(1), fakes.dir, "Clip", "", "")
	j.runID, j.stoppedRun = 1, 1
	from := filepath.Join(fakes.dir, "Clip [jNQXAC9IVRw]."+j.id[:8]+".sized.mp4")
	if err := os.WriteFile(from, []byte("encode"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fakes.dir, "Clip [jNQXAC9IVRw]")
	res, stopped := q.commit(j, 1, from, base)
	if !stopped || res.filepath != "" || !exists(from) || j.status == StatusComplete {
		t.Fatalf("stopped run committed: %+v %v", res, stopped)
	}
	// A live run commits and is complete before the lock is released
	j.stoppedRun = -1
	res, stopped = q.commit(j, 1, from, base)
	if stopped || res.errMsg != "" || j.status != StatusComplete || j.filename != "Clip (1 MB).mp4" {
		t.Fatalf("live commit %+v, job %s %s", res, j.status, j.filename)
	}
}

func TestSizedJobTooLong(t *testing.T) {
	long := strings.Replace(bigClipInfo, `"duration": 30`, `"duration": 3600`, 1)
	q, starter, _ := sizedQueue(t, long, "libx264")
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	// 7,600,000 bits / ((50 + 32) * 1000) = 92.7 s
	if str(job.Error) != "Too long to fit 1 MB" ||
		str(job.Detail) != "At 1 MB the longest video that fits is about 1:32; this one is 1:00:00." {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
	if starter.count() != 0 {
		t.Fatal("a too-long video should not be downloaded")
	}
}

func TestSizedJobInfoFailure(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, func(c *QueueConfig) {
		c.VideoInfo = func(context.Context, string) (map[string]any, error) {
			return nil, &core.YtDlpError{Message: "Video unavailable", Detail: "ERROR: Video unavailable"}
		}
	})
	id := create(t, q, watchURL(videos[0]), sizedOptions(10))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "Video unavailable" || str(job.Detail) != "ERROR: Video unavailable" || starter.count() != 0 {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}

	q = makeQueue(t, t.TempDir(), starter)
	id = create(t, q, watchURL(videos[1]), sizedOptions(10))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	if str(snapshot(t, q, id).Error) != "Size limits are not available" {
		t.Fatal("missing VideoInfo should fail the job")
	}
}

func TestSizedJobStopDuringInfoFetchIsImmediate(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			fetching := make(chan struct{}, 1)
			ended := make(chan error, 1)
			q := makeQueue(t, t.TempDir(), newBlockingStarter(), withConcurrency(1), func(c *QueueConfig) {
				c.VideoInfo = func(ctx context.Context, _ string) (map[string]any, error) {
					fetching <- struct{}{}
					<-ctx.Done()
					ended <- ctx.Err()
					return nil, ctx.Err()
				}
			})
			id := create(t, q, watchURL(videos[0]), sizedOptions(10))
			<-fetching
			var err error
			if action == "pause" {
				_, err = q.Pause(id)
			} else {
				_, err = q.Cancel(id)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-ended:
				if !errors.Is(got, context.Canceled) {
					t.Fatalf("fetch ended with %v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("the info fetch was not cancelled")
			}
			if !q.WaitIdle(time.Second) {
				t.Fatal("runner still holds the slot")
			}
			want := map[string]string{"pause": "paused", "cancel": "cancelled"}[action]
			if job := snapshot(t, q, id); job.Status != want || job.Error != nil {
				t.Fatalf("job %+v", job)
			}
		})
	}
}

func TestSizedJobFailsWhenFfmpegDisappears(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, func(c *QueueConfig) { c.Ffmpeg = func() bool { return false } })
	id := create(t, q, watchURL(videos[0]), sizedOptions(10))
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	if job := snapshot(t, q, id); str(job.Error) != "This download needs ffmpeg" || starter.count() != 0 {
		t.Fatalf("job %+v ran %d processes", job, starter.count())
	}
}

// startConverting runs a job up to a blocked ffmpeg pass with progress reported.
func startConverting(t *testing.T, q *JobQueue, starter *fakeStarter, fakes *sizedFakes) (string, *fakeProc) {
	t.Helper()
	fakes.set(func(f *sizedFakes) { f.blockFfmpeg = true })
	id := create(t, q, watchURL(videos[0]), sizedOptions(1))
	waitFor(t, func() bool { return starter.count() == 2 })
	ffmpeg := starter.proc(1)
	// 15 of 30 s in pass 1 of 2 = 25%; (60 - 15) media seconds left at 2x = 22 s
	for _, line := range []string{"out_time_us=15000000", "speed=2.0x", "progress=continue"} {
		ffmpeg.emit(line)
	}
	waitFor(t, func() bool { return snapshot(t, q, id).Percent == 25 })
	job := snapshot(t, q, id)
	if job.Status != "processing" || job.Stage != "converting" || str(job.Speed) != "2.0x" || str(job.Eta) != "0:22" {
		t.Fatalf("converting job %+v speed %q eta %q", job, str(job.Speed), str(job.Eta))
	}
	// The working file is not the result, so no file name is shown yet
	if job.Filename != nil || job.Filepath != nil {
		t.Fatalf("converting job shows file %q", str(job.Filename))
	}
	return id, ffmpeg
}

func TestSizedJobPauseDuringConversionRestartsFromSource(t *testing.T) {
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 700_000)
	id, ffmpeg := startConverting(t, q, starter, fakes)
	final, source, _ := clipPaths(fakes.dir, id)

	if view, err := q.Pause(id); err != nil || view.Status != "paused" || view.Stage != "" {
		t.Fatalf("pause: %v %+v", err, view)
	}
	if !ffmpeg.wasKilled() {
		t.Fatal("ffmpeg not killed")
	}
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if !exists(source) {
		t.Fatal("pause must keep the downloaded source")
	}

	if _, err := q.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	procs := starter.all()
	// yt-dlp once, the killed pass, then a fresh two-pass encode: no second download
	if len(procs) != 4 || !isFfmpeg(procs[2].args) || procs[2].argAfter("-pass") != "1" {
		t.Fatalf("processes after resume: %d", len(procs))
	}
	if fakes.calls() != 1 {
		t.Fatalf("info fetched %d times", fakes.calls())
	}
	if !exists(final) || exists(source) {
		t.Fatal("resume did not finish the conversion")
	}
}

func TestShutdownRemovesPausedSizedWork(t *testing.T) {
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 700_000)
	id, _ := startConverting(t, q, starter, fakes)
	_, source, _ := clipPaths(fakes.dir, id)
	if _, err := q.Pause(id); err != nil {
		t.Fatal(err)
	}
	if !q.WaitIdle(3*time.Second) || !exists(source) {
		t.Fatal("paused job should keep its source while the helper runs")
	}
	// Another video's normal download in the same folder is not touched
	other := filepath.Join(fakes.dir, "Other [dQw4w9WgXcQ].mp4")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	q.Shutdown()
	if exists(source) || !exists(other) {
		t.Fatalf("after shutdown: %v", dirNames(t, fakes.dir))
	}
}

func TestShutdownRemovesRunningSizedWork(t *testing.T) {
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 700_000)
	id, _ := startConverting(t, q, starter, fakes)
	_, source, _ := clipPaths(fakes.dir, id)
	q.Shutdown()
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if exists(source) {
		t.Fatalf("after shutdown: %v", dirNames(t, fakes.dir))
	}
}

func TestSizedJobCancelDuringConversionRemovesFiles(t *testing.T) {
	q, starter, fakes := sizedQueue(t, bigClipInfo, "libx264", 700_000)
	id, ffmpeg := startConverting(t, q, starter, fakes)
	_, source, sized := clipPaths(fakes.dir, id)
	// A half-written encode, as ffmpeg leaves it when killed
	if err := os.WriteFile(sized, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if view, err := q.Cancel(id); err != nil || view.Status != "cancelled" {
		t.Fatalf("cancel: %v %+v", err, view)
	}
	if !ffmpeg.wasKilled() {
		t.Fatal("ffmpeg not killed")
	}
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if exists(source) || exists(sized) {
		t.Fatal("cancel left the source or encode behind")
	}
	if status(t, q, id) != "cancelled" || starter.count() != 2 {
		t.Fatal("cancelled job ran on")
	}
}

func TestSizedOptionIgnoredForAudio(t *testing.T) {
	q, starter, fakes := sizedQueue(t, clipInfo, "libx264")
	opts := defaults()
	opts.Kind, opts.MaxSizeMB = "audio", 10
	id := create(t, q, watchURL(videos[0]), opts)
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if fakes.calls() != 0 || starter.proc(0).argAfter("--audio-format") != "mp3" {
		t.Fatalf("audio job took the size path: %v", starter.proc(0).args)
	}
}

func TestSizedOptionReachesPlaylistChildren(t *testing.T) {
	fetcher := func(string) (map[string]any, error) {
		return map[string]any{"title": "List", "entries": []any{
			map[string]any{"id": videos[0], "title": "A"}, map[string]any{"id": videos[1], "title": "B"},
		}}, nil
	}
	q := makeQueue(t, t.TempDir(), newBlockingStarter(), withConcurrency(0), func(c *QueueConfig) { c.FetchInfo = fetcher })
	jobs, err := q.Create("https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE", sizedOptions(25), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Options.MaxSizeMB != 25 {
			t.Fatalf("child options %+v", j.Options)
		}
	}
}

func TestSizedPaths(t *testing.T) {
	dir := filepath.Join("out", "List")
	cases := []struct{ source, base, sized string }{
		{"A [x].1a2b3c4d.source.mp4", "A [x]", "A [x].1a2b3c4d.sized.mp4"},
		{"A.1a2b3c4d.source.b [x].1a2b3c4d.source.webm", "A.1a2b3c4d.source.b [x]", "A.1a2b3c4d.source.b [x].1a2b3c4d.sized.mp4"},
		{"odd [x].mkv", "odd [x]", "odd [x].1a2b3c4d.sized.mp4"},
	}
	for _, c := range cases {
		source := filepath.Join(dir, c.source)
		if base, sized := workBase(source, "1a2b3c4d"), encodePath(source, "1a2b3c4d"); base != filepath.Join(dir, c.base) || sized != filepath.Join(dir, c.sized) {
			t.Errorf("%s: %s, %s", c.source, base, sized)
		}
	}
}

func TestPartialPatternCoversSizedFiles(t *testing.T) {
	for _, name := range []string{"A [x].1a2b3c4d.source.mp4", "A [x].1a2b3c4d.source.webm", "A [x].1a2b3c4d.sized.mp4",
		"A [x].1a2b3c4d.source.mp4.part", "A [x].1a2b3c4d.source.f136.mp4"} {
		if !partialRe.MatchString(name) {
			t.Errorf("%s not treated as partial", name)
		}
	}
	for _, name := range []string{"A [x].mp4", "A.mp4", "A (2).m4a", "A (20 MB).mp4", "A (20 MB) (2).mp4"} {
		if partialRe.MatchString(name) {
			t.Errorf("final file %s treated as partial", name)
		}
	}
}

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vidgrab/internal/core"
)

var videos = []string{"jNQXAC9IVRw", "dQw4w9WgXcQ", "9bZkp7q19f0", "kJQP7kiw5Fk"}

func watchURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

var nextPid atomic.Int64

// fakeProc stands in for a yt-dlp process: it yields scripted lines and, when blocking, runs
// until finish or kill.
type fakeProc struct {
	pid    int
	args   []string
	r      *io.PipeReader
	w      *io.PipeWriter
	mu     sync.Mutex
	killed bool
	code   int
	once   sync.Once
	done   chan struct{}
}

func newFakeProc(lines []string, code int, block bool) *fakeProc {
	r, w := io.Pipe()
	p := &fakeProc{pid: int(40000 + nextPid.Add(1)), r: r, w: w, code: code, done: make(chan struct{})}
	if len(lines) > 0 || !block {
		go func() {
			for _, line := range lines {
				if _, err := io.WriteString(w, line+"\n"); err != nil {
					return
				}
			}
			if !block {
				p.end(code, false)
			}
		}()
	}
	return p
}

func (p *fakeProc) Output() io.Reader { return p.r }
func (p *fakeProc) Pid() int          { return p.pid }

func (p *fakeProc) Wait() int {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

func (p *fakeProc) end(code int, killed bool) {
	p.once.Do(func() {
		p.mu.Lock()
		p.code = code
		p.killed = p.killed || killed
		p.mu.Unlock()
		_ = p.w.Close()
		close(p.done)
	})
}

// emit sends another output line; it returns once the runner has read it.
func (p *fakeProc) emit(line string) { _, _ = io.WriteString(p.w, line+"\n") }

// finish ends the output with the given exit code.
func (p *fakeProc) finish(code int) { p.end(code, false) }

// kill simulates the process tree being killed.
func (p *fakeProc) kill() {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.end(1, true)
}

func (p *fakeProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

func (p *fakeProc) argAfter(flag string) string {
	i := slices.Index(p.args, flag)
	if i < 0 || i+1 >= len(p.args) {
		return ""
	}
	return p.args[i+1]
}

// fakeStarter hands out fakeProcs made by a factory and records them.
type fakeStarter struct {
	mu      sync.Mutex
	factory func() *fakeProc
	// byArgs, when set, replaces factory and sees the command line (to fake ffmpeg output files)
	byArgs func(argv []string) *fakeProc
	procs  []*fakeProc
	err    error
}

func newBlockingStarter() *fakeStarter {
	return &fakeStarter{factory: func() *fakeProc { return newFakeProc(nil, 0, true) }}
}

func (s *fakeStarter) start(argv []string) (Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var p *fakeProc
	if s.byArgs != nil {
		p = s.byArgs(argv)
	} else {
		p = s.factory()
	}
	p.args = append([]string(nil), argv...)
	s.procs = append(s.procs, p)
	return p, nil
}

func (s *fakeStarter) setFactory(f func() *fakeProc) {
	s.mu.Lock()
	s.factory = f
	s.mu.Unlock()
}

func (s *fakeStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs)
}

func (s *fakeStarter) proc(i int) *fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[i]
}

// procFor returns the process started for url; runner goroutines started together may call
// Start in either order, so tests look processes up by URL rather than by position.
func (s *fakeStarter) procFor(t *testing.T, url string) *fakeProc {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.procs) - 1; i >= 0; i-- {
		if last(s.procs[i].args) == url {
			return s.procs[i]
		}
	}
	t.Fatalf("no process for %s", url)
	return nil
}

func fakeKill(p Process) { p.(*fakeProc).kill() }

// waitFor polls cond until it holds or fails the test after 3 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

type queueOpt func(*QueueConfig)

func withConcurrency(n int) queueOpt { return func(c *QueueConfig) { c.Concurrency = n } }

func makeQueue(t *testing.T, dir string, starter *fakeStarter, opts ...queueOpt) *JobQueue {
	t.Helper()
	cfg := QueueConfig{
		Concurrency:  2,
		Fragments:    4,
		DownloadsDir: func() string { return dir },
		Ffmpeg:       func() bool { return true },
		Tools:        func() core.Tools { return core.Tools{YtDlp: "yt-dlp"} },
		Start:        starter.start,
		Kill:         fakeKill,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	q := NewJobQueue(cfg)
	t.Cleanup(func() {
		q.Shutdown()
		q.WaitIdle(3 * time.Second)
	})
	return q
}

func create(t *testing.T, q *JobQueue, url string, opts core.JobOptions) string {
	t.Helper()
	jobs, err := q.Create(url, opts, nil)
	if err != nil {
		t.Fatalf("Create(%s): %v", url, err)
	}
	return jobs[0].ID
}

func status(t *testing.T, q *JobQueue, id string) string {
	t.Helper()
	view, err := q.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	return view.Status
}

func snapshot(t *testing.T, q *JobQueue, id string) JobView {
	t.Helper()
	view, err := q.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func defaults() core.JobOptions { return core.DefaultJobOptions() }

func TestConcurrencyLimitAndFIFOOrder(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter)
	var ids []string
	for _, v := range videos[:3] {
		ids = append(ids, create(t, q, watchURL(v), defaults()))
	}
	waitFor(t, func() bool { return starter.count() == 2 })
	got := []string{status(t, q, ids[0]), status(t, q, ids[1]), status(t, q, ids[2])}
	if !slices.Equal(got, []string{"downloading", "downloading", "queued"}) {
		t.Fatalf("statuses %v", got)
	}
	first, second := starter.procFor(t, watchURL(videos[0])), starter.procFor(t, watchURL(videos[1]))

	first.finish(0)
	waitFor(t, func() bool { return starter.count() == 3 })
	waitFor(t, func() bool { return status(t, q, ids[0]) == "complete" })
	if status(t, q, ids[2]) != "downloading" || last(starter.proc(2).args) != watchURL(videos[2]) {
		t.Fatal("third job did not take the free slot")
	}

	second.finish(0)
	starter.proc(2).finish(0)
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runners still alive")
	}
	if q.Busy() {
		t.Fatal("queue still busy")
	}
}

func last(args []string) string { return args[len(args)-1] }

func TestRaisingConcurrencyStartsWaitingJobs(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, withConcurrency(1))
	for _, v := range videos[:3] {
		create(t, q, watchURL(v), defaults())
	}
	waitFor(t, func() bool { return starter.count() == 1 })
	q.ApplySettings(3, 8, true)
	waitFor(t, func() bool { return starter.count() == 3 })
	third := starter.proc(2)
	if !slices.Contains(third.args, "--cookies-from-browser") {
		t.Fatal("cookies setting not applied")
	}
	if third.argAfter("--concurrent-fragments") != "8" {
		t.Fatalf("fragments = %q", third.argAfter("--concurrent-fragments"))
	}
}

func TestSuccessfulRunRecordsProgressTitleAndFile(t *testing.T) {
	dir := t.TempDir()
	merged := filepath.Join(dir, "Me at the zoo [jNQXAC9IVRw].mp4")
	if err := os.WriteFile(merged, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "Me at the zoo.mp4")
	lines := []string{
		"YTDLP_TITLE Me at the zoo",
		"[download] Destination: " + filepath.Join(dir, "Me at the zoo [jNQXAC9IVRw].f134.mp4"),
		`YTDLP_PROGRESS {"status": "downloading", "_percent": 40.0, "speed": 1048576, "eta": 9}`,
		`[Merger] Merging formats into "` + merged + `"`,
		"YTDLP_FILE " + merged,
	}
	starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc(lines, 0, false) }}
	q := makeQueue(t, dir, starter)
	id := create(t, q, watchURL(videos[0]), defaults())

	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	job := snapshot(t, q, id)
	if str(job.Title) != "Me at the zoo" || job.Percent != 100 {
		t.Fatalf("title %q percent %v", str(job.Title), job.Percent)
	}
	if str(job.Filepath) != final || str(job.Filename) != "Me at the zoo.mp4" {
		t.Fatalf("file %q / %q", str(job.Filepath), str(job.Filename))
	}
	if !exists(final) || exists(merged) {
		t.Fatalf("files %v", listDir(t, dir))
	}
	if job.Error != nil || job.Speed != nil {
		t.Fatalf("error %q speed %q", str(job.Error), str(job.Speed))
	}
	// The command comes from core.BuildArgs and runs the configured yt-dlp
	args := starter.proc(0).args
	if args[0] != "yt-dlp" || !slices.Contains(args, "--newline") {
		t.Fatalf("unexpected argv %v", args)
	}
}

func TestProcessingStateWhileMerging(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })
	starter.proc(0).emit(`[Merger] Merging formats into "x [jNQXAC9IVRw].mp4"`)
	waitFor(t, func() bool { return status(t, q, id) == "processing" })
	starter.proc(0).finish(0)
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
}

func TestFailedRunRecordsErrorAndDetail(t *testing.T) {
	lines := []string{"[youtube] Extracting URL", "ERROR: [youtube] jNQXAC9IVRw: Video unavailable"}
	starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc(lines, 1, false) }}
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "Video unavailable" || !strings.Contains(str(job.Detail), "Extracting URL") {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
}

func TestFailedRunWithoutErrorLineReportsExitCode(t *testing.T) {
	starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc(nil, 3, false) }}
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "yt-dlp exited with code 3" || job.Detail != nil {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
}

func TestStartFailureBecomesError(t *testing.T) {
	starter := newBlockingStarter()
	starter.err = errors.New("yt-dlp missing")
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	job := snapshot(t, q, id)
	if str(job.Error) != "Could not start yt-dlp" || str(job.Detail) != "yt-dlp missing" {
		t.Fatalf("error %q detail %q", str(job.Error), str(job.Detail))
	}
}

func TestPauseResumeKeepsContinue(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })
	starter.proc(0).emit(`YTDLP_PROGRESS {"status": "downloading", "_percent": 50.0}`)
	// Video with ffmpeg expects two streams, so the first stream's 50% is 45% overall
	waitFor(t, func() bool { return snapshot(t, q, id).Percent == 45 })

	paused, err := q.Pause(id)
	if err != nil || paused.Status != "paused" {
		t.Fatalf("pause: %v %s", err, paused.Status)
	}
	if !starter.proc(0).wasKilled() {
		t.Fatal("process not killed")
	}
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if status(t, q, id) != "paused" || snapshot(t, q, id).Percent != 45 {
		t.Fatal("pause lost status or progress")
	}

	resumed, err := q.Resume(id)
	if err != nil || resumed.Status != "queued" {
		t.Fatalf("resume: %v %s", err, resumed.Status)
	}
	waitFor(t, func() bool { return starter.count() == 2 })
	if status(t, q, id) != "downloading" || !slices.Contains(starter.proc(1).args, "--continue") {
		t.Fatal("resumed run missing --continue")
	}
	starter.proc(1).finish(0)
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
}

func TestPauseQueuedJobAndFreesSlot(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, withConcurrency(1))
	first := create(t, q, watchURL(videos[0]), defaults())
	second := create(t, q, watchURL(videos[1]), defaults())
	third := create(t, q, watchURL(videos[2]), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })

	if view, err := q.Pause(second); err != nil || view.Status != "paused" {
		t.Fatalf("pause queued: %v", err)
	}
	if _, err := q.Pause(first); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return starter.count() == 2 })
	if last(starter.proc(1).args) != watchURL(videos[2]) || status(t, q, third) != "downloading" {
		t.Fatal("freed slot did not go to the next queued job")
	}
}

func TestResumeWaitsForOldProcessToExit(t *testing.T) {
	// A resumed job must not start while the paused process still holds the .part file
	starter := newBlockingStarter()
	var mu sync.Mutex
	var killed []Process
	q := makeQueue(t, t.TempDir(), starter, func(c *QueueConfig) {
		// Record the kill but let the process linger
		c.Kill = func(p Process) {
			mu.Lock()
			killed = append(killed, p)
			mu.Unlock()
		}
	})
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })
	if _, err := q.Pause(id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Resume(id); err != nil {
		t.Fatal(err)
	}
	if status(t, q, id) != "queued" || starter.count() != 1 {
		t.Fatal("resumed job started while the old process was alive")
	}
	mu.Lock()
	lingering := killed[0].(*fakeProc)
	mu.Unlock()
	lingering.kill()
	waitFor(t, func() bool { return starter.count() == 2 })
	if status(t, q, id) != "downloading" {
		t.Fatal("resumed job did not start")
	}
	// This queue's Kill only records, so end the second run here rather than in cleanup
	starter.proc(1).kill()
}

func TestPauseDuringStartKillsTheNewProcess(t *testing.T) {
	// A pause that lands between dispatch and the process starting has nothing to kill yet;
	// the runner must kill the process as soon as it exists
	release := make(chan struct{})
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, func(c *QueueConfig) {
		c.Start = func(argv []string) (Process, error) {
			<-release
			return starter.start(argv)
		}
	})
	id := create(t, q, watchURL(videos[0]), defaults())
	if view, err := q.Pause(id); err != nil || view.Status != "paused" {
		t.Fatalf("pause: %v", err)
	}
	close(release)
	waitFor(t, func() bool { return starter.count() == 1 && starter.proc(0).wasKilled() })
	if !q.WaitIdle(3*time.Second) || status(t, q, id) != "paused" {
		t.Fatal("job did not stay paused")
	}
}

func TestCancelRunningJobRemovesPartials(t *testing.T) {
	dir := t.TempDir()
	vid := videos[0]
	partials := []string{
		"T [" + vid + "].f134.mp4.part", "T [" + vid + "].f134.mp4", "T [" + vid + "].mp4.ytdl",
		"T [" + vid + "].f140.m4a.part-Frag3",
	}
	keep := []string{"Other [" + videos[1] + "].mp4.part", "Done [" + vid + "].mp3"}
	for _, name := range append(append([]string(nil), partials...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	starter := newBlockingStarter()
	q := makeQueue(t, dir, starter)
	id := create(t, q, watchURL(vid), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })
	if view, err := q.Cancel(id); err != nil || view.Status != "cancelled" {
		t.Fatalf("cancel: %v", err)
	}
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if status(t, q, id) != "cancelled" {
		t.Fatal("status changed after cancel")
	}
	if got := listDir(t, dir); !slices.Equal(got, sorted(keep)) {
		t.Fatalf("remaining files %v, want %v", got, sorted(keep))
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func sorted(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func TestCancelQueuedAndPausedJobs(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, withConcurrency(1))
	first := create(t, q, watchURL(videos[0]), defaults())
	second := create(t, q, watchURL(videos[1]), defaults())
	waitFor(t, func() bool { return starter.count() == 1 })
	if view, err := q.Cancel(second); err != nil || view.Status != "cancelled" {
		t.Fatalf("cancel queued: %v", err)
	}
	if _, err := q.Pause(first); err != nil {
		t.Fatal(err)
	}
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	if view, err := q.Cancel(first); err != nil || view.Status != "cancelled" {
		t.Fatalf("cancel paused: %v", err)
	}
	if starter.count() != 1 {
		t.Fatalf("%d processes started", starter.count())
	}
}

func TestRetryFailedJob(t *testing.T) {
	var runs atomic.Int32
	starter := &fakeStarter{factory: func() *fakeProc {
		code := 0
		if runs.Add(1) == 1 {
			code = 1
		}
		return newFakeProc([]string{"ERROR: boom"}, code, false)
	}}
	q := makeQueue(t, t.TempDir(), starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "error" })
	retried, err := q.Retry(id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Error != nil || retried.Percent != 0 {
		t.Fatalf("retry kept error %q percent %v", str(retried.Error), retried.Percent)
	}
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if starter.count() != 2 {
		t.Fatalf("%d runs", starter.count())
	}
}

func TestInvalidTransitionsReturn409(t *testing.T) {
	cases := []struct{ action, state string }{
		{"resume", "queued"}, {"retry", "queued"}, {"pause", "complete"}, {"cancel", "complete"}, {"retry", "complete"},
	}
	for _, tc := range cases {
		t.Run(tc.action+"_"+tc.state, func(t *testing.T) {
			starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc(nil, 0, tc.state == "queued") }}
			concurrency := 0
			if tc.state == "complete" {
				concurrency = 1
			}
			q := makeQueue(t, t.TempDir(), starter, withConcurrency(concurrency))
			id := create(t, q, watchURL(videos[0]), defaults())
			if tc.state == "complete" {
				waitFor(t, func() bool { return status(t, q, id) == "complete" })
			}
			_, err := actionByName(q, tc.action)(id)
			var jobErr *JobError
			if !errors.As(err, &jobErr) || jobErr.Status != 409 {
				t.Fatalf("got %v, want 409", err)
			}
		})
	}
}

func actionByName(q *JobQueue, name string) func(string) (JobView, error) {
	return map[string]func(string) (JobView, error){
		"pause": q.Pause, "resume": q.Resume, "cancel": q.Cancel, "retry": q.Retry, "snapshot": q.Snapshot,
		"finished_file": func(id string) (JobView, error) {
			_, err := q.FinishedFile(id)
			return JobView{}, err
		},
	}[name]
}

func TestUnknownJobReturns404(t *testing.T) {
	q := makeQueue(t, t.TempDir(), newBlockingStarter())
	for _, action := range []string{"pause", "resume", "cancel", "retry", "finished_file", "snapshot"} {
		_, err := actionByName(q, action)("nope")
		var jobErr *JobError
		if !errors.As(err, &jobErr) || jobErr.Status != 404 || jobErr.Message != "Download not found" {
			t.Fatalf("%s: got %v, want 404", action, err)
		}
	}
}

// audioFixStarter fakes a video download that reports acodec, then an ffmpeg that writes
// "fixed" to its output.
func audioFixStarter(t *testing.T, path, acodec string) *fakeStarter {
	return &fakeStarter{byArgs: func(argv []string) *fakeProc {
		if strings.HasPrefix(filepath.Base(argv[0]), "ffmpeg") {
			if err := os.WriteFile(argv[len(argv)-1], []byte("fixed"), 0o600); err != nil {
				t.Error(err)
			}
			return newFakeProc(nil, 0, false)
		}
		if err := os.WriteFile(path, []byte("opus"), 0o600); err != nil {
			t.Error(err)
		}
		return newFakeProc([]string{"YTDLP_FILE " + path, "YTDLP_CODECS vp09.00.40.08 " + acodec + " 130 2"}, 0, false)
	}}
}

func TestOpusInMP4IsReencodedToAAC(t *testing.T) {
	dir := t.TempDir()
	starter := audioFixStarter(t, filepath.Join(dir, "a [jNQXAC9IVRw].mp4"), "opus")
	q := makeQueue(t, dir, starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if !slices.Contains(starter.procs[0].args, "after_move:YTDLP_CODECS %(vcodec)s %(acodec)s %(abr)s %(audio_channels)s") {
		t.Fatalf("no codecs print in %v", starter.procs[0].args)
	}
	if starter.count() != 2 || starter.procs[1].argAfter("-c:a") != "aac" || starter.procs[1].argAfter("-b:a") != "160k" ||
		starter.procs[1].argAfter("-c") != "copy" {
		t.Fatalf("processes %d, last %v", starter.count(), starter.procs[starter.count()-1].args)
	}
	data, err := os.ReadFile(str(snapshot(t, q, id).Filepath))
	if err != nil || string(data) != "fixed" {
		t.Fatalf("final file %q, %v", data, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.audio.mp4")); len(leftovers) > 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestAACInMP4IsKept(t *testing.T) {
	dir := t.TempDir()
	starter := audioFixStarter(t, filepath.Join(dir, "a [jNQXAC9IVRw].mp4"), "mp4a.40.2")
	q := makeQueue(t, dir, starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if starter.count() != 1 {
		t.Fatalf("%d processes, want only yt-dlp", starter.count())
	}
}

func TestFinishedFileRequiresExistingFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "a [jNQXAC9IVRw].mp4")
	starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc([]string{"YTDLP_FILE " + final}, 0, false) }}
	q := makeQueue(t, dir, starter)
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	_, err := q.FinishedFile(id)
	var jobErr *JobError
	if !errors.As(err, &jobErr) || jobErr.Status != 409 || jobErr.Message != "The file was moved or deleted" {
		t.Fatalf("got %v, want 409", err)
	}
	if err := os.WriteFile(final, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := q.FinishedFile(id); err != nil || path != final {
		t.Fatalf("got %q, %v", path, err)
	}
}

func TestClearFinishedKeepsActive(t *testing.T) {
	starter := &fakeStarter{factory: func() *fakeProc { return newFakeProc(nil, 0, false) }}
	q := makeQueue(t, t.TempDir(), starter, withConcurrency(1))
	done := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, done) == "complete" })
	q.SetConcurrency(0)
	waiting := create(t, q, watchURL(videos[1]), defaults())
	if removed := q.ClearFinished(false); removed != 1 {
		t.Fatalf("removed %d", removed)
	}
	listed := q.List()
	if len(listed) != 1 || listed[0].ID != waiting {
		t.Fatalf("remaining %v", listed)
	}
}

func TestPruneFinishedKeepsNewestAndActive(t *testing.T) {
	q := NewJobQueue(QueueConfig{})
	add := func(id, status string, running bool) {
		q.jobs[id] = &job{id: id, status: status, running: running}
		q.order = append(q.order, id)
	}
	add("queued", StatusQueued, false)
	add("exiting", StatusCancelled, true)
	for i := 0; i < MaxFinishedJobs+3; i++ {
		add(fmt.Sprintf("done-%d", i), StatusComplete, false)
	}
	q.pruneFinishedLocked()
	if len(q.order) != len(q.jobs) {
		t.Fatalf("order %d, jobs %d", len(q.order), len(q.jobs))
	}
	for _, gone := range []string{"done-0", "done-1", "done-2"} {
		if _, ok := q.jobs[gone]; ok {
			t.Errorf("%s should be pruned", gone)
		}
	}
	for _, kept := range []string{"queued", "exiting", "done-3", fmt.Sprintf("done-%d", MaxFinishedJobs+2)} {
		if _, ok := q.jobs[kept]; !ok {
			t.Errorf("%s should be kept", kept)
		}
	}
	if q.order[0] != "queued" || q.order[1] != "exiting" || q.order[2] != "done-3" {
		t.Errorf("order %v", q.order[:3])
	}
}

func TestPruneFinishedKeepsWholeGroups(t *testing.T) {
	q := NewJobQueue(QueueConfig{})
	add := func(id, group, status string) {
		q.jobs[id] = &job{id: id, groupID: group, status: status}
		q.order = append(q.order, id)
	}
	// Oldest first: a finished group, a group still downloading, a cancelled 1000-item group,
	// a group with one failure, then enough single videos to go over the limit
	for i := 0; i < 10; i++ {
		add(fmt.Sprintf("old-%d", i), "old", StatusComplete)
	}
	add("busy-0", "busy", StatusComplete)
	add("busy-1", "busy", StatusQueued)
	for i := 0; i < MaxGroupItems; i++ {
		add(fmt.Sprintf("big-%d", i), "big", StatusCancelled)
	}
	add("mixed-0", "mixed", StatusComplete)
	add("mixed-1", "mixed", StatusError)
	for i := 0; i < MaxFinishedJobs; i++ {
		add(fmt.Sprintf("one-%d", i), "", StatusComplete)
	}
	q.pruneFinishedLocked()
	if len(q.order) != len(q.jobs) {
		t.Fatalf("order %d, jobs %d", len(q.order), len(q.jobs))
	}
	// 510 complete jobs counted: the old group (10) goes in full, and nothing else
	for _, gone := range []string{"old-0", "old-9"} {
		if _, ok := q.jobs[gone]; ok {
			t.Errorf("%s should be pruned", gone)
		}
	}
	for _, kept := range []string{"busy-0", "busy-1", "big-0", fmt.Sprintf("big-%d", MaxGroupItems-1), "mixed-0", "mixed-1", "one-0"} {
		if _, ok := q.jobs[kept]; !ok {
			t.Errorf("%s should be kept", kept)
		}
	}
	if want := 2 + MaxGroupItems + 2 + MaxFinishedJobs; len(q.jobs) != want {
		t.Errorf("%d jobs kept, want %d", len(q.jobs), want)
	}

	// One more success pushes out the oldest single video, never the cancelled group
	add("one-new", "", StatusComplete)
	q.pruneFinishedLocked()
	if _, ok := q.jobs["one-0"]; ok {
		t.Error("one-0 should be pruned")
	}
	if _, ok := q.jobs["big-0"]; !ok {
		t.Error("the cancelled group should be kept")
	}
}

func TestPruneHardCapForgetsOldFailures(t *testing.T) {
	q := NewJobQueue(QueueConfig{})
	add := func(id, group, status string) {
		q.jobs[id] = &job{id: id, groupID: group, status: status}
		q.order = append(q.order, id)
	}
	// Six cancelled 1000-video groups, oldest first, plus one queued video: 6001 jobs
	for g := 0; g < 6; g++ {
		for i := 0; i < MaxGroupItems; i++ {
			add(fmt.Sprintf("g%d-%d", g, i), fmt.Sprint("g", g), StatusCancelled)
		}
	}
	add("waiting", "", StatusQueued)
	q.pruneFinishedLocked()
	if len(q.order) != len(q.jobs) || len(q.jobs) != 4*MaxGroupItems+1 {
		t.Fatalf("order %d, jobs %d", len(q.order), len(q.jobs))
	}
	for _, gone := range []string{"g0-0", "g1-999"} {
		if _, ok := q.jobs[gone]; ok {
			t.Errorf("%s should be pruned", gone)
		}
	}
	for _, kept := range []string{"g2-0", "g5-999", "waiting"} {
		if _, ok := q.jobs[kept]; !ok {
			t.Errorf("%s should be kept", kept)
		}
	}
}

func TestClearFinishedWholeDownloads(t *testing.T) {
	q := NewJobQueue(QueueConfig{})
	add := func(id, group, status string) {
		q.jobs[id] = &job{id: id, groupID: group, status: status}
		q.order = append(q.order, id)
	}
	add("done", "", StatusComplete)
	add("stopped", "", StatusCancelled)
	add("broke", "", StatusError)
	// A group with a failure, and a group still downloading
	add("mixed-0", "mixed", StatusComplete)
	add("mixed-1", "mixed", StatusError)
	add("busy-0", "busy", StatusComplete)
	add("busy-1", "busy", StatusDownloading)

	if removed := q.ClearFinished(false); removed != 2 {
		t.Fatalf("removed %d, want done and stopped", removed)
	}
	if !slices.Equal(q.order, []string{"broke", "mixed-0", "mixed-1", "busy-0", "busy-1"}) {
		t.Fatalf("order %v", q.order)
	}
	if removed := q.ClearFinished(true); removed != 3 {
		t.Fatalf("removed %d, want broke and the mixed group", removed)
	}
	if !slices.Equal(q.order, []string{"busy-0", "busy-1"}) || len(q.jobs) != 2 {
		t.Fatalf("order %v", q.order)
	}
}

func TestPlaylistExpansionWithRange(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	fetcher := func(canonical string, _ []core.ItemRange) (map[string]any, error) {
		calls = append(calls, canonical)
		var entries []any
		for i, v := range videos {
			entries = append(entries, map[string]any{"id": v, "title": fmt.Sprintf("Video %d", i+1)})
		}
		entries = append(entries, map[string]any{"id": "bad id!", "title": "skip"})
		return map[string]any{"title": `My: "List"?`, "entries": entries}, nil
	}
	starter := newBlockingStarter()
	q := makeQueue(t, dir, starter, withConcurrency(0), func(c *QueueConfig) { c.FetchInfo = fetcher })
	audio := defaults()
	audio.Kind = "audio"
	playlist := "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"
	jobs, err := q.Create(playlist, audio, []core.ItemRange{{Start: 2, End: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{playlist}) {
		t.Fatalf("fetch calls %v", calls)
	}
	if len(jobs) != 2 || jobs[0].URL != watchURL(videos[1]) || jobs[1].URL != watchURL(videos[2]) {
		t.Fatalf("jobs %+v", jobs)
	}
	if str(jobs[0].Title) != "Video 2" || str(jobs[1].Title) != "Video 3" {
		t.Fatal("entry titles not kept")
	}
	if jobs[0].GroupID == nil || *jobs[0].GroupID == "" || str(jobs[0].GroupID) != str(jobs[1].GroupID) {
		t.Fatal("jobs do not share a group id")
	}
	for _, j := range jobs {
		if str(j.GroupTitle) != `My: "List"?` || j.Kind != "audio" {
			t.Fatalf("group title %q kind %q", str(j.GroupTitle), j.Kind)
		}
	}

	q.SetConcurrency(1)
	waitFor(t, func() bool { return starter.count() == 1 })
	out := starter.proc(0).argAfter("-o")
	if !strings.HasPrefix(out, filepath.Join(dir, "My_ _List__")) {
		t.Fatalf("output template %q", out)
	}
}

func TestSameVideoInSameFolderRunsOneAtATime(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter, withConcurrency(3))
	first := create(t, q, watchURL(videos[0]), defaults())
	second := create(t, q, watchURL(videos[0]), defaults())
	other := create(t, q, watchURL(videos[1]), defaults())

	waitFor(t, func() bool { return starter.count() == 2 })
	got := []string{status(t, q, first), status(t, q, second), status(t, q, other)}
	if !slices.Equal(got, []string{"downloading", "queued", "downloading"}) {
		t.Fatalf("statuses %v", got)
	}
	starter.procFor(t, watchURL(videos[0])).finish(0)
	waitFor(t, func() bool { return starter.count() == 3 })
	if status(t, q, second) != "downloading" || last(starter.proc(2).args) != watchURL(videos[0]) {
		t.Fatal("duplicate did not start after the first run exited")
	}
}

func TestChannelFolderDropsTabSuffix(t *testing.T) {
	dir := t.TempDir()
	fetcher := func(string, []core.ItemRange) (map[string]any, error) {
		return map[string]any{"title": "Jawed - Videos", "entries": []any{map[string]any{"id": videos[0], "title": "a"}}}, nil
	}
	starter := newBlockingStarter()
	q := makeQueue(t, dir, starter, withConcurrency(1), func(c *QueueConfig) { c.FetchInfo = fetcher })
	jobs, err := q.Create("https://www.youtube.com/@jawed", defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if str(jobs[0].GroupTitle) != "Jawed" || str(jobs[0].Channel) != "Jawed" {
		t.Fatalf("group title %q, channel %q", str(jobs[0].GroupTitle), str(jobs[0].Channel))
	}
	waitFor(t, func() bool { return starter.count() == 1 })
	out := starter.proc(0).argAfter("-o")
	if !strings.HasPrefix(out, filepath.Join(dir, "Jawed")) || strings.Contains(out, "Videos") {
		t.Fatalf("output template %q", out)
	}
}

func TestPlaylistEntriesKeepTheirChannel(t *testing.T) {
	fetcher := func(string, []core.ItemRange) (map[string]any, error) {
		return map[string]any{"title": "Mix", "entries": []any{
			map[string]any{"id": videos[0], "title": "a", "channel": "First"},
			map[string]any{"id": videos[1], "title": "b", "uploader": "second"},
			map[string]any{"id": videos[2], "title": "c"},
		}}, nil
	}
	q := makeQueue(t, t.TempDir(), newBlockingStarter(), func(c *QueueConfig) { c.FetchInfo = fetcher })
	jobs, err := q.Create("https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE", defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{str(jobs[0].Channel), str(jobs[1].Channel), str(jobs[2].Channel)}
	if !slices.Equal(got, []string{"First", "second", "<nil>"}) {
		t.Fatalf("channels %q", got)
	}
}

func TestPlaylistEmptyRangeAndCap(t *testing.T) {
	dir := t.TempDir()
	few := func(string, []core.ItemRange) (map[string]any, error) {
		return map[string]any{"title": "t", "entries": []any{map[string]any{"id": videos[0], "title": "a"}}}, nil
	}
	q := makeQueue(t, dir, newBlockingStarter(), func(c *QueueConfig) { c.FetchInfo = few })
	_, err := q.Create("https://www.youtube.com/@jawed", defaults(), []core.ItemRange{{Start: 5, End: 9}})
	var jobErr *JobError
	if !errors.As(err, &jobErr) || jobErr.Status != 400 || jobErr.Message != "No videos in that range" {
		t.Fatalf("got %v", err)
	}

	many := func(string, []core.ItemRange) (map[string]any, error) {
		var entries []any
		for i := 0; i < 1001; i++ {
			entries = append(entries, map[string]any{"id": fmt.Sprintf("%011d", i), "title": fmt.Sprint(i)})
		}
		return map[string]any{"title": "t", "entries": entries}, nil
	}
	q = makeQueue(t, dir, newBlockingStarter(), func(c *QueueConfig) { c.FetchInfo = many })
	jobs, next, err := q.CreateCapped("https://www.youtube.com/@jawed", defaults(), nil)
	if err != nil || next != MaxGroupItems+1 || len(jobs) != MaxGroupItems || str(jobs[MaxGroupItems-1].Title) != "999" {
		t.Fatalf("got %d jobs, next %d, %v", len(jobs), next, err)
	}
}

// limitedListing answers like yt-dlp --flat-playlist --playlist-items on a list of total
// videos: only the entries in items, with their positions in requested_entries.
func limitedListing(total int, items []core.ItemRange) map[string]any {
	entries, positions := []any{}, []any{}
	for pos := 1; pos <= total; pos++ {
		if core.InRanges(pos, items) {
			entries = append(entries, map[string]any{"id": fmt.Sprintf("v%010d", pos), "title": fmt.Sprint(pos)})
			positions = append(positions, json.Number(fmt.Sprint(pos)))
		}
	}
	return map[string]any{"title": "Linus Tech Tips - Videos", "entries": entries, "requested_entries": positions}
}

func TestPlaylistListsOnlyTheItemsInRange(t *testing.T) {
	var asked [][]core.ItemRange
	fetcher := func(_ string, items []core.ItemRange) (map[string]any, error) {
		asked = append(asked, items)
		return limitedListing(5000, items), nil
	}
	q := makeQueue(t, t.TempDir(), newBlockingStarter(), withConcurrency(0), func(c *QueueConfig) { c.FetchInfo = fetcher })
	channel := "https://www.youtube.com/@LinusTechTips"
	ranges := []core.ItemRange{{Start: 1, End: 4}, {Start: 4990, End: 6000}}
	jobs, err := q.Create(channel, defaults(), ranges)
	if err != nil {
		t.Fatal(err)
	}
	// Cut to the cap plus one: 4 + 997 positions
	if len(asked) != 1 || core.FormatItems(asked[0]) != "1-4,4990-5986" {
		t.Fatalf("listing asked for %v", asked)
	}
	var titles []string
	for _, j := range jobs {
		titles = append(titles, str(j.Title))
	}
	want := []string{"1", "2", "3", "4", "4990", "4991", "4992", "4993", "4994", "4995", "4996", "4997", "4998", "4999", "5000"}
	if !slices.Equal(titles, want) {
		t.Fatalf("titles %v", titles)
	}
	if str(jobs[0].GroupTitle) != "Linus Tech Tips" || jobs[0].URL != watchURL("v0000000001") {
		t.Fatalf("job %+v", jobs[0])
	}

	// Every item, or a range far past the cap, lists only one entry past it and queues the
	// first 1000
	asked = nil
	for _, r := range [][]core.ItemRange{nil, {{Start: 1, End: 100000}}} {
		capJobs, next, err := q.CreateCapped(channel, defaults(), r)
		if err != nil || next != 1001 || len(capJobs) != MaxGroupItems || str(capJobs[0].Title) != "1" || str(capJobs[MaxGroupItems-1].Title) != "1000" {
			t.Fatalf("ranges %v: %d jobs, next %d, %v", r, len(capJobs), next, err)
		}
	}
	if len(asked) != 2 || core.FormatItems(asked[0]) != "1-1001" || core.FormatItems(asked[1]) != "1-1001" {
		t.Fatalf("listing asked for %v", asked)
	}

	// A selection starting part-way names the real next position, not 1001
	capJobs, next, err := q.CreateCapped(channel, defaults(), []core.ItemRange{{Start: 500, End: 2000}})
	if err != nil || next != 1500 || len(capJobs) != MaxGroupItems || str(capJobs[MaxGroupItems-1].Title) != "1499" {
		t.Fatalf("500-2000: %d jobs, next %d, %v", len(capJobs), next, err)
	}

	var jobErr *JobError
	_, err = q.Create(channel, defaults(), []core.ItemRange{{Start: 6000, End: 6010}})
	if !errors.As(err, &jobErr) || jobErr.Message != "No videos in that range" {
		t.Fatalf("past the end: %v", err)
	}
	if len(q.List()) != len(want)+3*MaxGroupItems {
		t.Fatalf("%d jobs queued", len(q.List()))
	}
}

func TestPlaylistRejectsMismatchedPositions(t *testing.T) {
	for _, positions := range []any{[]any{json.Number("1")}, []any{json.Number("1"), json.Number("0")}, []any{1.5, 2.0}, "1,2"} {
		fetcher := func(string, []core.ItemRange) (map[string]any, error) {
			raw := limitedListing(2, nil)
			raw["requested_entries"] = positions
			return raw, nil
		}
		q := makeQueue(t, t.TempDir(), newBlockingStarter(), func(c *QueueConfig) { c.FetchInfo = fetcher })
		_, err := q.Create("https://www.youtube.com/@jawed", defaults(), []core.ItemRange{{Start: 1, End: 2}})
		var ytErr *core.YtDlpError
		if !errors.As(err, &ytErr) || len(q.List()) != 0 {
			t.Errorf("positions %v: %v", positions, err)
		}
	}
}

func TestPlaylistWithoutFetcherFails(t *testing.T) {
	q := makeQueue(t, t.TempDir(), newBlockingStarter())
	_, err := q.Create("https://www.youtube.com/@jawed", defaults(), nil)
	var jobErr *JobError
	if !errors.As(err, &jobErr) || jobErr.Status != 500 {
		t.Fatalf("got %v", err)
	}
}

func TestVideoTitleFromLookup(t *testing.T) {
	q := makeQueue(t, t.TempDir(), newBlockingStarter(), withConcurrency(0), func(c *QueueConfig) {
		c.TitleLookup = func(string) string { return "Known title" }
	})
	jobs, err := q.Create(watchURL(videos[0]), defaults(), nil)
	if err != nil || str(jobs[0].Title) != "Known title" {
		t.Fatalf("title %q, %v", str(jobs[0].Title), err)
	}
}

func TestJobShape(t *testing.T) {
	q := makeQueue(t, t.TempDir(), newBlockingStarter(), withConcurrency(0))
	opts := defaults()
	opts.Quality = "1080"
	jobs, err := q.Create(watchURL(videos[0]), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := marshalMap(t, jobs[0])
	want := []string{"id", "group_id", "group_title", "url", "title", "channel", "kind", "status", "stage", "percent", "speed",
		"eta", "filename", "filepath", "error", "detail", "options", "created_at"}
	if got := sortedKeys(raw); !slices.Equal(got, sorted(want)) {
		t.Fatalf("keys %v", got)
	}
	// stage is a plain string, "" outside a conversion; options always carry max_size_mb
	if raw["stage"] != "" || raw["options"].(map[string]any)["max_size_mb"] != 0.0 {
		t.Fatalf("stage %v options %v", raw["stage"], raw["options"])
	}
	if raw["options"].(map[string]any)["quality"] != "1080" || raw["status"] != "queued" {
		t.Fatalf("job %v", raw)
	}
	if raw["group_id"] != nil || raw["title"] != nil || raw["error"] != nil {
		t.Fatalf("unset fields must be null: %v", raw)
	}
	if id, _ := raw["id"].(string); len(id) != 32 {
		t.Fatalf("id %q", id)
	}
}

func TestSanitiseFolderName(t *testing.T) {
	cases := []struct{ title, want string }{
		{"Normal title", "Normal title"},
		{`a/b\c:d*e?f"g<h>i|j%k`, "a_b_c_d_e_f_g_h_i_j_k"},
		{"trailing dots...  ", "trailing dots"},
		{"CON", "_CON"},
		{"con.txt", "_con.txt"},
		{"", "Playlist"},
		{"///", "Playlist"},
		{"many   spaces\there", "many spaces_here"},
		{"many   spaces  here", "many spaces here"},
		{strings.Repeat("x", 300), strings.Repeat("x", 100)},
		{strings.Repeat("é", 150), strings.Repeat("é", 100)},
	}
	for _, tc := range cases {
		if got := SanitiseFolderName(tc.title, "Playlist"); got != tc.want {
			t.Errorf("SanitiseFolderName(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
}

func TestHoldBlocksDispatchUntilReleased(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter)
	release := q.Hold()
	id := create(t, q, watchURL(videos[0]), defaults())
	if status(t, q, id) != "queued" || starter.count() != 0 {
		t.Fatal("job started while held")
	}
	release()
	release() // a second call must not release another hold
	waitFor(t, func() bool { return starter.count() == 1 })
	if status(t, q, id) != "downloading" {
		t.Fatal("job did not start after release")
	}
}

func TestShutdownKillsRunningJobs(t *testing.T) {
	starter := newBlockingStarter()
	q := makeQueue(t, t.TempDir(), starter)
	create(t, q, watchURL(videos[0]), defaults())
	create(t, q, watchURL(videos[1]), defaults())
	waitFor(t, func() bool { return starter.count() == 2 })
	q.Shutdown()
	if !q.WaitIdle(3 * time.Second) {
		t.Fatal("runners still alive")
	}
	if !starter.proc(0).wasKilled() || !starter.proc(1).wasKilled() {
		t.Fatal("processes not killed")
	}
	create(t, q, watchURL(videos[2]), defaults())
	time.Sleep(50 * time.Millisecond)
	if starter.count() != 2 {
		t.Fatal("a job started after shutdown")
	}
}

func TestReadLines(t *testing.T) {
	var got []string
	readLines(strings.NewReader("a\nb\r\nc\rd\xffe\n\nlast"), func(line string) { got = append(got, line) })
	want := []string{"a", "b", "c", "d�e", "", "last"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func marshalMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// helperEnv makes the test binary act as a fake yt-dlp instead of running tests, so the real
// process path (StartYtDlp, the shared pipe, exit codes, KillProcessTree) runs without yt-dlp.
const helperEnv = "VIDGRAB_SERVER_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		log.SetOutput(io.Discard)
		os.Exit(m.Run())
	case "ytdlp":
		fmt.Println("YTDLP_TITLE Helper video")
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] abc: from stderr")
		fmt.Println("YTDLP_FILE " + os.Getenv("PYTHONUTF8"))
		os.Exit(7)
	case "sleep":
		fmt.Println("YTDLP_TITLE Sleeper")
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	os.Exit(4)
}

func TestStartYtDlpRunsRealProcess(t *testing.T) {
	t.Setenv(helperEnv, "ytdlp")
	proc, err := StartYtDlp([]string{os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	readLines(proc.Output(), func(line string) { lines = append(lines, line) })
	if code := proc.Wait(); code != 7 {
		t.Fatalf("exit code %d", code)
	}
	joined := strings.Join(lines, "|")
	// Both streams arrive on one pipe, and ChildEnv's PYTHONUTF8=1 reaches the child
	for _, want := range []string{"YTDLP_TITLE Helper video", "ERROR: [youtube] abc: from stderr", "YTDLP_FILE 1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("output %q lacks %q", joined, want)
		}
	}
}

func TestStartYtDlpKillTree(t *testing.T) {
	t.Setenv(helperEnv, "sleep")
	proc, err := StartYtDlp([]string{os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		readLines(proc.Output(), func(string) {})
		done <- proc.Wait()
	}()
	KillProcessTree(proc)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("killed process did not exit")
	}
}

func TestStartYtDlpMissingBinary(t *testing.T) {
	if _, err := StartYtDlp([]string{filepath.Join(t.TempDir(), "no-such-yt-dlp")}); err == nil {
		t.Fatal("start of a missing binary succeeded")
	}
	if _, err := StartYtDlp(nil); err == nil {
		t.Fatal("empty argv accepted")
	}
}

func TestKillAfterWaitSendsNothing(t *testing.T) {
	t.Setenv(helperEnv, "ytdlp")
	proc, err := StartYtDlp([]string{os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	readLines(proc.Output(), func(string) {})
	proc.Wait()
	// Once Wait has started the id may be reaped and reused, so no signal may go out
	sent, err := proc.(*execProcess).signal(func(pid int) error {
		t.Errorf("signal sent to process %d after Wait", pid)
		return nil
	})
	if sent || err != nil {
		t.Fatalf("sent %v, err %v", sent, err)
	}
	KillProcessTree(proc) // must be a harmless no-op
}

// TestKilledDownloadTempIsSwept kills a real child and checks the private temp folder is
// emptied once it has exited, as a killed PyInstaller yt-dlp leaves its _MEI folder there.
func TestKilledDownloadTempIsSwept(t *testing.T) {
	temp := t.TempDir()
	core.SetChildTempDir(temp)
	defer core.SetChildTempDir("")
	mei := filepath.Join(temp, "_MEI4242")
	if err := os.MkdirAll(mei, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperEnv, "sleep")
	proc, err := StartYtDlp([]string{os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		readLines(proc.Output(), func(string) {})
		proc.Wait()
		close(done)
	}()
	if _, err := os.Stat(mei); err != nil {
		t.Fatal("swept while the download was alive")
	}
	KillProcessTree(proc)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("killed process did not exit")
	}
	if _, err := os.Stat(mei); !os.IsNotExist(err) {
		t.Fatal("leftovers of the killed run were not swept")
	}
}

func TestPrepareChildTempEmptiesFolder(t *testing.T) {
	dataDir := t.TempDir()
	mei := filepath.Join(dataDir, "tmp", "_MEI1")
	if err := os.MkdirAll(mei, 0o755); err != nil {
		t.Fatal(err)
	}
	prepareChildTemp(dataDir)
	defer core.SetChildTempDir("")
	if _, err := os.Stat(mei); !os.IsNotExist(err) {
		t.Fatal("start-up sweep left the leftovers")
	}
	if core.ChildTempDir() != filepath.Join(dataDir, "tmp") {
		t.Fatalf("child temp %q", core.ChildTempDir())
	}
}

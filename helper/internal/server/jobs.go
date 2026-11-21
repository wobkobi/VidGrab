// In-memory download queue running yt-dlp processes with a concurrency limit.

package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"vidgrab/internal/core"
	"vidgrab/internal/system"
)

// Job statuses, as the API reports them.
const (
	StatusQueued      = "queued"
	StatusDownloading = "downloading"
	StatusProcessing  = "processing"
	StatusPaused      = "paused"
	StatusComplete    = "complete"
	StatusError       = "error"
	StatusCancelled   = "cancelled"
)

// MaxGroupItems caps how many child jobs one playlist or channel may queue.
const MaxGroupItems = 1000

// MaxFinishedJobs caps how many complete, failed and cancelled jobs the queue remembers; the
// oldest are forgotten first and their files are untouched. Active jobs never count.
const MaxFinishedJobs = 500

// maxLineBytes bounds one line of yt-dlp output; anything longer is cut, never buffered.
const maxLineBytes = 1 << 20

var (
	// Leftovers of an interrupted download: .part/.ytdl files and per-format intermediates such
	// as "Title [id].f137.mp4" that only exist until the merge finishes. The classes spell out
	// a Unicode \w so non-ASCII extensions match too. A size-limited job adds
	// its working "Title [id].<tag>.source.mp4" and the "Title [id].<tag>.sized.mp4" being
	// encoded
	partialRe = regexp.MustCompile(`(\.part(-Frag\d+)?|\.ytdl|\.f\d+[\p{L}\p{N}_-]*\.[\p{L}\p{N}_]+|\.temp\.[\p{L}\p{N}_]+|\.source\.[\p{L}\p{N}_]+|\.sized\.mp4)$`)
	// yt-dlp titles a channel tab listing "<Channel> - Videos" (or Shorts/Live); the folder uses
	// the channel name alone
	channelTabSuffixRe = regexp.MustCompile(`[\s\p{Z}]+-[\s\p{Z}]+(Videos|Shorts|Live)$`)
	// "%" is unsafe too because the folder becomes part of yt-dlp's output template
	unsafeNameRe  = regexp.MustCompile(`[<>:"/\\|?*%\x00-\x1f]`)
	reservedNames = func() map[string]bool {
		names := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
		for i := 1; i <= 9; i++ {
			names[fmt.Sprintf("COM%d", i)] = true
			names[fmt.Sprintf("LPT%d", i)] = true
		}
		return names
	}()
)

// JobError is a job action that cannot be applied; Status is the HTTP code to answer with.
type JobError struct {
	Status  int
	Message string
	Detail  string
}

// Error returns Message.
func (e *JobError) Error() string { return e.Message }

// Process is one started yt-dlp run, as the queue sees it.
type Process interface {
	// Output streams the combined stdout and stderr; it reaches EOF once the process (and
	// anything holding its pipe) has exited.
	Output() io.Reader
	// Wait blocks until the process has exited and returns its exit code. It is called once,
	// after Output reached EOF.
	Wait() int
	// Pid returns the operating-system process id.
	Pid() int
}

// StartFunc starts argv (argv[0] is the yt-dlp binary) and returns the running process.
type StartFunc func(argv []string) (Process, error)

// KillFunc stops a process and everything it started.
type KillFunc func(Process)

// JobView is the API representation of a job; nil pointers marshal as null.
type JobView struct {
	ID         string          `json:"id"`
	GroupID    *string         `json:"group_id"`
	GroupTitle *string         `json:"group_title"`
	URL        string          `json:"url"`
	Title      *string         `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Stage      string          `json:"stage"`
	Percent    float64         `json:"percent"`
	Speed      *string         `json:"speed"`
	Eta        *string         `json:"eta"`
	Filename   *string         `json:"filename"`
	Filepath   *string         `json:"filepath"`
	Error      *string         `json:"error"`
	Detail     *string         `json:"detail"`
	Options    core.JobOptions `json:"options"`
	CreatedAt  float64         `json:"created_at"`
}

// job is one download. Empty strings stand for null in the API. url, videoID, outDir, options
// and the group fields never change after creation, so runners read them without the lock.
type job struct {
	id         string
	url        string
	kind       string
	options    core.JobOptions
	outDir     string
	videoID    string
	groupID    string
	groupTitle string
	title      string
	status     string
	stage      string // StageConverting while a size-limited job re-encodes, else ""
	percent    float64
	speed      string
	eta        string
	filename   string
	filepath   string
	errMsg     string
	detail     string
	createdAt  float64

	// Runner bookkeeping, never serialised. running is true from dispatch until the runner
	// has recorded the outcome; runID numbers each dispatch, and stoppedRun is the run a
	// pause or cancel claimed, so that run's late output and exit code are ignored.
	process    Process
	running    bool
	runID      int
	stoppedRun int
	// sized is set once a size-limited job has downloaded its source, so a resume after a
	// pause during conversion re-encodes that file instead of starting over.
	sized *sizedState
	// cancelRun cancels a size-limited run's context (its info fetch and encoder probe); set
	// only while that run is live.
	cancelRun context.CancelFunc
	// unrenamed is a finished "Title [id].ext" a stopped run left before moving it to its final
	// name; the next run renames it even though yt-dlp reports it as already downloaded.
	unrenamed string
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// view returns the API representation; the caller holds the queue lock.
func (j *job) view() JobView {
	return JobView{
		ID:         j.id,
		GroupID:    optional(j.groupID),
		GroupTitle: optional(j.groupTitle),
		URL:        j.url,
		Title:      optional(j.title),
		Kind:       j.kind,
		Status:     j.status,
		Stage:      j.stage,
		Percent:    j.percent,
		Speed:      optional(j.speed),
		Eta:        optional(j.eta),
		Filename:   optional(j.filename),
		Filepath:   optional(j.filepath),
		Error:      optional(j.errMsg),
		Detail:     optional(j.detail),
		Options:    j.options,
		CreatedAt:  j.createdAt,
	}
}

func (j *job) isRunningStatus() bool {
	return j.status == StatusDownloading || j.status == StatusProcessing
}

func (j *job) isFinishedStatus() bool {
	return j.status == StatusComplete || j.status == StatusError || j.status == StatusCancelled
}

// SanitiseFolderName turns a playlist or channel title into a safe single folder name: no
// separators, reserved device names or trailing dots/spaces, at most 100 characters. "%" is
// removed too because the folder becomes part of yt-dlp's output template. fallback is used
// when nothing usable remains.
func SanitiseFolderName(title, fallback string) string {
	name := unsafeNameRe.ReplaceAllString(title, "_")
	name = strings.TrimRight(strings.TrimSpace(name), ". ")
	name = collapseSpace(name)
	if utf8.RuneCountInString(name) > 100 {
		name = string([]rune(name)[:100])
	}
	name = strings.TrimRight(name, ". ")
	if strings.Trim(name, "_.") == "" {
		return fallback
	}
	stem, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToUpper(stem)] {
		name = "_" + name
	}
	return name
}

// collapseSpace replaces every run of whitespace with one space.
func collapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// newJobID returns a random uuid4 as 32 hex characters.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// QueueConfig wires a JobQueue to its settings and to the outside world. Nil functions get
// the real implementations.
type QueueConfig struct {
	// Concurrency is the maximum number of parallel downloads (0 keeps every job queued).
	Concurrency int
	// Fragments is yt-dlp's --concurrent-fragments for jobs started from now on.
	Fragments int
	// Cookies makes jobs started from now on use Firefox cookies.
	Cookies bool
	// DownloadsDir returns the folder downloads land in (playlists get a subfolder).
	DownloadsDir func() string
	// Ffmpeg reports whether ffmpeg is available when a job starts.
	Ffmpeg func() bool
	// Tools returns the yt-dlp binary and ffmpeg folder for a job's command line.
	Tools func() core.Tools
	// Start launches yt-dlp; StartYtDlp by default.
	Start StartFunc
	// Kill stops a process tree; KillProcessTree by default.
	Kill KillFunc
	// FetchInfo lists a playlist or channel (flat yt-dlp JSON) for expansion into jobs; nil
	// makes playlist downloads fail with 500.
	FetchInfo func(canonical string) (map[string]any, error)
	// TitleLookup returns a video title already known (from /info), or "".
	TitleLookup func(canonical string) string
	// VideoInfo returns full (not flat) yt-dlp JSON for one video, which a size-limited job
	// needs for its formats and duration; nil makes those jobs fail. ctx ends when the job is
	// paused, cancelled or the queue shuts down.
	VideoInfo func(ctx context.Context, canonical string) (map[string]any, error)
	// Encoder returns the H.264 encoder to re-encode with for an ffmpeg executable; by
	// default a process-wide core.EncoderProbe. ctx ends like VideoInfo's.
	Encoder func(ctx context.Context, ffmpeg string) string
}

// defaultEncoders probes encoders once per server run, whichever queue asks first.
var defaultEncoders = core.NewEncoderProbe(nil, "")

// JobQueue is the thread-safe download queue.
//
// A dispatcher starts one runner goroutine per job while fewer than Concurrency jobs run,
// oldest queued job first, so the limit can change at runtime. One mutex guards all job state
// and is never held while a process is killed or output is read. Pausing or cancelling kills
// the yt-dlp process tree and leaves .part files for --continue; a job is only restarted once
// its previous process has fully exited, since Windows keeps the .part file locked until then.
type JobQueue struct {
	mu          sync.Mutex
	concurrency int
	fragments   int
	cookies     bool
	jobs        map[string]*job
	order       []string // job ids, oldest first
	closed      bool
	holds       int
	runners     int // runner goroutines still alive

	downloadsDir func() string
	ffmpeg       func() bool
	tools        func() core.Tools
	start        StartFunc
	kill         KillFunc
	fetchInfo    func(string) (map[string]any, error)
	titleLookup  func(string) string
	videoInfo    func(context.Context, string) (map[string]any, error)
	encoder      func(context.Context, string) string
	// failedEncoders holds hardware encoders whose real encode failed; they are not used again
	// while the process lives
	failedEncoders map[string]bool
	// rename moves a finished file to its final name; os.Rename outside tests
	rename func(from, to string) error
}

// NewJobQueue returns an empty queue configured by cfg.
func NewJobQueue(cfg QueueConfig) *JobQueue {
	q := &JobQueue{
		concurrency:  cfg.Concurrency,
		fragments:    cfg.Fragments,
		cookies:      cfg.Cookies,
		jobs:         map[string]*job{},
		downloadsDir: cfg.DownloadsDir,
		ffmpeg:       cfg.Ffmpeg,
		tools:        cfg.Tools,
		start:        cfg.Start,
		kill:         cfg.Kill,
		fetchInfo:    cfg.FetchInfo,
		titleLookup:  cfg.TitleLookup,
		videoInfo:    cfg.VideoInfo,
		encoder:      cfg.Encoder,

		failedEncoders: map[string]bool{},
		rename:         os.Rename,
	}
	if q.encoder == nil {
		q.encoder = defaultEncoders.Pick
	}
	if q.downloadsDir == nil {
		q.downloadsDir = system.DownloadsDir
	}
	if q.ffmpeg == nil {
		q.ffmpeg = system.FfmpegOnPath
	}
	if q.tools == nil {
		q.tools = func() core.Tools { return core.Tools{YtDlp: "yt-dlp"} }
	}
	if q.start == nil {
		q.start = StartYtDlp
	}
	if q.kill == nil {
		q.kill = KillProcessTree
	}
	return q
}

// Queries

// List returns every job, oldest first.
func (q *JobQueue) List() []JobView {
	q.mu.Lock()
	defer q.mu.Unlock()
	views := make([]JobView, 0, len(q.order))
	for _, id := range q.order {
		views = append(views, q.jobs[id].view())
	}
	return views
}

// lookup returns a job by id or a 404 JobError; the caller holds the lock.
func (q *JobQueue) lookup(id string) (*job, error) {
	j, ok := q.jobs[id]
	if !ok {
		return nil, &JobError{Status: 404, Message: "Download not found"}
	}
	return j, nil
}

// Snapshot returns the API representation of one job, or a 404 JobError.
func (q *JobQueue) Snapshot(id string) (JobView, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.lookup(id)
	if err != nil {
		return JobView{}, err
	}
	return j.view(), nil
}

// Busy reports whether any job is queued or running (paused jobs do not count).
func (q *JobQueue) Busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.status == StatusQueued || j.isRunningStatus() || j.running {
			return true
		}
	}
	return false
}

// Running reports whether any yt-dlp process is alive.
func (q *JobQueue) Running() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.running {
			return true
		}
	}
	return false
}

// Creation

// Create queues a video, or one child job per selected entry of a playlist or channel.
//
// rawURL is validated again here; ranges are the parsed item ranges for a playlist or channel
// (nil selects every item). Errors are *core.InvalidUrl, a *JobError (400 when a list has no
// entries in range or too many, 500 without FetchInfo) or the FetchInfo error (normally a
// *core.YtDlpError).
func (q *JobQueue) Create(rawURL string, options core.JobOptions, ranges []core.ItemRange) ([]JobView, error) {
	parsed, err := core.Classify(rawURL)
	if err != nil {
		return nil, err
	}
	root := q.downloadsDir()
	var jobs []*job
	if parsed.Kind == core.KindVideo {
		title := ""
		if q.titleLookup != nil {
			title = q.titleLookup(parsed.Canonical)
		}
		jobs = []*job{newJob(parsed.Canonical, parsed.ID, options, root, title, "", "")}
	} else {
		if q.fetchInfo == nil {
			return nil, &JobError{Status: 500, Message: "Playlist downloads are not available"}
		}
		info, err := q.fetchInfo(parsed.Canonical)
		if err != nil {
			return nil, err
		}
		jobs, err = expandEntries(info, parsed, options, ranges, root)
		if err != nil {
			return nil, err
		}
	}

	q.mu.Lock()
	result := make([]JobView, 0, len(jobs))
	for _, j := range jobs {
		q.jobs[j.id] = j
		q.order = append(q.order, j.id)
		result = append(result, j.view())
	}
	log.Printf("jobs: queued %d job(s) for %s %s (%s)", len(jobs), parsed.Kind, parsed.ID, options.Kind)
	q.scheduleLocked()
	q.mu.Unlock()
	return result, nil
}

// expandEntries turns flat playlist/channel JSON into one job per selected, well-formed entry,
// all sharing a group id and a subfolder named after the list.
func expandEntries(info map[string]any, parsed core.ParsedUrl, options core.JobOptions, ranges []core.ItemRange, root string) ([]*job, error) {
	type entry struct{ id, title string }
	var entries []entry
	list, _ := info["entries"].([]any)
	for _, item := range list {
		e, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := ""
		if raw, ok := e["id"]; ok && raw != nil {
			id = fmt.Sprint(raw)
		}
		if !core.IsVideoID(id) {
			continue
		}
		title, _ := e["title"].(string)
		entries = append(entries, entry{id, title})
	}
	selected := core.SelectItems(entries, ranges)
	if len(selected) == 0 {
		return nil, &JobError{Status: 400, Message: "No videos in that range"}
	}
	if len(selected) > MaxGroupItems {
		return nil, &JobError{Status: 400, Message: fmt.Sprintf("Too many videos (%d); choose a range of at most %d", len(selected), MaxGroupItems)}
	}
	groupTitle, _ := info["title"].(string)
	if groupTitle == "" {
		groupTitle = parsed.ID
	}
	if parsed.Kind == core.KindChannel {
		if trimmed := channelTabSuffixRe.ReplaceAllString(groupTitle, ""); trimmed != "" {
			groupTitle = trimmed
		}
	}
	outDir := filepath.Join(root, SanitiseFolderName(groupTitle, "Playlist"))
	groupID := newJobID()
	jobs := make([]*job, 0, len(selected))
	for _, e := range selected {
		watch, err := core.WatchUrl(e.id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, newJob(watch, e.id, options, outDir, e.title, groupID, groupTitle))
	}
	return jobs, nil
}

func newJob(url, videoID string, options core.JobOptions, outDir, title, groupID, groupTitle string) *job {
	return &job{
		id:         newJobID(),
		url:        url,
		kind:       options.Kind,
		options:    options,
		outDir:     outDir,
		videoID:    videoID,
		title:      title,
		groupID:    groupID,
		groupTitle: groupTitle,
		status:     StatusQueued,
		createdAt:  float64(time.Now().UnixNano()) / 1e9,
		stoppedRun: -1,
	}
}

// Actions

// Pause stops a queued or running job, keeping its partial files. Errors are a *JobError: 404
// for an unknown id, 409 when the job cannot be paused.
func (q *JobQueue) Pause(id string) (JobView, error) {
	q.mu.Lock()
	j, err := q.lookup(id)
	if err != nil {
		q.mu.Unlock()
		return JobView{}, err
	}
	if j.status != StatusQueued && !j.isRunningStatus() {
		q.mu.Unlock()
		return JobView{}, &JobError{Status: 409, Message: fmt.Sprintf("Cannot pause a %s download", j.status)}
	}
	proc := q.stopLocked(j, StatusPaused)
	view := j.view()
	q.mu.Unlock()
	if proc != nil {
		q.kill(proc)
	}
	return view, nil
}

// Resume re-queues a paused job; yt-dlp picks up its .part files via --continue, and a
// size-limited job paused while converting restarts the conversion from its downloaded source.
// Errors are a *JobError: 404 for an unknown id, 409 when the job is not paused.
func (q *JobQueue) Resume(id string) (JobView, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.lookup(id)
	if err != nil {
		return JobView{}, err
	}
	if j.status != StatusPaused {
		return JobView{}, &JobError{Status: 409, Message: fmt.Sprintf("Cannot resume a %s download", j.status)}
	}
	j.status = StatusQueued
	view := j.view()
	q.scheduleLocked()
	return view, nil
}

// Cancel stops a job and removes its partial files. Errors are a *JobError: 404 for an
// unknown id, 409 when the job already finished.
func (q *JobQueue) Cancel(id string) (JobView, error) {
	q.mu.Lock()
	j, err := q.lookup(id)
	if err != nil {
		q.mu.Unlock()
		return JobView{}, err
	}
	if j.isFinishedStatus() {
		q.mu.Unlock()
		return JobView{}, &JobError{Status: 409, Message: fmt.Sprintf("Cannot cancel a %s download", j.status)}
	}
	proc := q.stopLocked(j, StatusCancelled)
	j.sized = nil
	// A running job's runner removes the partials once its process has exited
	cleanupNow := !j.running
	view := j.view()
	q.mu.Unlock()
	if proc != nil {
		q.kill(proc)
	}
	if cleanupNow {
		q.removePartials(j)
	}
	return view, nil
}

// Retry re-queues a failed or cancelled job from scratch. Errors are a *JobError: 404 for an
// unknown id, 409 when the job is not failed or cancelled.
func (q *JobQueue) Retry(id string) (JobView, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, err := q.lookup(id)
	if err != nil {
		return JobView{}, err
	}
	if j.status != StatusError && j.status != StatusCancelled {
		return JobView{}, &JobError{Status: 409, Message: fmt.Sprintf("Cannot retry a %s download", j.status)}
	}
	j.status = StatusQueued
	j.percent = 0
	j.speed, j.eta, j.errMsg, j.detail, j.stage = "", "", "", "", ""
	j.sized = nil
	view := j.view()
	q.scheduleLocked()
	return view, nil
}

// FinishedFile returns the recorded file of a completed job, for open/reveal. Errors are a
// *JobError: 404 for an unknown id, 409 when the job is not complete or the file is gone.
func (q *JobQueue) FinishedFile(id string) (string, error) {
	q.mu.Lock()
	j, err := q.lookup(id)
	if err != nil {
		q.mu.Unlock()
		return "", err
	}
	if j.status != StatusComplete || j.filepath == "" {
		q.mu.Unlock()
		return "", &JobError{Status: 409, Message: "The download has not finished"}
	}
	path := j.filepath
	q.mu.Unlock()
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return "", &JobError{Status: 409, Message: "The file was moved or deleted"}
	}
	return path, nil
}

// ClearFinished removes complete, failed and cancelled jobs (their files are untouched) and
// returns how many were removed.
func (q *JobQueue) ClearFinished() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.order[:0]
	removed := 0
	for _, id := range q.order {
		j := q.jobs[id]
		if j.isFinishedStatus() && !j.running {
			delete(q.jobs, id)
			removed++
			continue
		}
		kept = append(kept, id)
	}
	q.order = kept
	return removed
}

// pruneFinishedLocked forgets the oldest finished jobs beyond MaxFinishedJobs. A finished job
// whose process is still exiting is skipped, as in ClearFinished. Callers hold q.mu.
func (q *JobQueue) pruneFinishedLocked() {
	finished := 0
	for _, id := range q.order {
		if j := q.jobs[id]; j.isFinishedStatus() && !j.running {
			finished++
		}
	}
	excess := finished - MaxFinishedJobs
	if excess <= 0 {
		return
	}
	kept := q.order[:0]
	for _, id := range q.order {
		if j := q.jobs[id]; excess > 0 && j.isFinishedStatus() && !j.running {
			delete(q.jobs, id)
			excess--
			continue
		}
		kept = append(kept, id)
	}
	q.order = kept
}

// ApplySettings updates the queue settings; a higher concurrency starts waiting jobs straight
// away, while fragments and cookies apply to jobs started from now on.
func (q *JobQueue) ApplySettings(concurrency, fragments int, cookies bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.concurrency = concurrency
	q.fragments = fragments
	q.cookies = cookies
	q.scheduleLocked()
}

// SetConcurrency changes only the parallel download limit.
func (q *JobQueue) SetConcurrency(concurrency int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.concurrency = concurrency
	q.scheduleLocked()
}

// Hold keeps queued jobs from starting until the returned release function is called (used
// while yt-dlp is replaced). Holds nest; calling one release function twice is harmless.
func (q *JobQueue) Hold() (release func()) {
	q.mu.Lock()
	q.holds++
	q.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.holds--
			q.scheduleLocked()
		})
	}
}

// Shutdown stops dispatching and kills every running process; used when the server stops.
//
// Jobs live only in memory, so nothing can resume a size-limited job after the helper exits.
// Its working files (a full-size source can be hundreds of MB) are removed: here for paused
// and queued jobs, and by settle for running ones once their process has exited.
func (q *JobQueue) Shutdown() {
	q.mu.Lock()
	q.closed = true
	var procs []Process
	var idle []*job
	for _, j := range q.jobs {
		if j.process != nil {
			procs = append(procs, j.process)
		}
		switch {
		case j.running:
			j.stoppedRun = j.runID
			if j.cancelRun != nil {
				j.cancelRun()
			}
		case j.options.SizeLimited() && (j.status == StatusPaused || j.status == StatusQueued):
			idle = append(idle, j)
		}
	}
	q.mu.Unlock()
	for _, proc := range procs {
		q.kill(proc)
	}
	for _, j := range idle {
		q.removeWork(j, "")
	}
}

// WaitIdle waits until no runner goroutine is alive and reports whether that happened within
// timeout (used by tests and shutdown).
func (q *JobQueue) WaitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		idle := q.runners == 0
		q.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Internals

// stopLocked marks a job paused or cancelled and returns the process to kill, if any. The
// caller holds the lock and kills the process after releasing it.
func (q *JobQueue) stopLocked(j *job, status string) Process {
	j.status = status
	j.speed, j.eta, j.stage = "", "", ""
	if j.running {
		j.stoppedRun = j.runID
		if j.cancelRun != nil {
			j.cancelRun()
		}
		return j.process
	}
	return nil
}

// scheduleLocked starts queued jobs, oldest first, while below the concurrency limit. The
// caller holds the lock.
func (q *JobQueue) scheduleLocked() {
	if q.closed || q.holds > 0 {
		return
	}
	type target struct{ videoID, outDir string }
	active := 0
	// Two runs of one video into one folder would write the same .part and .fNNN files, so a
	// duplicate waits in the queue until the earlier run has exited
	busyTargets := map[target]bool{}
	for _, j := range q.jobs {
		if j.running {
			active++
			busyTargets[target{j.videoID, j.outDir}] = true
		}
	}
	for _, id := range q.order {
		if active >= q.concurrency {
			break
		}
		j := q.jobs[id]
		if j.status != StatusQueued || j.running {
			continue
		}
		t := target{j.videoID, j.outDir}
		if busyTargets[t] {
			continue
		}
		busyTargets[t] = true
		j.running = true
		j.runID++
		j.status = StatusDownloading
		active++
		q.runners++
		go q.run(j, j.runID)
	}
}

// run executes yt-dlp for one job and records the outcome; it runs on its own goroutine.
func (q *JobQueue) run(j *job, runID int) {
	defer func() {
		q.mu.Lock()
		q.runners--
		q.mu.Unlock()
	}()
	ffmpeg := q.ffmpeg()
	tools := q.tools()
	if err := os.MkdirAll(j.outDir, 0o755); err != nil {
		log.Printf("jobs: could not create %s for %s: %v", j.outDir, j.videoID, err)
		q.settle(j, runID, outcome{errMsg: "Could not start yt-dlp", detail: err.Error()})
		return
	}
	if j.options.SizeLimited() {
		// ffmpeg was there when the job was created but may have gone since; a plain
		// download would silently ignore the limit
		if !ffmpeg {
			q.settle(j, runID, outcome{errMsg: errNeedsFfmpeg, detail: needsFfmpegDetail})
			return
		}
		q.settle(j, runID, q.runSized(j, runID, tools))
		return
	}
	q.mu.Lock()
	args := core.BuildArgs(j.options, j.url, j.outDir, q.cookies, q.fragments, ffmpeg, tools)
	q.mu.Unlock()
	parser := core.NewOutputParser(core.ExpectedStreams(j.options, ffmpeg))
	// Without the video's length only a clip with a set end has a known length
	parser.ClipSeconds, _ = j.options.ClipLength(0)
	res := q.download(j, runID, args, parser, true)
	if res.errMsg == "" && res.filepath != "" && !q.stopped(j, runID) &&
		j.options.Kind == "video" && j.options.Container == "mp4" && core.MP4AudioNeedsFix(parser.ACodec) {
		res = q.fixMP4Audio(j, runID, tools, res.filepath, parser.ACodec, parser.AudioChannels)
	}
	// The after_move print is the path after every post-processor (audio extraction, metadata),
	// so the rename moves the file as it will stay
	if res.errMsg == "" && res.filepath != "" && !q.stopped(j, runID) {
		res = q.finishDownload(j, runID, res.filepath, parser.PreExisting())
	}
	q.settle(j, runID, res)
}

// fixMP4Audio re-encodes the sound of the MP4 at path to AAC, copying every other stream, for
// sound yt-dlp merged in a codec MP4 players handle badly (Opus when a site offers no AAC; see
// core.MP4AudioNeedsFix). The new file replaces the old one under the same name. A failed or
// stopped fix leaves the download as it was, since it still holds everything.
func (q *JobQueue) fixMP4Audio(j *job, runID int, tools core.Tools, path, acodec string, channels int) outcome {
	kept := outcome{filepath: path}
	temp := strings.TrimSuffix(path, filepath.Ext(path)) + ".audio.mp4"
	log.Printf("jobs: %s has %s sound in MP4, re-encoding it to AAC %dk", j.videoID, acodec, core.FixAudioKbps(channels))
	argv := core.MP4AudioFixArgs(core.FfmpegPath(tools), path, temp, channels)
	code, err := q.runProcess(j, runID, argv, func(string) {})
	if err != nil || code != 0 || q.stopped(j, runID) {
		log.Printf("jobs: could not re-encode the sound of %s (exit %d, %v), keeping it as it is", j.videoID, code, err)
		removeWithRetry(temp)
		return kept
	}
	if err := os.Rename(temp, path); err != nil {
		log.Printf("jobs: could not replace %s with its AAC copy: %v", j.videoID, err)
		removeWithRetry(temp)
	}
	return kept
}

// download runs one yt-dlp command for j, applying its progress to the job, and returns the
// outcome (a complete outcome carries the file yt-dlp reported). showFile records the file
// yt-dlp reports on the job as it goes; a size-limited job passes false because its download
// is only a working file until it is committed.
func (q *JobQueue) download(j *job, runID int, args []string, parser *core.OutputParser, showFile bool) outcome {
	log.Printf("jobs: starting yt-dlp for %s (%s)", j.videoID, j.options.Kind)
	code, err := q.runProcess(j, runID, args, func(line string) {
		update := parser.Feed(line)
		if update == nil {
			return
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if j.stoppedRun == runID {
			return
		}
		if update.Percent != nil {
			j.percent = max(j.percent, *update.Percent)
		}
		if update.HasProgress() {
			j.speed, j.eta = update.Speed, update.Eta
		}
		if update.Processing && j.status == StatusDownloading {
			j.status = StatusProcessing
			j.speed, j.eta = "", ""
		}
		if update.Title != "" && j.title == "" {
			j.title = update.Title
		}
		if update.Filepath != "" && showFile {
			j.filepath = update.Filepath
			j.filename = filepath.Base(update.Filepath)
		}
	})
	switch {
	case err != nil:
		log.Printf("jobs: could not start yt-dlp for %s: %v", j.videoID, err)
		return outcome{errMsg: "Could not start yt-dlp", detail: err.Error()}
	case code == 0:
		return outcome{filepath: parser.Filepath}
	}
	result := outcome{errMsg: parser.LastError, detail: parser.Detail()}
	if result.errMsg == "" {
		result.errMsg = fmt.Sprintf("yt-dlp exited with code %d", code)
	}
	return result
}

// runProcess starts argv as j's current process, calls onLine for every output line and
// returns the exit code once the process has exited; err is set only when it never started.
// onLine runs without the lock and must itself skip updates once the run was stopped.
func (q *JobQueue) runProcess(j *job, runID int, argv []string, onLine func(string)) (int, error) {
	proc, err := q.start(argv)
	if err != nil {
		return 0, err
	}
	// A pause or cancel that arrived while the process was starting (or between two processes
	// of one run) found no process to kill, so kill it here instead
	q.mu.Lock()
	j.process = proc
	killNow := j.stoppedRun == runID
	q.mu.Unlock()
	if killNow {
		q.kill(proc)
	}
	readLines(proc.Output(), onLine)
	code := proc.Wait()
	q.mu.Lock()
	j.process = nil
	q.mu.Unlock()
	return code, nil
}

// outcome is how a run ended: complete when errMsg is empty, with filepath the finished file
// ("" keeps whatever the progress output recorded).
type outcome struct {
	filepath string
	errMsg   string
	detail   string
	// keep is a working file a failed size-limited job must leave in place (a finished
	// encode that could not be renamed); every other working file is removed on failure
	keep string
}

// stopped reports whether a pause, cancel or shutdown claimed the run.
func (q *JobQueue) stopped(j *job, runID int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return j.stoppedRun == runID
}

// settle records a finished run and starts the next queued job. A run that was paused or
// cancelled keeps the status the action set, whatever res says. A size-limited run stopped by
// Shutdown has its working files removed, since nothing can resume it.
func (q *JobQueue) settle(j *job, runID int, res outcome) {
	q.mu.Lock()
	cleanup := false
	// A failed size-limited job clears its sized state, so Retry starts over and its working
	// files would otherwise be orphaned
	failed := j.stoppedRun != runID && res.errMsg != ""
	dropWork := j.options.SizeLimited() && (failed || (j.stoppedRun == runID && q.closed))
	j.running = false
	j.process = nil
	j.stage = ""
	switch {
	case j.stoppedRun == runID:
		cleanup = j.status == StatusCancelled
		log.Printf("jobs: stopped %s (%s)", j.videoID, j.status)
	case res.errMsg == "":
		j.status = StatusComplete
		j.percent = 100
		j.speed, j.eta = "", ""
		j.sized = nil
		j.unrenamed = ""
		if res.filepath != "" {
			j.filepath = res.filepath
			j.filename = filepath.Base(res.filepath)
		}
		log.Printf("jobs: completed %s", j.videoID)
	default:
		j.status = StatusError
		j.speed, j.eta = "", ""
		j.sized = nil
		j.errMsg, j.detail = res.errMsg, res.detail
		log.Printf("jobs: failed %s: %s", j.videoID, j.errMsg)
	}
	q.mu.Unlock()
	if cleanup {
		q.removePartials(j)
	}
	if dropWork {
		q.removeWork(j, res.keep)
	}
	q.mu.Lock()
	q.pruneFinishedLocked()
	q.scheduleLocked()
	q.mu.Unlock()
}

// removePartials deletes a cancelled job's leftover partial files.
//
// Only files carrying the job's "[video id]" in its output folder are touched, and nothing is
// removed while another live job downloads the same video into the same folder.
func (q *JobQueue) removePartials(j *job) {
	q.mu.Lock()
	for _, other := range q.jobs {
		if other != j && other.videoID == j.videoID && other.outDir == j.outDir &&
			(other.running || other.status == StatusQueued || other.status == StatusPaused) {
			q.mu.Unlock()
			return
		}
	}
	q.mu.Unlock()
	marker := "[" + j.videoID + "]"
	entries, err := os.ReadDir(j.outDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, marker) || !partialRe.MatchString(name) {
			continue
		}
		removeWithRetry(filepath.Join(j.outDir, name))
	}
}

// readLines calls fn for every line of r until EOF, decoding invalid UTF-8 as U+FFFD.
//
// "\n", "\r\n" and a lone "\r" all end a line. A line longer
// than maxLineBytes is cut there and the rest of it dropped, so the reader keeps draining the
// pipe (a full pipe would block yt-dlp) without unbounded memory.
func readLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	afterCR := false
	emit := func() {
		fn(strings.ToValidUTF8(string(line), "�"))
		line = line[:0]
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			if len(line) > 0 {
				emit()
			}
			return
		}
		switch {
		case b == '\n' && afterCR:
			afterCR = false
		case b == '\n' || b == '\r':
			afterCR = b == '\r'
			emit()
		default:
			afterCR = false
			if len(line) < maxLineBytes {
				line = append(line, b)
			}
		}
	}
}

// execProcess is a real yt-dlp process whose stdout and stderr share one pipe.
//
// kill and Wait share mu so a kill can never reach a reused process id. The id stays
// reserved until cmd.Wait reaps the process (Unix keeps it as a zombie, Windows keeps it
// while cmd.Process holds its handle), and Wait sets waited under mu before calling
// cmd.Wait. So a signal sent under mu while !waited still reaches this process tree, and
// once waited is set nothing is sent. Wait is only called after Output reached EOF, which
// means the tree has closed the pipe and is exiting anyway.
type execProcess struct {
	cmd    *exec.Cmd
	output *os.File
	exited func(killed bool) // core.TrackChild's exit callback

	mu     sync.Mutex
	waited bool
	killed bool
}

func (p *execProcess) Output() io.Reader { return p.output }

func (p *execProcess) Pid() int { return p.cmd.Process.Pid }

// kill stops the process tree the way system.KillTree does (terminate, wait out
// system.KillGrace, force), except that each signal is sent under mu only while Wait has not
// started, and the grace period ends as soon as Wait starts rather than polling the group.
func (p *execProcess) kill() error {
	sent, err := p.signal(system.TerminateTree)
	if !sent || err != nil || system.KillGrace == 0 {
		return err
	}
	deadline := time.Now().Add(system.KillGrace)
	for time.Now().Before(deadline) {
		if p.isWaited() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, err = p.signal(system.ForceKillTree)
	return err
}

// signal runs send on the process id under mu unless Wait has started, and reports whether
// it ran.
func (p *execProcess) signal(send func(pid int) error) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited {
		return false, nil
	}
	p.killed = true
	return true, send(p.cmd.Process.Pid)
}

func (p *execProcess) isWaited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

func (p *execProcess) Wait() int {
	p.mu.Lock()
	p.waited = true
	killed := p.killed
	p.mu.Unlock()
	err := p.cmd.Wait()
	p.output.Close()
	p.exited(killed)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	}
	return -1
}

// StartYtDlp starts argv (yt-dlp, or ffmpeg for a size-limited conversion) with no stdin,
// core.ChildEnv and both output streams on one pipe,
// hidden and at low priority in its own process group (system.PrepareChild), so
// KillProcessTree takes ffmpeg down with it.
//
// The parent's copy of the pipe's write end is closed once the child has started, so Output
// reaches EOF when the child and everything it spawned have exited.
func StartYtDlp(argv []string) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.Env = core.ChildEnv()
	system.PrepareChild(cmd)
	exited := core.TrackChild()
	err = cmd.Start()
	writer.Close()
	if err != nil {
		reader.Close()
		exited(false)
		return nil, err
	}
	system.AfterStart(cmd)
	return &execProcess{cmd: cmd, output: reader, exited: exited}, nil
}

// KillProcessTree kills a process and everything it started (system.KillTree), logging a
// failure rather than returning it, since a caller could do nothing more. A process from
// StartYtDlp is killed only while it has not been reaped (see execProcess).
func KillProcessTree(p Process) {
	var err error
	if ep, ok := p.(*execProcess); ok {
		err = ep.kill()
	} else {
		err = system.KillTree(p.Pid())
	}
	if err != nil {
		log.Printf("jobs: could not kill process %d: %v", p.Pid(), err)
	}
}

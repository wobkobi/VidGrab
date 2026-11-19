// Size-limited video jobs (max_size_mb): download H.264 + AAC streams that already fit, or
// download a source and re-encode it with ffmpeg to fit.
//
// Every download goes to a per-job working file, "Title [id].<tag>.source.mp4", and one
// finalise step decides what happens to it: kept as it is when it plays everywhere and is
// within the limit, otherwise re-encoded to "Title [id].<tag>.sized.mp4". Either way the
// result is moved to "Title (N MB).mp4" (with " (2)" and so on when that name is taken, see
// moveToFinal), so it never replaces a normal download of the same video or an earlier sized
// one.
//
// A pause during the conversion kills ffmpeg and keeps the working file; the resume restarts
// the conversion from the beginning of it (ffmpeg cannot resume an encode). A cancel or a
// failure removes it, so a retry starts the whole job again.

package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vidgrab/internal/core"
)

// StageConverting is the job stage while a size-limited video is re-encoded to fit.
const StageConverting = "converting"

// retryBitrateFactor scales the video bitrate for the second encode when the first came out
// over the limit. Overshoots are a few percent (hardware rate control, MP4 overhead on short
// clips), so 15% less clears them without giving up much quality.
const retryBitrateFactor = 0.85

// errConvert is the job error for an encode that failed; a hardware encoder failing with it
// is retried with libx264.
const errConvert = "Could not convert the video"

// sizedState is what a size-limited job keeps between its download and its conversion.
type sizedState struct {
	source   string
	plan     core.EncodePlan
	duration float64
}

// workTag returns the part of a size-limited job's working file names unique to the job: the
// first 8 hex digits of its id.
func workTag(j *job) string { return j.id[:8] }

// workBase returns "dir/Title [id]" from a working file "dir/Title [id].<tag>.source.<ext>".
// A name without the marker (which yt-dlp never produces) loses just its extension.
func workBase(source, tag string) string {
	dir, name := filepath.Split(source)
	if i := strings.LastIndex(name, "."+tag+".source."); i >= 0 {
		return filepath.Join(dir, name[:i])
	}
	return filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name)))
}

// encodePath returns the "Title [id].<tag>.sized.mp4" the encoder writes for a working file.
func encodePath(source, tag string) string {
	return workBase(source, tag) + "." + tag + ".sized.mp4"
}

// runSized runs a size-limited video job and returns its outcome: fetch the formats, download
// streams that fit or a capped source, then finalise. A job paused during conversion skips
// straight to converting its kept working file.
//
// The run gets its own context, cancelled by a pause, cancel or shutdown (see stopLocked), so
// the info fetch and the encoder probe stop at once instead of holding the queue slot.
func (q *JobQueue) runSized(j *job, runID int, tools core.Tools) outcome {
	ctx, cancel := context.WithCancel(context.Background())
	q.mu.Lock()
	if j.stoppedRun == runID {
		q.mu.Unlock()
		cancel()
		return outcome{}
	}
	j.cancelRun = cancel
	state := j.sized
	cookies, fragments := q.cookies, q.fragments
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		j.cancelRun = nil
		q.mu.Unlock()
		cancel()
	}()

	if state != nil {
		if info, err := os.Stat(state.source); err == nil && info.Mode().IsRegular() {
			return q.convert(ctx, j, runID, tools, *state)
		}
	}
	if q.videoInfo == nil {
		return outcome{errMsg: "Size limits are not available"}
	}
	raw, err := q.videoInfo(ctx, j.url)
	if q.stopped(j, runID) {
		return outcome{}
	}
	if err != nil {
		res := outcome{errMsg: err.Error()}
		var ytErr *core.YtDlpError
		if errors.As(err, &ytErr) {
			res.detail = ytErr.Detail
		}
		return res
	}
	if title, _ := raw["title"].(string); title != "" {
		q.mu.Lock()
		if j.title == "" {
			j.title = title
		}
		q.mu.Unlock()
	}

	limitMB := j.options.MaxSizeMB
	formats := core.FitFormats(raw)
	qualityCap := core.QualityCap(j.options.Quality)
	duration := core.InfoDuration(raw)
	sourceHeight := core.SourceHeight(formats, qualityCap)
	if sourceHeight == 0 {
		sourceHeight = qualityCap
	}
	plan, planErr := core.PlanEncode(limitMB, duration, sourceHeight)
	choice, fits := core.SelectFit(formats, qualityCap, core.LimitBytes(limitMB))
	// A fit shorter than the conversion would be loses to converting, which gives the
	// better picture; when no conversion is possible any fit is better than failing
	if fits && planErr == nil && choice.Height < plan.Height {
		log.Printf("jobs: %s fits %d MB only at %dp, converting at %dp instead", j.videoID, limitMB,
			choice.Height, plan.Height)
		fits = false
	}

	tag := workTag(j)
	var args []string
	streams := 2
	if fits {
		log.Printf("jobs: %s fits %d MB as %s (%dp, %.1f MB)", j.videoID, limitMB,
			choice.Selector(), choice.Height, choice.Size/core.BytesPerMB)
		args = core.BuildFitArgs(j.url, j.outDir, cookies, fragments, tools, tag, choice.Selector())
		if choice.Audio == "" {
			streams = 1
		}
	} else {
		if planErr != nil {
			return planOutcome(planErr)
		}
		log.Printf("jobs: %s needs converting to fit %d MB: %dp at %d+%d kbps", j.videoID, limitMB,
			plan.Height, plan.VideoKbps, plan.AudioKbps)
		args = core.BuildSourceArgs(j.url, j.outDir, cookies, fragments, tools, tag, plan.Height)
	}
	parser := core.NewOutputParser(streams)
	res := q.download(j, runID, args, parser, false)
	if res.errMsg == "" && res.filepath == "" {
		res.errMsg = "yt-dlp did not report the downloaded file"
	}
	if res.errMsg != "" || q.stopped(j, runID) {
		return res
	}
	vcodec, acodec := parser.VCodec, parser.ACodec
	if vcodec == "" && fits {
		vcodec, acodec = choice.VCodec, choice.ACodec
	}
	return q.finalise(ctx, j, runID, tools, res.filepath, vcodec, acodec, plan, planErr, duration)
}

// planOutcome turns a core.PlanEncode error into a job error, with the longest length that
// would fit as the detail when the video is too long.
func planOutcome(err error) outcome {
	res := outcome{errMsg: err.Error()}
	var tooLong *core.TooLongError
	if errors.As(err, &tooLong) {
		res.detail = tooLong.Detail()
	}
	return res
}

// finalise decides what happens to a finished working download: an MP4 holding H.264 + AAC
// within the limit is kept as it is, anything else is converted. This measures the file
// itself, so a fit whose estimated sizes were low is converted rather than delivered over the
// limit, and a source that already fits is never re-encoded (and padded up to the limit).
func (q *JobQueue) finalise(ctx context.Context, j *job, runID int, tools core.Tools, path, vcodec, acodec string,
	plan core.EncodePlan, planErr error, duration float64) outcome {
	info, err := os.Stat(path)
	if err != nil {
		return outcome{errMsg: "Could not find the downloaded video", detail: err.Error()}
	}
	limitMB := j.options.MaxSizeMB
	size := info.Size()
	playable := core.PlaysAsIs(filepath.Ext(path), vcodec, acodec)
	if playable && size <= core.LimitBytes(limitMB) {
		log.Printf("jobs: %s downloaded at %.2f MB, within %d MB as it is", j.videoID, float64(size)/core.BytesPerMB, limitMB)
		res, stopped := q.commit(j, runID, path, workBase(path, workTag(j)))
		if res.errMsg != "" && !stopped {
			res.detail = fmt.Sprintf("The video was kept as %s. %s", filepath.Base(path), res.detail)
		}
		return res
	}
	log.Printf("jobs: %s downloaded at %.2f MB (%s %s), converting", j.videoID, float64(size)/core.BytesPerMB, vcodec, acodec)
	if planErr != nil {
		removeWithRetry(path)
		return planOutcome(planErr)
	}
	state := &sizedState{source: path, plan: plan.CappedToSource(size, duration), duration: duration}
	q.mu.Lock()
	// A cancel clears sized and removes the working file itself, so only a live or paused run
	// keeps it
	if j.stoppedRun != runID || j.status == StatusPaused {
		j.sized = state
	}
	q.mu.Unlock()
	return q.convert(ctx, j, runID, tools, *state)
}

// commit moves a finished file to the job's final name, base (the "dir/Title [id]" of its
// working file) without its " [id]", plus " (N MB).mp4", and marks the job complete in the
// same locked step (see moveToFinal). stopped is true when a pause or cancel landed first.
//
// Unlike a normal download, a failed rename fails the job: the file left behind is a working
// file ("Title [id].<tag>.sized.mp4") that cleanup would treat as partial, so the outcome names
// it in keep and the caller explains where it is.
func (q *JobQueue) commit(j *job, runID int, from, base string) (res outcome, stopped bool) {
	stem, _ := core.StripID(base, j.videoID)
	final, stopped, err := q.moveToFinal(j, runID, from, stem+fmt.Sprintf(" (%d MB)", j.options.MaxSizeMB), ".mp4")
	switch {
	case stopped:
		return outcome{}, true
	case err != nil:
		log.Printf("jobs: could not save %s: %v", j.videoID, err)
		return outcome{errMsg: "Could not save the video", detail: err.Error(), keep: from}, false
	}
	return outcome{filepath: final}, false
}

// pickEncoder returns the encoder to convert with: the probe's pick, unless a real encode with
// it already failed in this process, in which case libx264.
func (q *JobQueue) pickEncoder(ctx context.Context, ffmpeg string) string {
	encoder := q.encoder(ctx, ffmpeg)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failedEncoders[encoder] {
		return core.EncoderX264
	}
	return encoder
}

// convert re-encodes st.source to fit the job's limit and returns the outcome; a stopped run
// returns an empty outcome, which settle ignores.
//
// A hardware encoder that fails is dropped for the rest of the process and the attempt is
// rerun with libx264. An encode that still comes out over the limit is retried once at
// retryBitrateFactor of the bitrate. On success the encode is committed and the working file
// deleted; on failure both are deleted, except that an encode that fits but cannot be moved
// to its final name is kept. A pause keeps the working file for the resume.
func (q *JobQueue) convert(ctx context.Context, j *job, runID int, tools core.Tools, st sizedState) outcome {
	q.mu.Lock()
	if j.stoppedRun == runID {
		q.mu.Unlock()
		return outcome{}
	}
	j.status, j.stage = StatusProcessing, StageConverting
	j.percent, j.speed, j.eta = 0, "", ""
	q.mu.Unlock()

	ffmpeg := core.FfmpegPath(tools)
	encoder := q.pickEncoder(ctx, ffmpeg)
	if q.stopped(j, runID) {
		return outcome{}
	}
	output := encodePath(st.source, workTag(j))
	limitMB := j.options.MaxSizeMB
	limit := core.LimitBytes(limitMB)
	tempDir := core.ChildTempDir()
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	passLog := filepath.Join(tempDir, "vidgrab-pass-"+j.id)
	// Count the conversion as a live child for its whole length, so the temp folder holding
	// the pass log is never swept between libx264's two passes (see core.TrackChild)
	exited := core.TrackChild()
	defer exited(false)
	defer core.RemovePassLogs(passLog)

	discard := func() {
		removeWithRetry(output)
		removeWithRetry(st.source)
	}
	var sizes []string
	for attempt := 0; attempt < 2; attempt++ {
		plan := st.plan
		if attempt > 0 {
			plan = plan.Scaled(retryBitrateFactor)
		}
		enc := core.EncodeJob{Ffmpeg: ffmpeg, Encoder: encoder, Source: st.source, Output: output,
			PassLog: passLog, Plan: plan}
		log.Printf("jobs: converting %s with %s at %d kbps", j.videoID, encoder, plan.VideoKbps)
		res, size := q.encode(j, runID, enc, st.duration)
		if res.errMsg == errConvert && encoder != core.EncoderX264 {
			log.Printf("jobs: %s failed for %s (%s), retrying with libx264", encoder, j.videoID, res.detail)
			q.mu.Lock()
			q.failedEncoders[encoder] = true
			q.mu.Unlock()
			encoder, enc.Encoder = core.EncoderX264, core.EncoderX264
			res, size = q.encode(j, runID, enc, st.duration)
		}
		if q.stopped(j, runID) {
			removeWithRetry(output)
			return outcome{}
		}
		if res.errMsg != "" {
			discard()
			if encoder == core.EncoderX264 && strings.Contains(res.detail, "Unknown encoder") {
				return outcome{errMsg: "This ffmpeg cannot encode H.264",
					detail: "No hardware encoder worked and this ffmpeg has no libx264.\n" + res.detail}
			}
			return res
		}
		if size <= limit {
			res, stopped := q.commit(j, runID, output, workBase(st.source, workTag(j)))
			switch {
			case stopped:
				removeWithRetry(output)
				return outcome{}
			case res.errMsg != "":
				res.errMsg = "Could not save the converted video"
				res.detail = fmt.Sprintf("The converted video was kept as %s. %s", filepath.Base(output), res.detail)
			}
			removeWithRetry(st.source)
			return res
		}
		sizes = append(sizes, fmt.Sprintf("%.2f MB", float64(size)/core.BytesPerMB))
		log.Printf("jobs: %s came out at %s, over %d MB", j.videoID, sizes[len(sizes)-1], limitMB)
	}
	discard()
	return outcome{
		errMsg: fmt.Sprintf("Could not fit %d MB", limitMB),
		detail: fmt.Sprintf("The converted video came out at %s, over the %d MB limit.", strings.Join(sizes, ", then "), limitMB),
	}
}

// encode runs every ffmpeg pass of enc and returns the size of the file written. A failed pass
// returns an errConvert outcome (or "Could not start ffmpeg"); a stopped run returns an empty
// one.
func (q *JobQueue) encode(j *job, runID int, enc core.EncodeJob, duration float64) (outcome, int64) {
	passes := enc.Passes()
	progress := &core.FfmpegProgress{Duration: duration, Passes: len(passes)}
	q.mu.Lock()
	if j.stoppedRun != runID {
		j.percent = 0
	}
	q.mu.Unlock()
	for i, argv := range passes {
		if i > 0 {
			progress.NextPass()
		}
		code, err := q.runProcess(j, runID, argv, func(line string) {
			update := progress.Feed(line)
			if update == nil {
				return
			}
			q.mu.Lock()
			defer q.mu.Unlock()
			if j.stoppedRun == runID {
				return
			}
			if update.Percent != nil {
				j.percent = *update.Percent
			}
			j.speed, j.eta = update.Speed, update.Eta
		})
		if err != nil {
			log.Printf("jobs: could not start ffmpeg for %s: %v", j.videoID, err)
			return outcome{errMsg: "Could not start ffmpeg", detail: err.Error()}, 0
		}
		if q.stopped(j, runID) {
			return outcome{}, 0
		}
		if code != 0 {
			detail := progress.Detail()
			if detail == "" {
				detail = fmt.Sprintf("ffmpeg exited with code %d", code)
			}
			return outcome{errMsg: errConvert, detail: detail}, 0
		}
	}
	info, err := os.Stat(enc.Output)
	if err != nil {
		return outcome{errMsg: errConvert, detail: "ffmpeg wrote no file"}, 0
	}
	return outcome{}, info.Size()
}

// removeWork deletes a size-limited job's working files (its download, any .part pieces of
// it and a half-written encode). Only names carrying the job's own "[id].<tag>." are touched,
// so other jobs for the same video are safe. keep, when set, is a path left in place.
func (q *JobQueue) removeWork(j *job, keep string) {
	marker := "[" + j.videoID + "]." + workTag(j) + "."
	entries, err := os.ReadDir(j.outDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(j.outDir, name)
		if strings.Contains(name, marker) && partialRe.MatchString(name) && path != keep {
			removeWithRetry(path)
		}
	}
}

// removeWithRetry deletes path, retrying briefly because Windows can hold the handle for a
// moment after a process is killed. A missing file is fine.
func removeWithRetry(path string) {
	for try := 0; try < 5; try++ {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

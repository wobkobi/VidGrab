// Final file names. yt-dlp writes "Title [id].ext" (core.OutputTemplate), so two videos with
// one title never share a working or finished file; once a job is done its file moves to a
// free name without the id: "Title.ext" for a normal download, "Title (N MB).mp4" for a
// size-limited one, with " (2)", " (3)" and so on when that name is taken.

package server

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vidgrab/internal/core"
)

// maxNameTries bounds the " (2)", " (3)" suffixes freeName tries before giving up.
const maxNameTries = 100

// renameTries is how many times moveToFinal tries the rename, 200 ms apart.
const renameTries = 5

// freeName returns stem+ext, or the first of stem+" (2)"+ext, stem+" (3)"+ext... that does not
// exist yet.
func freeName(stem, ext string) (string, error) {
	for n := 1; n <= maxNameTries; n++ {
		name := stem + ext
		if n > 1 {
			name = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		_, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s%s and %d numbered copies already exist", filepath.Base(stem), ext, maxNameTries-1)
}

// moveToFinal moves a finished file to freeName(stem, ext), never over an existing file, and
// marks the job complete with that file in the same locked step. It returns the final path,
// or stopped when a pause, cancel or shutdown claimed the run first.
//
// Checking the run, picking the name and renaming under the lock means a pause or cancel either
// lands first (nothing is renamed) or finds the job already complete and is refused, so a
// stopped job never has its file moved behind its back, and two jobs finishing with one title
// cannot pick the same free name. The rename is retried for about a second because Windows can
// briefly hold a handle on a file ffmpeg, yt-dlp or an antivirus scanner just closed; on
// persistent failure the last error is returned and from stays where it is.
func (q *JobQueue) moveToFinal(j *job, runID int, from, stem, ext string) (final string, stopped bool, err error) {
	for try := 0; try < renameTries; try++ {
		if try > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		q.mu.Lock()
		if j.stoppedRun == runID {
			q.mu.Unlock()
			return "", true, nil
		}
		if final, err = freeName(stem, ext); err == nil {
			err = q.rename(from, final)
		}
		if err == nil {
			j.status, j.stage = StatusComplete, ""
			j.percent, j.speed, j.eta = 100, "", ""
			j.filepath, j.filename = final, filepath.Base(final)
			q.mu.Unlock()
			return final, false, nil
		}
		q.mu.Unlock()
	}
	return "", false, err
}

// finishDownload moves a normal download from the "Title [id].ext" yt-dlp reported to its
// id-less final name and returns the outcome to settle.
//
// The file keeps its yt-dlp name (and the job still completes with it) when there is nothing
// this job should rename: yt-dlp found the file already on disk (preExisting, normally a
// download made before files lost their id, which is left as the user has it), the name lacks
// the " [id]" marker, the file is not there, or the rename keeps failing. The exception is a
// file this job downloaded itself in an earlier run that was stopped just before its rename:
// yt-dlp reports that one as already downloaded too, but it is still renamed.
func (q *JobQueue) finishDownload(j *job, runID int, path string, preExisting bool) outcome {
	keep := outcome{filepath: path}
	q.mu.Lock()
	own := core.SamePath(j.unrenamed, path)
	q.mu.Unlock()
	if preExisting && !own {
		log.Printf("jobs: %s was already downloaded as %s, leaving its name", j.videoID, filepath.Base(path))
		return keep
	}
	ext := filepath.Ext(path)
	stem, ok := core.StripID(strings.TrimSuffix(path, ext), j.videoID)
	if !ok {
		return keep
	}
	if _, err := os.Lstat(path); err != nil {
		return keep
	}
	final, stopped, err := q.moveToFinal(j, runID, path, stem, ext)
	switch {
	case stopped:
		// The next run (resume or retry) finds this file "already downloaded" and renames it
		q.mu.Lock()
		j.unrenamed = path
		q.mu.Unlock()
		return outcome{}
	case err != nil:
		log.Printf("jobs: could not rename %s, keeping it as %s: %v", j.videoID, filepath.Base(path), err)
		return keep
	}
	return outcome{filepath: final}
}

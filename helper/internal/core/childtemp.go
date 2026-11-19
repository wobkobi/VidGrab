// Private temp folder for yt-dlp children, and the live-process count that decides when it
// is safe to empty.
//
// The standalone yt-dlp build is a PyInstaller onefile program: it unpacks about 30 MB into
// a "_MEI*" folder under TEMP and deletes it on exit, but a hard kill (pause, cancel, an
// /info timeout, shutdown) skips that clean-up. yt-dlp's cookie database copies land in TEMP
// too. Pointing TEMP at a folder only VidGrab uses lets the helper delete those leftovers
// wholesale once no yt-dlp process is alive.

package core

import (
	"os"
	"path/filepath"
	"sync"
)

var childTemp struct {
	mu   sync.Mutex
	dir  string
	live int
	// dirty is set by a killed run; the folder is emptied once live drops to 0
	dirty bool
}

// SetChildTempDir makes ChildEnv point TEMP, TMP and TMPDIR at dir ("" restores the
// inherited values). The caller creates dir.
func SetChildTempDir(dir string) {
	childTemp.mu.Lock()
	childTemp.dir = dir
	childTemp.mu.Unlock()
}

// ChildTempDir returns the folder set by SetChildTempDir, or "".
func ChildTempDir() string {
	childTemp.mu.Lock()
	defer childTemp.mu.Unlock()
	return childTemp.dir
}

// TrackChild records a yt-dlp process about to start. Call it before the start and call the
// returned function exactly once after the process has exited (or failed to start), with
// killed set when the run was killed rather than allowed to finish. Once the last live
// process has exited after a killed run, the temp folder is emptied.
func TrackChild() (exited func(killed bool)) {
	childTemp.mu.Lock()
	childTemp.live++
	childTemp.mu.Unlock()
	var once sync.Once
	return func(killed bool) {
		once.Do(func() {
			childTemp.mu.Lock()
			defer childTemp.mu.Unlock()
			childTemp.live--
			if killed {
				childTemp.dirty = true
			}
			if childTemp.live == 0 && childTemp.dirty {
				childTemp.dirty = false
				sweepLocked()
			}
		})
	}
}

// SweepChildTemp empties the temp folder unless a yt-dlp process is alive, and reports
// whether it ran. The server calls it at start, before any yt-dlp runs.
func SweepChildTemp() bool {
	childTemp.mu.Lock()
	defer childTemp.mu.Unlock()
	if childTemp.live > 0 {
		return false
	}
	childTemp.dirty = false
	sweepLocked()
	return true
}

// sweepLocked deletes everything inside the temp folder, keeping the folder itself. Errors
// are ignored: a file still locked is retried on the next sweep. The caller holds
// childTemp.mu, so no tracked process can start meanwhile.
func sweepLocked() {
	if childTemp.dir == "" {
		return
	}
	entries, err := os.ReadDir(childTemp.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(childTemp.dir, entry.Name()))
	}
}

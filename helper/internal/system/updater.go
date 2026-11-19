// yt-dlp version check against GitHub releases, and checksum-verified self-update.

package system

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// UpdateCheckInterval is the minimum time between two successful release checks.
const UpdateCheckInterval = 24 * time.Hour

// UpdateRetryInterval is the minimum time between failed release checks within one run. A
// failure is not saved, so the next start checks again whatever this says.
const UpdateRetryInterval = time.Hour

// installedRetry is the minimum time between two background reads of the installed version
// started by WarmInstalled after one came back empty.
const installedRetry = 30 * time.Second

// UpdateStateFile is the update state's file name inside the data directory.
const UpdateStateFile = "update_state.json"

// ErrUpdateBusy means downloads are running, so yt-dlp must not be replaced right now (the
// server answers 409).
// The text is shown to the user as is.
var ErrUpdateBusy = errors.New("Wait for downloads to finish before updating yt-dlp")

// UpdateError means the yt-dlp update failed; Detail holds the underlying cause.
type UpdateError struct {
	Message string
	Detail  string
}

// Error joins Message and Detail.
func (e *UpdateError) Error() string {
	if e.Detail == "" {
		return e.Message
	}
	return e.Message + ": " + e.Detail
}

// UpdateResult is the body of a successful POST /update.
type UpdateResult struct {
	Updated bool   `json:"updated"`
	Version string `json:"version"`
}

// Updater tracks the latest yt-dlp release (checked at most once a day) and installs updates.
//
// The last successful check is stored in the state file so restarts do not hit GitHub each
// time. The installed version is cached in memory: reading it runs `yt-dlp --version`, which
// takes seconds for the self-extracting Windows build, so /health only ever reads the cache
// and RefreshInstalled fills it in the background. The function fields are the seams tests
// replace.
type Updater struct {
	// FetchLatest returns the newest release tag; an error keeps the previous value.
	FetchLatest func(ctx context.Context) (string, error)
	// Install replaces yt-dlp with the newest release.
	Install func(ctx context.Context) error
	// Installed returns the installed version, or "" when unknown. It may be slow.
	Installed func(ctx context.Context) string
	// Busy reports whether downloads are running; Update refuses while it returns true.
	Busy func() bool
	// Now is the clock.
	Now func() time.Time

	statePath string
	mu        sync.Mutex
	updateMu  sync.Mutex
	state     updateState
	// failedAt is when the last release check failed (zero after a success); never saved
	failedAt time.Time

	// Installed version cache. installedGen counts installs, so a read that started before
	// an update cannot overwrite the version the update recorded.
	installedMu  sync.Mutex
	installed    string
	installedGen int
	refreshing   chan struct{} // closed when the running refresh ends; nil when none runs
	refreshedAt  time.Time
}

// updateState is the persisted check state. last_check is Unix seconds, as the Python
// backend stored it, so an existing file keeps working.
type updateState struct {
	LastCheck float64 `json:"last_check,omitempty"`
	Latest    string  `json:"latest,omitempty"`
	Attempted string  `json:"attempted,omitempty"`
}

// NewUpdater returns an Updater whose release check and update go through tools, storing
// its state at statePath (normally DataDir()/update_state.json).
func NewUpdater(statePath string, tools *Tools) *Updater {
	u := &Updater{
		FetchLatest: tools.LatestYtdlpVersion,
		Install: func(ctx context.Context) error {
			_, err := tools.Update(ctx)
			return err
		},
		Installed: func(ctx context.Context) string {
			version, err := tools.Version(ctx)
			if err != nil {
				return ""
			}
			return version
		},
		Now: time.Now,
	}
	return u.init(statePath)
}

// init points u at statePath and loads any saved state.
func (u *Updater) init(statePath string) *Updater {
	u.statePath = statePath
	if u.Now == nil {
		u.Now = time.Now
	}
	u.state = u.loadState()
	return u
}

// Latest returns the newest known release, or "" if never checked.
func (u *Updater) Latest() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state.Latest
}

// CachedInstalled returns the cached installed version, or "" until a read succeeded.
func (u *Updater) CachedInstalled() string {
	u.installedMu.Lock()
	defer u.installedMu.Unlock()
	return u.installed
}

// InstalledVersion returns the cached installed yt-dlp version, or "unknown". It never runs
// yt-dlp.
func (u *Updater) InstalledVersion() string {
	if version := u.CachedInstalled(); version != "" {
		return version
	}
	return "unknown"
}

// UpdateAvailable reports whether the newest known release is newer than the cached
// installed version; false while either is unknown.
func (u *Updater) UpdateAvailable() bool {
	latest, installed := u.Latest(), u.CachedInstalled()
	return latest != "" && installed != "" && compareVersions(latest, installed) > 0
}

// RefreshInstalled reads the installed version through Installed and caches it; an empty
// answer keeps the previous value. Calls are single-flight: one that arrives while a read
// runs waits for that read (or for ctx) instead of starting another yt-dlp. Pass a context
// that lives as long as the server, not a request's, so a slow read is never cut short.
func (u *Updater) RefreshInstalled(ctx context.Context) {
	u.installedMu.Lock()
	if running := u.refreshing; running != nil {
		u.installedMu.Unlock()
		select {
		case <-running:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	u.refreshing = done
	gen := u.installedGen
	u.installedMu.Unlock()

	version := u.Installed(ctx)

	u.installedMu.Lock()
	defer u.installedMu.Unlock()
	if version != "" && gen == u.installedGen {
		u.installed = version
	}
	u.refreshedAt = u.Now()
	u.refreshing = nil
	close(done)
}

// WarmInstalled starts RefreshInstalled in the background when the version is still
// unknown, unless a read is already running or the last one ended under installedRetry ago.
// It returns at once.
func (u *Updater) WarmInstalled(ctx context.Context) {
	u.installedMu.Lock()
	skip := u.installed != "" || u.refreshing != nil ||
		(!u.refreshedAt.IsZero() && u.Now().Sub(u.refreshedAt) < installedRetry)
	u.installedMu.Unlock()
	if !skip {
		go u.RefreshInstalled(ctx)
	}
}

// recordInstalled caches the version an install left behind.
func (u *Updater) recordInstalled(version string) {
	u.installedMu.Lock()
	defer u.installedMu.Unlock()
	u.installedGen++
	if version != "" {
		u.installed = version
	}
}

// Check refreshes the latest version from GitHub unless the last successful check is under
// a day old (or force is set), and returns the newest known release.
//
// Only a success is stamped and saved, so a failed check is retried on the next start, or
// within this run once UpdateRetryInterval has passed.
func (u *Updater) Check(ctx context.Context, force bool) string {
	u.mu.Lock()
	last := u.state.LastCheck
	due := force || u.nowSeconds()-last >= UpdateCheckInterval.Seconds()
	if !force && !u.failedAt.IsZero() && u.Now().Sub(u.failedAt) < UpdateRetryInterval {
		due = false
	}
	u.mu.Unlock()
	if !due {
		return u.Latest()
	}
	latest, err := u.FetchLatest(ctx)
	if err == nil && latest == "" {
		err = errors.New("no release tag")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		log.Printf("updater: version check failed: %v", err)
		u.failedAt = u.Now()
		return u.state.Latest
	}
	u.failedAt = time.Time{}
	u.state.LastCheck = u.nowSeconds()
	u.state.Latest = latest
	u.saveState()
	return u.state.Latest
}

// Update installs the newest yt-dlp release.
//
// It returns ErrUpdateBusy while Busy reports running downloads, and an *UpdateError when
// the download or verification fails (the server answers 502 with Message and Detail). Busy is checked once at the start; the caller should
// also keep new downloads from starting until Update returns.
func (u *Updater) Update(ctx context.Context) (UpdateResult, error) {
	if u.Busy != nil && u.Busy() {
		return UpdateResult{}, ErrUpdateBusy
	}
	u.updateMu.Lock()
	defer u.updateMu.Unlock()
	before := u.CachedInstalled()
	if before == "" {
		before = u.Installed(ctx)
	}
	if err := u.Install(ctx); err != nil {
		return UpdateResult{}, &UpdateError{Message: "Could not update yt-dlp", Detail: err.Error()}
	}
	after := u.Installed(ctx)
	u.recordInstalled(after)
	u.mu.Lock()
	if compareVersions(after, u.state.Latest) > 0 {
		u.state.Latest = after
	}
	u.state.Attempted = u.state.Latest
	u.saveState()
	u.mu.Unlock()
	log.Printf("updater: yt-dlp %s > %s", before, after)
	return UpdateResult{Updated: after != before, Version: after}, nil
}

// AutoUpdate runs the daily check and, when enabled() allows, installs an available update.
//
// Each release is tried once: an install that settles on an older version marks the
// release as tried, so later startups do not download again for the same tag. A postponed
// update (downloads running) or a failed one is not marked, so the next startup retries.
// Failures are logged, never returned, as this runs in the background.
func (u *Updater) AutoUpdate(ctx context.Context, enabled func() bool) {
	if u.CachedInstalled() == "" {
		u.RefreshInstalled(ctx)
	}
	latest := u.Check(ctx, false)
	u.mu.Lock()
	attempted := u.state.Attempted
	u.mu.Unlock()
	if !enabled() || !u.UpdateAvailable() || attempted == latest {
		return
	}
	if _, err := u.Update(ctx); err != nil {
		if errors.Is(err, ErrUpdateBusy) {
			log.Printf("updater: automatic yt-dlp update postponed: downloads are running")
			return
		}
		log.Printf("updater: automatic yt-dlp update failed: %v", err)
	}
}

func (u *Updater) nowSeconds() float64 {
	return float64(u.Now().UnixNano()) / 1e9
}

// loadState reads the saved check state, ignoring a missing or corrupt file.
func (u *Updater) loadState() updateState {
	var state updateState
	data, err := os.ReadFile(u.statePath)
	if err != nil {
		return updateState{}
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return updateState{}
	}
	return state
}

// saveState writes the check state; the caller holds u.mu.
func (u *Updater) saveState() {
	data, err := json.Marshal(u.state)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(u.statePath), 0o700); err == nil {
			err = os.WriteFile(u.statePath, data, 0o600)
		}
	}
	if err != nil {
		log.Printf("updater: could not save update state: %v", err)
	}
}

var digitsRe = regexp.MustCompile(`\d+`)

// versionKey turns "2026.03.03" or "2026.03.03.1" into its numeric parts; "" or "unknown"
// gives none, which sorts lowest.
func versionKey(version string) []int {
	var key []int
	for _, part := range digitsRe.FindAllString(version, -1) {
		n, err := strconv.Atoi(part)
		if err != nil {
			n = 0
		}
		key = append(key, n)
	}
	return key
}

// compareVersions compares two versions part by part like Python tuple comparison: -1, 0 or 1.
// A version that is a prefix of the other sorts first (2026.03.03 < 2026.03.03.1).
func compareVersions(a, b string) int {
	ka, kb := versionKey(a), versionKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			if ka[i] < kb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ka) < len(kb):
		return -1
	case len(ka) > len(kb):
		return 1
	}
	return 0
}

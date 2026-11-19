package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRelease is a mutable fake GitHub, installer and clock for Updater tests.
type fakeRelease struct {
	latest       string
	fetchErr     error
	installed    string
	now          time.Time
	fetchCalls   int
	installCalls int
	installErr   error
	// installTo is what an install leaves behind; "" means the latest release
	installTo string
	busy      bool
}

func newFakeRelease() *fakeRelease {
	return &fakeRelease{latest: "2026.11.01", installed: "2026.03.03", now: time.Unix(1_000_000, 0)}
}

func (f *fakeRelease) updater(path string) *Updater {
	u := &Updater{
		FetchLatest: func(context.Context) (string, error) {
			f.fetchCalls++
			return f.latest, f.fetchErr
		},
		Install: func(context.Context) error {
			f.installCalls++
			if f.installErr != nil {
				return f.installErr
			}
			if f.installTo != "" {
				f.installed = f.installTo
			} else {
				f.installed = f.latest
			}
			return nil
		},
		Installed: func(context.Context) string { return f.installed },
		Busy:      func() bool { return f.busy },
		Now:       func() time.Time { return f.now },
	}
	return u.init(path)
}

var bg = context.Background()

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.11.01", "2026.03.03", 1},
		{"2026.03.03.1", "2026.03.03", 1},
		{"2026.03.03", "2026.03.03", 0},
		{"", "2020.01.01", -1},
		{"unknown", "2020.01.01", -1},
		{"2026.10.21", "2026.10.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckAtMostOnceADay(t *testing.T) {
	f := newFakeRelease()
	path := filepath.Join(t.TempDir(), "state.json")
	u := f.updater(path)
	if got := u.Check(bg, false); got != "2026.11.01" {
		t.Fatalf("Check() = %q", got)
	}
	u.Check(bg, false)
	if f.fetchCalls != 1 {
		t.Fatalf("fetchCalls = %d, want 1", f.fetchCalls)
	}
	// State survives a restart
	if got := f.updater(path).Check(bg, false); got != "2026.11.01" || f.fetchCalls != 1 {
		t.Fatalf("after restart: %q, fetchCalls %d", got, f.fetchCalls)
	}
	f.now = f.now.Add(UpdateCheckInterval)
	u.Check(bg, false)
	if f.fetchCalls != 2 {
		t.Fatalf("fetchCalls = %d, want 2", f.fetchCalls)
	}
	u.Check(bg, true)
	if f.fetchCalls != 3 {
		t.Fatalf("fetchCalls = %d, want 3", f.fetchCalls)
	}
}

func TestOfflineCheckKeepsPreviousLatest(t *testing.T) {
	f := newFakeRelease()
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	u.Check(bg, false)
	f.latest, f.fetchErr = "", errors.New("offline")
	if got := u.Check(bg, true); got != "2026.11.01" {
		t.Fatalf("Check() = %q, want the previous latest", got)
	}
}

func TestUpdateAvailable(t *testing.T) {
	f := newFakeRelease()
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	if u.UpdateAvailable() {
		t.Fatal("no check yet, so no update known")
	}
	u.Check(bg, false)
	if u.UpdateAvailable() {
		t.Fatal("installed version not read yet, so no update known")
	}
	u.RefreshInstalled(bg)
	if !u.UpdateAvailable() {
		t.Fatal("newer release should be available")
	}
	f.installed = "2026.11.01"
	u.RefreshInstalled(bg)
	if u.UpdateAvailable() {
		t.Fatal("installed is the latest")
	}
}

func TestUpdateSuccessAndFailure(t *testing.T) {
	f := newFakeRelease()
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	u.Check(bg, false)
	if got, err := u.Update(bg); err != nil || got != (UpdateResult{Updated: true, Version: "2026.11.01"}) {
		t.Fatalf("Update() = %+v, %v", got, err)
	}
	if got, err := u.Update(bg); err != nil || got != (UpdateResult{Updated: false, Version: "2026.11.01"}) {
		t.Fatalf("second Update() = %+v, %v", got, err)
	}
	f.installErr = errors.New("checksum mismatch")
	_, err := u.Update(bg)
	var updateErr *UpdateError
	if !errors.As(err, &updateErr) || updateErr.Detail != "checksum mismatch" {
		t.Fatalf("want an UpdateError with the cause as detail, got %v", err)
	}
}

func TestUpdateRefusedWhileBusy(t *testing.T) {
	f := newFakeRelease()
	f.busy = true
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	if _, err := u.Update(bg); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("want ErrUpdateBusy, got %v", err)
	}
	if f.installCalls != 0 {
		t.Fatal("nothing may be installed while downloads run")
	}
}

func TestAutoUpdate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		f := newFakeRelease()
		u := f.updater(filepath.Join(t.TempDir(), "state.json"))
		u.AutoUpdate(bg, func() bool { return enabled })
		want := 0
		if enabled {
			want = 1
		}
		if f.installCalls != want {
			t.Errorf("enabled=%v: installCalls = %d, want %d", enabled, f.installCalls, want)
		}
	}
}

func TestAutoUpdatePostponedWhileBusy(t *testing.T) {
	f := newFakeRelease()
	f.busy = true
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	u.AutoUpdate(bg, func() bool { return true })
	if f.installCalls != 0 {
		t.Fatal("must not install while busy")
	}
	// Not marked as attempted, so the next run tries again
	f.busy = false
	u.AutoUpdate(bg, func() bool { return true })
	if f.installCalls != 1 {
		t.Fatalf("installCalls = %d, want 1", f.installCalls)
	}
}

// TestAutoUpdateTriesEachReleaseOnce covers an install that settles on an older version
// than the latest tag: later runs must not download again for the same release.
func TestAutoUpdateTriesEachReleaseOnce(t *testing.T) {
	f := newFakeRelease()
	f.installTo = "2026.10.21"
	path := filepath.Join(t.TempDir(), "state.json")
	u := f.updater(path)
	u.AutoUpdate(bg, func() bool { return true })
	if !u.UpdateAvailable() {
		t.Fatal("the partial update leaves a newer release available")
	}
	f.updater(path).AutoUpdate(bg, func() bool { return true })
	if f.installCalls != 1 {
		t.Fatalf("installCalls = %d, want 1", f.installCalls)
	}
	f.latest = "2026.12.01"
	f.now = f.now.Add(UpdateCheckInterval)
	f.updater(path).AutoUpdate(bg, func() bool { return true })
	if f.installCalls != 2 {
		t.Fatalf("installCalls = %d, want 2 after a new release", f.installCalls)
	}
}

func TestAutoUpdateSwallowsFailure(t *testing.T) {
	f := newFakeRelease()
	f.installErr = errors.New("network down")
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	u.AutoUpdate(bg, func() bool { return true })
	if f.installCalls != 1 {
		t.Fatalf("installCalls = %d, want 1", f.installCalls)
	}
}

func TestCorruptStateFileIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := newFakeRelease().updater(path).Latest(); got != "" {
		t.Fatalf("Latest() = %q, want empty", got)
	}
}

func TestReadsPythonStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"last_check": 1000000.5, "latest": "2026.10.21", "attempted": "2026.10.21"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeRelease()
	u := f.updater(path)
	if got := u.Check(bg, false); got != "2026.10.21" || f.fetchCalls != 0 {
		t.Fatalf("Check() = %q with %d fetches; want the saved state reused", got, f.fetchCalls)
	}
}

func TestInstalledVersionUnknown(t *testing.T) {
	f := newFakeRelease()
	f.installed = ""
	u := f.updater(filepath.Join(t.TempDir(), "s.json"))
	u.RefreshInstalled(bg)
	if got := u.InstalledVersion(); got != "unknown" {
		t.Fatalf("InstalledVersion() = %q", got)
	}
}

func TestFailedCheckIsNotStamped(t *testing.T) {
	f := newFakeRelease()
	f.latest, f.fetchErr = "", errors.New("offline")
	path := filepath.Join(t.TempDir(), "state.json")
	u := f.updater(path)
	u.Check(bg, false)
	// Within the retry interval the same run does not ask again
	u.Check(bg, false)
	if f.fetchCalls != 1 {
		t.Fatalf("fetchCalls = %d, want 1", f.fetchCalls)
	}
	// A restart retries at once: nothing was saved
	f.latest, f.fetchErr = "2026.11.01", nil
	if got := f.updater(path).Check(bg, false); got != "2026.11.01" || f.fetchCalls != 2 {
		t.Fatalf("after restart: %q, fetchCalls %d", got, f.fetchCalls)
	}
	// So does the same run once the retry interval has passed
	f.fetchErr = errors.New("offline")
	u2 := f.updater(filepath.Join(t.TempDir(), "other.json"))
	u2.Check(bg, false)
	f.fetchErr = nil
	f.now = f.now.Add(UpdateRetryInterval)
	if got := u2.Check(bg, false); got != "2026.11.01" || f.fetchCalls != 4 {
		t.Fatalf("after the retry interval: %q, fetchCalls %d", got, f.fetchCalls)
	}
}

// slowInstalled is an Installed seam that blocks until released and counts its calls.
type slowInstalled struct {
	calls   atomic.Int32
	release chan struct{}
	version atomic.Value
}

func newSlowInstalled(version string) *slowInstalled {
	s := &slowInstalled{release: make(chan struct{})}
	s.version.Store(version)
	return s
}

func (s *slowInstalled) read(ctx context.Context) string {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ""
	}
	return s.version.Load().(string)
}

func TestRefreshInstalledIsSingleFlight(t *testing.T) {
	slow := newSlowInstalled("2026.03.03")
	u := (&Updater{Installed: slow.read}).init(filepath.Join(t.TempDir(), "s.json"))
	if u.InstalledVersion() != "unknown" {
		t.Fatal("version known before any read")
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.RefreshInstalled(bg)
		}()
	}
	if !waitFor(5*time.Second, func() bool { return slow.calls.Load() == 1 }) {
		t.Fatal("no read started")
	}
	// WarmInstalled must not start a second yt-dlp either
	u.WarmInstalled(bg)
	time.Sleep(50 * time.Millisecond)
	if u.InstalledVersion() != "unknown" {
		t.Fatal("version known before the read finished")
	}
	close(slow.release)
	wg.Wait()
	if n := slow.calls.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	if u.InstalledVersion() != "2026.03.03" {
		t.Fatalf("InstalledVersion() = %q", u.InstalledVersion())
	}
}

func TestWarmInstalledRetriesAfterAPause(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	var calls int
	version := ""
	u := (&Updater{
		Installed: func(context.Context) string {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return version
		},
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	}).init(filepath.Join(t.TempDir(), "s.json"))
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	u.WarmInstalled(bg)
	if !waitFor(5*time.Second, func() bool { return count() == 1 }) {
		t.Fatal("WarmInstalled did not read the version")
	}
	waitFor(time.Second, func() bool {
		u.installedMu.Lock()
		defer u.installedMu.Unlock()
		return u.refreshing == nil
	})
	u.WarmInstalled(bg)
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Fatal("a failed read was retried at once")
	}
	mu.Lock()
	now = now.Add(installedRetry)
	version = "2026.03.03"
	mu.Unlock()
	u.WarmInstalled(bg)
	if !waitFor(5*time.Second, func() bool { return u.InstalledVersion() == "2026.03.03" }) {
		t.Fatal("the retry did not fill in the version")
	}
	u.WarmInstalled(bg)
	time.Sleep(50 * time.Millisecond)
	if count() != 2 {
		t.Fatalf("%d reads, want 2 (a known version is not read again)", count())
	}
}

func TestReadBeforeUpdateDoesNotOverwriteIt(t *testing.T) {
	slow := newSlowInstalled("2026.03.03")
	u := (&Updater{Installed: slow.read}).init(filepath.Join(t.TempDir(), "s.json"))
	done := make(chan struct{})
	go func() {
		u.RefreshInstalled(bg)
		close(done)
	}()
	if !waitFor(5*time.Second, func() bool { return slow.calls.Load() == 1 }) {
		t.Fatal("no read started")
	}
	u.recordInstalled("2026.11.01")
	close(slow.release)
	<-done
	if got := u.InstalledVersion(); got != "2026.11.01" {
		t.Fatalf("InstalledVersion() = %q; the stale read won", got)
	}
}

func TestUpdateRecordsInstalledVersion(t *testing.T) {
	f := newFakeRelease()
	u := f.updater(filepath.Join(t.TempDir(), "state.json"))
	u.Check(bg, false)
	if _, err := u.Update(bg); err != nil {
		t.Fatal(err)
	}
	if got := u.InstalledVersion(); got != "2026.11.01" || u.UpdateAvailable() {
		t.Fatalf("after update: %q, update available %v", got, u.UpdateAvailable())
	}
}

package system

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vidgrab/internal/auth"
)

// testRegistryKey is a scratch HKCU key; tests never touch Firefox's real registration.
const testRegistryKey = `Software\VidGrabTest\NativeMessagingHosts\vidgrab`

type installFixture struct {
	in        *Installer
	self      string
	stops     int
	scheduled []string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	root := t.TempDir()
	self := filepath.Join(root, "Downloads", "VidGrab-Setup.exe")
	if err := os.MkdirAll(filepath.Dir(self), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("helper binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &installFixture{self: self}
	installDir := filepath.Join(root, "Local", AppName)
	f.in = &Installer{
		InstallDir:   installDir,
		ManifestPath: defaultManifestPath(installDir),
		RegistryKey:  testRegistryKey,
		StopServer: func() (bool, error) {
			f.stops++
			return false, nil
		},
		ScheduleRemoval: func(dir string) error {
			f.scheduled = append(f.scheduled, dir)
			return nil
		},
	}
	t.Cleanup(cleanupTestRegistry)
	return f
}

func TestHostManifestShape(t *testing.T) {
	data, err := json.Marshal(HostManifest(`C:\VidGrab\vidgrab.exe`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"allowed_extensions":["yt-dl-firefox@local"],"description":"VidGrab download helper","name":"vidgrab","path":"C:\\VidGrab\\vidgrab.exe","type":"stdio"}`
	if string(data) != want {
		t.Fatalf("got %s", data)
	}
}

func TestInstallCopiesAndRegisters(t *testing.T) {
	f := newInstallFixture(t)
	report, err := f.in.Install(f.self)
	if err != nil {
		t.Fatal(err)
	}
	if f.stops != 1 {
		t.Fatal("a running server must be stopped before the copy")
	}
	target := f.in.InstalledBinary()
	if filepath.Base(target) != ExeName("vidgrab") || report.InstalledPath != target {
		t.Fatalf("installed path %q", target)
	}
	if readFile(t, target) != "helper binary v1" {
		t.Fatal("binary not copied")
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(readFile(t, f.in.ManifestPath)), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["path"] != target || manifest["name"] != "vidgrab" || manifest["type"] != "stdio" {
		t.Fatalf("manifest %v", manifest)
	}
	if got := readTestRegistry(t); got != f.in.ManifestPath {
		t.Fatalf("registry default value %q, want %q", got, f.in.ManifestPath)
	}
	if !strings.Contains(report.String(), "Restart Firefox") {
		t.Fatalf("report %q", report.String())
	}

	// Reinstalling a newer download replaces the binary
	if err := os.WriteFile(f.self, []byte("helper binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.in.Install(f.self); err != nil {
		t.Fatal(err)
	}
	if readFile(t, target) != "helper binary v2" {
		t.Fatal("binary not replaced")
	}
	if left := leftovers(t, f.in.InstallDir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestInstallFromInstalledCopySkipsCopy(t *testing.T) {
	f := newInstallFixture(t)
	if _, err := f.in.Install(f.self); err != nil {
		t.Fatal(err)
	}
	report, err := f.in.Install(f.in.InstalledBinary())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "Already running from") {
		t.Fatalf("report %q", report.String())
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	f := newInstallFixture(t)
	if _, err := f.in.Install(f.self); err != nil {
		t.Fatal(err)
	}
	dataFile := filepath.Join(f.in.InstallDir, "data", "tools", "yt-dlp.exe")
	if err := os.MkdirAll(filepath.Dir(dataFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := f.in.Uninstall(f.self)
	if err != nil {
		t.Fatal(err)
	}
	if isDir(f.in.InstallDir) {
		t.Fatal("install dir still exists")
	}
	if _, err := os.Stat(f.in.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest still exists")
	}
	if testRegistryExists() {
		t.Fatal("registry key still exists")
	}
	if report.PendingRemoval || len(f.scheduled) != 0 {
		t.Fatal("removal from outside the install dir must be immediate")
	}

	// A second uninstall is harmless
	if _, err := f.in.Uninstall(f.self); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestUninstallFromInstalledCopy(t *testing.T) {
	f := newInstallFixture(t)
	if _, err := f.in.Install(f.self); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.in.InstallDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := f.in.Uninstall(f.in.InstalledBinary())
	if err != nil {
		t.Fatal(err)
	}
	if !report.PendingRemoval || len(f.scheduled) != 1 || f.scheduled[0] != f.in.InstallDir {
		t.Fatalf("PendingRemoval %v, scheduled %v", report.PendingRemoval, f.scheduled)
	}
	entries, _ := os.ReadDir(f.in.InstallDir)
	if len(entries) != 1 || entries[0].Name() != "vidgrab.exe" {
		t.Fatalf("only the running exe should remain, got %v", entries)
	}
}

func TestPathWithin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "VidGrab")
	if !pathWithin(filepath.Join(dir, "vidgrab.exe"), dir) {
		t.Fatal("file inside should match")
	}
	if pathWithin(filepath.Join(dir+"2", "vidgrab.exe"), dir) {
		t.Fatal("sibling folder with the same prefix must not match")
	}
	if !pathWithin(strings.ToUpper(filepath.Join(dir, "x.exe")), dir) {
		t.Fatal("paths compare case-insensitively")
	}
}

// fakeServer answers /health with a valid proof and records /shutdown calls.
func fakeServer(t *testing.T, token string) (*httptest.Server, int, *http.Header) {
	t.Helper()
	var mu sync.Mutex
	var shutdownHeaders http.Header
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "2.1.0", "proof": auth.Proof(token, r.URL.Query().Get("nonce"))})
		case "/shutdown":
			mu.Lock()
			shutdownHeaders = r.Header.Clone()
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			// Closing the listener frees the port, as the real server does on exit
			go srv.Listener.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, srv.Listener.Addr().(*net.TCPAddr).Port, &shutdownHeaders
}

func TestStopServerShutsDownOurServer(t *testing.T) {
	token := strings.Repeat("k", 43)
	_, port, headers := fakeServer(t, token)
	stopped, err := stopServer(port, token)
	if err != nil || !stopped {
		t.Fatalf("stopServer = %v, %v", stopped, err)
	}
	if headers.Get(auth.ClientHeader) != "1" || headers.Get(auth.TokenHeader) != token {
		t.Fatalf("shutdown headers %v", *headers)
	}
}

func TestStopServerLeavesForeignProgramAlone(t *testing.T) {
	_, port, headers := fakeServer(t, strings.Repeat("k", 43))
	stopped, err := stopServer(port, strings.Repeat("x", 43))
	if stopped || !errors.Is(err, auth.ErrForeignBackend) {
		t.Fatalf("stopServer = %v, %v; want a foreign backend error", stopped, err)
	}
	if *headers != nil {
		t.Fatal("a foreign program must not be sent /shutdown")
	}
}

func TestStopServerWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if stopped, err := stopServer(port, "tok"); stopped || err != nil {
		t.Fatalf("stopServer = %v, %v", stopped, err)
	}
}

func TestReadExistingTokenDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "token")
	if got := readExistingToken(path); got != "" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading must not create the token")
	}
}

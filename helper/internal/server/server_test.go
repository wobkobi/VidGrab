package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vidgrab/internal/auth"
	"vidgrab/internal/core"
	"vidgrab/internal/system"
)

// Token and proof (the API parts of the Python test_auth.py)

func TestAPIRequiresToken(t *testing.T) {
	e := newEnv(t, withToken(testToken))
	resp := e.get("/jobs")
	expectStatus(t, resp, 401)
	body := resp.json(t)
	if body["error"] != "Backend key rejected" || body["detail"] != "Reload the extension so it fetches the key again." {
		t.Fatalf("body %s", resp.body)
	}
	expectStatus(t, e.do("GET", "/jobs", nil, map[string]string{auth.TokenHeader: "wrong"}), 401)
	expectStatus(t, e.do("PUT", "/settings", map[string]any{"cookies": true}, nil), 401)
	expectStatus(t, e.do("POST", "/shutdown", nil, nil), 401)
	expectStatus(t, e.do("GET", "/jobs", nil, map[string]string{auth.TokenHeader: testToken}), 200)
}

func TestHealthIsOpenAndProvesToken(t *testing.T) {
	e := newEnv(t, withToken(testToken))
	plain := e.get("/health")
	expectStatus(t, plain, 200)
	if _, ok := plain.json(t)["proof"]; ok {
		t.Fatal("proof without a nonce")
	}
	body := e.get("/health?nonce=abc123").json(t)
	if body["proof"] != auth.Proof(testToken, "abc123") {
		t.Fatalf("proof %v", body["proof"])
	}
	expectStatus(t, e.get("/health?nonce="+strings.Repeat("n", auth.MaxNonceLength)), 200)
	resp := e.get("/health?nonce=" + strings.Repeat("n", 200))
	expectStatus(t, resp, 400)
	if detail := resp.json(t)["detail"]; detail != "query.nonce: String should have at most 128 characters" {
		t.Fatalf("detail %v", detail)
	}
}

func TestHealthWithoutTokenHasNoProof(t *testing.T) {
	e := newEnv(t)
	if _, ok := e.get("/health?nonce=abc").json(t)["proof"]; ok {
		t.Fatal("proof without a token")
	}
}

func TestCORSAllowsTokenHeader(t *testing.T) {
	e := newEnv(t, withToken(testToken))
	resp := e.do("OPTIONS", "/jobs", nil, map[string]string{
		"Origin":                         extOrigin,
		"Access-Control-Request-Method":  "GET",
		"Access-Control-Request-Headers": "x-ytdl-client, x-ytdl-token",
		"X-YTDL-Client":                  "",
	})
	expectStatus(t, resp, 200)
	if !strings.Contains(resp.header.Get("Access-Control-Allow-Headers"), "X-YTDL-Token") {
		t.Fatalf("allow headers %q", resp.header.Get("Access-Control-Allow-Headers"))
	}
}

// Lifecycle

func TestListenRefusesTakenPort(t *testing.T) {
	first, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	port := first.Addr().(*net.TCPAddr).Port
	if second, err := Listen(port); err == nil {
		second.Close()
		t.Fatal("second listener on a taken port")
	}
}

func TestRunExitsCleanlyWhenPortInUse(t *testing.T) {
	cases := []struct {
		name  string
		probe func(int, string, time.Duration) (map[string]any, error)
	}{
		{"nothing answers", func(int, string, time.Duration) (map[string]any, error) { return nil, nil }},
		{"our helper", func(int, string, time.Duration) (map[string]any, error) { return map[string]any{"status": "ok"}, nil }},
		{"foreign program", func(int, string, time.Duration) (map[string]any, error) {
			return nil, &auth.ForeignBackendError{Msg: "Port is used by another program"}
		}},
	}
	holder, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	port := holder.Addr().(*net.TCPAddr).Port
	original := probeBackend
	defer func() { probeBackend = original }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probed := 0
			probeBackend = func(p int, token string, timeout time.Duration) (map[string]any, error) {
				probed++
				if p != port || token == "" {
					t.Errorf("probe(%d, %q)", p, token)
				}
				return tc.probe(p, token, timeout)
			}
			dataDir := t.TempDir()
			// Another helper on this data directory may be running yt-dlp from here
			mei := filepath.Join(dataDir, "tmp", "_MEI9")
			if err := os.MkdirAll(mei, 0o755); err != nil {
				t.Fatal(err)
			}
			if code := Run(context.Background(), RunOptions{Port: port, DataDir: dataDir}); code != 0 {
				t.Fatalf("exit code %d", code)
			}
			if probed != 1 {
				t.Fatalf("probed %d times", probed)
			}
			// Tool setup only starts once the port is ours
			if _, err := os.Stat(filepath.Join(dataDir, "tools")); !os.IsNotExist(err) {
				t.Fatal("tools folder created by a server that never started")
			}
			if _, err := os.Stat(mei); err != nil {
				t.Fatal("temp folder swept by a server that never started")
			}
		})
	}
}

func TestRunFailsWithoutToken(t *testing.T) {
	// A data directory that is a file cannot hold the token
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(context.Background(), RunOptions{Port: 0, DataDir: file}); code != 1 {
		t.Fatalf("exit code %d", code)
	}
}

func TestIdleExpired(t *testing.T) {
	cases := []struct {
		idle, timeout time.Duration
		busy, want    bool
	}{
		{100 * time.Second, 300 * time.Second, false, false},
		{300 * time.Second, 300 * time.Second, false, true},
		{1000 * time.Second, 300 * time.Second, true, false},
		{1000 * time.Second, 0, false, false},
	}
	for _, tc := range cases {
		if got := IdleExpired(tc.idle, tc.timeout, tc.busy); got != tc.want {
			t.Errorf("IdleExpired(%s, %s, %v) = %v", tc.idle, tc.timeout, tc.busy, got)
		}
	}
}

func TestWatchdogStopsIdleServer(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		watchIdle(context.Background(), 200*time.Millisecond,
			func() time.Duration { return 10 * time.Second }, busy.Load, func() { stopped.Store(true) })
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	if stopped.Load() {
		t.Fatal("stopped while busy")
	}
	busy.Store(false)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not stop the idle server")
	}
	if !stopped.Load() {
		t.Fatal("stop not called")
	}
}

func TestWatchdogEndsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchIdle(ctx, time.Hour, func() time.Duration { return 0 }, func() bool { return false }, func() {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog ignored the context")
	}
}

func TestRequestsResetIdleClock(t *testing.T) {
	e := newEnv(t)
	e.server.base = time.Now().Add(-time.Hour)
	e.server.lastRequest.Store(0)
	before := e.server.idleFor()
	e.do("GET", "/health", nil, map[string]string{"X-YTDL-Client": ""}) // rejected: must not count
	if e.server.lastRequest.Load() != 0 {
		t.Fatal("a rejected request reset the idle clock")
	}
	e.get("/health")
	if e.server.lastRequest.Load() == 0 || e.server.idleFor() >= before {
		t.Fatal("an accepted request did not reset the idle clock")
	}
}

// serveInBackground runs e.server on a real loopback port and returns its base URL and a
// channel with serve's exit code.
func serveInBackground(t *testing.T, e *testEnv, idle time.Duration) (string, <-chan int) {
	t.Helper()
	listener, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	exit := make(chan int, 1)
	go func() { exit <- e.server.serve(context.Background(), listener, idle, nil) }()
	return fmt.Sprintf("http://%s", listener.Addr()), exit
}

func TestShutdownRepliesThenStopsAndKillsDownloads(t *testing.T) {
	e := newEnv(t, withToken(testToken))
	base, exit := serveInBackground(t, e, 0)
	if _, err := e.queue.Create(videoURL, core.DefaultJobOptions(), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.starter.count() == 1 })

	req, _ := http.NewRequest(http.MethodPost, base+"/shutdown", nil)
	req.Header.Set(auth.ClientHeader, "1")
	req.Header.Set(auth.TokenHeader, testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	_, _ = fmt.Fprint(&body, readAll(t, resp))
	if resp.StatusCode != 200 || body.String() != "{\"status\":\"stopping\"}\n" {
		t.Fatalf("shutdown: %d %q", resp.StatusCode, body.String())
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop")
	}
	if !e.starter.proc(0).wasKilled() {
		t.Fatal("download not killed on shutdown")
	}
	if _, err := http.Get(base + "/health"); err == nil {
		t.Fatal("server still answers after shutdown")
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestServeStopsWhenIdle(t *testing.T) {
	e := newEnv(t)
	_, exit := serveInBackground(t, e, 300*time.Millisecond)
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle server did not stop")
	}
}

func TestAutoUpdateWaitsForTools(t *testing.T) {
	tools := &fakeTools{}
	var checks atomic.Int32
	u := newUpdater(t.TempDir(), "2026.11.01", func() string { return "2026.11.01" }, func() error { return nil })
	u.FetchLatest = func(context.Context) (string, error) {
		checks.Add(1)
		return "2026.11.01", nil
	}
	done := make(chan struct{})
	go func() {
		autoUpdate(context.Background(), tools, u, func() bool { return true })
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	if checks.Load() != 0 {
		t.Fatal("update check ran before the tools were ready")
	}
	tools.set(system.ToolsStatus{State: system.ToolsReady}, true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-update did not run once the tools were ready")
	}
	if checks.Load() != 1 {
		t.Fatalf("%d checks", checks.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	autoUpdate(ctx, &fakeTools{}, u, func() bool { return true }) // returns at once
}

// TestRealInfoSmoke fetches real metadata for the first YouTube video. It needs the network
// and a yt-dlp binary, named by VIDGRAB_TEST_YTDLP, so it is skipped otherwise.
func TestRealInfoSmoke(t *testing.T) {
	ytdlp := os.Getenv("VIDGRAB_TEST_YTDLP")
	if ytdlp == "" {
		t.Skip("set VIDGRAB_TEST_YTDLP to a yt-dlp binary to run")
	}
	if _, err := exec.LookPath(ytdlp); err != nil {
		t.Skipf("yt-dlp not found: %v", err)
	}
	tools := &fakeTools{status: system.ToolsStatus{State: system.ToolsReady}, ready: true}
	s := New(Config{
		Tools:   toolsAt{tools, ytdlp},
		Updater: newUpdater(t.TempDir(), "", func() string { return "" }, func() error { return nil }),
	})
	e := &testEnv{t: t, server: s, headers: map[string]string{"X-YTDL-Client": "1"}}
	resp := e.get(infoURL(videoURL))
	expectStatus(t, resp, 200)
	body := resp.json(t)
	if body["id"] != "jNQXAC9IVRw" || len(body["qualities"].([]any)) == 0 {
		t.Fatalf("body %s", resp.body)
	}
}

// toolsAt reports a ready setup with yt-dlp at a fixed path.
type toolsAt struct {
	*fakeTools
	ytdlp string
}

func (t toolsAt) Paths() (string, string, bool) { return t.ytdlp, "", true }

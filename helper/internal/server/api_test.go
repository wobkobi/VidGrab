package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vidgrab/internal/core"
	"vidgrab/internal/system"
)

const (
	videoURL    = "https://www.youtube.com/watch?v=jNQXAC9IVRw"
	playlistURL = "https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE"
	extOrigin   = "moz-extension://0d6c8f43-1f39-4a1b-9e1c-0f6a1d0b2e3c"
	testToken   = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
)

func videoRaw() map[string]any {
	return map[string]any{
		"id": "jNQXAC9IVRw", "title": "Me at the zoo", "uploader": "jawed", "duration": json.Number("19"),
		"formats": []any{map[string]any{"vcodec": "avc1", "acodec": "none", "height": json.Number("360"), "filesize": json.Number("10")}},
	}
}

func playlistRaw() map[string]any {
	return map[string]any{
		"id": "PLBCF2DAC6FFB574DE", "title": "Stories",
		"entries": []any{
			map[string]any{"id": "jNQXAC9IVRw", "title": "A"},
			map[string]any{"id": "dQw4w9WgXcQ", "title": "B"},
			map[string]any{"id": "9bZkp7q19f0", "title": "C"},
		},
	}
}

type fetchCall struct {
	url  string
	flat bool
}

// fakeFetcher replaces yt-dlp -J: it records calls and can fail on demand.
type fakeFetcher struct {
	mu    sync.Mutex
	calls []fetchCall
	err   error
}

func (f *fakeFetcher) fetch(_ context.Context, url string, flat, _ bool) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fetchCall{url, flat})
	if f.err != nil {
		return nil, f.err
	}
	if flat {
		return playlistRaw(), nil
	}
	return videoRaw(), nil
}

func (f *fakeFetcher) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeFetcher) recorded() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetchCall(nil), f.calls...)
}

// fakeTools reports a settable setup state.
type fakeTools struct {
	mu     sync.Mutex
	status system.ToolsStatus
	ready  bool
}

func readyTools() *fakeTools {
	return &fakeTools{status: system.ToolsStatus{State: system.ToolsReady, Percent: 100}, ready: true}
}

func (f *fakeTools) Status() system.ToolsStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeTools) Paths() (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ready {
		return "", "", false
	}
	return "yt-dlp", "", true
}

func (f *fakeTools) set(status system.ToolsStatus, ready bool) {
	f.mu.Lock()
	f.status, f.ready = status, ready
	f.mu.Unlock()
}

// testEnv bundles a Server with its fakes.
type testEnv struct {
	t          *testing.T
	dir        string
	server     *Server
	queue      *JobQueue
	fetcher    *fakeFetcher
	starter    *fakeStarter
	tools      *fakeTools
	updater    *system.Updater
	ffmpeg     bool
	opened     []string
	mu         sync.Mutex
	installed  string
	installErr error
	headers    map[string]string
}

type envOpt func(*testEnv, *Config)

func withFfmpeg(ok bool) envOpt { return func(e *testEnv, _ *Config) { e.ffmpeg = ok } }
func withToken(t string) envOpt { return func(_ *testEnv, c *Config) { c.Token = t } }
func withFinishingProcs() envOpt {
	return func(e *testEnv, _ *Config) { e.starter.setFactory(finishedProc(nil)) }
}

func finishedProc(lines []string) func() *fakeProc {
	return func() *fakeProc { return newFakeProc(lines, 0, false) }
}

// newUpdater returns an Updater whose release check, install and version read are fakes.
func newUpdater(dir string, latest string, installed func() string, install func() error) *system.Updater {
	u := system.NewUpdater(filepath.Join(dir, "update_state.json"), system.NewTools(filepath.Join(dir, "tools")))
	u.FetchLatest = func(context.Context) (string, error) { return latest, nil }
	u.Installed = func(context.Context) string { return installed() }
	u.Install = func(context.Context) error { return install() }
	return u
}

func newEnv(t *testing.T, opts ...envOpt) *testEnv {
	t.Helper()
	e := &testEnv{
		t:         t,
		dir:       t.TempDir(),
		fetcher:   &fakeFetcher{},
		starter:   newBlockingStarter(),
		tools:     readyTools(),
		ffmpeg:    true,
		installed: "2026.03.03",
		headers:   map[string]string{"X-YTDL-Client": "1"},
	}
	e.updater = newUpdater(e.dir, "2026.11.01", e.installedVersion, e.install)
	cfg := Config{
		Version:      "2.1.0",
		SettingsPath: filepath.Join(e.dir, "settings.json"),
		Tools:        e.tools,
		Updater:      e.updater,
		FetchInfo:    e.fetcher.fetch,
		Ffmpeg:       func() bool { return e.ffmpeg },
		DownloadsDir: func() string { return e.dir },
		StartProcess: e.starter.start,
		KillProcess:  fakeKill,
		OpenFile:     func(p string) error { e.record("open " + p); return nil },
		Reveal:       func(p string) error { e.record("reveal " + p); return nil },
	}
	for _, opt := range opts {
		opt(e, &cfg)
	}
	e.server = New(cfg)
	e.queue = e.server.Queue()
	// Fill the installed-version cache the way the server's background start-up does
	e.updater.RefreshInstalled(context.Background())
	t.Cleanup(func() {
		e.server.endLife()
		e.queue.Shutdown()
		e.queue.WaitIdle(3 * time.Second)
	})
	return e
}

func (e *testEnv) record(s string) {
	e.mu.Lock()
	e.opened = append(e.opened, s)
	e.mu.Unlock()
}

func (e *testEnv) installedVersion() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.installed
}

// install is the fake yt-dlp update: success installs the newer version.
func (e *testEnv) install() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.installErr != nil {
		return e.installErr
	}
	e.installed = "2026.11.01"
	return nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("body %q is not a JSON object: %v", r.body, err)
	}
	return out
}

// do sends a request through the server's handler as the extension would (Host 127.0.0.1,
// X-YTDL-Client: 1). A header given as "" is removed instead.
func (e *testEnv) do(method, target string, body any, headers map[string]string) response {
	e.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, target, reader)
	req.Host = "127.0.0.1:18956"
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		if v == "" && k != "Origin" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.server.ServeHTTP(rec, req)
	return response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (e *testEnv) get(target string) response { return e.do("GET", target, nil, nil) }

func (e *testEnv) post(target string, body any) response { return e.do("POST", target, body, nil) }

func (e *testEnv) createJob(body any) string {
	e.t.Helper()
	resp := e.post("/jobs", body)
	if resp.status != 202 {
		e.t.Fatalf("POST /jobs: %d %s", resp.status, resp.body)
	}
	return resp.json(e.t)["jobs"].([]any)[0].(map[string]any)["id"].(string)
}

func (e *testEnv) jobStatus(id string) string {
	view, err := e.queue.Snapshot(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return view.Status
}

func infoURL(url string) string { return "/info?url=" + strings.ReplaceAll(url, "?", "%3F") }

func expectStatus(t *testing.T, resp response, want int) {
	t.Helper()
	if resp.status != want {
		t.Fatalf("status %d, want %d (body %s)", resp.status, want, resp.body)
	}
}

// Guard

func TestHealthRequiresClientHeader(t *testing.T) {
	e := newEnv(t)
	resp := e.do("GET", "/health", nil, map[string]string{"X-YTDL-Client": ""})
	expectStatus(t, resp, 403)
	if resp.json(t)["error"] != "Missing X-YTDL-Client header" {
		t.Fatalf("body %s", resp.body)
	}
	expectStatus(t, e.do("GET", "/health", nil, map[string]string{"X-YTDL-Client": "yes"}), 403)
}

func TestForeignOriginRejected(t *testing.T) {
	e := newEnv(t)
	for _, origin := range []string{"https://evil.example", "null", ""} {
		resp := e.do("GET", "/health", nil, map[string]string{"Origin": origin})
		expectStatus(t, resp, 403)
		if resp.json(t)["error"] != "Forbidden origin" {
			t.Fatalf("origin %q: body %s", origin, resp.body)
		}
	}
}

func TestExtensionOriginAllowedWithCORS(t *testing.T) {
	e := newEnv(t)
	resp := e.do("GET", "/health", nil, map[string]string{"Origin": extOrigin})
	expectStatus(t, resp, 200)
	if resp.header.Get("Access-Control-Allow-Origin") != extOrigin {
		t.Fatalf("CORS header %q", resp.header.Get("Access-Control-Allow-Origin"))
	}
	// An extension origin outside the allowed pattern passes the guard but gets no CORS header
	resp = e.do("GET", "/health", nil, map[string]string{"Origin": "moz-extension://not hex"})
	expectStatus(t, resp, 200)
	if resp.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header for a malformed origin")
	}
}

func TestPreflight(t *testing.T) {
	e := newEnv(t)
	preflight := map[string]string{
		"Origin":                         extOrigin,
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "x-ytdl-client,content-type",
		"X-YTDL-Client":                  "",
	}
	ok := e.do("OPTIONS", "/jobs", nil, preflight)
	expectStatus(t, ok, 200)
	if ok.header.Get("Access-Control-Allow-Origin") != extOrigin {
		t.Fatalf("allow origin %q", ok.header.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(ok.header.Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("allow methods %q", ok.header.Get("Access-Control-Allow-Methods"))
	}

	bad := map[string]string{}
	for k, v := range preflight {
		bad[k] = v
	}
	bad["Origin"] = "https://evil.example"
	expectStatus(t, e.do("OPTIONS", "/jobs", nil, bad), 403)

	bad["Origin"] = extOrigin
	bad["Access-Control-Request-Headers"] = "x-ytdl-client, x-evil"
	resp := e.do("OPTIONS", "/jobs", nil, bad)
	expectStatus(t, resp, 400)
	if string(resp.body) != "Disallowed CORS headers" {
		t.Fatalf("body %q", resp.body)
	}

	bad["Access-Control-Request-Headers"] = "x-ytdl-client"
	bad["Access-Control-Request-Method"] = "PATCH"
	expectStatus(t, e.do("OPTIONS", "/jobs", nil, bad), 400)
}

func TestOptionsWithoutPreflightIs405(t *testing.T) {
	e := newEnv(t)
	resp := e.do("OPTIONS", "/jobs", nil, map[string]string{"X-YTDL-Client": ""})
	expectStatus(t, resp, 405)
	if resp.json(t)["error"] != "Method Not Allowed" {
		t.Fatalf("body %s", resp.body)
	}
}

func TestForeignHostRejected(t *testing.T) {
	e := newEnv(t)
	for host, want := range map[string]int{
		"evil.example:18956": 403, "localhost:18956": 200, "LOCALHOST": 200, "127.0.0.1": 200,
		"[::1]:18956": 403, "127.0.0.1.evil.example": 403,
	} {
		req := httptest.NewRequest("GET", "/health", nil)
		req.Host = host
		req.Header.Set("X-YTDL-Client", "1")
		rec := httptest.NewRecorder()
		e.server.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %q: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestUnknownRouteAndMethodUseErrorShape(t *testing.T) {
	e := newEnv(t)
	resp := e.get("/nope")
	expectStatus(t, resp, 404)
	if body := resp.json(t); body["error"] != "Not Found" || len(body) != 1 {
		t.Fatalf("body %s", resp.body)
	}
	resp = e.post("/health", nil)
	expectStatus(t, resp, 405)
	if resp.json(t)["error"] != "Method Not Allowed" {
		t.Fatalf("body %s", resp.body)
	}
	expectStatus(t, e.get("/jobs/abc/pause"), 405)
	expectStatus(t, e.post("/jobs/a/b/c", nil), 404)
}

// Health and info

func TestHealthBody(t *testing.T) {
	e := newEnv(t)
	e.updater.Check(context.Background(), true)
	body := e.get("/health").json(t)
	checks := map[string]any{
		"status": "ok", "version": "2.1.0", "ffmpeg": true, "yt_dlp_version": "2026.03.03",
		"yt_dlp_latest": "2026.11.01", "update_available": true, "concurrency": 2.0, "fragments": 4.0,
		"cookies": false, "auto_update": true,
	}
	for key, want := range checks {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
	tools := body["tools"].(map[string]any)
	if tools["state"] != "ready" || tools["percent"] != 100.0 {
		t.Errorf("tools %v", tools)
	}
	if _, ok := body["proof"]; ok {
		t.Error("proof without a nonce")
	}
}

// TestHealthNeverWaitsForYtDlp covers a slow `yt-dlp --version` (seconds for the Windows
// build): /health answers at once with "unknown", one background read fills the cache, and
// that read outlives the request that started it.
func TestHealthNeverWaitsForYtDlp(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var reads atomic.Int32
	slow := newUpdater(t.TempDir(), "2026.11.01", func() string { return "" }, func() error { return nil })
	slow.Installed = func(ctx context.Context) string {
		reads.Add(1)
		select {
		case <-release:
			return "2026.03.03"
		case <-ctx.Done():
			return ""
		}
	}
	slow.Check(context.Background(), true)
	// An updater whose cache is still empty, as right after the tools became ready
	e.server.cfg.Updater = slow

	for i := 0; i < 3; i++ {
		started := time.Now()
		body := e.get("/health").json(t)
		if time.Since(started) > time.Second {
			t.Fatal("/health waited for yt-dlp")
		}
		if body["yt_dlp_version"] != "unknown" || body["update_available"] != false {
			t.Fatalf("version %v, update available %v", body["yt_dlp_version"], body["update_available"])
		}
	}
	waitFor(t, func() bool { return reads.Load() == 1 })
	close(release)
	waitFor(t, func() bool { return e.get("/health").json(t)["yt_dlp_version"] == "2026.03.03" })
	body := e.get("/health").json(t)
	if body["update_available"] != true {
		t.Fatal("update not reported once the version is known")
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads of the installed version, want 1", n)
	}
}

func TestHealthBeforeAnyCheckHasNullLatest(t *testing.T) {
	e := newEnv(t)
	body := e.get("/health").json(t)
	if v, ok := body["yt_dlp_latest"]; !ok || v != nil {
		t.Fatalf("yt_dlp_latest = %v (present %v)", v, ok)
	}
	if body["update_available"] != false {
		t.Fatal("update available without a check")
	}
}

func TestHealthReportsToolsSetup(t *testing.T) {
	// The default ffmpeg check (tools ready) rather than the env's fixed answer
	e := newEnv(t, func(_ *testEnv, c *Config) { c.Ffmpeg = nil })
	e.tools.set(system.ToolsStatus{State: system.ToolsInstalling, Step: "Downloading ffmpeg", Percent: 42}, false)
	body := e.get("/health").json(t)
	tools := body["tools"].(map[string]any)
	if tools["state"] != "installing" || tools["step"] != "Downloading ffmpeg" || tools["percent"] != 42.0 {
		t.Fatalf("tools %v", tools)
	}
	if body["ffmpeg"] != false {
		t.Fatal("ffmpeg reported before setup finished")
	}
}

func TestInfoVideoAndCache(t *testing.T) {
	e := newEnv(t)
	first := e.get(infoURL("https://youtu.be/jNQXAC9IVRw"))
	expectStatus(t, first, 200)
	body := first.json(t)
	if body["kind"] != "video" || body["title"] != "Me at the zoo" {
		t.Fatalf("body %s", first.body)
	}
	if h := body["qualities"].([]any)[0].(map[string]any)["height"]; h != 360.0 {
		t.Fatalf("height %v", h)
	}
	e.get(infoURL(videoURL))
	if calls := e.fetcher.recorded(); !slices.Equal(calls, []fetchCall{{videoURL, false}}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestInfoCacheKeysOnCookies(t *testing.T) {
	e := newEnv(t)
	e.get(infoURL(videoURL))
	e.do("PUT", "/settings", map[string]any{"cookies": true}, nil)
	e.get(infoURL(videoURL))
	if n := len(e.fetcher.recorded()); n != 2 {
		t.Fatalf("%d fetches, want 2", n)
	}
}

func TestInfoPlaylist(t *testing.T) {
	e := newEnv(t)
	body := e.get(infoURL(playlistURL)).json(t)
	if body["kind"] != "playlist" || body["entry_count"] != 3.0 {
		t.Fatalf("body %v", body)
	}
	if calls := e.fetcher.recorded(); !slices.Equal(calls, []fetchCall{{playlistURL, true}}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestInfoErrors(t *testing.T) {
	e := newEnv(t)
	expectStatus(t, e.get(infoURL("https://example.com")), 400)
	missing := e.get("/info")
	expectStatus(t, missing, 400)
	if body := missing.json(t); body["error"] != "Invalid request" || body["detail"] != "query.url: Field required" {
		t.Fatalf("body %s", missing.body)
	}
	expectStatus(t, e.get("/info?url="+strings.Repeat("a", 2049)), 400)

	e.fetcher.setErr(&core.YtDlpError{Message: "Private video", Detail: "ERROR: Private video"})
	resp := e.get(infoURL(videoURL))
	expectStatus(t, resp, 502)
	if body := resp.json(t); body["error"] != "Private video" || body["detail"] != "ERROR: Private video" || len(body) != 2 {
		t.Fatalf("body %s", resp.body)
	}
	e.fetcher.setErr(&core.YtDlpTimeout{YtDlpError: core.YtDlpError{Message: "YouTube took too long to answer"}})
	resp = e.get(infoURL("https://youtu.be/dQw4w9WgXcQ"))
	expectStatus(t, resp, 504)
	if body := resp.json(t); body["error"] != "YouTube took too long to answer" {
		t.Fatalf("body %s", resp.body)
	}
}

func TestInfoAndJobsWaitForToolSetup(t *testing.T) {
	e := newEnv(t)
	e.tools.set(system.ToolsStatus{State: system.ToolsInstalling, Step: "Downloading yt-dlp", Percent: 5}, false)
	for _, resp := range []response{e.get(infoURL(videoURL)), e.post("/jobs", map[string]any{"url": videoURL})} {
		expectStatus(t, resp, 503)
		if body := resp.json(t); body["error"] != "Setting up yt-dlp and ffmpeg" || body["detail"] != "Downloading yt-dlp" {
			t.Fatalf("body %s", resp.body)
		}
	}
	e.tools.set(system.ToolsStatus{State: system.ToolsError, Step: "Downloading ffmpeg", Error: "checksum mismatch"}, false)
	if body := e.get(infoURL(videoURL)).json(t); body["detail"] != "checksum mismatch" {
		t.Fatalf("body %v", body)
	}
	if len(e.fetcher.recorded()) != 0 || len(e.queue.List()) != 0 {
		t.Fatal("work started before setup finished")
	}
	// Everything else keeps working during setup
	expectStatus(t, e.get("/jobs"), 200)

	e.tools.set(system.ToolsStatus{State: system.ToolsReady, Percent: 100}, true)
	expectStatus(t, e.get(infoURL(videoURL)), 200)
}

// Jobs

func TestCreateVideoJob(t *testing.T) {
	e := newEnv(t)
	e.get(infoURL(videoURL))
	resp := e.post("/jobs", map[string]any{"url": videoURL, "options": map[string]any{"kind": "video", "quality": "1080"}})
	expectStatus(t, resp, 202)
	jobs := resp.json(t)["jobs"].([]any)
	job := jobs[0].(map[string]any)
	if len(jobs) != 1 || job["title"] != "Me at the zoo" || job["options"].(map[string]any)["quality"] != "1080" {
		t.Fatalf("jobs %s", resp.body)
	}
	listed := e.get("/jobs").json(t)["jobs"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["id"] != job["id"] {
		t.Fatalf("listed %v", listed)
	}
}

func TestCreatePlaylistJobsWithItems(t *testing.T) {
	e := newEnv(t)
	resp := e.post("/jobs", map[string]any{"url": playlistURL, "options": map[string]any{"kind": "audio"}, "items": "1,3"})
	expectStatus(t, resp, 202)
	jobs := resp.json(t)["jobs"].([]any)
	var titles []string
	for _, j := range jobs {
		titles = append(titles, j.(map[string]any)["title"].(string))
	}
	if !slices.Equal(titles, []string{"A", "C"}) || jobs[0].(map[string]any)["group_title"] != "Stories" {
		t.Fatalf("jobs %s", resp.body)
	}
}

func TestCreatePlaylistReusesCachedListing(t *testing.T) {
	e := newEnv(t)
	e.get(infoURL(playlistURL))
	expectStatus(t, e.post("/jobs", map[string]any{"url": playlistURL}), 202)
	if n := len(e.fetcher.recorded()); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

func TestCreateJobsRejectsBadInput(t *testing.T) {
	cases := []any{
		map[string]any{"url": "https://example.com/x"},
		map[string]any{"url": playlistURL, "items": "3-1"},
		map[string]any{"url": videoURL, "options": map[string]any{"quality": "999"}},
		map[string]any{"url": videoURL, "options": map[string]any{"kind": "gif"}},
		map[string]any{"url": videoURL, "options": map[string]any{"audio_bitrate": "64"}},
		map[string]any{"url": videoURL, "options": map[string]any{"max_size_mb": 4001}},
		map[string]any{"url": videoURL, "options": map[string]any{"max_size_mb": -5}},
		map[string]any{"url": videoURL, "options": map[string]any{"max_size_mb": "big"}},
		map[string]any{"options": map[string]any{}},
		"not json",
		nil,
	}
	for _, body := range cases {
		e := newEnv(t)
		resp := e.post("/jobs", body)
		if resp.status != 400 {
			t.Errorf("body %v: status %d (%s)", body, resp.status, resp.body)
			continue
		}
		if _, ok := resp.json(t)["error"]; !ok {
			t.Errorf("body %v: no error field", body)
		}
	}
}

func TestCreateJobsValidationDetail(t *testing.T) {
	e := newEnv(t)
	resp := e.post("/jobs", map[string]any{"options": map[string]any{}})
	if body := resp.json(t); body["error"] != "Invalid request" || body["detail"] != "body.url: Field required" {
		t.Fatalf("body %s", resp.body)
	}
	// The detail is cut to 500 characters
	long := invalidRequest(strings.Repeat("é", 600))
	if n := len([]rune(long.detail)); n != 500 || long.status != 400 || long.msg != "Invalid request" {
		t.Fatalf("detail length %d status %d", n, long.status)
	}
}

func TestCreateJobsListFailure(t *testing.T) {
	e := newEnv(t)
	e.fetcher.setErr(&core.YtDlpError{Message: "The playlist does not exist"})
	expectStatus(t, e.post("/jobs", map[string]any{"url": playlistURL}), 502)
	e.fetcher.setErr(nil)
	resp := e.post("/jobs", map[string]any{"url": playlistURL, "items": "10-20"})
	expectStatus(t, resp, 400)
	if resp.json(t)["error"] != "No videos in that range" {
		t.Fatalf("body %s", resp.body)
	}
}

func TestFfmpegRequirement(t *testing.T) {
	cases := []struct {
		options map[string]any
		status  int
	}{
		{map[string]any{"kind": "audio", "audio_format": "mp3"}, 422},
		{map[string]any{"kind": "video", "quality": "best"}, 422},
		{map[string]any{"kind": "video", "quality": "1080"}, 422},
		{map[string]any{"kind": "video", "quality": "720"}, 202},
		{map[string]any{"kind": "audio", "audio_format": "m4a"}, 202},
		{map[string]any{"kind": "video", "quality": "720", "max_size_mb": 10}, 422},
		{map[string]any{"kind": "video", "quality": "720", "max_size_mb": 0}, 202},
		{map[string]any{"kind": "audio", "audio_format": "m4a", "max_size_mb": 10}, 202},
	}
	for _, tc := range cases {
		e := newEnv(t, withFfmpeg(false))
		resp := e.post("/jobs", map[string]any{"url": videoURL, "options": tc.options})
		if resp.status != tc.status {
			t.Errorf("%v: status %d, want %d", tc.options, resp.status, tc.status)
			continue
		}
		if tc.status == 422 && resp.json(t)["error"] != "This download needs ffmpeg" {
			t.Errorf("%v: body %s", tc.options, resp.body)
		}
	}
}

func TestJobActions(t *testing.T) {
	e := newEnv(t)
	id := e.createJob(map[string]any{"url": videoURL, "options": map[string]any{"quality": "720"}})
	waitFor(t, func() bool { return e.starter.count() == 1 })

	paused := e.post("/jobs/"+id+"/pause", nil)
	expectStatus(t, paused, 200)
	if paused.json(t)["job"].(map[string]any)["status"] != "paused" {
		t.Fatalf("pause %s", paused.body)
	}
	conflict := e.post("/jobs/"+id+"/pause", nil)
	expectStatus(t, conflict, 409)
	if conflict.json(t)["error"] != "Cannot pause a paused download" {
		t.Fatalf("body %s", conflict.body)
	}
	if !e.queue.WaitIdle(3 * time.Second) {
		t.Fatal("runner still alive")
	}
	resumed := e.post("/jobs/"+id+"/resume", nil).json(t)["job"].(map[string]any)["status"]
	if resumed != "queued" && resumed != "downloading" {
		t.Fatalf("resume status %v", resumed)
	}
	expectStatus(t, e.post("/jobs/"+id+"/open", nil), 409)
	if s := e.post("/jobs/"+id+"/cancel", nil).json(t)["job"].(map[string]any)["status"]; s != "cancelled" {
		t.Fatalf("cancel status %v", s)
	}
	expectStatus(t, e.post("/jobs/"+id+"/retry", nil), 200)
	missing := e.post("/jobs/missing/pause", nil)
	expectStatus(t, missing, 404)
	if missing.json(t)["error"] != "Download not found" {
		t.Fatalf("body %s", missing.body)
	}
	unknown := e.post("/jobs/"+id+"/explode", nil)
	expectStatus(t, unknown, 404)
	if unknown.json(t)["error"] != "Unknown action" {
		t.Fatalf("body %s", unknown.body)
	}
}

func TestOpenAndRevealFinishedFile(t *testing.T) {
	e := newEnv(t)
	downloaded := filepath.Join(e.dir, "Me [jNQXAC9IVRw].mp4")
	if err := os.WriteFile(downloaded, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(e.dir, "Me.mp4")
	e.starter.setFactory(finishedProc([]string{"YTDLP_FILE " + downloaded}))

	id := e.createJob(map[string]any{"url": videoURL})
	waitFor(t, func() bool { return e.jobStatus(id) == "complete" })
	expectStatus(t, e.post("/jobs/"+id+"/open", nil), 200)
	revealed := e.post("/jobs/"+id+"/reveal", nil)
	if revealed.json(t)["job"].(map[string]any)["filepath"] != final {
		t.Fatalf("reveal %s", revealed.body)
	}
	e.mu.Lock()
	opened := append([]string(nil), e.opened...)
	e.mu.Unlock()
	if !slices.Equal(opened, []string{"open " + final, "reveal " + final}) {
		t.Fatalf("opened %v", opened)
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, e.post("/jobs/"+id+"/open", nil), 409)
}

func TestOpenFailureIs409(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, c *Config) {
		c.OpenFile = func(string) error { return errors.New("no handler") }
	})
	final := filepath.Join(e.dir, "Me [jNQXAC9IVRw].mp4")
	if err := os.WriteFile(final, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.starter.setFactory(finishedProc([]string{"YTDLP_FILE " + final}))
	id := e.createJob(map[string]any{"url": videoURL})
	waitFor(t, func() bool { return e.jobStatus(id) == "complete" })
	resp := e.post("/jobs/"+id+"/open", nil)
	expectStatus(t, resp, 409)
	if body := resp.json(t); body["error"] != "Could not open the file" || body["detail"] != "no handler" {
		t.Fatalf("body %s", resp.body)
	}
}

func TestClearFinished(t *testing.T) {
	e := newEnv(t, withFinishingProcs())
	id := e.createJob(map[string]any{"url": videoURL})
	waitFor(t, func() bool { return e.jobStatus(id) == "complete" })
	resp := e.do("DELETE", "/jobs?finished=true", nil, nil)
	if string(resp.body) != "{\"removed\":1}\n" {
		t.Fatalf("body %q", resp.body)
	}
	if string(e.get("/jobs").body) != "{\"jobs\":[]}\n" {
		t.Fatalf("jobs %s", e.get("/jobs").body)
	}
	resp = e.do("DELETE", "/jobs", nil, nil)
	expectStatus(t, resp, 400)
	if resp.json(t)["error"] != "Only finished=true is supported" {
		t.Fatalf("body %s", resp.body)
	}
	expectStatus(t, e.do("DELETE", "/jobs?finished=maybe", nil, nil), 400)
	expectStatus(t, e.do("DELETE", "/jobs?finished=yes", nil, nil), 200)
}

// Settings and update

func TestPutSettingsPartialAndPersisted(t *testing.T) {
	e := newEnv(t)
	resp := e.do("PUT", "/settings", map[string]any{"concurrency": 4, "cookies": true}, nil)
	expectStatus(t, resp, 200)
	if string(resp.body) != "{\"concurrency\":4,\"fragments\":4,\"cookies\":true,\"auto_update\":true}\n" {
		t.Fatalf("body %s", resp.body)
	}
	e.queue.mu.Lock()
	concurrency, cookies := e.queue.concurrency, e.queue.cookies
	e.queue.mu.Unlock()
	if concurrency != 4 || !cookies {
		t.Fatalf("queue concurrency %d cookies %v", concurrency, cookies)
	}
	if e.get("/health").json(t)["concurrency"] != 4.0 {
		t.Fatal("health does not report the new concurrency")
	}
	saved, err := os.ReadFile(filepath.Join(e.dir, "settings.json"))
	if err != nil || !strings.Contains(string(saved), `"concurrency":4`) {
		t.Fatalf("saved %q, %v", saved, err)
	}

	reloaded := New(Config{
		SettingsPath: filepath.Join(e.dir, "settings.json"),
		Tools:        readyTools(),
		Updater:      newUpdater(t.TempDir(), "", func() string { return "" }, func() error { return nil }),
	})
	if got := reloaded.settings.get(); got.Concurrency != 4 || !got.Cookies {
		t.Fatalf("reloaded settings %+v", got)
	}
	reloaded.queue.mu.Lock()
	defer reloaded.queue.mu.Unlock()
	if reloaded.queue.concurrency != 4 {
		t.Fatal("reloaded queue ignores saved concurrency")
	}
}

func TestCorruptSettingsFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"concurrency": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{SettingsPath: path, Tools: readyTools(), Updater: newUpdater(dir, "", func() string { return "" }, func() error { return nil })})
	if got := s.settings.get(); got != core.DefaultSettings() {
		t.Fatalf("settings %+v", got)
	}
}

func TestPutSettingsBounds(t *testing.T) {
	e := newEnv(t)
	for _, body := range []map[string]any{{"concurrency": 0}, {"concurrency": 6}, {"fragments": 9}, {"cookies": "maybe"}} {
		resp := e.do("PUT", "/settings", body, nil)
		if resp.status != 400 || resp.json(t)["error"] != "Invalid request" {
			t.Errorf("%v: %d %s", body, resp.status, resp.body)
		}
	}
}

func TestUpdateBlockedWhileDownloading(t *testing.T) {
	e := newEnv(t)
	e.createJob(map[string]any{"url": videoURL})
	waitFor(t, func() bool { return e.starter.count() == 1 })
	resp := e.post("/update", nil)
	expectStatus(t, resp, 409)
	if resp.json(t)["error"] != "Wait for downloads to finish before updating yt-dlp" {
		t.Fatalf("body %s", resp.body)
	}
	if e.installedVersion() != "2026.03.03" {
		t.Fatal("update ran while downloading")
	}
}

func TestUpdateInstallsAndClearsInfoCache(t *testing.T) {
	e := newEnv(t)
	e.get(infoURL(videoURL))
	resp := e.post("/update", nil)
	expectStatus(t, resp, 200)
	if string(resp.body) != "{\"updated\":true,\"version\":\"2026.11.01\"}\n" {
		t.Fatalf("body %s", resp.body)
	}
	e.get(infoURL(videoURL))
	if n := len(e.fetcher.recorded()); n != 2 {
		t.Fatalf("%d fetches; the update should clear the /info cache", n)
	}
}

func TestUpdateFailure(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.installErr = errors.New("ERROR: no network")
	e.mu.Unlock()
	resp := e.post("/update", nil)
	expectStatus(t, resp, 502)
	body := resp.json(t)
	if body["error"] != "Could not update yt-dlp" || !strings.Contains(body["detail"].(string), "no network") {
		t.Fatalf("body %s", resp.body)
	}
}

func TestUpdateHoldsQueue(t *testing.T) {
	// A job queued while yt-dlp is being replaced waits until the update is done
	entered := make(chan struct{})
	release := make(chan struct{})
	e := newEnv(t, func(_ *testEnv, c *Config) {
		c.Updater.Install = func(context.Context) error {
			close(entered)
			<-release
			return nil
		}
	})
	done := make(chan response, 1)
	go func() { done <- e.post("/update", nil) }()
	<-entered
	id := e.createJob(map[string]any{"url": videoURL})
	time.Sleep(50 * time.Millisecond)
	if e.jobStatus(id) != "queued" || e.starter.count() != 0 {
		t.Fatal("job started during the update")
	}
	close(release)
	expectStatus(t, <-done, 200)
	waitFor(t, func() bool { return e.starter.count() == 1 })
}

func TestAutoUpdateRefusesWhenDownloadStartsFirst(t *testing.T) {
	// The automatic update checks Busy without a hold; the wrapped Install re-checks under the
	// hold, so a download that slipped in between is never run against a replaced yt-dlp
	installed := false
	e := newEnv(t, func(_ *testEnv, c *Config) {
		c.Updater.Install = func(context.Context) error {
			installed = true
			return nil
		}
	})
	e.createJob(map[string]any{"url": videoURL})
	waitFor(t, func() bool { return e.starter.count() == 1 })
	err := e.updater.Install(context.Background())
	if !errors.Is(err, system.ErrUpdateBusy) || installed {
		t.Fatalf("Install returned %v (installed %v)", err, installed)
	}
}

func TestSizedJobFetchesFullInfo(t *testing.T) {
	e := newEnv(t)
	e.createJob(map[string]any{"url": videoURL, "options": map[string]any{"max_size_mb": 10}})
	waitFor(t, func() bool { return e.starter.count() == 1 })
	calls := e.fetcher.recorded()
	if len(calls) != 1 || calls[0].flat || calls[0].url != videoURL {
		t.Fatalf("fetch calls %+v", calls)
	}
	// The fixture has no stream that fits, so the source is fetched capped at its 360p
	if got := e.starter.proc(0).argAfter("-S"); got != "res:360,vcodec:h264,acodec:aac" {
		t.Fatalf("source sort %q", got)
	}
}

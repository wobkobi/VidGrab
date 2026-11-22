// Package server is the helper's local HTTP API on 127.0.0.1: the request guard, CORS, the
// routes the extension calls, the download queue and the server's lifecycle (idle timeout,
// port-in-use handling, shutdown).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"vidgrab/internal/auth"
	"vidgrab/internal/core"
	"vidgrab/internal/system"
)

const (
	// Host is the only interface the server listens on.
	Host = "127.0.0.1"
	// InfoCacheTTL is how long an /info summary is reused.
	InfoCacheTTL = 10 * time.Minute
	// InfoCacheMax bounds the number of cached /info summaries.
	InfoCacheMax = 200
	// ExpandTimeout bounds listing a playlist or channel when queueing it, which can take far
	// longer than the panel's /info call for a large channel.
	ExpandTimeout = 120 * time.Second
	// SettingsFile is the settings file name inside the data directory.
	SettingsFile = "settings.json"
	// SetupError is the 503 message while yt-dlp and ffmpeg are still being installed.
	SetupError = "Setting up yt-dlp and ffmpeg"
	// errNeedsFfmpeg and needsFfmpegDetail refuse (or fail) a download that cannot work
	// without ffmpeg.
	errNeedsFfmpeg    = "This download needs ffmpeg"
	needsFfmpegDetail = "ffmpeg is not set up yet; check the VidGrab popup for setup errors."

	maxBodyBytes  = 1 << 20
	maxDetailLen  = 500
	clientHeader  = "X-Ytdl-Client" // canonical form of auth.ClientHeader
	corsMaxAge    = "600"
	corsMethods   = "GET, POST, PUT, DELETE"
	corsHeaders   = "Accept, Accept-Language, Content-Language, Content-Type, X-YTDL-Client, X-YTDL-Token"
	shutdownGrace = 5 * time.Second
)

var (
	allowedHosts    = map[string]bool{"127.0.0.1": true, "localhost": true}
	extensionOrigin = regexp.MustCompile(`^moz-extension://[0-9a-fA-F-]+$`)
	// Request headers a preflight may ask for (lower case): the CORS-safelisted ones plus ours
	corsAllowedHeaders = map[string]bool{
		"accept": true, "accept-language": true, "content-language": true, "content-type": true,
		"x-ytdl-client": true, "x-ytdl-token": true,
	}
	corsAllowedMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true}
)

// ToolsState is the part of system.Tools the server needs: the setup state for /health and
// the 503 answer, and the tool paths once ready.
type ToolsState interface {
	Status() system.ToolsStatus
	Paths() (ytdlp string, ffmpegDir string, ok bool)
}

// InfoFetcher returns raw yt-dlp JSON for a canonical URL (core.FetchRawInfo's contract: a
// *core.YtDlpTimeout when ctx's deadline passes, a *core.YtDlpError for other failures).
type InfoFetcher func(ctx context.Context, url string, flat, cookies bool) (map[string]any, error)

// Config holds everything a Server depends on; tests replace the functions with fakes. Nil
// fields get the real implementations unless noted.
type Config struct {
	// Version is the helper version reported by /health.
	Version string
	// Token is the install token every request except GET /health must carry; "" turns the
	// check off (tests only).
	Token string
	// SettingsPath is where settings persist; "" keeps them in memory only.
	SettingsPath string
	// Tools reports the yt-dlp/ffmpeg setup. Required.
	Tools ToolsState
	// Updater checks and installs yt-dlp updates. Required. New sets its Busy field and wraps
	// its Install so an update holds the queue (see New).
	Updater *system.Updater
	// FetchInfo runs yt-dlp -J; by default core.FetchRawInfo with the Tools paths.
	FetchInfo InfoFetcher
	// Ffmpeg reports whether ffmpeg is usable; by default whether the tools are ready, since
	// setup always provides ffmpeg.
	Ffmpeg func() bool
	// DownloadsDir returns the download folder; system.DownloadsDir by default.
	DownloadsDir func() string
	// StartProcess and KillProcess run yt-dlp for downloads and ffmpeg for size-limited
	// conversions; StartYtDlp and KillProcessTree by default.
	StartProcess StartFunc
	KillProcess  KillFunc
	// Encoder picks the H.264 encoder for size-limited conversions; a process-wide
	// core.EncoderProbe by default.
	Encoder func(ctx context.Context, ffmpeg string) string
	// OpenFile and Reveal open a finished download or show it in the file manager;
	// system.OpenFile and system.Reveal by default.
	OpenFile func(path string) error
	Reveal   func(path string) error
}

// apiError is an error answered as {"error", "detail"} with the given status.
type apiError struct {
	status int
	msg    string
	detail string
}

func (e *apiError) Error() string { return e.msg }

func newAPIError(status int, msg, detail string) *apiError {
	return &apiError{status: status, msg: msg, detail: detail}
}

// settingsStore holds the backend settings, persisted as JSON so they survive restarts before
// the extension pushes them.
type settingsStore struct {
	path    string
	mu      sync.Mutex
	current core.Settings
}

func newSettingsStore(path string) *settingsStore {
	s := &settingsStore{path: path, current: core.DefaultSettings()}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if loaded, err := core.DecodeSettings(data); err == nil {
				s.current = loaded
			}
		}
	}
	return s
}

// get returns the current settings.
func (s *settingsStore) get() core.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// update merges the fields present in a PUT /settings body, persists the result and returns
// the full settings. A failed save is logged; the new settings still apply.
func (s *settingsStore) update(changes core.SettingsUpdate) core.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = s.current.Apply(changes)
	if s.path != "" {
		data, _ := json.Marshal(s.current)
		err := os.MkdirAll(filepath.Dir(s.path), 0o700)
		if err == nil {
			err = os.WriteFile(s.path, data, 0o600)
		}
		if err != nil {
			log.Printf("server: could not save settings: %v", err)
		}
	}
	return s.current
}

type cacheKey struct {
	url     string
	cookies bool
}

type cacheEntry struct {
	at      time.Time
	summary core.Info
}

// Server is the HTTP API: the routes share the queue, settings, updater, /info cache and the
// clock of the last accepted request.
type Server struct {
	cfg      Config
	queue    *JobQueue
	settings *settingsStore
	handler  http.Handler

	cacheMu sync.Mutex
	cache   map[cacheKey]cacheEntry

	// lastRequest is nanoseconds since base (a monotonic reading) of the last request that
	// passed the guard
	base        time.Time
	lastRequest atomic.Int64

	stopOnce sync.Once
	stopped  chan struct{}

	// life ends when serve returns; background work started by requests (the installed
	// version read) runs under it rather than under the request's context
	life    context.Context
	endLife context.CancelFunc
}

// New builds a Server from cfg.
//
// It also wires cfg.Updater to the queue: Busy reports running downloads, and Install is
// wrapped so queued jobs cannot start while yt-dlp is replaced (the automatic update included)
// and the /info cache is cleared after a successful update. Install refuses with
// system.ErrUpdateBusy when a download started between Busy's check and the hold.
func New(cfg Config) *Server {
	s := &Server{
		cfg:      cfg,
		settings: newSettingsStore(cfg.SettingsPath),
		cache:    map[cacheKey]cacheEntry{},
		base:     time.Now(),
		stopped:  make(chan struct{}),
	}
	s.life, s.endLife = context.WithCancel(context.Background())
	s.touch()
	if s.cfg.FetchInfo == nil {
		s.cfg.FetchInfo = s.fetchWithTools
	}
	if s.cfg.Ffmpeg == nil {
		s.cfg.Ffmpeg = func() bool {
			_, _, ok := s.cfg.Tools.Paths()
			return ok
		}
	}
	if s.cfg.OpenFile == nil {
		s.cfg.OpenFile = system.OpenFile
	}
	if s.cfg.Reveal == nil {
		s.cfg.Reveal = system.Reveal
	}
	current := s.settings.get()
	s.queue = NewJobQueue(QueueConfig{
		Concurrency:  current.Concurrency,
		Fragments:    current.Fragments,
		Cookies:      current.Cookies,
		DownloadsDir: cfg.DownloadsDir,
		Ffmpeg:       s.cfg.Ffmpeg,
		Tools:        s.coreTools,
		Start:        cfg.StartProcess,
		Kill:         cfg.KillProcess,
		FetchInfo: func(canonical string) (map[string]any, error) {
			parsed, err := core.Classify(canonical)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), ExpandTimeout)
			defer cancel()
			summary, err := s.info(ctx, parsed)
			if err != nil {
				return nil, err
			}
			return listing(summary), nil
		},
		TitleLookup: s.cachedTitle,
		// The /info cache keeps only summaries, without the per-format sizes a size-limited
		// job needs, so those jobs always fetch the full JSON
		VideoInfo: func(ctx context.Context, canonical string) (map[string]any, error) {
			ctx, cancel := context.WithTimeout(ctx, core.InfoTimeout)
			defer cancel()
			return s.cfg.FetchInfo(ctx, canonical, false, s.settings.get().Cookies)
		},
		Encoder: cfg.Encoder,
	})

	updater := cfg.Updater
	updater.Busy = s.queue.Running
	install := updater.Install
	updater.Install = func(ctx context.Context) error {
		release := s.queue.Hold()
		defer release()
		if s.queue.Running() {
			return system.ErrUpdateBusy
		}
		if err := install(ctx); err != nil {
			return err
		}
		s.clearInfoCache()
		return nil
	}

	s.handler = s.routes()
	return s
}

// Queue returns the server's download queue.
func (s *Server) Queue() *JobQueue { return s.queue }

// Handler returns the guarded HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Stopped is closed once POST /shutdown has been answered.
func (s *Server) Stopped() <-chan struct{} { return s.stopped }

// touch records a request that passed the guard, for the idle timeout.
func (s *Server) touch() { s.lastRequest.Store(int64(time.Since(s.base))) }

// idleFor returns how long ago the last accepted request arrived.
func (s *Server) idleFor() time.Duration {
	return time.Since(s.base) - time.Duration(s.lastRequest.Load())
}

func (s *Server) coreTools() core.Tools {
	ytdlp, ffmpegDir, _ := s.cfg.Tools.Paths()
	return core.Tools{YtDlp: ytdlp, FfmpegDir: ffmpegDir}
}

// fetchWithTools is the default InfoFetcher: core.FetchRawInfo with the downloaded tools.
func (s *Server) fetchWithTools(ctx context.Context, url string, flat, cookies bool) (map[string]any, error) {
	ytdlp, ffmpegDir, ok := s.cfg.Tools.Paths()
	if !ok {
		return nil, &core.YtDlpError{Message: SetupError}
	}
	return core.FetchRawInfo(ctx, core.Tools{YtDlp: ytdlp, FfmpegDir: ffmpegDir}, url, flat, cookies, nil)
}

// info returns the /info summary for a URL, from the 10-minute cache when possible. Errors are
// the fetcher's (*core.YtDlpError or *core.YtDlpTimeout).
func (s *Server) info(ctx context.Context, parsed core.ParsedUrl) (core.Info, error) {
	cookies := s.settings.get().Cookies
	key := cacheKey{parsed.Canonical, cookies}
	now := time.Now()
	s.cacheMu.Lock()
	hit, ok := s.cache[key]
	s.cacheMu.Unlock()
	if ok && now.Sub(hit.at) < InfoCacheTTL {
		return hit.summary, nil
	}
	raw, err := s.cfg.FetchInfo(ctx, parsed.Canonical, parsed.Kind != core.KindVideo, cookies)
	if err != nil {
		return core.Info{}, err
	}
	summary := core.SummariseInfo(raw, parsed.Kind)
	s.cacheMu.Lock()
	if len(s.cache) >= InfoCacheMax {
		s.evictLocked(now)
	}
	s.cache[key] = cacheEntry{at: now, summary: summary}
	s.cacheMu.Unlock()
	return summary, nil
}

// listing turns an /info summary into the {title, entries: [{id, title}]} shape the queue
// expands, so queueing a playlist right after /info reuses the cached listing.
func listing(summary core.Info) map[string]any {
	entries := make([]any, len(summary.Entries))
	for i, e := range summary.Entries {
		entries[i] = map[string]any{"id": e.ID, "title": e.Title}
	}
	return map[string]any{"title": summary.Title, "entries": entries}
}

// evictLocked drops every expired entry, or the oldest one when none has expired; the caller
// holds cacheMu.
func (s *Server) evictLocked(now time.Time) {
	removed := false
	var oldestKey cacheKey
	var oldest time.Time
	first := true
	for k, e := range s.cache {
		if now.Sub(e.at) >= InfoCacheTTL {
			delete(s.cache, k)
			removed = true
			continue
		}
		if first || e.at.Before(oldest) {
			oldestKey, oldest, first = k, e.at, false
		}
	}
	if !removed && !first {
		delete(s.cache, oldestKey)
	}
}

// cachedTitle returns a video title already known from /info, without calling yt-dlp.
func (s *Server) cachedTitle(canonical string) string {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	hit, ok := s.cache[cacheKey{canonical, s.settings.get().Cookies}]
	if !ok {
		return ""
	}
	title, _ := hit.summary.Title.(string)
	return title
}

func (s *Server) clearInfoCache() {
	s.cacheMu.Lock()
	s.cache = map[cacheKey]cacheEntry{}
	s.cacheMu.Unlock()
}

// Responses

// writeJSON writes body as JSON with the given status. HTML escaping is off so "<", ">" and "&"
// in titles and errors arrive as written.
func writeJSON(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":"Internal Server Error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeError writes the standard {"error", "detail"} body; detail is left out when empty.
func writeError(w http.ResponseWriter, status int, msg, detail string) {
	body := map[string]string{"error": msg}
	if detail != "" {
		body["detail"] = detail
	}
	writeJSON(w, status, body)
}

// invalidRequest is the 400 answer for a body or query that fails validation, with the problem
// list cut to 500 characters.
func invalidRequest(detail string) *apiError {
	if utf8.RuneCountInString(detail) > maxDetailLen {
		detail = string([]rune(detail)[:maxDetailLen])
	}
	return newAPIError(http.StatusBadRequest, "Invalid request", detail)
}

// fromError maps a queue, URL, yt-dlp or update error to its API answer.
func fromError(err error) *apiError {
	var api *apiError
	var invalidURL *core.InvalidUrl
	var invalidItems *core.InvalidItems
	var jobErr *JobError
	var timeout *core.YtDlpTimeout
	var ytErr *core.YtDlpError
	var updateErr *system.UpdateError
	switch {
	case errors.As(err, &api):
		return api
	case errors.As(err, &invalidURL):
		return newAPIError(http.StatusBadRequest, invalidURL.Message, "")
	case errors.As(err, &invalidItems):
		return newAPIError(http.StatusBadRequest, invalidItems.Message, "")
	case errors.As(err, &jobErr):
		return newAPIError(jobErr.Status, jobErr.Message, jobErr.Detail)
	// The timeout first: errors.As also matches it as *core.YtDlpError
	case errors.As(err, &timeout):
		return newAPIError(http.StatusGatewayTimeout, timeout.Message, timeout.Detail)
	case errors.As(err, &ytErr):
		return newAPIError(http.StatusBadGateway, ytErr.Message, ytErr.Detail)
	case errors.Is(err, system.ErrUpdateBusy):
		return newAPIError(http.StatusConflict, err.Error(), "")
	case errors.As(err, &updateErr):
		return newAPIError(http.StatusBadGateway, updateErr.Message, updateErr.Detail)
	}
	log.Printf("server: unexpected error: %v", err)
	return newAPIError(http.StatusInternalServerError, "Internal Server Error", "")
}

// handlerFunc is a route that returns its body or an error.
type handlerFunc func(r *http.Request) (status int, body any, err error)

func (h handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, body, err := h(r)
	if err != nil {
		api := fromError(err)
		writeError(w, api.status, api.msg, api.detail)
		return
	}
	writeJSON(w, status, body)
}

// Query parameters

// queryValue returns the last value of a query parameter (as Starlette reads repeated ones),
// checking its length in characters. present is false when the parameter is absent.
func queryValue(r *http.Request, name string, maxLen int) (value string, present bool, err error) {
	values, ok := r.URL.Query()[name]
	if !ok || len(values) == 0 {
		return "", false, nil
	}
	value = values[len(values)-1]
	if maxLen > 0 && utf8.RuneCountInString(value) > maxLen {
		return "", true, invalidRequest(fmt.Sprintf("query.%s: String should have at most %d characters", name, maxLen))
	}
	return value, true, nil
}

// queryBool parses a boolean query parameter (1/0, true/false, t/f, yes/no, y/n, on/off, any
// case); absent means false.
func queryBool(r *http.Request, name string) (bool, error) {
	value, present, _ := queryValue(r, name, 0)
	if !present {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "on", "t", "true", "y", "yes":
		return true, nil
	case "0", "off", "f", "false", "n", "no":
		return false, nil
	}
	return false, invalidRequest(fmt.Sprintf("query.%s: Input should be a valid boolean, unable to interpret input", name))
}

// readBody reads a request body of at most maxBodyBytes.
func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, invalidRequest("body: " + err.Error())
	}
	if len(data) > maxBodyBytes {
		return nil, newAPIError(http.StatusRequestEntityTooLarge, "Request body too large", "")
	}
	return data, nil
}

// decodeError turns a core.Decode* failure into the 400 "Invalid request" answer.
func decodeError(err error) error {
	return invalidRequest(err.Error())
}

// Guard, CORS and routing

// hostAllowed accepts only loopback Host headers, which blocks DNS-rebinding pages.
func hostAllowed(hostHeader string) bool {
	host := hostHeader
	if !strings.HasPrefix(hostHeader, "[") {
		if i := strings.LastIndex(hostHeader, ":"); i >= 0 {
			host = hostHeader[:i]
			// A port, when present, must be all digits ("127.0.0.1:x.evil.com" is not a host)
			port := hostHeader[i+1:]
			if port == "" || strings.Trim(port, "0123456789") != "" {
				return false
			}
		}
	}
	return allowedHosts[strings.ToLower(host)]
}

// ServeHTTP runs the guard, then CORS, then the routes.
//
// The guard rejects anything a web page could send. The custom X-YTDL-Client header forces a
// CORS preflight that only moz-extension:// origins pass, and any other Origin is refused
// outright. Preflights themselves carry no custom header, so they go through to CORS once the
// Origin has been checked. Every other extension shares the moz-extension:// scheme, and local
// programs send no Origin at all, so the install token is what limits the API to this
// extension. GET /health is exempt: the extension checks the server's proof there before it
// sends the token. Guard rejections carry no CORS headers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostAllowed(r.Host) {
		writeError(w, http.StatusForbidden, "Forbidden host", "")
		return
	}
	origins, hasOrigin := r.Header["Origin"]
	origin := ""
	if hasOrigin && len(origins) > 0 {
		origin = origins[0]
	}
	if hasOrigin && !strings.HasPrefix(origin, "moz-extension://") {
		writeError(w, http.StatusForbidden, "Forbidden origin", "")
		return
	}
	if r.Method != http.MethodOptions && r.Header.Get(clientHeader) != "1" {
		writeError(w, http.StatusForbidden, "Missing X-YTDL-Client header", "")
		return
	}
	if s.cfg.Token != "" && r.Method != http.MethodOptions && r.URL.Path != "/health" &&
		!auth.TokenMatches(s.cfg.Token, r.Header.Get(auth.TokenHeader)) {
		writeError(w, http.StatusUnauthorized, "Backend key rejected", "Reload the extension so it fetches the key again.")
		return
	}
	s.touch()

	// CORS, following Starlette's CORSMiddleware: a preflight is answered here for any path
	if _, isPreflight := r.Header["Access-Control-Request-Method"]; isPreflight && r.Method == http.MethodOptions {
		s.preflight(w, r, origin)
		return
	}
	if hasOrigin {
		w.Header().Add("Vary", "Origin")
		if extensionOrigin.MatchString(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
	}
	s.handler.ServeHTTP(w, r)
}

// preflight answers a CORS preflight: 200 "OK" when the origin, method and requested headers
// are all allowed, else 400 naming what was not.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request, origin string) {
	h := w.Header()
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", corsMethods)
	h.Set("Access-Control-Max-Age", corsMaxAge)
	h.Set("Access-Control-Allow-Headers", corsHeaders)
	var failures []string
	if extensionOrigin.MatchString(origin) {
		h.Set("Access-Control-Allow-Origin", origin)
	} else {
		failures = append(failures, "origin")
	}
	if !corsAllowedMethods[r.Header.Get("Access-Control-Request-Method")] {
		failures = append(failures, "method")
	}
	if requested, ok := r.Header["Access-Control-Request-Headers"]; ok && len(requested) > 0 {
		for _, name := range strings.Split(requested[0], ",") {
			if !corsAllowedHeaders[strings.ToLower(strings.TrimSpace(name))] {
				failures = append(failures, "headers")
				break
			}
		}
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if len(failures) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Disallowed CORS "+strings.Join(failures, ", "))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "OK")
}

// statusRecorder captures the status an unmatched-route handler would send, discarding its
// plain-text body so the JSON error shape can be written instead.
type statusRecorder struct {
	header http.Header
	status int
}

func (s *statusRecorder) Header() http.Header         { return s.header }
func (s *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusRecorder) WriteHeader(status int)      { s.status = status }

// routes builds the API mux, answering unknown paths (404) and wrong methods (405) in the
// {"error"} shape.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", handlerFunc(s.handleHealth))
	mux.Handle("GET /info", handlerFunc(s.handleInfo))
	mux.Handle("POST /jobs", handlerFunc(s.handleCreateJobs))
	mux.Handle("GET /jobs", handlerFunc(s.handleListJobs))
	mux.Handle("DELETE /jobs", handlerFunc(s.handleClearJobs))
	mux.Handle("POST /jobs/{id}/{action}", handlerFunc(s.handleJobAction))
	mux.Handle("PUT /settings", handlerFunc(s.handlePutSettings))
	mux.Handle("POST /update", handlerFunc(s.handleUpdate))
	mux.Handle("POST /shutdown", handlerFunc(s.handleShutdown))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusMethodNotAllowed:
			if allow := rec.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed", "")
		case http.StatusNotFound:
			writeError(w, http.StatusNotFound, "Not Found", "")
		default:
			// A redirect for an uncleaned path: replay it for real
			mux.ServeHTTP(w, r)
		}
	})
}

// Routes

// healthBody is the GET /health response.
type healthBody struct {
	Status          string             `json:"status"`
	Version         string             `json:"version"`
	Ffmpeg          bool               `json:"ffmpeg"`
	YtDlpVersion    string             `json:"yt_dlp_version"`
	YtDlpLatest     *string            `json:"yt_dlp_latest"`
	UpdateAvailable bool               `json:"update_available"`
	Concurrency     int                `json:"concurrency"`
	Fragments       int                `json:"fragments"`
	Cookies         bool               `json:"cookies"`
	AutoUpdate      bool               `json:"auto_update"`
	Tools           system.ToolsStatus `json:"tools"`
	Proof           string             `json:"proof,omitempty"`
}

func (s *Server) handleHealth(r *http.Request) (int, any, error) {
	nonce, _, err := queryValue(r, "nonce", auth.MaxNonceLength)
	if err != nil {
		return 0, nil, err
	}
	settings := s.settings.get()
	updater := s.cfg.Updater
	// Only the cached version is read here: `yt-dlp --version` can outlast the callers'
	// timeouts. Until it is known, a background read is (re)started under the server's life
	if updater.CachedInstalled() == "" {
		if _, _, ok := s.cfg.Tools.Paths(); ok {
			updater.WarmInstalled(s.life)
		}
	}
	body := healthBody{
		Status:          "ok",
		Version:         s.cfg.Version,
		Ffmpeg:          s.cfg.Ffmpeg(),
		YtDlpVersion:    updater.InstalledVersion(),
		YtDlpLatest:     optional(updater.Latest()),
		UpdateAvailable: updater.UpdateAvailable(),
		Concurrency:     settings.Concurrency,
		Fragments:       settings.Fragments,
		Cookies:         settings.Cookies,
		AutoUpdate:      settings.AutoUpdate,
		Tools:           s.cfg.Tools.Status(),
	}
	if s.cfg.Token != "" && nonce != "" {
		body.Proof = auth.Proof(s.cfg.Token, nonce)
	}
	return http.StatusOK, body, nil
}

// requireTools answers 503 while yt-dlp and ffmpeg are being set up, with the current step (or
// the setup error) as the detail.
func (s *Server) requireTools() error {
	if _, _, ok := s.cfg.Tools.Paths(); ok {
		return nil
	}
	status := s.cfg.Tools.Status()
	detail := status.Step
	if status.State == system.ToolsError && status.Error != "" {
		detail = status.Error
	}
	return newAPIError(http.StatusServiceUnavailable, SetupError, detail)
}

func (s *Server) handleInfo(r *http.Request) (int, any, error) {
	if err := s.requireTools(); err != nil {
		return 0, nil, err
	}
	raw, present, err := queryValue(r, "url", core.MaxURLLength)
	if err != nil {
		return 0, nil, err
	}
	if !present {
		return 0, nil, invalidRequest("query.url: Field required")
	}
	parsed, err := core.Classify(raw)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), core.InfoTimeout)
	defer cancel()
	summary, err := s.info(ctx, parsed)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, summary, nil
}

func (s *Server) handleCreateJobs(r *http.Request) (int, any, error) {
	if err := s.requireTools(); err != nil {
		return 0, nil, err
	}
	data, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	body, err := core.DecodeJobRequest(data)
	if err != nil {
		return 0, nil, decodeError(err)
	}
	parsed, err := core.Classify(body.URL)
	if err != nil {
		return 0, nil, err
	}
	if parsed.Kind != core.KindVideo && body.Options.Clipped() {
		return 0, nil, newAPIError(http.StatusBadRequest, "Clips work on single videos only", "")
	}
	if core.NeedsFfmpeg(body.Options) && !s.cfg.Ffmpeg() {
		return 0, nil, newAPIError(http.StatusUnprocessableEntity, errNeedsFfmpeg, needsFfmpegDetail)
	}
	var ranges []core.ItemRange
	if parsed.Kind != core.KindVideo && body.Items != nil {
		if ranges, err = core.ParseItems(*body.Items); err != nil {
			return 0, nil, err
		}
	}
	jobs, err := s.queue.Create(parsed.Canonical, body.Options, ranges)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, map[string]any{"jobs": jobs}, nil
}

func (s *Server) handleListJobs(*http.Request) (int, any, error) {
	return http.StatusOK, map[string]any{"jobs": s.queue.List()}, nil
}

func (s *Server) handleClearJobs(r *http.Request) (int, any, error) {
	finished, err := queryBool(r, "finished")
	if err != nil {
		return 0, nil, err
	}
	if !finished {
		return 0, nil, newAPIError(http.StatusBadRequest, "Only finished=true is supported", "")
	}
	return http.StatusOK, map[string]any{"removed": s.queue.ClearFinished()}, nil
}

func (s *Server) handleJobAction(r *http.Request) (int, any, error) {
	id, action := r.PathValue("id"), r.PathValue("action")
	actions := map[string]func(string) (JobView, error){
		"pause":  s.queue.Pause,
		"resume": s.queue.Resume,
		"cancel": s.queue.Cancel,
		"retry":  s.queue.Retry,
	}
	if run, ok := actions[action]; ok {
		job, err := run(id)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]any{"job": job}, nil
	}
	if action != "open" && action != "reveal" {
		return 0, nil, newAPIError(http.StatusNotFound, "Unknown action", "")
	}
	path, err := s.queue.FinishedFile(id)
	if err != nil {
		return 0, nil, err
	}
	opener := s.cfg.OpenFile
	if action == "reveal" {
		opener = s.cfg.Reveal
	}
	if err := opener(path); err != nil {
		return 0, nil, newAPIError(http.StatusConflict, "Could not open the file", err.Error())
	}
	job, err := s.queue.Snapshot(id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"job": job}, nil
}

func (s *Server) handlePutSettings(r *http.Request) (int, any, error) {
	data, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	changes, err := core.DecodeSettingsUpdate(data)
	if err != nil {
		return 0, nil, decodeError(err)
	}
	settings := s.settings.update(changes)
	s.queue.ApplySettings(settings.Concurrency, settings.Fragments, settings.Cookies)
	return http.StatusOK, settings, nil
}

func (s *Server) handleUpdate(r *http.Request) (int, any, error) {
	result, err := s.runUpdate(r.Context())
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, result, nil
}

// runUpdate updates yt-dlp with the queue held, so no download starts against a
// half-replaced binary. Errors are system.ErrUpdateBusy (409) or a *system.UpdateError (502).
func (s *Server) runUpdate(ctx context.Context) (system.UpdateResult, error) {
	release := s.queue.Hold()
	defer release()
	return s.cfg.Updater.Update(ctx)
}

func (s *Server) handleShutdown(*http.Request) (int, any, error) {
	log.Printf("server: shutdown requested")
	s.stop()
	return http.StatusOK, map[string]string{"status": "stopping"}, nil
}

// stop asks Run to shut the server down; the reply in flight is still delivered, since
// http.Server.Shutdown waits for active requests.
func (s *Server) stop() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// Lifecycle

// IdleExpired reports whether the server should exit for being idle: idle is the time since
// the last accepted request, timeout <= 0 disables the check, and busy (jobs queued or
// running) always keeps it alive.
func IdleExpired(idle, timeout time.Duration, busy bool) bool {
	return timeout > 0 && !busy && idle >= timeout
}

// watchIdle calls stop once no request arrived for timeout and nothing is downloading, and
// returns then or when ctx ends. It checks every timeout/4, clamped to 0.1-5 seconds.
func watchIdle(ctx context.Context, timeout time.Duration, idleFor func() time.Duration, busy func() bool, stop func()) {
	interval := min(5*time.Second, max(timeout/4, 100*time.Millisecond))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if IdleExpired(idleFor(), timeout, busy()) {
				log.Printf("server: no requests for %s and no active downloads; exiting", timeout)
				stop()
				return
			}
		}
	}
}

// RunOptions configure Run.
type RunOptions struct {
	// Version is reported by /health.
	Version string
	// Port is the loopback port to listen on.
	Port int
	// IdleTimeout makes the server exit after this long without requests or active
	// downloads; 0 means never.
	IdleTimeout time.Duration
	// DataDir holds the token, settings, update state and tools.
	DataDir string
	// DownloadsDir overrides the download folder; "" uses the user's Downloads.
	DownloadsDir string
}

// probeBackend checks who holds the port; tests replace it.
var probeBackend = auth.Probe

// Listen binds the server's socket on 127.0.0.1:port exclusively, so no other process can
// bind the same port alongside it.
func Listen(port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: exclusiveControl}
	return lc.Listen(context.Background(), "tcp", fmt.Sprintf("%s:%d", Host, port))
}

// Run serves the API until ctx ends, POST /shutdown arrives or the idle timeout passes, and
// returns the process exit code.
//
// A port already in use is not an error: when the owner proves it knows the token another
// helper is running, and otherwise the extension refuses that program, so both exit 0
// quietly. Once the port is bound, yt-dlp's private temp folder is emptied (no yt-dlp of
// this data directory can be running then) and tool setup starts. Once the tools are ready
// the installed version is read and the automatic yt-dlp update runs. On the way out every
// download is killed.
func Run(ctx context.Context, opts RunOptions) int {
	token, err := auth.LoadToken(filepath.Join(opts.DataDir, auth.TokenFile))
	if err != nil {
		log.Printf("server: could not read or create the backend key: %v", err)
		return 1
	}
	listener, err := Listen(opts.Port)
	if err != nil {
		running, probeErr := probeBackend(opts.Port, token, 1500*time.Millisecond)
		switch {
		case probeErr != nil:
			log.Printf("server: %v; the extension will refuse it until that program stops", probeErr)
		case running != nil:
			log.Printf("server: port %d is already in use by another helper", opts.Port)
		default:
			log.Printf("server: port %d is in use but nothing answered as the helper", opts.Port)
		}
		return 0
	}

	prepareChildTemp(opts.DataDir)
	tools := system.NewTools(filepath.Join(opts.DataDir, "tools"))
	tools.Start()
	updater := system.NewUpdater(filepath.Join(opts.DataDir, system.UpdateStateFile), tools)
	cfg := Config{
		Version:      opts.Version,
		Token:        token,
		SettingsPath: filepath.Join(opts.DataDir, SettingsFile),
		Tools:        tools,
		Updater:      updater,
	}
	if opts.DownloadsDir != "" {
		dir := opts.DownloadsDir
		cfg.DownloadsDir = func() string { return dir }
	}
	srv := New(cfg)
	return srv.serve(ctx, listener, opts.IdleTimeout, func(ctx context.Context) {
		autoUpdate(ctx, tools, updater, func() bool { return srv.settings.get().AutoUpdate })
	})
}

// prepareChildTemp points yt-dlp's TEMP at dataDir/tmp and empties it. Call it only once
// the port is bound: then no other helper on this data directory can be running yt-dlp.
// When the folder cannot be created, yt-dlp keeps the system temp folder.
func prepareChildTemp(dataDir string) {
	dir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("server: could not create %s, yt-dlp keeps the system temp folder: %v", dir, err)
		return
	}
	core.SetChildTempDir(dir)
	core.SweepChildTemp()
}

// serve runs the HTTP server on listener with the idle watchdog and the background task until
// a stop is requested, then shuts down: downloads are killed and in-flight replies delivered.
func (s *Server) serve(ctx context.Context, listener net.Listener, idleTimeout time.Duration, background func(context.Context)) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpServer := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	if idleTimeout > 0 {
		go watchIdle(ctx, idleTimeout, s.idleFor, s.queue.Busy, s.stop)
	}
	if background != nil {
		go background(ctx)
	}
	log.Printf("server: VidGrab helper %s on http://%s (idle timeout %s)", s.cfg.Version, listener.Addr(), idleTimeout)

	code := 0
	select {
	case <-ctx.Done():
	case <-s.stopped:
	case err := <-serveErr:
		log.Printf("server: %v", err)
		code = 1
	}
	cancel()
	s.endLife()
	s.queue.Shutdown()
	shutdownCtx, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	_ = httpServer.Shutdown(shutdownCtx)
	if !s.queue.WaitIdle(shutdownGrace) {
		log.Printf("server: downloads did not stop in time")
	}
	log.Printf("server: stopped")
	return code
}

// autoUpdate waits for the first-run tool setup, then runs system.Updater.AutoUpdate: it
// reads the installed yt-dlp version into the cache /health serves (whatever the auto-update
// setting), runs the daily yt-dlp check and, when enabled, the update (which holds the queue
// through the wrapped Install, see New).
func autoUpdate(ctx context.Context, tools ToolsState, updater *system.Updater, enabled func() bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if _, _, ok := tools.Paths(); ok {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
	updater.AutoUpdate(ctx, enabled)
}

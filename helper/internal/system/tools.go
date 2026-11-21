// yt-dlp and ffmpeg downloader: fetches both on first run, verifies their SHA-256 sums and
// reports progress for /health.

package system

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"

	"vidgrab/internal/core"
)

// Tool installation states reported by ToolsStatus.State.
const (
	ToolsReady      = "ready"
	ToolsInstalling = "installing"
	ToolsError      = "error"
)

const (
	// DefaultGitHubAPI is the GitHub REST API base used for release lookups.
	DefaultGitHubAPI = "https://api.github.com"
	// DefaultMacFFmpegBase serves the static macOS ffmpeg builds.
	DefaultMacFFmpegBase = "https://ffmpeg.martin-riedl.de"
	// IgnorePathFFmpegEnv set to "1" makes the helper download ffmpeg even when PATH has one.
	IgnorePathFFmpegEnv = "VIDGRAB_IGNORE_PATH_FFMPEG"

	userAgent      = "VidGrab-helper"
	ytdlpRepo      = "yt-dlp/yt-dlp"
	ffmpegRepo     = "yt-dlp/FFmpeg-Builds"
	ytdlpSumsName  = "SHA2-256SUMS"
	ffmpegSumsName = "checksums.sha256"

	maxDownloadBytes = 1 << 30
	maxSmallBytes    = 1 << 20
	stallTimeout     = 60 * time.Second
	versionTimeout   = 60 * time.Second
)

// ToolsStatus is the observable state of the tool setup, reported under /health "tools".
type ToolsStatus struct {
	State   string  `json:"state"`
	Step    string  `json:"step"`
	Percent float64 `json:"percent"`
	Error   string  `json:"error"`
}

// Tools makes sure yt-dlp and ffmpeg exist under Dir, downloading them when missing.
//
// The exported fields are read when a download starts; set them before calling Start.
type Tools struct {
	// Dir holds yt-dlp and the ffmpeg/ subfolder, normally DataDir()/tools.
	Dir string
	// GitHubAPI is the GitHub REST API base URL.
	GitHubAPI string
	// MacFFmpegBase is the base URL of the macOS ffmpeg builds.
	MacFFmpegBase string
	// Client performs every HTTP request.
	Client *http.Client
	// GOOS and GOARCH pick the assets to download.
	GOOS, GOARCH string
	// IgnorePathFFmpeg forces the ffmpeg download even when PATH has ffmpeg and ffprobe.
	IgnorePathFFmpeg bool
	// LookPath resolves commands on PATH.
	LookPath func(string) (string, error)

	opMu sync.Mutex // serialises downloads: first-run setup against Update

	mu        sync.Mutex
	status    ToolsStatus
	ready     bool
	running   bool
	ffmpegDir string
	version   versionCache
}

// versionCache remembers the yt-dlp version for one exact file, so --version (a slow
// self-extracting start) runs only when the binary changes.
type versionCache struct {
	modTime time.Time
	size    int64
	version string
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// httpsOnly refuses any request that is not HTTPS. It sits in the transport, so it also
// covers every redirect hop and the release asset URLs, which come from the API response.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-HTTPS download URL %s", r.URL.Redacted())
	}
	return h.next.RoundTrip(r)
}

// NewTools returns a Tools for dir with the real network, platform and PATH.
func NewTools(dir string) *Tools {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &Tools{
		Dir:              dir,
		GitHubAPI:        DefaultGitHubAPI,
		MacFFmpegBase:    DefaultMacFFmpegBase,
		Client:           &http.Client{Transport: httpsOnly{transport}},
		GOOS:             runtime.GOOS,
		GOARCH:           runtime.GOARCH,
		IgnorePathFFmpeg: os.Getenv(IgnorePathFFmpegEnv) == "1",
		LookPath:         exec.LookPath,
		status:           ToolsStatus{State: ToolsInstalling, Step: "Checking tools"},
	}
}

// Status returns the current setup state.
func (t *Tools) Status() ToolsStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// Paths returns the yt-dlp binary and the folder holding ffmpeg and ffprobe ("" when they
// come from PATH). ok is false until setup has finished.
func (t *Tools) Paths() (ytdlp string, ffmpegDir string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ready {
		return "", "", false
	}
	return t.YtdlpPath(), t.ffmpegDir, true
}

// YtdlpPath returns where the yt-dlp binary lives (whether or not it exists yet).
func (t *Tools) YtdlpPath() string {
	return filepath.Join(t.Dir, t.exe("yt-dlp"))
}

func (t *Tools) localFFmpegDir() string {
	return filepath.Join(t.Dir, "ffmpeg")
}

func (t *Tools) exe(name string) string {
	if t.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// setupPlan lists what Ensure still has to download and where ffmpeg comes from.
type setupPlan struct {
	ytdlp     bool
	ffmpeg    bool
	ffmpegDir string
}

func (t *Tools) plan() setupPlan {
	var p setupPlan
	p.ytdlp = !fileExists(t.YtdlpPath())
	if !t.IgnorePathFFmpeg && ffmpegOnPath(t.LookPath) {
		return p
	}
	local := t.localFFmpegDir()
	p.ffmpegDir = local
	p.ffmpeg = !fileExists(filepath.Join(local, t.exe("ffmpeg"))) ||
		!fileExists(filepath.Join(local, t.exe("ffprobe")))
	return p
}

// Start makes the tools ready: at once when everything is present, else by downloading in a
// background goroutine while Status reports progress. It does nothing while a setup runs or
// once ready, and retries after an error. Leftovers of an interrupted earlier run are swept
// first, unless a download holds opMu.
func (t *Tools) Start() {
	if t.opMu.TryLock() {
		t.sweepLeftovers()
		t.opMu.Unlock()
	}
	t.mu.Lock()
	if t.running || t.ready {
		t.mu.Unlock()
		return
	}
	p := t.plan()
	if !p.ytdlp && !p.ffmpeg {
		t.markReady(p.ffmpegDir)
		t.mu.Unlock()
		return
	}
	t.running = true
	t.status = ToolsStatus{State: ToolsInstalling, Step: "Preparing"}
	t.mu.Unlock()
	go func() { _ = t.ensure(context.Background()) }()
}

// Ensure is the synchronous form of Start: it returns once the tools are ready or setup
// failed (the error is also kept in Status).
func (t *Tools) Ensure(ctx context.Context) error {
	t.mu.Lock()
	if t.ready {
		t.mu.Unlock()
		return nil
	}
	t.running = true
	t.status = ToolsStatus{State: ToolsInstalling, Step: "Preparing"}
	t.mu.Unlock()
	return t.ensure(ctx)
}

// markReady records a finished setup; the caller holds t.mu.
func (t *Tools) markReady(ffmpegDir string) {
	t.ready = true
	t.ffmpegDir = ffmpegDir
	t.status = ToolsStatus{State: ToolsReady, Step: "", Percent: 100}
}

// ensure plans under opMu, so a setup that waited for another one only downloads what is
// still missing.
func (t *Tools) ensure(ctx context.Context) error {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	t.sweepLeftovers()
	p := t.plan()
	err := t.runPlan(ctx, p)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false
	if err != nil {
		t.status.State = ToolsError
		t.status.Error = err.Error()
		return err
	}
	t.markReady(p.ffmpegDir)
	return nil
}

// runPlan downloads what p lists. Progress is one 0-100 bar over both steps: yt-dlp is
// about a tenth of ffmpeg's size, so it takes the first 15% when both are needed.
func (t *Tools) runPlan(ctx context.Context, p setupPlan) error {
	ytdlpShare := 0.0
	if p.ytdlp {
		ytdlpShare = 1
		if p.ffmpeg {
			ytdlpShare = 0.15
		}
	}
	if p.ytdlp {
		t.setStep("Downloading yt-dlp", 0)
		rel, err := t.fetchRelease(ctx, ytdlpRepo)
		if err != nil {
			return err
		}
		if err := t.installYtdlp(ctx, rel, t.progress(0, ytdlpShare)); err != nil {
			return err
		}
	}
	if p.ffmpeg {
		t.setStep("Downloading ffmpeg", ytdlpShare*100)
		if err := t.installFFmpeg(ctx, t.progress(ytdlpShare, 1-ytdlpShare)); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tools) setStep(step string, percent float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = ToolsStatus{State: ToolsInstalling, Step: step, Percent: percent}
}

// progress maps a step's done/total bytes onto its slice [base, base+share] of the bar.
func (t *Tools) progress(base, share float64) func(done, total int64) {
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		frac := math.Min(float64(done)/float64(total), 1)
		percent := math.Round((base+share*frac)*1000) / 10
		t.mu.Lock()
		t.status.Percent = percent
		t.mu.Unlock()
	}
}

// LatestYtdlpVersion returns the tag of the newest yt-dlp release on GitHub.
func (t *Tools) LatestYtdlpVersion(ctx context.Context) (string, error) {
	rel, err := t.fetchRelease(ctx, ytdlpRepo)
	if err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// Version returns the installed yt-dlp version by running `yt-dlp --version`, cached until
// the binary changes.
func (t *Tools) Version(ctx context.Context) (string, error) {
	binary := t.YtdlpPath()
	info, err := os.Stat(binary)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	cached := t.version
	t.mu.Unlock()
	if cached.version != "" && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.version, nil
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = core.ChildEnv()
	PrepareChild(cmd)
	// Kill the whole tree on cancel: the self-extracting build runs as a bootloader plus a
	// child, and killing only the bootloader would leave the child holding its temp files
	cmd.Cancel = func() error { return KillTree(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	exited := core.TrackChild()
	out, err := cmd.Output()
	exited(ctx.Err() != nil)
	if err != nil {
		return "", fmt.Errorf("could not run yt-dlp --version: %w", err)
	}
	version := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if version == "" {
		return "", errors.New("yt-dlp --version printed nothing")
	}
	t.rememberVersion(binary, version)
	return version, nil
}

func (t *Tools) rememberVersion(binary, version string) {
	info, err := os.Stat(binary)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.version = versionCache{modTime: info.ModTime(), size: info.Size(), version: version}
	t.mu.Unlock()
}

// Update downloads the latest yt-dlp release (checksum-verified) when its tag differs from
// the installed version, and returns the version installed afterwards. The caller must make
// sure yt-dlp is not running: Windows cannot replace a running executable.
func (t *Tools) Update(ctx context.Context) (string, error) {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	if !fileExists(t.YtdlpPath()) {
		return "", errors.New("yt-dlp is still being set up")
	}
	rel, err := t.fetchRelease(ctx, ytdlpRepo)
	if err != nil {
		return "", err
	}
	if current, err := t.Version(ctx); err == nil && current == rel.TagName {
		return current, nil
	}
	if err := t.installYtdlp(ctx, rel, nil); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// ytdlpAssetName returns the standalone yt-dlp release asset for a platform. The macOS
// build is universal (amd64 and arm64).
func ytdlpAssetName(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "windows/amd64":
		return "yt-dlp.exe", nil
	case "windows/arm64":
		return "yt-dlp_arm64.exe", nil
	case "windows/386":
		return "yt-dlp_x86.exe", nil
	case "darwin/amd64", "darwin/arm64":
		return "yt-dlp_macos", nil
	case "linux/amd64":
		return "yt-dlp_linux", nil
	case "linux/arm64":
		return "yt-dlp_linux_aarch64", nil
	}
	return "", fmt.Errorf("no yt-dlp build for %s/%s", goos, goarch)
}

// ffmpegAssetName returns the yt-dlp/FFmpeg-Builds asset for Windows and Linux. The static
// "gpl" builds are used: the "-shared" ones need their DLLs next to the binaries.
func ffmpegAssetName(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "windows/amd64":
		return "ffmpeg-master-latest-win64-gpl.zip", nil
	case "windows/arm64":
		return "ffmpeg-master-latest-winarm64-gpl.zip", nil
	case "linux/amd64":
		return "ffmpeg-master-latest-linux64-gpl.tar.xz", nil
	case "linux/arm64":
		return "ffmpeg-master-latest-linuxarm64-gpl.tar.xz", nil
	}
	return "", fmt.Errorf("no ffmpeg build for %s/%s", goos, goarch)
}

func (t *Tools) installYtdlp(ctx context.Context, rel *release, progress func(int64, int64)) error {
	name, err := ytdlpAssetName(t.GOOS, t.GOARCH)
	if err != nil {
		return err
	}
	want, file, err := t.verifiedAsset(ctx, rel, name, ytdlpSumsName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := t.download(ctx, file.URL, file.Size, want, progress)
	if err != nil {
		return fmt.Errorf("yt-dlp download failed: %w", err)
	}
	if err := replaceFile(tmp, t.YtdlpPath()); err != nil {
		return fmt.Errorf("could not install yt-dlp: %w", err)
	}
	t.rememberVersion(t.YtdlpPath(), rel.TagName)
	return nil
}

// verifiedAsset finds asset name in rel and its expected SHA-256 from the release's
// checksum file sumsName.
func (t *Tools) verifiedAsset(ctx context.Context, rel *release, name, sumsName string) (string, releaseAsset, error) {
	var file, sums *releaseAsset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case name:
			file = &rel.Assets[i]
		case sumsName:
			sums = &rel.Assets[i]
		}
	}
	if file == nil {
		return "", releaseAsset{}, fmt.Errorf("release %s has no %s", rel.TagName, name)
	}
	if sums == nil {
		return "", releaseAsset{}, fmt.Errorf("release %s has no %s", rel.TagName, sumsName)
	}
	sumsMap, err := t.fetchChecksums(ctx, sums.URL)
	if err != nil {
		return "", releaseAsset{}, err
	}
	want, ok := sumsMap[name]
	if !ok {
		return "", releaseAsset{}, fmt.Errorf("%s lists no checksum for %s", sumsName, name)
	}
	return want, *file, nil
}

func (t *Tools) installFFmpeg(ctx context.Context, progress func(int64, int64)) error {
	dest := t.localFFmpegDir()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if t.GOOS == "darwin" {
		return t.installMacFFmpeg(ctx, dest, progress)
	}
	name, err := ffmpegAssetName(t.GOOS, t.GOARCH)
	if err != nil {
		return err
	}
	rel, err := t.fetchRelease(ctx, ffmpegRepo)
	if err != nil {
		return err
	}
	want, file, err := t.verifiedAsset(ctx, rel, name, ffmpegSumsName)
	if err != nil {
		return err
	}
	tmp, err := t.download(ctx, file.URL, file.Size, want, progress)
	if err != nil {
		return fmt.Errorf("ffmpeg download failed: %w", err)
	}
	defer os.Remove(tmp)
	t.setStep("Unpacking ffmpeg", t.Status().Percent)
	kind := "zip"
	if strings.HasSuffix(name, ".tar.xz") {
		kind = "tar.xz"
	}
	return extractTools(tmp, kind, dest, []string{t.exe("ffmpeg"), t.exe("ffprobe")})
}

// installMacFFmpeg fetches ffmpeg and ffprobe from the martin-riedl.de static builds.
//
// The site's /redirect/latest/... URLs answer GET with a 307 to a versioned folder (HEAD
// gets a 404), and the checksum sits next to the real file as "<file>.sha256" - the
// redirect path itself has no .sha256 variant. So the redirect is resolved without
// following it, and both files are fetched from the versioned folder.
func (t *Tools) installMacFFmpeg(ctx context.Context, dest string, progress func(int64, int64)) error {
	arch := t.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("no ffmpeg build for darwin/%s", arch)
	}
	tools := []string{"ffmpeg", "ffprobe"}
	for i, tool := range tools {
		redirect := fmt.Sprintf("%s/redirect/latest/macos/%s/release/%s.zip", strings.TrimRight(t.MacFFmpegBase, "/"), arch, tool)
		location, err := t.resolveRedirect(ctx, redirect)
		if err != nil {
			return err
		}
		sums, err := t.fetchChecksums(ctx, location+".sha256")
		if err != nil {
			return err
		}
		want, ok := sums[tool+".zip"]
		if !ok {
			return fmt.Errorf("no checksum for %s.zip", tool)
		}
		share := 1 / float64(len(tools))
		stepProgress := func(done, total int64) {
			if progress != nil && total > 0 {
				progress(int64((float64(i)+float64(done)/float64(total))*share*1e6), 1e6)
			}
		}
		tmp, err := t.download(ctx, location, 0, want, stepProgress)
		if err != nil {
			return fmt.Errorf("%s download failed: %w", tool, err)
		}
		err = extractTools(tmp, "zip", dest, []string{tool})
		os.Remove(tmp)
		if err != nil {
			return err
		}
	}
	return nil
}

// resolveRedirect GETs url without following the redirect and returns the absolute target.
func (t *Tools) resolveRedirect(ctx context.Context, url string) (string, error) {
	client := *t.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := t.newRequest(ctx, url)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("%s answered HTTP %d instead of a redirect", url, resp.StatusCode)
	}
	location, err := resp.Location()
	if err != nil {
		return "", err
	}
	return location.String(), nil
}

func (t *Tools) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return http.DefaultClient
}

func (t *Tools) newRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// fetchRelease reads the latest release of repo ("owner/name") from the GitHub API.
func (t *Tools) fetchRelease(ctx context.Context, repo string) (*release, error) {
	url := strings.TrimRight(t.GitHubAPI, "/") + "/repos/" + repo + "/releases/latest"
	req, err := t.newRequest(ctx, url)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("GitHub refused the request for %s (HTTP %d, rate limit?)", repo, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered HTTP %d for %s", resp.StatusCode, repo)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*maxSmallBytes)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("unreadable release data for %s: %w", repo, err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release data for %s has no tag", repo)
	}
	return &rel, nil
}

// fetchChecksums downloads a sha256sum-style file ("<hex>  <name>" per line, with an
// optional "*" binary marker before the name) and maps names to lower-case hex digests.
func (t *Tools) fetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	req, err := t.newRequest(ctx, url)
	if err != nil {
		return nil, err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checksum file answered HTTP %d", resp.StatusCode)
	}
	return parseChecksums(io.LimitReader(resp.Body, maxSmallBytes))
}

func parseChecksums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(digest); err != nil {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = digest
	}
	return sums, scanner.Err()
}

// download streams url into a temp file in t.Dir while hashing it, and returns the temp
// path once the SHA-256 matches want. On any failure the temp file is removed. A body that
// delivers nothing for stallTimeout is abandoned, so setup cannot hang forever.
func (t *Tools) download(ctx context.Context, url string, sizeHint int64, want string, progress func(int64, int64)) (path string, err error) {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	req, err := t.newRequest(ctx, url)
	if err != nil {
		return "", err
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total <= 0 {
		total = sizeHint
	}

	out, err := os.CreateTemp(t.Dir, ".download-*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	hash := sha256.New()
	body := &watchedReader{r: io.LimitReader(resp.Body, maxDownloadBytes+1), onRead: func(n int64) {
		stall.Reset(stallTimeout)
		if progress != nil {
			progress(n, total)
		}
	}}
	written, err := io.Copy(io.MultiWriter(out, hash), body)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return "", errors.New("download stalled or was cancelled")
		}
		return "", err
	}
	if written > maxDownloadBytes {
		return "", errors.New("download is larger than expected")
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != want {
		err = fmt.Errorf("checksum mismatch (expected %s, got %s)", want, got)
		return "", err
	}
	return out.Name(), nil
}

// watchedReader reports the running byte count after every read.
type watchedReader struct {
	r      io.Reader
	n      int64
	onRead func(total int64)
}

func (w *watchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.n += int64(n)
		w.onRead(w.n)
	}
	return n, err
}

// extractTools copies the regular files whose base name is in names out of a zip or
// tar.xz archive into dest. Entry paths are never used to build the output path, only the
// fixed base names, so a hostile archive cannot write outside dest (zip-slip). Every name
// must be found.
func extractTools(archive, kind, dest string, names []string) error {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	found := map[string]bool{}
	take := func(entryName string, r io.Reader) error {
		base := path.Base(strings.ReplaceAll(entryName, `\`, "/"))
		if !wanted[base] || found[base] {
			return nil
		}
		if err := writeExecutable(r, dest, base); err != nil {
			return err
		}
		found[base] = true
		return nil
	}

	var err error
	switch kind {
	case "zip":
		err = extractZip(archive, take)
	case "tar.xz":
		err = extractTarXz(archive, take, func() bool { return len(found) == len(wanted) })
	default:
		err = fmt.Errorf("unknown archive type %q", kind)
	}
	if err != nil {
		return err
	}
	for _, name := range names {
		if !found[name] {
			return fmt.Errorf("the archive has no %s", name)
		}
	}
	return nil
}

func extractZip(archive string, take func(string, io.Reader) error) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = take(f.Name, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarXz(archive string, take func(string, io.Reader) error, done func() bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(bufio.NewReader(f))
	if err != nil {
		return err
	}
	tr := tar.NewReader(xr)
	for !done() {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := take(hdr.Name, tr); err != nil {
			return err
		}
	}
	return nil
}

// writeExecutable writes r to dest/name through a temp file and an atomic rename.
func writeExecutable(r io.Reader, dest, name string) (err error) {
	out, err := os.CreateTemp(dest, ".extract-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	n, err := io.Copy(out, io.LimitReader(r, maxDownloadBytes+1))
	if err != nil {
		return err
	}
	if n > maxDownloadBytes {
		return fmt.Errorf("%s is larger than expected", name)
	}
	if err = out.Close(); err != nil {
		return err
	}
	return replaceFile(out.Name(), filepath.Join(dest, name))
}

// replaceFile marks tmp executable and renames it over target, removing tmp on failure.
//
// Windows refuses to overwrite an executable that is running (yt-dlp serving an /info call)
// but does allow renaming it, so there the old file is first moved aside to "<target>.old".
// That file is deleted best-effort straight away, or by sweepLeftovers on a later start once
// the process holding it has exited. A failed swap puts the old file back.
func replaceFile(tmp, target string) error {
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	old := ""
	if runtime.GOOS == "windows" && fileExists(target) {
		old = target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		if old != "" {
			_ = os.Rename(old, target)
		}
		return err
	}
	if old != "" {
		_ = os.Remove(old)
	}
	return nil
}

// sweepLeftovers deletes what an interrupted download, extraction or replacement left in
// Dir and its ffmpeg folder: ".download-*.tmp", ".extract-*.tmp" and "*.old". The caller
// holds opMu, so no download of this Tools is in flight. Errors are ignored: an ".old"
// executable that is still running is retried on the next start.
func (t *Tools) sweepLeftovers() {
	// Names are matched rather than globbing the full path, which would misread "[" in Dir
	patterns := []string{".download-*.tmp", ".extract-*.tmp", "*.old"}
	for _, dir := range []string{t.Dir, t.localFFmpegDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			for _, pattern := range patterns {
				if ok, _ := filepath.Match(pattern, entry.Name()); ok {
					_ = os.Remove(filepath.Join(dir, entry.Name()))
					break
				}
			}
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

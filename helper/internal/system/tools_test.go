package system

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulikunitz/xz"

	"vidgrab/internal/core"
)

// fakeGitHub serves the two release APIs, their assets and the macOS ffmpeg site.
type fakeGitHub struct {
	srv       *httptest.Server
	mu        sync.Mutex
	ytdlpTag  string
	ytdlp     map[string][]byte // asset name > content
	ffmpeg    map[string][]byte
	badSums   map[string]bool // asset names whose listed checksum is wrong
	downloads map[string]int  // request path > count
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		ytdlpTag:  "2026.10.26",
		ytdlp:     map[string][]byte{},
		ffmpeg:    map[string][]byte{},
		badSums:   map[string]bool{},
		downloads: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) sums(files map[string][]byte) []byte {
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		digest := sha(files[name])
		if f.badSums[name] {
			digest = strings.Repeat("0", 64)
		}
		fmt.Fprintf(&b, "%s  %s\n", digest, name)
	}
	return []byte(b.String())
}

func (f *fakeGitHub) release(tag, prefix string, files map[string][]byte, sumsName string) []byte {
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int    `json:"size"`
	}
	assets := []asset{{Name: sumsName, URL: f.srv.URL + prefix + sumsName, Size: 1}}
	for name, data := range files {
		assets = append(assets, asset{Name: name, URL: f.srv.URL + prefix + name, Size: len(data)})
	}
	body, _ := json.Marshal(map[string]any{"tag_name": tag, "assets": assets})
	return body
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads[r.URL.Path]++
	if r.Header.Get("User-Agent") != "VidGrab-helper" {
		http.Error(w, "missing user agent", http.StatusForbidden)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/repos/yt-dlp/yt-dlp/releases/latest":
		_, _ = w.Write(f.release(f.ytdlpTag, "/yt/", f.ytdlp, "SHA2-256SUMS"))
	case path == "/repos/yt-dlp/FFmpeg-Builds/releases/latest":
		_, _ = w.Write(f.release("latest", "/ff/", f.ffmpeg, "checksums.sha256"))
	case path == "/yt/SHA2-256SUMS":
		_, _ = w.Write(f.sums(f.ytdlp))
	case path == "/ff/checksums.sha256":
		_, _ = w.Write(f.sums(f.ffmpeg))
	case strings.HasPrefix(path, "/yt/"):
		_, _ = w.Write(f.ytdlp[strings.TrimPrefix(path, "/yt/")])
	case strings.HasPrefix(path, "/ff/"):
		_, _ = w.Write(f.ffmpeg[strings.TrimPrefix(path, "/ff/")])
	case strings.HasPrefix(path, "/redirect/latest/macos/arm64/release/"):
		// Mirrors martin-riedl.de: the redirect has no .sha256 form, the real folder does
		name := strings.TrimPrefix(path, "/redirect/latest/macos/arm64/release/")
		if strings.HasSuffix(name, ".sha256") || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/download/macos/arm64/1789931890_9.0.2/"+name, http.StatusTemporaryRedirect)
	case strings.HasPrefix(path, "/download/macos/arm64/1789931890_9.0.2/"):
		name := strings.TrimPrefix(path, "/download/macos/arm64/1789931890_9.0.2/")
		if zipName, ok := strings.CutSuffix(name, ".sha256"); ok {
			digest := sha(f.ffmpeg[zipName])
			if f.badSums[zipName] {
				digest = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", digest, zipName)
			return
		}
		_, _ = w.Write(f.ffmpeg[name])
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads[path]
}

type archiveEntry struct {
	name    string
	body    string
	symlink bool
}

func makeZip(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(e.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTarXz(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(xw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.symlink {
			hdr = &tar.Header{Name: e.name, Linkname: e.body, Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !e.symlink {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func notFound(string) (string, error) { return "", errors.New("not found") }

func onPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func (f *fakeGitHub) tools(t *testing.T, goos, goarch string) *Tools {
	t.Helper()
	tools := NewTools(filepath.Join(t.TempDir(), "tools"))
	tools.GitHubAPI = f.srv.URL
	tools.MacFFmpegBase = f.srv.URL
	tools.Client = f.srv.Client()
	tools.GOOS, tools.GOARCH = goos, goarch
	tools.IgnorePathFFmpeg = false
	tools.LookPath = notFound
	return tools
}

// leftovers lists temp files left in dir and its subfolders.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".tmp") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAssetSelection(t *testing.T) {
	cases := []struct{ goos, goarch, ytdlp, ffmpeg string }{
		{"windows", "amd64", "yt-dlp.exe", "ffmpeg-master-latest-win64-gpl.zip"},
		{"windows", "arm64", "yt-dlp_arm64.exe", "ffmpeg-master-latest-winarm64-gpl.zip"},
		{"linux", "amd64", "yt-dlp_linux", "ffmpeg-master-latest-linux64-gpl.tar.xz"},
		{"linux", "arm64", "yt-dlp_linux_aarch64", "ffmpeg-master-latest-linuxarm64-gpl.tar.xz"},
		{"darwin", "amd64", "yt-dlp_macos", ""},
		{"darwin", "arm64", "yt-dlp_macos", ""},
	}
	for _, c := range cases {
		got, err := ytdlpAssetName(c.goos, c.goarch)
		if err != nil || got != c.ytdlp {
			t.Errorf("yt-dlp %s/%s = %q, %v; want %q", c.goos, c.goarch, got, err, c.ytdlp)
		}
		if c.ffmpeg == "" {
			continue
		}
		got, err = ffmpegAssetName(c.goos, c.goarch)
		if err != nil || got != c.ffmpeg {
			t.Errorf("ffmpeg %s/%s = %q, %v; want %q", c.goos, c.goarch, got, err, c.ffmpeg)
		}
	}
	if _, err := ytdlpAssetName("plan9", "amd64"); err == nil {
		t.Error("unsupported platform should be an error")
	}
	if _, err := ffmpegAssetName("linux", "riscv64"); err == nil {
		t.Error("unsupported platform should be an error")
	}
}

func TestParseChecksums(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	sums, err := parseChecksums(strings.NewReader(digest + "  yt-dlp.exe\r\n" + strings.ToUpper(digest) + " *ffmpeg.zip\nnot a line\nshort  x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sums["yt-dlp.exe"] != digest || sums["ffmpeg.zip"] != digest {
		t.Fatalf("got %v", sums)
	}
	if _, ok := sums["x"]; ok {
		t.Fatal("a malformed digest must be ignored")
	}
}

func TestPathFFmpegDownloadsOnlyYtdlp(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp.exe"] = []byte("windows yt-dlp")
	f.ytdlp["yt-dlp_linux"] = []byte("linux yt-dlp")
	tools := f.tools(t, "windows", "amd64")
	tools.LookPath = onPath

	if _, _, ok := tools.Paths(); ok {
		t.Fatal("Paths must not be ok before setup")
	}
	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	ytdlp, ffmpegDir, ok := tools.Paths()
	if !ok || ffmpegDir != "" {
		t.Fatalf("Paths() = %q, %q, %v; want PATH ffmpeg", ytdlp, ffmpegDir, ok)
	}
	if got := readFile(t, ytdlp); got != "windows yt-dlp" {
		t.Fatalf("installed %q", got)
	}
	if filepath.Base(ytdlp) != "yt-dlp.exe" {
		t.Fatalf("binary name %q", filepath.Base(ytdlp))
	}
	if f.count("/repos/yt-dlp/FFmpeg-Builds/releases/latest") != 0 {
		t.Fatal("ffmpeg must not be downloaded when PATH has it")
	}
	if status := tools.Status(); status.State != ToolsReady || status.Percent != 100 {
		t.Fatalf("status %+v", status)
	}
	if v, err := tools.Version(context.Background()); err != nil || v != "2026.10.26" {
		t.Fatalf("Version() = %q, %v; want the release tag without running the fake binary", v, err)
	}
}

func TestForcedFFmpegDownloadFromZip(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp.exe"] = []byte("yt-dlp")
	f.ffmpeg["ffmpeg-master-latest-win64-gpl.zip"] = makeZip(t, []archiveEntry{
		{name: "ffmpeg-master-latest-win64-gpl/LICENSE.txt", body: "GPL"},
		{name: "ffmpeg-master-latest-win64-gpl/bin/ffplay.exe", body: "ffplay"},
		{name: "ffmpeg-master-latest-win64-gpl/bin/ffmpeg.exe", body: "ffmpeg binary"},
		{name: "ffmpeg-master-latest-win64-gpl/bin/ffprobe.exe", body: "ffprobe binary"},
		{name: "../../escape.exe", body: "zip-slip"},
		{name: "ffmpeg-master-latest-win64-gpl/doc/ffmpeg.html", body: "docs"},
	})
	tools := f.tools(t, "windows", "amd64")
	tools.LookPath = onPath
	tools.IgnorePathFFmpeg = true

	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, ffmpegDir, ok := tools.Paths()
	if !ok || ffmpegDir != filepath.Join(tools.Dir, "ffmpeg") {
		t.Fatalf("ffmpegDir = %q, ok %v", ffmpegDir, ok)
	}
	entries, _ := os.ReadDir(ffmpegDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "ffmpeg.exe,ffprobe.exe" {
		t.Fatalf("extracted %v, want only ffmpeg.exe and ffprobe.exe", names)
	}
	if readFile(t, filepath.Join(ffmpegDir, "ffmpeg.exe")) != "ffmpeg binary" {
		t.Fatal("wrong ffmpeg content")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(tools.Dir), "escape.exe")); err == nil {
		t.Fatal("zip-slip entry was written outside the tools folder")
	}
	if left := leftovers(t, tools.Dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	topEntries, _ := os.ReadDir(tools.Dir)
	if len(topEntries) != 2 {
		t.Fatalf("tools dir should hold yt-dlp.exe and ffmpeg/ only, got %d entries", len(topEntries))
	}
}

func TestFFmpegFromTarXzSkipsLinks(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp_linux_aarch64"] = []byte("arm yt-dlp")
	f.ffmpeg["ffmpeg-master-latest-linuxarm64-gpl.tar.xz"] = makeTarXz(t, []archiveEntry{
		{name: "ffmpeg-master-latest-linuxarm64-gpl/bin/ffprobe", body: "/etc/passwd", symlink: true},
		{name: "ffmpeg-master-latest-linuxarm64-gpl/bin/ffmpeg", body: "ffmpeg elf"},
		{name: "ffmpeg-master-latest-linuxarm64-gpl/bin/ffprobe", body: "ffprobe elf"},
		{name: "ffmpeg-master-latest-linuxarm64-gpl/man/man1/ffmpeg.1", body: "man"},
	})
	tools := f.tools(t, "linux", "arm64")

	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	ytdlp, ffmpegDir, ok := tools.Paths()
	if !ok || filepath.Base(ytdlp) != "yt-dlp" {
		t.Fatalf("Paths() = %q, %q, %v", ytdlp, ffmpegDir, ok)
	}
	if readFile(t, filepath.Join(ffmpegDir, "ffprobe")) != "ffprobe elf" {
		t.Fatal("the symlink entry must be skipped in favour of the regular file")
	}
	if readFile(t, filepath.Join(ffmpegDir, "ffmpeg")) != "ffmpeg elf" {
		t.Fatal("wrong ffmpeg content")
	}
}

func TestArchiveMissingBinaryIsAnError(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")
	if err := os.WriteFile(archive, makeZip(t, []archiveEntry{{name: "bin/ffmpeg.exe", body: "x"}}), 0o600); err != nil {
		t.Fatal(err)
	}
	err := extractTools(archive, "zip", dir, []string{"ffmpeg.exe", "ffprobe.exe"})
	if err == nil || !strings.Contains(err.Error(), "ffprobe.exe") {
		t.Fatalf("want a missing ffprobe error, got %v", err)
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp.exe"] = []byte("tampered")
	f.badSums["yt-dlp.exe"] = true
	tools := f.tools(t, "windows", "amd64")
	tools.LookPath = onPath

	err := tools.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(tools.YtdlpPath()); err == nil {
		t.Fatal("a binary with the wrong checksum must not be installed")
	}
	if left := leftovers(t, tools.Dir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	status := tools.Status()
	if status.State != ToolsError || !strings.Contains(status.Error, "checksum mismatch") {
		t.Fatalf("status %+v", status)
	}
	if _, _, ok := tools.Paths(); ok {
		t.Fatal("Paths must not be ok after a failed setup")
	}

	// A retry after the release is fixed succeeds
	f.mu.Lock()
	f.ytdlp["yt-dlp.exe"] = []byte("good")
	delete(f.badSums, "yt-dlp.exe")
	f.mu.Unlock()
	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMissingChecksumEntryRejected(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp_linux"] = []byte("only linux")
	tools := f.tools(t, "windows", "amd64")
	tools.LookPath = onPath
	if err := tools.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "yt-dlp.exe") {
		t.Fatalf("want an error naming the missing asset, got %v", err)
	}
}

func TestStartRunsInBackgroundAndSkipsWhenPresent(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp_linux"] = []byte("linux yt-dlp")
	tools := f.tools(t, "linux", "amd64")
	tools.LookPath = onPath

	tools.Start()
	if !waitFor(10*time.Second, func() bool { return tools.Status().State == ToolsReady }) {
		t.Fatalf("setup never finished: %+v", tools.Status())
	}
	calls := f.count("/repos/yt-dlp/yt-dlp/releases/latest")

	// A second Tools over the same folder finds everything present and stays offline
	again := f.tools(t, "linux", "amd64")
	again.Dir = tools.Dir
	again.LookPath = onPath
	again.Start()
	if _, _, ok := again.Paths(); !ok {
		t.Fatal("present tools should be ready at once")
	}
	if f.count("/repos/yt-dlp/yt-dlp/releases/latest") != calls {
		t.Fatal("present tools must not hit the network")
	}
}

func TestMacFFmpegViaRedirect(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp_macos"] = []byte("mac yt-dlp")
	f.ffmpeg["ffmpeg.zip"] = makeZip(t, []archiveEntry{{name: "ffmpeg", body: "mac ffmpeg"}})
	f.ffmpeg["ffprobe.zip"] = makeZip(t, []archiveEntry{{name: "ffprobe", body: "mac ffprobe"}})
	tools := f.tools(t, "darwin", "arm64")

	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, ffmpegDir, _ := tools.Paths()
	if readFile(t, filepath.Join(ffmpegDir, "ffprobe")) != "mac ffprobe" {
		t.Fatal("wrong ffprobe content")
	}

	// A bad checksum on the macOS build is rejected too
	f2 := newFakeGitHub(t)
	f2.ytdlp["yt-dlp_macos"] = []byte("mac yt-dlp")
	f2.ffmpeg["ffmpeg.zip"] = f.ffmpeg["ffmpeg.zip"]
	f2.ffmpeg["ffprobe.zip"] = f.ffmpeg["ffprobe.zip"]
	f2.badSums["ffmpeg.zip"] = true
	bad := f2.tools(t, "darwin", "arm64")
	if err := bad.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum mismatch, got %v", err)
	}
}

func TestUpdateRedownloadsOnlyNewTag(t *testing.T) {
	f := newFakeGitHub(t)
	f.ytdlp["yt-dlp.exe"] = []byte("old")
	tools := f.tools(t, "windows", "amd64")
	tools.LookPath = onPath
	if err := tools.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	version, err := tools.Update(context.Background())
	if err != nil || version != "2026.10.26" {
		t.Fatalf("Update() = %q, %v", version, err)
	}
	if f.count("/yt/yt-dlp.exe") != 1 {
		t.Fatal("same tag must not download again")
	}

	f.mu.Lock()
	f.ytdlpTag = "2026.11.15"
	f.ytdlp["yt-dlp.exe"] = []byte("new")
	f.mu.Unlock()
	version, err = tools.Update(context.Background())
	if err != nil || version != "2026.11.15" {
		t.Fatalf("Update() = %q, %v", version, err)
	}
	if readFile(t, tools.YtdlpPath()) != "new" {
		t.Fatal("yt-dlp was not replaced")
	}
	if v, _ := tools.Version(context.Background()); v != "2026.11.15" {
		t.Fatalf("Version() = %q after update", v)
	}
}

func TestUpdateBeforeSetupIsAnError(t *testing.T) {
	f := newFakeGitHub(t)
	tools := f.tools(t, "windows", "amd64")
	if _, err := tools.Update(context.Background()); err == nil {
		t.Fatal("Update must refuse while yt-dlp is missing")
	}
}

func TestStatusJSONShape(t *testing.T) {
	data, err := json.Marshal(ToolsStatus{State: ToolsInstalling, Step: "Downloading ffmpeg", Percent: 42.5})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"installing","step":"Downloading ffmpeg","percent":42.5,"error":""}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

func TestStartSweepsLeftovers(t *testing.T) {
	f := newFakeGitHub(t)
	tools := f.tools(t, "linux", "amd64")
	tools.LookPath = onPath
	ffmpegDir := filepath.Join(tools.Dir, "ffmpeg")
	if err := os.MkdirAll(ffmpegDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []string{
		filepath.Join(tools.Dir, ".download-123.tmp"),
		filepath.Join(tools.Dir, "yt-dlp.old"),
		filepath.Join(ffmpegDir, ".extract-456.tmp"),
		filepath.Join(ffmpegDir, "ffmpeg.old"),
	}
	kept := []string{tools.YtdlpPath(), filepath.Join(tools.Dir, "notes.tmp"), filepath.Join(ffmpegDir, "ffmpeg")}
	for _, path := range append(append([]string{}, stale...), kept...) {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tools.Start()
	if _, _, ok := tools.Paths(); !ok {
		t.Fatal("present tools should be ready at once")
	}
	for _, path := range stale {
		if fileExists(path) {
			t.Errorf("%s was not swept", path)
		}
	}
	for _, path := range kept {
		if !fileExists(path) {
			t.Errorf("%s was removed", path)
		}
	}
}

// copyTestBinary copies this test binary to path, so it can stand in for yt-dlp.
func copyTestBinary(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestVersionRunsWithPrivateTemp(t *testing.T) {
	tools := NewTools(filepath.Join(t.TempDir(), "tools"))
	copyTestBinary(t, tools.YtdlpPath())
	temp := t.TempDir()
	core.SetChildTempDir(temp)
	defer core.SetChildTempDir("")
	t.Setenv(helperEnv, "printtemp")
	version, err := tools.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != temp {
		t.Fatalf("yt-dlp saw TEMP %q, want %q", version, temp)
	}
}

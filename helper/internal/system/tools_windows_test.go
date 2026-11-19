package system

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceFileWhileRunning replaces an executable that is running, as an update does while
// yt-dlp serves an /info call: Windows refuses to overwrite it but allows moving it aside.
func TestReplaceFileWhileRunning(t *testing.T) {
	tools := NewTools(filepath.Join(t.TempDir(), "tools"))
	target := tools.YtdlpPath()
	copyTestBinary(t, target)
	running := helperCommand(t, "sleep")
	running.Path = target
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = running.Process.Kill()
		_ = running.Wait()
	}()

	tmp := filepath.Join(tools.Dir, ".download-1.tmp")
	if err := os.WriteFile(tmp, []byte("new yt-dlp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(tmp, target); err != nil {
		t.Fatalf("replaceFile over a running executable: %v", err)
	}
	if got := readFile(t, target); got != "new yt-dlp" {
		t.Fatalf("target holds %d bytes, not the new file", len(got))
	}
	if fileExists(tmp) {
		t.Fatal("temp file left behind")
	}
	// The running copy cannot be deleted yet; the next start sweeps it
	old := target + ".old"
	if !fileExists(old) {
		t.Fatal("the running executable was not moved aside")
	}
	_ = running.Process.Kill()
	_ = running.Wait()
	tools.sweepLeftovers()
	if fileExists(old) {
		t.Fatal(".old not swept once the process exited")
	}
}

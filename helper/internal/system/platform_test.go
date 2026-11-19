package system

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseUserDirs(t *testing.T) {
	home := filepath.FromSlash("/home/kiri")
	content := "# written by xdg-user-dirs-update\nXDG_DESKTOP_DIR=\"$HOME/Desktop\"\nXDG_DOWNLOAD_DIR=\"$HOME/Tikiake Mai\"\n"
	if got, want := parseUserDirs(content, home), filepath.Join(home, "Tikiake Mai"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := parseUserDirs(`XDG_DOWNLOAD_DIR="/mnt/data/dl"`, home); got != filepath.FromSlash("/mnt/data/dl") {
		t.Fatalf("absolute form: got %q", got)
	}
	if got := parseUserDirs(`XDG_DOWNLOAD_DIR="$HOME"`, home); got != home {
		t.Fatalf("bare $HOME: got %q", got)
	}
	for _, bad := range []string{"", "XDG_DESKTOP_DIR=\"$HOME/Desktop\"", `XDG_DOWNLOAD_DIR="relative/dir"`} {
		if got := parseUserDirs(bad, home); got != "" {
			t.Fatalf("%q: got %q, want empty", bad, got)
		}
	}
}

func TestDownloadsDirIsAbsolute(t *testing.T) {
	if dir := DownloadsDir(); !filepath.IsAbs(dir) {
		t.Fatalf("DownloadsDir() = %q, want an absolute path", dir)
	}
}

func TestDataDirIsInsideInstallDir(t *testing.T) {
	if got, want := DataDir(), filepath.Join(InstallDir(), "data"); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(InstallDir()) {
		t.Fatalf("InstallDir() = %q, want an absolute path", InstallDir())
	}
}

func TestFfmpegOnPathNeedsBoth(t *testing.T) {
	only := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, n := range names {
				if n == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	if !ffmpegOnPath(only("ffmpeg", "ffprobe")) {
		t.Fatal("both present should count as available")
	}
	if ffmpegOnPath(only("ffmpeg")) {
		t.Fatal("ffmpeg without ffprobe should count as missing")
	}
}

// TestKillTreeKillsGrandchildren starts a real parent > child pair: both must die, as
// ffmpeg must die with yt-dlp.
func TestKillTreeKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := helperCommand(t, "parent", "VIDGRAB_TEST_PIDFILE="+pidFile)
	PrepareChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	AfterStart(cmd)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	var childPid int
	if !waitFor(15*time.Second, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil || len(data) == 0 {
			return false
		}
		childPid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil
	}) {
		t.Fatal("parent never reported its child")
	}
	if !processAlive(childPid) {
		t.Fatal("child should be running before the kill")
	}

	if err := KillTree(cmd.Process.Pid); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("parent still running after KillTree")
	}
	if !waitFor(5*time.Second, func() bool { return !processAlive(childPid) }) {
		t.Fatal("grandchild survived KillTree")
	}
}

func TestSpawnDetachedAppendsToLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "logs", "server.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("earlier line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// SpawnDetached takes argv only, so the helper mode reaches the child via the environment
	t.Setenv(helperEnv, "echo")
	if err := SpawnDetached([]string{os.Args[0]}, logPath); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if !waitFor(10*time.Second, func() bool {
		data, _ = os.ReadFile(logPath)
		return strings.Contains(string(data), "helper says hello")
	}) {
		t.Fatalf("log never received the child's output: %q", data)
	}
	if !strings.HasPrefix(string(data), "earlier line\n") {
		t.Fatalf("log was truncated instead of appended: %q", data)
	}
}

func TestSpawnDetachedRejectsEmptyArgv(t *testing.T) {
	if err := SpawnDetached(nil, filepath.Join(t.TempDir(), "x.log")); err == nil {
		t.Fatal("want an error for an empty argv")
	}
}

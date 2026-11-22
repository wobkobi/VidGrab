// Package system holds the OS-facing parts of the helper: per-user paths, child process
// control, the yt-dlp/ffmpeg downloader, the yt-dlp updater, the Firefox native messaging
// host and the install/uninstall steps.
package system

import (
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// HostName is the native messaging host name registered with Firefox.
	HostName = "vidgrab"
	// ExtensionID is the add-on allowed to talk to the native host.
	ExtensionID = "yt-dl-firefox@local"
	// AppName is the folder name of the install directory.
	AppName = "VidGrab"
)

// DataDir returns the runtime data directory (token, settings, update state, server.log,
// tools). It does not create the directory; callers that write there create it first.
func DataDir() string {
	return filepath.Join(InstallDir(), "data")
}

// ExeName returns name with ".exe" appended.
func ExeName(name string) string {
	return name + ".exe"
}

// FfmpegOnPath reports whether both ffmpeg and ffprobe resolve on PATH; yt-dlp needs the
// pair, so one without the other counts as missing.
func FfmpegOnPath() bool {
	return ffmpegOnPath(exec.LookPath)
}

func ffmpegOnPath(lookPath func(string) (string, error)) bool {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := lookPath(name); err != nil {
			return false
		}
	}
	return true
}

// homeDir returns the user's home directory, falling back to $HOME when the OS lookup fails.
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return os.Getenv("HOME")
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// openLogFile opens path for appending, creating its directory first.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

package system

import (
	"os"
	"path/filepath"
)

// InstallDir returns the per-user install directory, $XDG_DATA_HOME/vidgrab
// (~/.local/share/vidgrab by default).
func InstallDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		base = userHomeSubdir(".local", "share")
	}
	return filepath.Join(base, "vidgrab")
}

// DownloadsDir returns the XDG download directory from user-dirs.dirs when it exists,
// else ~/Downloads.
func DownloadsDir() string {
	home := homeDir()
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	if data, err := os.ReadFile(filepath.Join(config, "user-dirs.dirs")); err == nil {
		if dir := parseUserDirs(string(data), home); dir != "" && isDir(dir) {
			return dir
		}
	}
	return filepath.Join(home, "Downloads")
}

// OpenFile opens path with its default application.
func OpenFile(path string) error {
	return startQuiet("xdg-open", path)
}

// Reveal opens the folder holding path; xdg-open has no portable way to select a file.
func Reveal(path string) error {
	return startQuiet("xdg-open", filepath.Dir(path))
}

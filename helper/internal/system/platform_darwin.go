package system

import "path/filepath"

// InstallDir returns the per-user install directory, ~/Library/Application Support/VidGrab.
func InstallDir() string {
	return userHomeSubdir("Library", "Application Support", AppName)
}

// DownloadsDir returns ~/Downloads.
func DownloadsDir() string {
	return userHomeSubdir("Downloads")
}

// OpenFile opens path with its default application.
func OpenFile(path string) error {
	return startQuiet("open", path)
}

// Reveal shows path selected in Finder.
func Reveal(path string) error {
	return startQuiet("open", "-R", filepath.Clean(path))
}

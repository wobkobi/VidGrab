// Per-user install and uninstall: copy the helper into place and register it with Firefox
// as a native messaging host.

package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"vidgrab/internal/auth"
)

const (
	stopWait        = 10 * time.Second
	replaceRetry    = 300 * time.Millisecond
	replaceTries    = 15
	hostDescription = "VidGrab download helper"
)

// Report describes what Install or Uninstall did. Lines is the human-readable summary.
type Report struct {
	InstalledPath  string
	ManifestPath   string
	RegistryKey    string
	ServerStopped  bool
	PendingRemoval bool
	Lines          []string
}

func (r *Report) addf(format string, args ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, args...))
}

// String returns the summary lines joined by newlines.
func (r Report) String() string {
	return strings.Join(r.Lines, "\n")
}

// Installer holds the locations Install and Uninstall touch, so tests can point them at
// temp folders and a scratch registry key.
type Installer struct {
	// InstallDir receives the helper binary and its data folder.
	InstallDir string
	// ManifestPath is where the native host manifest is written.
	ManifestPath string
	// RegistryKey is the HKCU-relative key pointing Firefox at the manifest (Windows only;
	// empty elsewhere).
	RegistryKey string
	// StopServer stops a running helper server and reports whether one was running.
	StopServer func() (bool, error)
	// ScheduleRemoval deletes dir after this process exits (Windows, where a running
	// executable cannot delete itself).
	ScheduleRemoval func(dir string) error
}

// DefaultInstaller returns an Installer for the current user's real locations.
func DefaultInstaller() *Installer {
	dir := InstallDir()
	return &Installer{
		InstallDir:   dir,
		ManifestPath: defaultManifestPath(dir),
		RegistryKey:  defaultRegistryKey(),
		StopServer: func() (bool, error) {
			return stopServer(ServerPort, readExistingToken(filepath.Join(dir, "data", auth.TokenFile)))
		},
		ScheduleRemoval: scheduleDirRemoval,
	}
}

// Install copies the helper at selfPath into the install directory and registers it with
// Firefox, using the current user's real locations.
func Install(selfPath string) (Report, error) {
	return DefaultInstaller().Install(selfPath)
}

// Uninstall stops the server and removes the registration and install directory, using the
// current user's real locations.
func Uninstall() (Report, error) {
	self, err := os.Executable()
	if err != nil {
		self = ""
	}
	return DefaultInstaller().Uninstall(self)
}

// InstalledBinary returns the path of the installed helper binary.
func (in *Installer) InstalledBinary() string {
	return filepath.Join(in.InstallDir, ExeName(HostName))
}

// HostManifest returns the native host manifest for the binary at path.
func HostManifest(path string) map[string]any {
	return map[string]any{
		"name":               HostName,
		"description":        hostDescription,
		"path":               path,
		"type":               "stdio",
		"allowed_extensions": []string{ExtensionID},
	}
}

// Install copies selfPath to the install directory (skipped when it already runs from
// there), writes the native host manifest and, on Windows, the registry key.
//
// A running server is stopped first, since Windows cannot replace a running executable.
func (in *Installer) Install(selfPath string) (Report, error) {
	report := Report{InstalledPath: in.InstalledBinary(), ManifestPath: in.ManifestPath, RegistryKey: in.RegistryKey}
	in.stop(&report)

	if err := os.MkdirAll(in.InstallDir, 0o755); err != nil {
		return report, err
	}
	target := in.InstalledBinary()
	if sameFile(selfPath, target) {
		report.addf("Already running from %s", target)
	} else {
		if err := copyExecutable(selfPath, target); err != nil {
			return report, fmt.Errorf("could not copy the helper to %s: %w", target, err)
		}
		report.addf("Installed %s", target)
	}

	data, err := json.MarshalIndent(HostManifest(target), "", "  ")
	if err != nil {
		return report, err
	}
	if err := os.MkdirAll(filepath.Dir(in.ManifestPath), 0o755); err != nil {
		return report, err
	}
	if err := os.WriteFile(in.ManifestPath, append(data, '\n'), 0o644); err != nil {
		return report, err
	}
	report.addf("Manifest: %s", in.ManifestPath)

	if in.RegistryKey != "" {
		if err := registerHost(in.RegistryKey, in.ManifestPath); err != nil {
			return report, fmt.Errorf("could not write the registry key: %w", err)
		}
		report.addf(`Registered HKCU\%s`, in.RegistryKey)
	}
	report.addf("Restart Firefox (or reload the add-on) so it picks up the helper.")
	return report, nil
}

// Uninstall stops the server, removes the registry key, the manifest and the install
// directory. selfPath is the running executable: when it lives inside the install
// directory on Windows, the folder is deleted by a short-lived cmd.exe after this process
// exits, and Report.PendingRemoval is set.
func (in *Installer) Uninstall(selfPath string) (Report, error) {
	report := Report{InstalledPath: in.InstalledBinary(), ManifestPath: in.ManifestPath, RegistryKey: in.RegistryKey}
	in.stop(&report)

	var errs []error
	if in.RegistryKey != "" {
		existed, err := unregisterHost(in.RegistryKey)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("could not remove the registry key: %w", err))
		case existed:
			report.addf(`Removed HKCU\%s`, in.RegistryKey)
		default:
			report.addf("Registry entry was not present")
		}
	}
	if err := os.Remove(in.ManifestPath); err == nil {
		report.addf("Removed %s", in.ManifestPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}

	if !isDir(in.InstallDir) {
		report.addf("Install folder was not present")
		return report, errors.Join(errs...)
	}
	if runtime.GOOS == "windows" && selfPath != "" && pathWithin(selfPath, in.InstallDir) {
		if err := removeAllExcept(in.InstallDir, selfPath); err != nil {
			errs = append(errs, err)
		}
		if err := in.ScheduleRemoval(in.InstallDir); err != nil {
			errs = append(errs, fmt.Errorf("could not schedule removal of %s: %w", in.InstallDir, err))
		} else {
			report.PendingRemoval = true
			report.addf("%s will be removed a few seconds after this window closes", in.InstallDir)
		}
	} else if err := os.RemoveAll(in.InstallDir); err != nil {
		errs = append(errs, err)
	} else {
		report.addf("Removed %s", in.InstallDir)
	}
	return report, errors.Join(errs...)
}

// stop runs StopServer and notes the outcome; a failure does not abort the caller, whose
// own file operations report anything that is still locked.
func (in *Installer) stop(report *Report) {
	if in.StopServer == nil {
		return
	}
	stopped, err := in.StopServer()
	switch {
	case err != nil:
		report.addf("Could not stop the running helper: %v", err)
	case stopped:
		report.ServerStopped = true
		report.addf("Stopped the running helper")
	}
}

// readExistingToken returns the install token without creating one ("" when missing).
func readExistingToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// stopServer asks the helper on 127.0.0.1:port to shut down and waits for the port to free.
//
// It returns false when nothing listens. Something that answers without proving it knows
// the token is left alone and reported as an error.
func stopServer(port int, token string) (bool, error) {
	health, err := auth.Probe(port, token, probeTimeout)
	if err != nil {
		return false, err
	}
	if health == nil {
		return false, nil
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/shutdown", port), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set(auth.ClientHeader, "1")
	req.Header.Set(auth.TokenHeader, token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxSmallBytes))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("the helper refused to shut down (HTTP %d)", resp.StatusCode)
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err != nil {
			return true, nil
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return true, errors.New("the helper did not stop within 10 seconds")
}

// copyExecutable copies src to dst through a temp file and an atomic rename.
//
// A just-stopped server can hold dst for a moment, so the rename is retried. Windows also
// refuses to overwrite a running executable but does allow renaming it, so a stuck dst is
// moved aside to "<dst>.old" (removed best-effort, or on the next install).
func copyExecutable(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".install-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chmod(out.Name(), 0o755); err != nil {
		return err
	}
	old := dst + ".old"
	_ = os.Remove(old)
	for i := 0; i < replaceTries; i++ {
		if err = os.Rename(out.Name(), dst); err == nil {
			_ = os.Remove(old)
			return nil
		}
		if i == replaceTries/2 && fileExists(dst) {
			_ = os.Rename(dst, old)
		}
		time.Sleep(replaceRetry)
	}
	return err
}

// sameFile reports whether a and b are the same existing file.
func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}

// pathWithin reports whether p lies inside dir (case-insensitively on Windows).
func pathWithin(p, dir string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	if runtime.GOOS == "windows" {
		dir, p = strings.ToLower(dir), strings.ToLower(p)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeAllExcept deletes everything in dir except the file keep and the folders leading
// to it.
func removeAllExcept(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		if sameFile(full, keep) {
			continue
		}
		if entry.IsDir() && pathWithin(keep, full) {
			errs = append(errs, removeAllExcept(full, keep))
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

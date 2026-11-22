package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func readTestRegistry(t *testing.T) string {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, testRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	value, _, err := k.GetStringValue("")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testRegistryExists() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, testRegistryKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// readTestUninstallEntry returns the string values of the scratch Settings > Apps entry.
func readTestUninstallEntry(t *testing.T) map[string]string {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, testUninstallKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	values := map[string]string{}
	for _, name := range []string{"DisplayName", "DisplayVersion", "UninstallString", "InstallLocation"} {
		values[name], _, _ = k.GetStringValue(name)
	}
	return values
}

func testUninstallEntryExists() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, testUninstallKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// cleanupTestRegistry deletes the scratch key tree (deepest key first; DeleteKey cannot
// remove a key that still has subkeys).
func cleanupTestRegistry() {
	for _, key := range []string{
		testRegistryKey,
		`Software\VidGrabTest\NativeMessagingHosts`,
		testUninstallKey,
		`Software\VidGrabTest\Uninstall`,
		`Software\VidGrabTest`,
	} {
		_ = registry.DeleteKey(registry.CURRENT_USER, key)
	}
}

func TestScheduleDirRemovalDeletesAfterExit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Vid Grab & co")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scheduleDirRemoval(dir); err != nil {
		t.Fatal(err)
	}
	if !waitFor(15*time.Second, func() bool { return !isDir(dir) }) {
		t.Fatal("folder was not removed")
	}
}

func TestScheduleDirRemovalRejectsPercent(t *testing.T) {
	if err := scheduleDirRemoval(`C:\Users\%USERNAME%\x`); err == nil {
		t.Fatal("a path with % must be refused")
	}
	if err := scheduleDirRemoval(`C:\Users\a"b\x`); err == nil {
		t.Fatal(`a path with " must be refused`)
	}
	if _, err := dirRemovalCommand(`C:\x`, `C:\%WINDIR%\System32`); err == nil {
		t.Fatal("a system folder with % must be refused")
	}
}

func TestDirRemovalCommandUsesSystemPaths(t *testing.T) {
	sysDir := systemDirectory()
	if !filepath.IsAbs(sysDir) || !fileExists(filepath.Join(sysDir, "cmd.exe")) {
		t.Fatalf("system folder %q has no cmd.exe", sysDir)
	}
	cmd, err := dirRemovalCommand(`C:\Users\me\AppData\Local\VidGrab`, sysDir)
	if err != nil {
		t.Fatal(err)
	}
	cmdExe := filepath.Join(sysDir, "cmd.exe")
	if cmd.Path != cmdExe || cmd.Dir != sysDir {
		t.Fatalf("Path %q, Dir %q", cmd.Path, cmd.Dir)
	}
	line := cmd.SysProcAttr.CmdLine
	for _, want := range []string{
		`"` + cmdExe + `" /d /v:off /c `,
		`@("` + filepath.Join(sysDir, "ping.exe") + `" -n 2 127.0.0.1 >nul`,
		`rmdir /s /q "C:\Users\me\AppData\Local\VidGrab"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("command line %q lacks %q", line, want)
		}
	}
}

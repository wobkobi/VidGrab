package system

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// defaultManifestPath keeps the manifest in the install directory; the registry key tells
// Firefox where it is.
func defaultManifestPath(installDir string) string {
	return filepath.Join(installDir, HostName+".json")
}

func defaultRegistryKey() string {
	return `Software\Mozilla\NativeMessagingHosts\` + HostName
}

// registerHost sets the default value of HKCU\key to the manifest path.
func registerHost(key, manifestPath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("", manifestPath)
}

func defaultUninstallKey() string {
	return `Software\Microsoft\Windows\CurrentVersion\Uninstall\` + AppName
}

// AppEntry is what Settings > Apps (and the older Programs and Features) shows for the
// helper. Exe is the installed binary, which also supplies the icon and the uninstall
// command.
type AppEntry struct {
	Exe       string
	Dir       string
	Version   string
	SizeBytes int64
}

// registerApp writes the per-user uninstall entry under HKCU\key, which needs no admin rights
// and lists the helper in Settings > Apps with a working Uninstall button. NoModify and
// NoRepair hide the buttons the helper has nothing behind.
func registerApp(key string, entry AppEntry) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	command := `"` + entry.Exe + `" uninstall`
	strs := [][2]string{
		{"DisplayName", appDisplayName},
		{"DisplayVersion", entry.Version},
		{"Publisher", appPublisher},
		{"DisplayIcon", entry.Exe + ",0"},
		{"InstallLocation", entry.Dir},
		{"UninstallString", command},
		{"QuietUninstallString", command + " --quiet"},
		{"URLInfoAbout", appURL},
		{"HelpLink", appURL},
	}
	for _, kv := range strs {
		if err := k.SetStringValue(kv[0], kv[1]); err != nil {
			return err
		}
	}
	// EstimatedSize is in KiB
	dwords := map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": uint32(entry.SizeBytes / 1024)}
	for name, value := range dwords {
		if err := k.SetDWordValue(name, value); err != nil {
			return err
		}
	}
	return nil
}

// unregisterHost deletes HKCU\key and reports whether it existed. The key has no subkeys.
// The uninstall entry is removed the same way.
func unregisterHost(key string) (bool, error) {
	err := registry.DeleteKey(registry.CURRENT_USER, key)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// scheduleDirRemoval deletes dir from a detached cmd.exe once this process has exited.
//
// The loop retries rmdir about once a second (ping is the stock batch sleep) for a minute,
// stopping as soon as the folder is gone, so it copes with the uninstaller waiting for
// Enter before it exits.
//
// cmd.exe parses its own command line, so the raw line is set rather than letting Go quote
// each argument. Windows paths cannot contain double quotes, so dir cannot escape them.
func scheduleDirRemoval(dir string) error {
	cmd, err := dirRemovalCommand(dir, systemDirectory())
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// dirRemovalCommand builds the detached cmd.exe for scheduleDirRemoval.
//
// cmd.exe and ping.exe are named by absolute path inside sysDir and the working folder is
// sysDir, so a planted cmd.exe or ping.exe in the current or install folder (or early on
// PATH) is never run. "/d" skips AutoRun commands from the registry and "/v:off" keeps "!"
// in the path literal. "%" is refused outright: cmd.exe would expand it even inside quotes.
func dirRemovalCommand(dir, sysDir string) (*exec.Cmd, error) {
	if strings.ContainsAny(dir, `"%`) {
		return nil, errors.New("unexpected characters in the install path")
	}
	if strings.ContainsAny(sysDir, `"%`) {
		return nil, errors.New("unexpected characters in the system folder path")
	}
	cmdExe := filepath.Join(sysDir, "cmd.exe")
	pingExe := filepath.Join(sysDir, "ping.exe")
	cmd := exec.Command(cmdExe)
	cmd.Dir = sysDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `"` + cmdExe + `" /d /v:off /c for /l %i in (1,1,60) do @("` + pingExe +
			`" -n 2 127.0.0.1 >nul & rmdir /s /q "` + dir + `" 2>nul & if not exist "` + dir + `" exit)`,
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
	}
	return cmd, nil
}

// systemDirectory returns the System32 folder, from the API first, then %SystemRoot%, then
// the usual C:\Windows.
func systemDirectory() string {
	if dir, err := windows.GetSystemDirectory(); err == nil && dir != "" {
		return dir
	}
	root := os.Getenv("SystemRoot")
	if root == "" || !filepath.IsAbs(root) {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32")
}

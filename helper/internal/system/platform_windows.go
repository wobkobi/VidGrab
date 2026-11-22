package system

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process creation flags.
const (
	belowNormalPriorityClass = 0x00004000
	createNewProcessGroup    = 0x00000200
	createNoWindow           = 0x08000000
	detachedProcess          = 0x00000008
	createBreakawayFromJob   = 0x01000000
)

// InstallDir returns the per-user install directory, %LOCALAPPDATA%\VidGrab.
func InstallDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(homeDir(), "AppData", "Local")
	}
	return filepath.Join(base, AppName)
}

// DownloadsDir returns the user's Downloads folder.
//
// The Known Folder API is asked first so a Downloads folder moved to another drive is
// respected; ~/Downloads is the fallback.
func DownloadsDir() string {
	if path, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0); err == nil && isDir(path) {
		return path
	}
	return filepath.Join(homeDir(), "Downloads")
}

// OpenFile opens path with its default application (ShellExecute "open").
func OpenFile(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// Reveal shows path selected in Explorer.
//
// Explorer parses its own command line and needs the path quoted after "/select,", which
// Go's argument quoting would wrap around the whole argument instead, so the raw command
// line is set. Windows paths cannot contain double quotes, so the path cannot escape it.
// Explorer exits with code 1 even on success, so only the start error is reported.
func Reveal(path string) error {
	cmd := exec.Command("explorer")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer /select,"` + path + `"`}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// PrepareChild makes cmd start hidden (CREATE_NO_WINDOW), at below-normal priority and in
// its own process group, so a console window never flashes up when the helper runs without
// one and ffmpeg spawned by yt-dlp inherits the lowered priority. Call before cmd.Start.
func PrepareChild(cmd *exec.Cmd) {
	attr := sysProcAttr(cmd)
	attr.HideWindow = true
	attr.CreationFlags |= belowNormalPriorityClass | createNewProcessGroup | createNoWindow
}

// HideChild makes a short helper command start without a console window, at normal
// priority. Call before cmd.Start.
func HideChild(cmd *exec.Cmd) {
	attr := sysProcAttr(cmd)
	attr.HideWindow = true
	attr.CreationFlags |= createNoWindow
}

// AfterStart finishes PrepareChild once the process runs; Windows sets everything at
// creation, so it does nothing here.
func AfterStart(*exec.Cmd) {}

// HoldProcess opens an extra handle to pid and returns a func that closes it.
//
// Windows only reuses a process id once every handle to the process is closed. Go releases
// its own handle as soon as Wait sees the exit, which can happen just before a ctx-driven
// cancel runs taskkill by pid; holding this handle until Wait returns keeps the pid from
// pointing at an unrelated process in that window. Failure to open returns a no-op.
func HoldProcess(pid int) (release func()) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return func() {}
	}
	return func() { _ = windows.CloseHandle(h) }
}

func sysProcAttr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// KillGrace is how long a caller of TerminateTree waits before ForceKillTree. Windows has
// no gentle first step, so there is nothing to wait for.
const KillGrace = 0

// TerminateTree is KillTree, which on Windows already forces the kill in one step.
func TerminateTree(pid int) error { return KillTree(pid) }

// ForceKillTree does nothing on Windows: TerminateTree already forced the kill.
func ForceKillTree(int) error { return nil }

// KillTree force-kills pid and every process it started (taskkill /T), so ffmpeg dies with
// yt-dlp. When taskkill fails the direct process is still killed as a last resort.
func KillTree(pid int) error {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	HideChild(cmd)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if proc, findErr := os.FindProcess(pid); findErr == nil {
		if killErr := proc.Kill(); killErr == nil {
			return nil
		}
	}
	return err
}

// SpawnDetached starts argv as a background process that outlives the caller, with
// stdout and stderr appended to logPath.
//
// Firefox runs native hosts inside a job object that is killed when the host exits, so the
// child must break away from it. If the start fails (the job forbids breakaway with access
// denied), it is retried without that flag; the server then lives only as long as the job
// allows.
func SpawnDetached(argv []string, logPath string) error {
	if len(argv) == 0 {
		return errors.New("no command to start")
	}
	log, err := openLogFile(logPath)
	if err != nil {
		return err
	}
	defer log.Close()
	base := uint32(detachedProcess | createNewProcessGroup | createNoWindow)
	err = startDetached(argv, log, filepath.Dir(logPath), base|createBreakawayFromJob)
	if err != nil {
		err = startDetached(argv, log, filepath.Dir(logPath), base)
	}
	return err
}

func startDetached(argv []string, log *os.File, dir string, flags uint32) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// StartServer starts the installed helper at exe as a background server with the usual idle
// timeout, on the port saved in its data folder. Install uses it so yt-dlp and ffmpeg
// download while the user is still setting up the add-on.
func StartServer(exe string) error {
	data := filepath.Join(filepath.Dir(exe), "data")
	argv := []string{exe, "serve", "--idle-timeout", strconv.Itoa(ServerIdleTimeout)}
	return SpawnDetached(argv, filepath.Join(data, ServerLogFile))
}

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// OwnsConsole reports whether this process is alone in its console window, which means
// Windows opened the window for it: a double-click in Explorer or the Uninstall button in
// Settings. Started from a terminal, the shell shares the console and this is false. The
// window then closes as soon as the process exits, so callers pause before exiting.
func OwnsConsole() bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// asfwAny is ASFW_ANY: every process may set the foreground window.
const asfwAny = 0xFFFFFFFF

var procAllowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

// AllowForeground lets the next window any process opens come to the front.
//
// Windows only gives focus to a new window when the process behind it may set the foreground,
// which the long-running server (started detached, long ago) may not: a file it opens appears
// behind Firefox. The native host is started by Firefox while Firefox is in front, so it holds
// that right and can pass it on with AllowSetForegroundWindow. The grant lasts until the next
// user input or focus change.
func AllowForeground() error {
	if ok, _, err := procAllowSetForegroundWindow.Call(asfwAny); ok == 0 {
		return err
	}
	return nil
}

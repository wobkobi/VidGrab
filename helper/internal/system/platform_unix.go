//go:build !windows

package system

import (
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// childNice is the niceness applied to download processes (the same as `nice -n 10`).
const childNice = 10

// KillGrace is how long KillTree waits after TerminateTree before ForceKillTree.
const KillGrace = 2500 * time.Millisecond

// PrepareChild puts cmd in its own process group, so KillTree can take down yt-dlp together
// with the ffmpeg it spawns. Call before cmd.Start, then AfterStart once it runs.
func PrepareChild(cmd *exec.Cmd) {
	sysProcAttr(cmd).Setpgid = true
}

// HideChild is a no-op outside Windows, where commands never open a console window.
func HideChild(*exec.Cmd) {}

// AfterStart lowers the priority of the process group PrepareChild created.
//
// Go cannot run code in the child between fork and exec, so the priority is set right after
// the start. Targeting the whole group (PRIO_PGRP) also covers any child the process forked
// in that short window; later children inherit the niceness. Failure is ignored, as the
// download still works at normal priority.
func AfterStart(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = unix.Setpriority(unix.PRIO_PGRP, cmd.Process.Pid, childNice)
}

// HoldProcess keeps pid from being reused until the returned func runs. On unix an exited
// child stays a zombie (holding its pid) until Wait reaps it, so there is nothing to hold.
func HoldProcess(int) (release func()) { return func() {} }

func sysProcAttr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// KillTree terminates the process group led by pid (created by PrepareChild): SIGTERM
// first, then SIGKILL for anything still alive after a short grace period.
//
// The leader stays a zombie until its owner calls cmd.Wait, and a zombie still counts as
// signalable, so the grace period can run in full when nobody is waiting yet; the final
// SIGKILL is then harmless.
func KillTree(pid int) error {
	if err := TerminateTree(pid); err != nil {
		return err
	}
	deadline := time.Now().Add(KillGrace)
	for time.Now().Before(deadline) {
		if unix.Kill(-pid, 0) != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ForceKillTree(pid)
}

// TerminateTree is KillTree's first step: SIGTERM to the process group led by pid. A group
// that is already gone is not an error.
func TerminateTree(pid int) error {
	if err := unix.Kill(-pid, unix.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// ForceKillTree is KillTree's last step: SIGKILL to the process group led by pid.
func ForceKillTree(pid int) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

// SpawnDetached starts argv in a new session (setsid), so it outlives the caller and the
// terminal or browser that started it, with stdout and stderr appended to logPath.
func SpawnDetached(argv []string, logPath string) error {
	if len(argv) == 0 {
		return errors.New("no command to start")
	}
	log, err := openLogFile(logPath)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Dir = filepath.Dir(logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// startQuiet starts a desktop opener and reaps it in the background.
func startQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// userHomeSubdir joins parts under the home directory.
func userHomeSubdir(parts ...string) string {
	return filepath.Join(append([]string{homeDir()}, parts...)...)
}

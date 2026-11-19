//go:build !windows

package system

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrepareChildOwnGroup(t *testing.T) {
	cmd := exec.Command("true")
	PrepareChild(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid should be set so KillTree can signal the group")
	}
}

func TestAfterStartLowersPriority(t *testing.T) {
	cmd := helperCommand(t, "sleep")
	PrepareChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = KillTree(cmd.Process.Pid); _ = cmd.Wait() }()
	AfterStart(cmd)
	prio, err := unix.Getpriority(unix.PRIO_PROCESS, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Linux returns 20 - nice from the raw syscall; darwin returns nice itself
	if prio != childNice && prio != 20-childNice {
		t.Fatalf("priority = %d, want nice %d", prio, childNice)
	}
}

// processAlive reports whether pid still exists, without signalling it.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

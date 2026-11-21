package system

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrepareChildFlags(t *testing.T) {
	cmd := exec.Command("cmd")
	PrepareChild(cmd)
	flags := cmd.SysProcAttr.CreationFlags
	for name, flag := range map[string]uint32{
		"BELOW_NORMAL_PRIORITY_CLASS": 0x00004000,
		"CREATE_NEW_PROCESS_GROUP":    0x00000200,
		"CREATE_NO_WINDOW":            0x08000000,
	} {
		if flags&flag == 0 {
			t.Errorf("missing %s", name)
		}
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow should be set")
	}
}

func TestHideChildKeepsNormalPriority(t *testing.T) {
	cmd := exec.Command("cmd")
	HideChild(cmd)
	flags := cmd.SysProcAttr.CreationFlags
	if flags&0x08000000 == 0 {
		t.Error("missing CREATE_NO_WINDOW")
	}
	if flags&0x00004000 != 0 {
		t.Error("HideChild must not lower the priority")
	}
}

// processAlive reports whether pid still runs, without signalling it.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

func TestHoldProcessKeepsPidReserved(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 3")
	HideChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	release := HoldProcess(pid)
	_ = cmd.Wait()
	// Go has closed its handle; the held one still resolves the pid to the exited process
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		release()
		t.Fatalf("pid %d no longer resolves while held: %v", pid, err)
	}
	var code uint32
	err = windows.GetExitCodeProcess(h, &code)
	_ = windows.CloseHandle(h)
	release()
	if err != nil || code != 3 {
		t.Fatalf("exit code = %d, %v; want 3 from the original process", code, err)
	}
}

package system

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// helperEnv makes the test binary act as a small child process instead of running tests,
// so process tests need no external programs.
const helperEnv = "VIDGRAB_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "sleep":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "echo":
		fmt.Println("helper says hello")
		os.Exit(0)
	case "printtemp":
		// Stands in for `yt-dlp --version`, answering with the temp folder it was given
		fmt.Println(os.Getenv("TEMP"))
		os.Exit(0)
	case "parent":
		// Start a grandchild, publish its pid, then wait to be killed
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("VIDGRAB_TEST_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	os.Exit(4)
}

// helperCommand returns a command that runs this test binary in the given helper mode.
func helperCommand(t *testing.T, mode string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode), extraEnv...)
	return cmd
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// VidGrab helper: one binary that installs itself, runs the local API server and answers
// Firefox as its native messaging host, depending on how it is started.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"vidgrab/internal/core"
	"vidgrab/internal/server"
	"vidgrab/internal/system"
)

// version is the helper version, set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Modes chosen from the command line by classify.
const (
	modeDoubleClick = "double-click"
	modeInstall     = "install"
	modeUninstall   = "uninstall"
	modeServe       = "serve"
	modeVersion     = "version"
	modeNativeHost  = "native-host"
	modeUsage       = "usage"
)

const usage = `VidGrab helper %s

Usage:
  vidgrab                      install (same as "install"), then wait for Enter
  vidgrab install              install for the current user and register with Firefox
  vidgrab uninstall            stop the helper and remove everything it installed
  vidgrab serve [--idle-timeout N]
                               run the local server (exit after N idle seconds; 0 = never)
  vidgrab version              print the version
`

// classify picks the mode from the arguments after the program name.
//
// Firefox starts a native host as "<exe> <manifest path> <extension id>" (on Windows it may
// add a parent window handle), so a first argument ending in ".json" or any argument equal to
// the extension id means native messaging, whatever else is on the line.
func classify(args []string) string {
	if len(args) == 0 {
		return modeDoubleClick
	}
	if strings.HasSuffix(strings.ToLower(args[0]), ".json") {
		return modeNativeHost
	}
	for _, arg := range args {
		if arg == system.ExtensionID {
			return modeNativeHost
		}
	}
	switch args[0] {
	case modeInstall, modeUninstall, modeServe, modeVersion:
		return args[0]
	}
	return modeUsage
}

func main() {
	args := os.Args[1:]
	switch classify(args) {
	case modeNativeHost:
		os.Exit(system.RunNativeHost(os.Stdin, os.Stdout))
	case modeServe:
		os.Exit(runServe(args[1:]))
	case modeInstall:
		os.Exit(runInstall())
	case modeDoubleClick:
		code := runInstall()
		fmt.Println()
		fmt.Println("Press Enter to close")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(code)
	case modeUninstall:
		os.Exit(runUninstall())
	case modeVersion:
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
}

// runInstall copies this binary into the install folder and registers it with Firefox.
func runInstall() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not find this program's path: %v\n", err)
		return 1
	}
	report, err := system.Install(self)
	if text := report.String(); text != "" {
		fmt.Println(text)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Install failed: %v\n", err)
		return 1
	}
	fmt.Println("VidGrab helper installed. Restart Firefox if the add-on still says the helper is missing.")
	return 0
}

// runUninstall stops the server and removes the install folder and the Firefox registration.
func runUninstall() int {
	report, err := system.Uninstall()
	if text := report.String(); text != "" {
		fmt.Println(text)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Uninstall failed: %v\n", err)
		return 1
	}
	return 0
}

// serveFlags holds the parsed "serve" arguments.
type serveFlags struct {
	idleTimeout  time.Duration
	port         int
	dataDir      string
	downloadsDir string
}

// parseServeFlags parses the "serve" arguments. --port, --data-dir and --downloads-dir are
// hidden test hooks (a second instance on a scratch port and folders), left out of the usage.
func parseServeFlags(args []string, output io.Writer) (serveFlags, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	idle := fs.Float64("idle-timeout", 0, "exit after this many seconds without requests or active downloads (0 = never)")
	port := fs.Int("port", system.ServerPort, "")
	dataDir := fs.String("data-dir", "", "")
	downloadsDir := fs.String("downloads-dir", "", "")
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: vidgrab serve [--idle-timeout N]")
	}
	if err := fs.Parse(args); err != nil {
		return serveFlags{}, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return serveFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *idle < 0 {
		*idle = 0
	}
	parsed := serveFlags{
		idleTimeout:  time.Duration(*idle * float64(time.Second)),
		port:         *port,
		dataDir:      *dataDir,
		downloadsDir: *downloadsDir,
	}
	if parsed.dataDir == "" {
		parsed.dataDir = system.DataDir()
	}
	return parsed, nil
}

// runServe runs the HTTP server until it is told to stop, goes idle or gets Ctrl+C.
func runServe(args []string) int {
	opts, err := parseServeFlags(args, os.Stderr)
	if err != nil {
		return 2
	}
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	// /info calls run yt-dlp through core.DefaultRunner: start them hidden and at low priority,
	// and kill the whole tree (yt-dlp plus anything it spawned) when the timeout cancels them
	core.PrepareCommand = func(cmd *exec.Cmd) {
		system.PrepareChild(cmd)
		cmd.Cancel = func() error { return system.KillTree(cmd.Process.Pid) }
	}
	core.AfterStartCommand = func(cmd *exec.Cmd) func() {
		system.AfterStart(cmd)
		return system.HoldProcess(cmd.Process.Pid)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return server.Run(ctx, server.RunOptions{
		Version:      version,
		Port:         opts.port,
		IdleTimeout:  opts.idleTimeout,
		DataDir:      opts.dataDir,
		DownloadsDir: opts.downloadsDir,
	})
}

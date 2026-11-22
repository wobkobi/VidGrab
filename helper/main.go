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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"vidgrab/internal/core"
	"vidgrab/internal/server"
	"vidgrab/internal/system"
)

// version is the helper version, set at build time with -ldflags "-X main.version=...".
var version = "dev"

// addonURL is the add-on's page on addons.mozilla.org, which the installer points to.
const addonURL = "https://addons.mozilla.org/firefox/addon/vidgrab/"

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
  vidgrab                      install (same as "install")
  vidgrab install              install for the current user and register with Firefox
  vidgrab uninstall            stop the helper and remove everything it installed

Install and uninstall wait for Enter when run in their own window; --quiet skips that.
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
	case modeInstall, modeDoubleClick:
		os.Exit(pauseIfOwnConsole(runInstall(), args))
	case modeUninstall:
		os.Exit(pauseIfOwnConsole(runUninstall(), args))
	case modeVersion:
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
}

// pauseIfOwnConsole waits for Enter before returning code when Windows opened a console window
// just for this run (a double-click, or Uninstall in Settings > Apps), so the result stays on
// screen instead of the window vanishing. "--quiet" anywhere in args skips the pause.
func pauseIfOwnConsole(code int, args []string) int {
	if !slices.Contains(args, "--quiet") && system.OwnsConsole() {
		fmt.Println()
		fmt.Println("Press Enter to close")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	return code
}

// printDetails prints the install or uninstall report indented under the headline.
func printDetails(report system.Report) {
	for _, line := range report.Lines {
		fmt.Println("  " + line)
	}
}

// runInstall copies this binary into the install folder, registers it with Firefox and in
// Settings > Apps, then starts the server so yt-dlp and ffmpeg download straight away.
func runInstall() int {
	fmt.Printf("VidGrab helper %s\n\n", version)
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not find this program's path: %v\n", err)
		return 1
	}
	report, err := system.Install(self, version)
	printDetails(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nInstall failed: %v\n", err)
		return 1
	}
	started := system.StartServer(report.InstalledPath) == nil

	fmt.Println()
	fmt.Println("Done, the helper is installed. Next:")
	fmt.Println()
	fmt.Println("  1. If Firefox doesn't have VidGrab for YouTube yet, add it from")
	fmt.Println("     " + addonURL)
	fmt.Println("  2. That's it. The add-on finds the helper within a few seconds, no restart needed.")
	if started {
		fmt.Println()
		fmt.Println("The helper is fetching yt-dlp and ffmpeg in the background if they are missing")
		fmt.Println("(up to about 125 MB); the add-on's toolbar popup shows the progress.")
	}
	fmt.Println()
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(report.InstalledPath)) {
		fmt.Println("You can delete the downloaded file now.")
	}
	fmt.Println("To remove the helper later, use Settings > Apps > Installed apps > VidGrab helper.")
	return 0
}

// runUninstall stops the server and removes the install folder, the Firefox registration and
// the Settings > Apps entry.
func runUninstall() int {
	fmt.Printf("VidGrab helper %s\n\n", version)
	report, err := system.Uninstall()
	printDetails(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nUninstall failed: %v\n", err)
		return 1
	}
	fmt.Println()
	fmt.Println("The helper is removed. Downloaded videos stay where they are.")
	fmt.Println("To remove the add-on too, open about:addons in Firefox.")
	return 0
}

// serveFlags holds the parsed "serve" arguments.
type serveFlags struct {
	idleTimeout  time.Duration
	port         int
	dataDir      string
	downloadsDir string
}

// parseServeFlags parses the "serve" arguments. --port is how the native host starts the
// server on the port it chose; without it the server uses the one saved in the data
// directory (see system.LoadPort). --port, --data-dir and --downloads-dir are also test
// hooks (a second instance on a scratch port and folders), so they stay out of the usage.
func parseServeFlags(args []string, output io.Writer) (serveFlags, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	idle := fs.Float64("idle-timeout", 0, "exit after this many seconds without requests or active downloads (0 = never)")
	port := fs.Int("port", 0, "")
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
	if parsed.port <= 0 {
		var err error
		if parsed.port, err = system.LoadPort(filepath.Join(parsed.dataDir, system.PortFile)); err != nil {
			fmt.Fprintf(output, "Could not choose a port: %v\n", err)
			return serveFlags{}, err
		}
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

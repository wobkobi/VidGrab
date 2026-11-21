package main

import (
	"io"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args is a double-click", nil, modeDoubleClick},
		{"install", []string{"install"}, modeInstall},
		{"uninstall", []string{"uninstall"}, modeUninstall},
		{"serve", []string{"serve", "--idle-timeout", "300"}, modeServe},
		{"version", []string{"version"}, modeVersion},
		{"unknown command", []string{"frobnicate"}, modeUsage},
		{"flag only", []string{"--help"}, modeUsage},
		{"firefox launch", []string{`C:\Users\me\AppData\Local\VidGrab\vidgrab.json`, "yt-dl-firefox@local"}, modeNativeHost},
		{"manifest suffix is case-insensitive", []string{"/home/me/.mozilla/native-messaging-hosts/VIDGRAB.JSON"}, modeNativeHost},
		{"windows parent window handle", []string{`C:\x\vidgrab.json`, "yt-dl-firefox@local", "--parent-window=0x1234"}, modeNativeHost},
		{"extension id anywhere", []string{"something", "yt-dl-firefox@local"}, modeNativeHost},
		{"json elsewhere is not a launch", []string{"serve", "x.json"}, modeServe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.args); got != tc.want {
				t.Fatalf("classify(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseServeFlags(t *testing.T) {
	opts, err := parseServeFlags([]string{"--idle-timeout", "2.5", "--port", "19000", "--data-dir", "D", "--downloads-dir", "O"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.idleTimeout != 2500*time.Millisecond || opts.port != 19000 || opts.dataDir != "D" || opts.downloadsDir != "O" {
		t.Fatalf("unexpected options %+v", opts)
	}

	data := t.TempDir()
	defaults, err := parseServeFlags([]string{"--data-dir", data}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.idleTimeout != 0 || defaults.port < 20000 || defaults.port > 32767 || defaults.downloadsDir != "" {
		t.Fatalf("unexpected defaults %+v", defaults)
	}
	// The picked port is saved, so the next start uses the same one
	again, err := parseServeFlags([]string{"--data-dir", data}, io.Discard)
	if err != nil || again.port != defaults.port {
		t.Fatalf("second start got port %d, %v; want %d", again.port, err, defaults.port)
	}

	for _, bad := range [][]string{{"--idle-timeout", "soon"}, {"extra"}, {"--nope"}} {
		if _, err := parseServeFlags(bad, io.Discard); err == nil {
			t.Fatalf("parseServeFlags(%q) succeeded", bad)
		}
	}
}

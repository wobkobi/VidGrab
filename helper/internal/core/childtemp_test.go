package core

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// useChildTemp points the child temp folder at a fresh directory for one test.
func useChildTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetChildTempDir(dir)
	t.Cleanup(func() { SetChildTempDir("") })
	return dir
}

// leaveMEI creates what a killed PyInstaller run leaves behind.
func leaveMEI(t *testing.T, dir string) string {
	t.Helper()
	mei := filepath.Join(dir, "_MEI12345")
	if err := os.MkdirAll(filepath.Join(mei, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mei, "lib", "python.dll"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mei
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestChildEnvUsesPrivateTemp(t *testing.T) {
	t.Setenv("TEMP", `C:\elsewhere`)
	if env := ChildEnv(); !slices.Contains(env, `TEMP=C:\elsewhere`) {
		t.Fatal("TEMP changed without a child temp folder")
	}
	dir := useChildTemp(t)
	env := ChildEnv()
	for _, w := range []string{"TEMP=" + dir, "TMP=" + dir} {
		if !slices.Contains(env, w) {
			t.Errorf("env lacks %q", w)
		}
	}
	if slices.Contains(env, `TEMP=C:\elsewhere`) {
		t.Error("the inherited TEMP is still there")
	}
}

func TestKilledRunSweptOnceNoChildIsAlive(t *testing.T) {
	dir := useChildTemp(t)
	mei := leaveMEI(t, dir)
	first, second := TrackChild(), TrackChild()
	first(true)
	if !exists(mei) {
		t.Fatal("swept while another yt-dlp was alive")
	}
	if SweepChildTemp() || !exists(mei) {
		t.Fatal("SweepChildTemp ran while a yt-dlp was alive")
	}
	second(false)
	if exists(mei) {
		t.Fatal("leftovers of the killed run were not swept")
	}
	if !exists(dir) {
		t.Fatal("the temp folder itself was removed")
	}
}

func TestCleanRunDoesNotSweep(t *testing.T) {
	dir := useChildTemp(t)
	mei := leaveMEI(t, dir)
	done := TrackChild()
	done(false)
	done(true) // a second call is ignored
	if !exists(mei) {
		t.Fatal("a run that finished normally triggered a sweep")
	}
	if !SweepChildTemp() || exists(mei) {
		t.Fatal("start-up sweep left the leftovers")
	}
}

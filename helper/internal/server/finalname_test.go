package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vidgrab/internal/core"
)

// writingStarter fakes yt-dlp runs that write "<title> [id].<ext>" through the -o template
// (so playlist subfolders work) and report it, after any extra lines.
func writingStarter(t *testing.T, title, ext string, extra ...string) *fakeStarter {
	t.Helper()
	return &fakeStarter{byArgs: func(argv []string) *fakeProc {
		_, id, _ := strings.Cut(last(argv), "v=")
		path := strings.NewReplacer("%(title)s", title, "%(id)s", id, "%(ext)s", ext).Replace(argAfter(argv, "-o"))
		if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
			t.Error(err)
		}
		lines := append(append([]string{"YTDLP_TITLE " + title}, extra...), "YTDLP_FILE "+path)
		return newFakeProc(lines, 0, false)
	}}
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	stem := filepath.Join(dir, "A (20 MB)")
	if got, err := freeName(stem, ".mp4"); err != nil || got != stem+".mp4" {
		t.Fatalf("free name %q %v", got, err)
	}
	for _, name := range []string{stem + ".mp4", stem + " (2).mp4", stem + " (3).m4a"} {
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := freeName(stem, ".mp4"); err != nil || got != stem+" (3).mp4" {
		t.Fatalf("taken names: %q %v", got, err)
	}
}

func TestDownloadIsRenamedWithoutID(t *testing.T) {
	dir := t.TempDir()
	q := makeQueue(t, dir, writingStarter(t, "Me at the zoo", "mkv"))
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	job := snapshot(t, q, id)
	if str(job.Filepath) != filepath.Join(dir, "Me at the zoo.mkv") || str(job.Filename) != "Me at the zoo.mkv" {
		t.Fatalf("file %q / %q", str(job.Filepath), str(job.Filename))
	}
	if got := listDir(t, dir); len(got) != 1 || got[0] != "Me at the zoo.mkv" {
		t.Fatalf("files %v", got)
	}
}

func TestRenameNeverReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Clip.mp4", "Clip (2).mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	q := makeQueue(t, dir, writingStarter(t, "Clip", "mp4"), withConcurrency(1))
	// Two different videos with the same title each get their own name
	first := create(t, q, watchURL(videos[0]), defaults())
	second := create(t, q, watchURL(videos[1]), defaults())
	waitFor(t, func() bool { return status(t, q, first) == "complete" && status(t, q, second) == "complete" })
	if got := str(snapshot(t, q, first).Filename); got != "Clip (3).mp4" {
		t.Fatalf("first saved as %s", got)
	}
	if got := str(snapshot(t, q, second).Filename); got != "Clip (4).mp4" {
		t.Fatalf("second saved as %s", got)
	}
	if fileSize(t, filepath.Join(dir, "Clip.mp4")) != 7 || fileSize(t, filepath.Join(dir, "Clip (2).mp4")) != 7 {
		t.Fatal("an existing file was replaced")
	}
}

func TestAudioTitleWithBracketsKeepsThem(t *testing.T) {
	dir := t.TempDir()
	// The title holds brackets, even the video's own id; only the marker the template added goes
	title := "Song [Live] [" + videos[0] + "]"
	q := makeQueue(t, dir, writingStarter(t, title, "mp3", "[ExtractAudio] Destination: x.mp3"))
	opts := defaults()
	opts.Kind = "audio"
	id := create(t, q, watchURL(videos[0]), opts)
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if got, want := str(snapshot(t, q, id).Filename), title+".mp3"; got != want {
		t.Fatalf("saved as %q, want %q", got, want)
	}
}

func TestPlaylistItemsAreRenamed(t *testing.T) {
	dir := t.TempDir()
	fetcher := func(string, []core.ItemRange) (map[string]any, error) {
		return map[string]any{"title": "List", "entries": []any{map[string]any{"id": videos[0], "title": "A"}}}, nil
	}
	q := makeQueue(t, dir, writingStarter(t, "A", "mp4"), func(c *QueueConfig) { c.FetchInfo = fetcher })
	jobs, err := q.Create("https://www.youtube.com/playlist?list=PLBCF2DAC6FFB574DE", defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return status(t, q, jobs[0].ID) == "complete" })
	if got := str(snapshot(t, q, jobs[0].ID).Filepath); got != filepath.Join(dir, "List", "A.mp4") {
		t.Fatalf("saved as %s", got)
	}
}

func TestRenameFailureKeepsIDNameAndCompletes(t *testing.T) {
	dir := t.TempDir()
	q := makeQueue(t, dir, writingStarter(t, "Clip", "mp4"))
	var mu sync.Mutex
	tries := 0
	q.rename = func(string, string) error {
		mu.Lock()
		defer mu.Unlock()
		tries++
		return errors.New("The process cannot access the file because it is being used by another process.")
	}
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	job := snapshot(t, q, id)
	kept := filepath.Join(dir, "Clip ["+videos[0]+"].mp4")
	if str(job.Filepath) != kept || job.Error != nil || job.Percent != 100 || !exists(kept) {
		t.Fatalf("job %+v", job)
	}
	mu.Lock()
	defer mu.Unlock()
	if tries != renameTries {
		t.Fatalf("tries %d", tries)
	}
}

func TestAlreadyDownloadedFileKeepsItsName(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "Clip ["+videos[0]+"].mp4")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{"[download] " + old + " has already been downloaded", "YTDLP_FILE " + old}
	q := makeQueue(t, dir, &fakeStarter{factory: func() *fakeProc { return newFakeProc(lines, 0, false) }})
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if got := str(snapshot(t, q, id).Filepath); got != old || !exists(old) {
		t.Fatalf("saved as %s, files %v", got, listDir(t, dir))
	}
}

func TestResumedMergeIsRenamed(t *testing.T) {
	// Finished streams reported as already downloaded do not make the merged file pre-existing
	dir := t.TempDir()
	stream := filepath.Join(dir, "Clip ["+videos[0]+"].f137.mp4")
	q := makeQueue(t, dir, writingStarter(t, "Clip", "mp4",
		"[download] "+stream+" has already been downloaded",
		`[Merger] Merging formats into "`+filepath.Join(dir, "Clip ["+videos[0]+"].mp4")+`"`))
	id := create(t, q, watchURL(videos[0]), defaults())
	waitFor(t, func() bool { return status(t, q, id) == "complete" })
	if got := str(snapshot(t, q, id).Filename); got != "Clip.mp4" {
		t.Fatalf("saved as %s", got)
	}
}

func TestStoppedRunIsNotRenamedUntilItsNextRun(t *testing.T) {
	dir := t.TempDir()
	q := makeQueue(t, dir, newBlockingStarter())
	j := newJob(watchURL(videos[0]), videos[0], defaults(), dir, "Clip", "", "")
	path := filepath.Join(dir, "Clip ["+videos[0]+"].mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pause landed between yt-dlp exiting and the rename
	j.runID, j.stoppedRun = 1, 1
	if res := q.finishDownload(j, 1, path, false); res != (outcome{}) || !exists(path) || j.status == StatusComplete {
		t.Fatalf("stopped run renamed: %+v, files %v", res, listDir(t, dir))
	}
	// The resumed run's yt-dlp finds the file already downloaded; it is still this job's own
	j.runID, j.stoppedRun = 2, 1
	res := q.finishDownload(j, 2, path, true)
	if want := filepath.Join(dir, "Clip.mp4"); res.filepath != want || !exists(want) || j.status != StatusComplete {
		t.Fatalf("resumed run %+v, files %v", res, listDir(t, dir))
	}
}

func TestStripIDNeedsTheMarker(t *testing.T) {
	// A reported path without " [id]" (or one only the marker would be left of) is not renamed
	dir := t.TempDir()
	q := makeQueue(t, dir, newBlockingStarter())
	j := newJob(watchURL(videos[0]), videos[0], defaults(), dir, "", "", "")
	for _, name := range []string{"Other [" + videos[1] + "].mp4", " [" + videos[0] + "].mp4"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if res := q.finishDownload(j, 0, path, false); res.filepath != path || !exists(path) {
			t.Fatalf("%q: %+v", name, res)
		}
	}
	if _, ok := core.StripID(filepath.Join(dir, "x"), ""); ok {
		t.Fatal("empty id stripped")
	}
}

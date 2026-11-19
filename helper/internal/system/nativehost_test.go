package system

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vidgrab/internal/auth"
)

func frame(t *testing.T, message any) []byte {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	binary.NativeEndian.PutUint32(header, uint32(len(body)))
	return append(header, body...)
}

func TestNativeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNativeMessage(&buf, map[string]any{"status": "running", "detail": "héllo"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadNativeMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["status"] != "running" || m["detail"] != "héllo" {
		t.Fatalf("got %v", got)
	}
}

func TestNativeEndOfInputReturnsNil(t *testing.T) {
	for _, input := range [][]byte{{}, {0x01, 0x00}} {
		got, err := ReadNativeMessage(bytes.NewReader(input))
		if got != nil || err != nil {
			t.Fatalf("%v: got %v, %v; want nil, nil", input, got, err)
		}
	}
}

func TestNativeTruncatedAndOversized(t *testing.T) {
	header := make([]byte, 4)
	binary.NativeEndian.PutUint32(header, 10)
	if _, err := ReadNativeMessage(bytes.NewReader(append(header, '{', '}'))); err == nil {
		t.Fatal("truncated body should be an error")
	}
	binary.NativeEndian.PutUint32(header, 50*1024*1024)
	if _, err := ReadNativeMessage(bytes.NewReader(header)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized message should be an error, got %v", err)
	}
}

// fakeHost builds a NativeHost whose probe answers come from answers in turn (the last
// repeats), with an instant fake clock.
type fakeHost struct {
	answers  []any // map[string]any, nil, or error
	probes   int
	spawned  int
	spawnErr error
	now      time.Time
}

func (f *fakeHost) host() *NativeHost {
	return &NativeHost{
		LoadToken: func() (string, error) { return strings.Repeat("t", 43), nil },
		Probe: func(string) (map[string]any, error) {
			answer := f.answers[min(f.probes, len(f.answers)-1)]
			f.probes++
			switch a := answer.(type) {
			case error:
				return nil, a
			case map[string]any:
				return a, nil
			}
			return nil, nil
		},
		Spawn: func() error {
			f.spawned++
			return f.spawnErr
		},
		Wait:  ServerStartWait,
		Sleep: func(d time.Duration) { f.now = f.now.Add(d) },
		Now:   func() time.Time { return f.now },
	}
}

func foreign() error {
	return &auth.ForeignBackendError{Msg: fmt.Sprintf("Port %d is used by another program, not the VidGrab helper", ServerPort)}
}

func TestAlreadyRunningDoesNotSpawn(t *testing.T) {
	f := &fakeHost{answers: []any{map[string]any{"version": "2.0.0"}}}
	reply := f.host().EnsureServer("tok")
	if reply["status"] != "running" || reply["detail"] != "Backend 2.0.0 is running" || f.spawned != 0 {
		t.Fatalf("reply %v, spawned %d", reply, f.spawned)
	}
}

func TestStartsServerWhenDown(t *testing.T) {
	f := &fakeHost{answers: []any{nil, nil, map[string]any{"version": "2.0.0"}}}
	reply := f.host().EnsureServer("tok")
	if reply["status"] != "started" || reply["detail"] != "Backend 2.0.0 started" || f.spawned != 1 {
		t.Fatalf("reply %v, spawned %d", reply, f.spawned)
	}
}

func TestStartThatNeverAnswers(t *testing.T) {
	f := &fakeHost{answers: []any{nil}}
	reply := f.host().EnsureServer("tok")
	if reply["status"] != "starting" {
		t.Fatalf("reply %v", reply)
	}
	// 10 s of 0.5 s polls, plus the first probe
	if f.probes != 21 {
		t.Fatalf("probes = %d, want 21", f.probes)
	}
}

func TestSpawnFailure(t *testing.T) {
	f := &fakeHost{answers: []any{nil}, spawnErr: errors.New("no such file")}
	reply := f.host().EnsureServer("tok")
	if reply["status"] != "error" || !strings.Contains(reply["detail"].(string), "no such file") {
		t.Fatalf("reply %v", reply)
	}
}

func TestForeignBackendIsAnError(t *testing.T) {
	f := &fakeHost{answers: []any{foreign()}}
	reply := f.host().EnsureServer("tok")
	if reply["status"] != "error" || !strings.Contains(reply["detail"].(string), "another program") || f.spawned != 0 {
		t.Fatalf("reply %v, spawned %d", reply, f.spawned)
	}
}

func runHost(t *testing.T, h *NativeHost, input []byte) (int, map[string]any) {
	t.Helper()
	var out bytes.Buffer
	code := h.Run(bytes.NewReader(input), &out)
	reply, err := ReadNativeMessage(&out)
	if err != nil {
		t.Fatal(err)
	}
	return code, reply.(map[string]any)
}

func TestRunStartCommand(t *testing.T) {
	f := &fakeHost{answers: []any{nil, map[string]any{"version": "2.1.0"}}}
	code, reply := runHost(t, f.host(), frame(t, map[string]any{"command": "start"}))
	want := map[string]any{"status": "started", "detail": "Backend 2.1.0 started", "token": strings.Repeat("t", 43)}
	if code != 0 || fmt.Sprint(reply) != fmt.Sprint(want) {
		t.Fatalf("code %d, reply %v", code, reply)
	}
}

func TestRunPing(t *testing.T) {
	f := &fakeHost{answers: []any{nil}}
	_, reply := runHost(t, f.host(), frame(t, map[string]any{"command": "ping"}))
	if reply["status"] != "stopped" || reply["token"] != strings.Repeat("t", 43) || f.spawned != 0 {
		t.Fatalf("reply %v, spawned %d", reply, f.spawned)
	}
}

func TestRunPingReportsForeignBackend(t *testing.T) {
	f := &fakeHost{answers: []any{foreign()}}
	_, reply := runHost(t, f.host(), frame(t, map[string]any{"command": "ping"}))
	if reply["status"] != "error" {
		t.Fatalf("reply %v", reply)
	}
}

func TestRunBadFrame(t *testing.T) {
	header := make([]byte, 4)
	binary.NativeEndian.PutUint32(header, 5)
	code, reply := runHost(t, (&fakeHost{answers: []any{nil}}).host(), append(header, '{'))
	if code != 1 || reply["status"] != "error" {
		t.Fatalf("code %d, reply %v", code, reply)
	}
}

func TestRunTokenFailure(t *testing.T) {
	h := (&fakeHost{answers: []any{nil}}).host()
	h.LoadToken = func() (string, error) { return "", errors.New("disk full") }
	code, reply := runHost(t, h, frame(t, map[string]any{"command": "start"}))
	if code != 1 || !strings.Contains(reply["detail"].(string), "Could not read the backend key") {
		t.Fatalf("code %d, reply %v", code, reply)
	}
}

package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testToken = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"

func TestLoadTokenCreatesOnceAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "token")
	first, err := LoadToken(path)
	if err != nil || len(first) < 32 {
		t.Fatalf("LoadToken = %q, %v", first, err)
	}
	if second, _ := LoadToken(path); second != first {
		t.Fatalf("token changed: %q != %q", second, first)
	}
	if data, _ := os.ReadFile(path); string(data) != first {
		t.Fatalf("file holds %q", data)
	}
}

func TestLoadTokenReplacesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("not a token"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := LoadToken(path)
	if err != nil || token == "not a token" {
		t.Fatalf("LoadToken = %q, %v", token, err)
	}
	if again, _ := LoadToken(path); again != token {
		t.Fatal("replacement token not reused")
	}
}

func TestTokenMatches(t *testing.T) {
	if !TokenMatches(testToken, testToken) {
		t.Fatal("same token rejected")
	}
	for _, bad := range []string{"", testToken[:len(testToken)-1] + "x", "short"} {
		if TokenMatches(testToken, bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func healthServer(t *testing.T, build func(nonce string) map[string]any) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(build(r.URL.Query().Get("nonce")))
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestProbeAcceptsValidProof(t *testing.T) {
	port := healthServer(t, func(nonce string) map[string]any {
		return map[string]any{"status": "ok", "proof": Proof(testToken, nonce)}
	})
	data, err := Probe(port, testToken, time.Second)
	if err != nil || data["status"] != "ok" {
		t.Fatalf("Probe = %v, %v", data, err)
	}
}

func TestProbeRejectsImpostor(t *testing.T) {
	builds := []func(string) map[string]any{
		func(string) map[string]any { return map[string]any{"status": "ok"} },
		func(n string) map[string]any {
			return map[string]any{"proof": Proof("other-other-other-other-other-other", n)}
		},
	}
	for _, build := range builds {
		if _, err := Probe(healthServer(t, build), testToken, time.Second); !errors.Is(err, ErrForeignBackend) {
			t.Fatalf("want ErrForeignBackend, got %v", err)
		}
	}
}

func TestProbeNothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if data, err := Probe(port, testToken, 500*time.Millisecond); data != nil || err != nil {
		t.Fatalf("Probe = %v, %v", data, err)
	}
}

package system

import (
	"net/http"
	"strings"
	"testing"
)

func TestDefaultClientRefusesPlainHTTP(t *testing.T) {
	client := NewTools(t.TempDir()).Client
	_, err := client.Get("http://127.0.0.1:1/yt-dlp.exe")
	if err == nil || !strings.Contains(err.Error(), "refusing non-HTTPS") {
		t.Fatalf("want refusal, got %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	if _, err := (httpsOnly{next: stubTransport{}}).RoundTrip(req); err != nil {
		t.Fatalf("HTTPS refused: %v", err)
	}
}

type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// Per-install shared secret: the extension sends it on every call and the backend proves it.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// TokenHeader carries the install token on every request except GET /health.
	TokenHeader = "X-YTDL-Token"
	// ClientHeader must be "1" on every request; it forces a CORS preflight web pages fail.
	ClientHeader = "X-YTDL-Client"
	// TokenFile is the token's file name inside the data directory.
	TokenFile = "token"
	// MaxNonceLength bounds the /health nonce query parameter.
	MaxNonceLength = 128
)

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

// ErrForeignBackend means something answered on the backend port without a valid proof.
var ErrForeignBackend = errors.New("foreign backend")

// ForeignBackendError explains which kind of impostor answered; it wraps ErrForeignBackend.
type ForeignBackendError struct{ Msg string }

func (e *ForeignBackendError) Error() string { return e.Msg }

// Unwrap lets errors.Is match ErrForeignBackend.
func (e *ForeignBackendError) Unwrap() error { return ErrForeignBackend }

func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// LoadToken reads the install token at path, creating it on first use.
//
// The file is created exclusively with owner-only permissions, so the server and the native
// host racing to create it share whichever token was written first. Content that is not a
// token (empty or partial while another process writes it, or garbage) is re-read briefly,
// then replaced.
func LoadToken(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	for i := 0; i < 20; i++ {
		data, err := os.ReadFile(path)
		if err == nil {
			existing := strings.TrimSpace(string(data))
			if tokenRe.MatchString(existing) {
				return existing, nil
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		token := newToken()
		_, werr := f.WriteString(token)
		cerr := f.Close()
		if werr != nil {
			return "", werr
		}
		if cerr != nil {
			return "", cerr
		}
		return token, nil
	}
	token := newToken()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	return token, nil
}

// TokenMatches compares a supplied token with the real one in constant time.
func TokenMatches(token, supplied string) bool {
	if supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(supplied)) == 1
}

// Proof returns the hex HMAC-SHA256 of a nonce under the token, as /health reports it.
func Proof(token, nonce string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// Probe asks whatever listens on 127.0.0.1:port for /health and checks its proof of the token.
//
// It returns the /health body when the real backend answered, (nil, nil) when nothing
// answered, and a *ForeignBackendError when something answered without a valid proof.
func Probe(port int, token string, timeout time.Duration) (map[string]any, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	target := fmt.Sprintf("http://127.0.0.1:%d/health?nonce=%s", port, url.QueryEscape(nonce))
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ClientHeader, "1")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		// Connection refused or timed out: nothing is listening
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &ForeignBackendError{fmt.Sprintf("Port %d answered with HTTP %d", port, resp.StatusCode)}
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return nil, &ForeignBackendError{fmt.Sprintf("Port %d answered with something that is not the backend", port)}
	}
	reported, _ := data["proof"].(string)
	if !hmac.Equal([]byte(reported), []byte(Proof(token, nonce))) {
		return nil, &ForeignBackendError{fmt.Sprintf("Port %d is used by another program, not the VidGrab helper", port)}
	}
	return data, nil
}

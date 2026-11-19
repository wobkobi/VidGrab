// Firefox native messaging host: hands the extension the install token and starts the
// server when it is not running.

package system

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"vidgrab/internal/auth"
)

const (
	// MaxNativeMessageBytes bounds one incoming native message.
	MaxNativeMessageBytes = 1024 * 1024
	// ServerIdleTimeout is the idle timeout, in seconds, of a server the host starts.
	ServerIdleTimeout = 300
	// ServerStartWait is how long the host waits for a freshly started server to answer.
	ServerStartWait = 10 * time.Second
	// ServerLogFile is the started server's log file name inside the data directory.
	ServerLogFile = "server.log"

	probeTimeout = 1500 * time.Millisecond
	pollInterval = 500 * time.Millisecond
)

// ReadNativeMessage reads one length-prefixed JSON message: a 4-byte native-order length,
// then that many bytes of UTF-8 JSON. It returns (nil, nil) at end of input.
func ReadNativeMessage(r io.Reader) (any, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil
		}
		return nil, err
	}
	length := binary.NativeEndian.Uint32(header[:])
	if length > MaxNativeMessageBytes {
		return nil, errors.New("Message too large")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, errors.New("Truncated message")
	}
	var message any
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, fmt.Errorf("Message is not JSON: %v", err)
	}
	return message, nil
}

// WriteNativeMessage writes one length-prefixed JSON message.
func WriteNativeMessage(w io.Writer, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.NativeEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// NativeHost answers one message from Firefox. The fields are the seams tests replace.
type NativeHost struct {
	// LoadToken returns the install token.
	LoadToken func() (string, error)
	// Probe returns the /health body of the real server, nil when nothing answers, or an
	// error wrapping auth.ErrForeignBackend when another program holds the port.
	Probe func(token string) (map[string]any, error)
	// Spawn starts the server detached.
	Spawn func() error
	// Wait bounds how long a freshly started server gets to answer.
	Wait time.Duration
	// Sleep and Now drive the wait loop.
	Sleep func(time.Duration)
	Now   func() time.Time
	// Log receives one diagnostic line, which Firefox shows in the Browser Console.
	Log io.Writer
}

// NewNativeHost returns a NativeHost wired to the real data directory, port and executable.
func NewNativeHost() *NativeHost {
	dataDir := DataDir()
	return &NativeHost{
		LoadToken: func() (string, error) { return auth.LoadToken(filepath.Join(dataDir, auth.TokenFile)) },
		Probe: func(token string) (map[string]any, error) {
			return auth.Probe(ServerPort, token, probeTimeout)
		},
		Spawn: func() error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			argv := []string{self, "serve", "--idle-timeout", fmt.Sprint(ServerIdleTimeout)}
			return SpawnDetached(argv, filepath.Join(dataDir, ServerLogFile))
		},
		Wait:  ServerStartWait,
		Sleep: time.Sleep,
		Now:   time.Now,
		Log:   os.Stderr,
	}
}

// RunNativeHost handles one message from Firefox on stdin, replies on stdout and returns
// the process exit code.
func RunNativeHost(stdin io.Reader, stdout io.Writer) int {
	return NewNativeHost().Run(stdin, stdout)
}

// Run handles one message, replies and returns the exit code.
//
// Every reply carries the install token, since this host is the only channel that hands it
// to the extension (the host manifest's allowed_extensions limits who can call it). The
// "ping" command only reports whether the server runs; anything else also starts it.
func (h *NativeHost) Run(stdin io.Reader, stdout io.Writer) int {
	message, err := ReadNativeMessage(stdin)
	if err != nil {
		_ = WriteNativeMessage(stdout, map[string]any{"status": "error", "detail": err.Error()})
		return 1
	}
	token, err := h.LoadToken()
	if err != nil {
		_ = WriteNativeMessage(stdout, map[string]any{"status": "error", "detail": "Could not read the backend key: " + err.Error()})
		return 1
	}
	var command any
	if m, ok := message.(map[string]any); ok {
		command = m["command"]
	}
	var reply map[string]any
	if command == "ping" {
		current, err := h.Probe(token)
		switch {
		case err != nil:
			reply = map[string]any{"status": "error", "detail": err.Error()}
		case current != nil:
			reply = map[string]any{"status": "running", "detail": "Backend is running"}
		default:
			reply = map[string]any{"status": "stopped", "detail": "Backend is not running"}
		}
	} else {
		reply = h.EnsureServer(token)
	}
	reply["token"] = token
	if err := WriteNativeMessage(stdout, reply); err != nil {
		return 1
	}
	if h.Log != nil {
		fmt.Fprintf(h.Log, "vidgrab: %v (pid %d)\n", reply["status"], os.Getpid())
	}
	return 0
}

// EnsureServer makes sure the server runs, starting it when needed, and returns
// {status, detail}: status is "running", "started", "starting" or "error". A program that
// answers on the port without proving it knows the token is reported as an error rather
// than trusted, and no server is started.
func (h *NativeHost) EnsureServer(token string) map[string]any {
	current, err := h.Probe(token)
	if err != nil {
		return map[string]any{"status": "error", "detail": err.Error()}
	}
	if current != nil {
		return map[string]any{"status": "running", "detail": fmt.Sprintf("Backend %s is running", healthVersion(current))}
	}
	if err := h.Spawn(); err != nil {
		return map[string]any{"status": "error", "detail": "Could not start the backend: " + err.Error()}
	}
	deadline := h.Now().Add(h.Wait)
	for h.Now().Before(deadline) {
		h.Sleep(pollInterval)
		current, err := h.Probe(token)
		if err != nil {
			return map[string]any{"status": "error", "detail": err.Error()}
		}
		if current != nil {
			return map[string]any{"status": "started", "detail": fmt.Sprintf("Backend %s started", healthVersion(current))}
		}
	}
	return map[string]any{"status": "starting", "detail": "Backend was launched but has not answered yet"}
}

// healthVersion returns the "version" field of a /health body, or "".
func healthVersion(health map[string]any) string {
	if v, ok := health["version"]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// The server's loopback port. Each install picks its own at random rather than sharing one
// fixed number, so it rarely collides with another program, and keeps it in the data
// directory so the native host, the server and the installer all agree on it.

package system

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// PortFile is the file in the data directory holding the server's port.
	PortFile = "port"
	// LegacyPort is the fixed port helpers used before ports were picked per install. The
	// installer still stops a server there when no port file exists yet.
	LegacyPort = 18956

	// portLow and portHigh bound the random pick. The range sits below the ports operating
	// systems hand out for outgoing connections (49152 and up on Windows and macOS, 32768 and
	// up on Linux), so a connection the OS opens can never be holding the port.
	portLow  = 20000
	portHigh = 32767
	// portTries bounds how many random ports are tried before giving up; a few busy ones are
	// normal, dozens mean something is wrong with the machine's networking.
	portTries = 50
)

// portFree reports whether a loopback listener can be opened on port right now. A test seam.
var portFree = func(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// ReadPort returns the port saved at path, and false when the file is missing or does not
// hold a port number.
func ReadPort(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// LoadPort returns the port saved at path, picking and saving a new one (see NewPort) when
// there is none yet.
func LoadPort(path string) (int, error) {
	if port, ok := ReadPort(path); ok {
		return port, nil
	}
	return NewPort(path, 0)
}

// NewPort picks a random free port between portLow and portHigh other than avoid (the port
// just found taken, 0 for none), saves it at path and returns it. The file is replaced in one
// rename, so a server or native host reading it at the same moment sees the old port or the
// new one, never half of it.
func NewPort(path string, avoid int) (int, error) {
	for try := 0; try < portTries; try++ {
		port := portLow + rand.IntN(portHigh-portLow+1)
		if port == avoid || !portFree(port) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return 0, err
		}
		temp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
		if err := os.WriteFile(temp, []byte(strconv.Itoa(port)+"\n"), 0o644); err != nil {
			return 0, err
		}
		if err := os.Rename(temp, path); err != nil {
			_ = os.Remove(temp)
			return 0, err
		}
		return port, nil
	}
	return 0, errors.New("could not find a free port for the helper")
}

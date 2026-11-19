//go:build !windows

package server

import "syscall"

// exclusiveControl does nothing outside Windows: without SO_REUSEPORT a second bind of the
// same port already fails.
func exclusiveControl(_, _ string, _ syscall.RawConn) error { return nil }

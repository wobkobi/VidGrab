package server

import "syscall"

// soExclusiveAddrUse is Winsock's SO_EXCLUSIVEADDRUSE, defined as ~SO_REUSEADDR.
const soExclusiveAddrUse = ^4

// exclusiveControl sets SO_EXCLUSIVEADDRUSE before bind, so no other process can bind the
// same port alongside the server (Windows otherwise allows that with SO_REUSEADDR).
func exclusiveControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}

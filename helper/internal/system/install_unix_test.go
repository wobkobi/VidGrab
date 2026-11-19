//go:build !windows

package system

import "testing"

// The registry exists only on Windows; these keep the shared install tests compiling.

func readTestRegistry(t *testing.T) string {
	t.Helper()
	return ""
}

func testRegistryExists() bool { return false }

func cleanupTestRegistry() {}

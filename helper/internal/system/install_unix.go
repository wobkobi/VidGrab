//go:build !windows

package system

import (
	"os"
	"runtime"
)

// defaultManifestPath returns the per-user file Firefox scans for the native host manifest.
func defaultManifestPath(string) string {
	if runtime.GOOS == "darwin" {
		return userHomeSubdir("Library", "Application Support", "Mozilla", "NativeMessagingHosts", HostName+".json")
	}
	return userHomeSubdir(".mozilla", "native-messaging-hosts", HostName+".json")
}

// defaultRegistryKey is empty: only Windows registers the host in the registry.
func defaultRegistryKey() string { return "" }

func registerHost(string, string) error { return nil }

func unregisterHost(string) (bool, error) { return false, nil }

// scheduleDirRemoval removes dir at once: unix can delete a running executable.
func scheduleDirRemoval(dir string) error {
	return os.RemoveAll(dir)
}

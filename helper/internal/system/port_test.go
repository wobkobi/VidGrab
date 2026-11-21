package system

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPortPicksAndKeeps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", PortFile)
	port, err := LoadPort(path)
	if err != nil || port < portLow || port > portHigh {
		t.Fatalf("LoadPort = %d, %v", port, err)
	}
	if again, err := LoadPort(path); err != nil || again != port {
		t.Fatalf("second LoadPort = %d, %v; want %d", again, err, port)
	}
	if saved, ok := ReadPort(path); !ok || saved != port {
		t.Fatalf("ReadPort = %d, %v", saved, ok)
	}
}

func TestReadPortRejectsJunk(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"", "abc", "0", "70000", "-5"} {
		path := filepath.Join(dir, "port")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if port, ok := ReadPort(path); ok {
			t.Errorf("ReadPort(%q) = %d", content, port)
		}
	}
	if _, ok := ReadPort(filepath.Join(dir, "missing")); ok {
		t.Error("a missing file should give no port")
	}
}

func TestNewPortSkipsBusyAndAvoided(t *testing.T) {
	saved := portFree
	t.Cleanup(func() { portFree = saved })
	tried := 0
	portFree = func(int) bool {
		tried++
		return tried > 3
	}
	path := filepath.Join(t.TempDir(), PortFile)
	port, err := NewPort(path, 0)
	if err != nil || tried != 4 {
		t.Fatalf("NewPort = %d, %v after %d tries", port, err, tried)
	}

	portFree = func(int) bool { return false }
	if _, err := NewPort(path, 0); err == nil {
		t.Fatal("NewPort should fail when nothing is free")
	}
	if kept, ok := ReadPort(path); !ok || kept != port {
		t.Fatalf("a failed pick should keep the saved port, got %d, %v", kept, ok)
	}
}

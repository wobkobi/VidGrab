package server

import "testing"

func TestHostAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:18956":         true,
		"localhost:18956":         true,
		"127.0.0.1":               true,
		"127.0.0.1:x.evil.com":    false,
		"127.0.0.1:":              false,
		"evil.com:18956":          false,
		"localhost.evil.com:1234": false,
	} {
		if got := hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

//go:build windows

package lifecycle

import "testing"

func createStaleSocket(t *testing.T, _ string) {
	t.Helper()
	t.Skip("stale Unix socket coverage is unavailable on Windows")
}

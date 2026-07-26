//go:build !windows

package lifecycle

import (
	"syscall"
	"testing"
)

func createStaleSocket(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("creating stale socket: %v", err)
	}
	addr := &syscall.SockaddrUnix{Name: path}
	if err := syscall.Bind(fd, addr); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("binding stale socket: %v", err)
	}
	// A raw Unix socket leaves its pathname behind when its descriptor closes.
	if err := syscall.Close(fd); err != nil {
		t.Fatalf("closing stale socket: %v", err)
	}
}

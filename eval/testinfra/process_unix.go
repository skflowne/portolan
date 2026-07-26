//go:build !windows

package testinfra

import (
	"errors"
	"os"
	"syscall"
)

func pidExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func killProcess(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

// Terminate asks a daemon process to shut down normally.
func Terminate(process *os.Process) error { return process.Signal(syscall.SIGTERM) }

// IsClosedConnError reports the normal read errors from a peer closing a socket.
func IsClosedConnError(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}

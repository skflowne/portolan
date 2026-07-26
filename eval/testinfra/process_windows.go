//go:build windows

package testinfra

import (
	"errors"
	"os"
	"syscall"
)

// Daemon evals are skipped on Windows; these definitions keep the shared
// harness cross-compilable without Unix-only syscall references.
func pidExists(int) bool          { return false }
func killProcess(int) error       { return nil }
func Terminate(*os.Process) error { return errors.ErrUnsupported }

func IsClosedConnError(err error) bool {
	return errors.Is(err, syscall.Errno(10054)) // WSAECONNRESET
}

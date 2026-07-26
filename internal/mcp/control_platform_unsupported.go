//go:build !linux && !darwin && !freebsd

package mcp

import (
	"errors"
	"net"
	"os"
)

func effectiveUserID() int { return 0 }

func ownedByEffectiveUser(os.FileInfo) bool { return false }

func openOwnershipLock(string) (*os.File, error) {
	return nil, errors.New("control sockets are not supported on this platform")
}

func tryLockFile(*os.File) error {
	return errors.New("control sockets are not supported on this platform")
}

func isLockBusy(error) bool { return false }

func authorizeControlPeer(net.Conn) error {
	return errors.New("control peer authorization is not supported on this platform")
}

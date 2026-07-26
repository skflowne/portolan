//go:build linux

package mcp

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func authorizeControlPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("control connection is not a unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if cred == nil {
		return errors.New("control peer credentials are unavailable")
	}
	if cred.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("control peer uid %d is not authorized", cred.Uid)
	}
	return nil
}

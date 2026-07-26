//go:build linux || darwin || freebsd

package mcp

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func effectiveUserID() int { return os.Geteuid() }

func ownedByEffectiveUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func openOwnershipLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByEffectiveUser(info) || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("ownership lock is not a private regular file owned by the effective user")
	}
	return file, nil
}

func tryLockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func isLockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

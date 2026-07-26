package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// preparePath rejects unsafe existing paths and probes socket paths for a live
// listener. It returns the stale inode so installation can prove the pathname
// did not change before publishing the staged listener.
func (c *ControlSocket) preparePath() (os.FileInfo, error) {
	info, err := os.Lstat(c.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("control socket: inspecting %s: %w", c.path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("control socket %s already exists and is not a unix socket", c.path)
	}

	conn, err := net.DialTimeout("unix", c.path, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("control socket %s already has a live listener", c.path)
	}
	if !confirmedStaleSocket(err) {
		return nil, fmt.Errorf("control socket: probing %s: %w", c.path, err)
	}
	return info, nil
}

// installStagedSocket only replaces the exact stale inode that preparePath
// inspected. Linking publishes the staged listener without overwriting a path
// installed by another process in the meantime.
func installStagedSocket(stagedPath, publicPath string, staleInfo os.FileInfo) error {
	if staleInfo != nil {
		quarantineDir, err := os.MkdirTemp(filepath.Dir(publicPath), ".cg-install-")
		if err != nil {
			return err
		}
		quarantine := filepath.Join(quarantineDir, "socket")
		if err := os.Rename(publicPath, quarantine); err != nil {
			_ = os.Remove(quarantineDir)
			if !os.IsNotExist(err) {
				return err
			}
		} else {
			currentInfo, statErr := os.Lstat(quarantine)
			if statErr != nil || !os.SameFile(currentInfo, staleInfo) {
				restoreErr := restorePathNoReplace(quarantine, publicPath)
				if restoreErr == nil {
					_ = os.Remove(quarantineDir)
				}
				if statErr != nil {
					return fmt.Errorf("checking quarantined stale socket: %w", statErr)
				}
				if restoreErr != nil {
					return fmt.Errorf("socket path changed after stale check; replacement preserved at %s: %w", quarantine, restoreErr)
				}
				return errors.New("socket path changed after stale check")
			}
			if err := os.Remove(quarantine); err != nil {
				return err
			}
			_ = os.Remove(quarantineDir)
		}
	}

	if err := os.Link(stagedPath, publicPath); err != nil {
		return err
	}
	if err := os.Remove(stagedPath); err != nil {
		_ = os.Remove(publicPath)
		return err
	}
	return nil
}

// restorePathNoReplace preserves a quarantined path without overwriting a
// newer occupant of its public name.
func restorePathNoReplace(quarantine, publicPath string) error {
	if err := os.Link(quarantine, publicPath); err != nil {
		return err
	}
	return os.Remove(quarantine)
}

func listenStaged(path string) (net.Listener, string, os.FileInfo, error) {
	dir := filepath.Dir(path)
	for range 8 {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return nil, "", nil, err
		}
		stagedPath := filepath.Join(dir, ".cg-"+hex.EncodeToString(random))
		ln, err := net.Listen("unix", stagedPath)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, "", nil, err
		}
		unixListener, ok := ln.(*net.UnixListener)
		if !ok {
			_ = ln.Close()
			_ = os.Remove(stagedPath)
			return nil, "", nil, errors.New("listener is not a unix listener")
		}
		unixListener.SetUnlinkOnClose(false)
		info, err := os.Lstat(stagedPath)
		if err != nil {
			_ = ln.Close()
			_ = os.Remove(stagedPath)
			return nil, "", nil, err
		}
		return ln, stagedPath, info, nil
	}
	return nil, "", nil, errors.New("could not allocate a staged socket pathname")
}

func confirmedStaleSocket(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENOTCONN)
}

func (c *ControlSocket) cleanup() {
	c.cleanupOnce.Do(func() {
		if c.socketInfo != nil {
			c.removeOwnedSocket()
		}
		c.releaseLock()
	})
}

// removeOwnedSocket first atomically moves the public pathname to an
// unpredictable quarantine name. It only unlinks that private name after
// verifying the moved inode; a replacement is restored instead.
func (c *ControlSocket) removeOwnedSocket() {
	// MkdirTemp atomically claims a mode-0700 quarantine directory on the
	// socket's filesystem. Once the public name is moved inside, other local
	// users cannot swap the private pathname between verification and unlink.
	quarantineDir, err := os.MkdirTemp(filepath.Dir(c.path), ".cg-clean-")
	if err != nil {
		return
	}
	quarantine := filepath.Join(quarantineDir, "socket")
	if err := os.Rename(c.path, quarantine); err != nil {
		_ = os.Remove(quarantineDir)
		return
	}
	info, err := os.Lstat(quarantine)
	if err == nil && os.SameFile(info, c.socketInfo) {
		_ = os.Remove(quarantine)
		_ = os.Remove(quarantineDir)
		return
	}
	// The public name was replaced after Start. Restore it only if no newer
	// pathname appeared while it was quarantined.
	if err := restorePathNoReplace(quarantine, c.path); err == nil {
		_ = os.Remove(quarantineDir)
	}
}

func (c *ControlSocket) releaseLock() {
	if c.lockFile == nil {
		return
	}
	_ = c.lockFile.Close()
	c.lockFile = nil
}

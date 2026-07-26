package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skflowne/code-graph-harness/internal/core"
)

// SocketPath derives the project-keyed control-socket path for cfg. Explicit
// paths are used verbatim; default paths live in a private per-user runtime
// directory rather than the shared temporary directory.
func SocketPath(cfg core.Config) string {
	if cfg.ControlSocket != "" {
		return cfg.ControlSocket
	}
	runtimeDir := controlRuntimeDir()
	if runtimeDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cfg.ProjectRoot))
	return filepath.Join(runtimeDir, fmt.Sprintf("cgraphd-%s.sock", hex.EncodeToString(sum[:])[:12]))
}

// ControlSocket is the Phase 0 scaffold for the Phase 1 staleness barrier: a
// unix-socket listener that accepts newline-delimited text commands and
// replies newline-delimited text. It understands exactly one command family
// today:
//
//	sync <file>   -> bumps the shared GenerationCounter, replies "ok generation=<n>\n"
//	<anything else> -> replies "err unknown\n"
//
// Phase 1 will replace/extend "sync" with real edit-observation logic and
// per-file staleness bookkeeping; the wire protocol and goroutine lifecycle
// established here are meant to carry forward unchanged.
type ControlSocket struct {
	path string
	gen  *core.GenerationCounter

	listener net.Listener
	lockFile *os.File
	// socketInfo identifies the inode created by net.Listen. It prevents a
	// later owner from being removed when this instance shuts down.
	socketInfo os.FileInfo
	authorize  func(net.Conn) error

	acceptWG     sync.WaitGroup
	handlers     sync.WaitGroup
	connMu       sync.Mutex
	connections  map[net.Conn]struct{}
	shuttingDown bool
	shutdownOnce sync.Once
	cleanupOnce  sync.Once
}

// NewControlSocket builds a ControlSocket bound to path, bumping/reading gen
// on commands. Call Start to begin listening.
func NewControlSocket(path string, gen *core.GenerationCounter) *ControlSocket {
	return &ControlSocket{
		path:        path,
		gen:         gen,
		authorize:   authorizeControlPeer,
		connections: make(map[net.Conn]struct{}),
	}
}

// Path returns the unix-socket path this ControlSocket listens (or will
// listen) on.
func (c *ControlSocket) Path() string { return c.path }

// Start acquires exclusive ownership of c.Path(), binds a new unix-socket
// listener, and begins accepting connections in a background goroutine. The
// ownership lock lives in a private runtime directory and is opened without
// following symlinks.
func (c *ControlSocket) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("control socket: nil context")
	}
	if err := ensureControlRuntimeDir(); err != nil {
		return fmt.Errorf("control socket: preparing private runtime directory: %w", err)
	}
	if err := validateUserOwnedDir(filepath.Dir(c.path)); err != nil {
		return fmt.Errorf("control socket: unsafe socket directory %s: %w", filepath.Dir(c.path), err)
	}

	lockPath := ownershipLockPath(c.path)
	lockFile, err := openOwnershipLock(lockPath)
	if err != nil {
		return fmt.Errorf("control socket: opening ownership lock %s: %w", lockPath, err)
	}
	if err := tryLockFile(lockFile); err != nil {
		_ = lockFile.Close()
		if isLockBusy(err) {
			return fmt.Errorf("control socket %s is already owned by another daemon", c.path)
		}
		return fmt.Errorf("control socket: locking %s: %w", lockPath, err)
	}
	c.lockFile = lockFile

	staleInfo, err := c.preparePath()
	if err != nil {
		c.releaseLock()
		return err
	}

	ln, stagedPath, stagedInfo, err := listenStaged(c.path)
	if err != nil {
		c.releaseLock()
		return fmt.Errorf("control socket: listen %s: %w", c.path, err)
	}
	// The staged socket is never accepted until it has restrictive mode and
	// has been published without overwriting a concurrently changed pathname.
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(stagedPath)
		c.releaseLock()
		return fmt.Errorf("control socket: chmod %s: %w", stagedPath, err)
	}
	if err := installStagedSocket(stagedPath, c.path, staleInfo); err != nil {
		_ = ln.Close()
		_ = os.Remove(stagedPath)
		c.releaseLock()
		return fmt.Errorf("control socket: installing %s: %w", c.path, err)
	}

	c.listener = ln
	c.socketInfo = stagedInfo
	c.acceptWG.Add(1)
	go c.acceptLoop()
	go func() {
		<-ctx.Done()
		c.beginShutdown()
	}()
	return nil
}

func controlRuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, "cgraphd")
	}
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		// A user-owned cache parent is a safer fallback than a predictable name
		// directly beneath the shared temporary directory.
		return filepath.Join(cache, "cgraphd-runtime")
	}
	return ""
}

func ensureControlRuntimeDir() error {
	dir := controlRuntimeDir()
	if dir == "" {
		return errors.New("no private runtime base: set XDG_RUNTIME_DIR or HOME")
	}
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" && filepath.IsAbs(base) {
		if err := validatePrivateDir(base); err != nil {
			return fmt.Errorf("unsafe XDG_RUNTIME_DIR %s: %w", base, err)
		}
	} else if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return fmt.Errorf("creating user cache directory: %w", err)
		}
		if err := validateUserOwnedDir(cache); err != nil {
			return fmt.Errorf("unsafe user cache directory %s: %w", cache, err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return validatePrivateDir(dir)
}

func validateUserOwnedDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("permissions %04o allow group or other writes", info.Mode().Perm())
	}
	if !ownedByEffectiveUser(info) {
		return errors.New("not owned by the effective user")
	}
	return nil
}

func validatePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("permissions %04o allow group or other access", info.Mode().Perm())
	}
	if !ownedByEffectiveUser(info) {
		return errors.New("not owned by the effective user")
	}
	return nil
}

func ownershipLockPath(socketPath string) string {
	absolute, err := filepath.Abs(socketPath)
	if err != nil {
		absolute = socketPath
	}
	sum := sha256.Sum256([]byte(absolute))
	return filepath.Join(controlRuntimeDir(), fmt.Sprintf("lock-%s", hex.EncodeToString(sum[:])[:24]))
}

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

// Wait blocks until the accept loop and all in-flight connections have
// finished. Call it after cancelling the context passed to Start.
func (c *ControlSocket) Wait() {
	c.acceptWG.Wait()
	c.handlers.Wait()
	c.cleanup()
}

func (c *ControlSocket) beginShutdown() {
	c.shutdownOnce.Do(func() {
		// Close the listener first. This unblocks Accept before the state lock
		// is changed, so an accepted connection can be classified atomically.
		if c.listener != nil {
			_ = c.listener.Close()
		}
		c.connMu.Lock()
		c.shuttingDown = true
		for conn := range c.connections {
			_ = conn.Close()
		}
		c.connMu.Unlock()
	})
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

func (c *ControlSocket) acceptLoop() {
	defer c.acceptWG.Done()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		if err := c.authorize(conn); err != nil {
			_ = conn.Close()
			continue
		}

		c.connMu.Lock()
		if c.shuttingDown {
			c.connMu.Unlock()
			_ = conn.Close()
			return
		}
		c.connections[conn] = struct{}{}
		c.handlers.Add(1)
		c.connMu.Unlock()

		go func() {
			defer c.handlers.Done()
			defer func() {
				c.connMu.Lock()
				delete(c.connections, conn)
				c.connMu.Unlock()
			}()
			c.handleConn(conn)
		}()
	}
}

func (c *ControlSocket) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, err := conn.Write([]byte(c.handleCommand(line))); err != nil {
			return
		}
	}
}

func (c *ControlSocket) handleCommand(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "err unknown\n"
	}
	switch fields[0] {
	case "sync":
		n := c.gen.Bump()
		return fmt.Sprintf("ok generation=%d\n", n)
	default:
		return "err unknown\n"
	}
}

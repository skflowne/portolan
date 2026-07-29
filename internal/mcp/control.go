package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/skflowne/portolan/internal/core"
)

// ControlSocket accepts newline-delimited control commands over a Unix socket
// and sends newline-delimited responses. Its wire contract is:
//
//	sync <file>   -> bumps the shared GenerationCounter, replies "ok generation=<n>\n"
//	<anything else> -> replies "err unknown\n"
type ControlSocket struct {
	path string
	gen  *core.GenerationCounter

	listener net.Listener
	lockFile *os.File
	// socketInfo identifies the inode created by net.Listen. It prevents a
	// replacement socket owner from being removed when this instance shuts down.
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

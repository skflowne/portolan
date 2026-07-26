package mcp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skflowne/code-graph-harness/internal/core"
)

func TestControlSocket_DefaultPathUsesPrivateRuntimeDirectory(t *testing.T) {
	runtimeBase := t.TempDir()
	if err := os.Chmod(runtimeBase, 0o700); err != nil {
		t.Fatalf("chmod runtime base: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	path := SocketPath(core.Config{ProjectRoot: "/repo/private-runtime"})
	wantDir := filepath.Join(runtimeBase, "cgraphd")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("default socket directory = %q, want %q", filepath.Dir(path), wantDir)
	}

	cs := NewControlSocket(path, &core.GenerationCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("socket permissions = %04o, want 0600", got)
	}
	dirInfo, err := os.Lstat(wantDir)
	if err != nil {
		t.Fatalf("stat runtime directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("runtime directory permissions = %04o, want 0700", got)
	}
	cancel()
	cs.Wait()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path remains after shutdown: %v", err)
	}
}

func TestControlSocket_RejectsSymlinkOwnershipLock(t *testing.T) {
	runtimeBase := t.TempDir()
	if err := os.Chmod(runtimeBase, 0o700); err != nil {
		t.Fatalf("chmod runtime base: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	if err := os.Mkdir(filepath.Join(runtimeBase, "cgraphd"), 0o700); err != nil {
		t.Fatalf("mkdir runtime directory: %v", err)
	}

	sockPath := filepath.Join(t.TempDir(), "control.sock")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, ownershipLockPath(sockPath)); err != nil {
		t.Fatalf("seed lock symlink: %v", err)
	}

	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
	if err := cs.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a symlink ownership lock")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("symlink target changed: %q", contents)
	}
}

func TestControlSocket_RejectsPrecreatedRuntimeDirectorySymlink(t *testing.T) {
	runtimeBase := t.TempDir()
	if err := os.Chmod(runtimeBase, 0o700); err != nil {
		t.Fatalf("chmod runtime base: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(runtimeBase, "cgraphd")); err != nil {
		t.Fatalf("seed runtime symlink: %v", err)
	}

	cs := NewControlSocket(filepath.Join(t.TempDir(), "control.sock"), &core.GenerationCounter{})
	err := cs.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private runtime directory") {
		t.Fatalf("expected unsafe runtime directory error, got %v", err)
	}
}

func TestControlSocket_RejectsMissingPrivateRuntimeBase(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	if err := ensureControlRuntimeDir(); err == nil {
		t.Fatal("expected missing private runtime base to fail closed")
	}
}

func TestControlSocket_RejectsPublicSocketDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod socket directory: %v", err)
	}
	cs := NewControlSocket(filepath.Join(dir, "control.sock"), &core.GenerationCounter{})
	if err := cs.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "socket directory") {
		t.Fatalf("expected unsafe socket directory error, got %v", err)
	}
}

func TestInstallStagedSocketRejectsChangedStalePath(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "control.sock")
	stale, err := net.Listen("unix", publicPath)
	if err != nil {
		t.Fatalf("listen stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}
	staleInfo, err := os.Lstat(publicPath)
	if err != nil {
		t.Fatalf("stat stale socket: %v", err)
	}

	if err := os.Remove(publicPath); err != nil {
		t.Fatalf("remove stale socket: %v", err)
	}
	if err := os.WriteFile(publicPath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	staged, stagedPath, _, err := listenStaged(publicPath)
	if err != nil {
		t.Fatalf("listen staged socket: %v", err)
	}
	defer staged.Close()
	defer os.Remove(stagedPath)

	if err := installStagedSocket(stagedPath, publicPath, staleInfo); err == nil {
		t.Fatal("install replaced a pathname changed after stale validation")
	}
	contents, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(contents) != "replacement" {
		t.Fatalf("replacement changed: %q", contents)
	}
}

func TestRestorePathNoReplacePreservesNewerPath(t *testing.T) {
	dir := t.TempDir()
	quarantine := filepath.Join(dir, "quarantine")
	publicPath := filepath.Join(dir, "control.sock")
	if err := os.WriteFile(quarantine, []byte("older"), 0o600); err != nil {
		t.Fatalf("write quarantined path: %v", err)
	}
	if err := os.WriteFile(publicPath, []byte("newer"), 0o600); err != nil {
		t.Fatalf("write newer path: %v", err)
	}

	if err := restorePathNoReplace(quarantine, publicPath); err == nil {
		t.Fatal("restore overwrote a newer pathname")
	}
	contents, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatalf("read newer path: %v", err)
	}
	if string(contents) != "newer" {
		t.Fatalf("newer path changed: %q", contents)
	}
	if _, err := os.Lstat(quarantine); err != nil {
		t.Fatalf("quarantined path was not preserved: %v", err)
	}
}

func TestControlSocket_UnauthorizedPeerCannotSync(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "control.sock")
	gen := &core.GenerationCounter{}
	cs := NewControlSocket(sockPath, gen)
	cs.authorize = func(net.Conn) error { return errors.New("denied by test") }
	ctx, cancel := context.WithCancel(context.Background())
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		cancel()
		cs.Wait()
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("sync forbidden.ts\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if reply, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
		t.Fatalf("unauthorized peer received reply %q", reply)
	}
	if got := gen.Current().Generation; got != 0 {
		t.Fatalf("unauthorized sync bumped generation to %d", got)
	}
}

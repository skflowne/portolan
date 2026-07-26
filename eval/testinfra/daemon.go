// Package testinfra provides the shared real-daemon harness used by eval tests.
package testinfra

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	PollInterval = 10 * time.Millisecond
	ShortWait    = 5 * time.Second
)

// ModuleRoot returns the repository root containing go.mod.
func ModuleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// FixtureRoot returns the pinned TypeScript fixture used by the eval suites.
func FixtureRoot() string {
	return filepath.Join(ModuleRoot(), "eval", "tiera", "fixtures")
}

// BuildDaemon builds cgraphd once for a package's TestMain.
func BuildDaemon() (string, func(), error) {
	if runtime.GOOS == "windows" {
		return "", func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "cgraphd-eval-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	bin := filepath.Join(tmp, "cgraphd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/cgraphd")
	build.Dir = ModuleRoot()
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("building cgraphd: %w", err)
	}
	return bin, cleanup, nil
}

// Config describes one real daemon process.
type Config struct {
	Binary        string
	ProjectRoot   string
	Telemetry     string
	SessionID     string
	ControlSocket string
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Daemon owns one cgraphd process and its wrapped tsgo child.
type Daemon struct {
	Cmd     *exec.Cmd
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser
	PIDFile string

	stderr   *lockedBuffer
	wait     chan struct{}
	mu       sync.Mutex
	exitErr  error
	childPID int
	cleaned  bool
}

// NewDaemon configures and starts a daemon with a PID-recording tsgo wrapper.
func NewDaemon(t *testing.T, cfg Config) *Daemon {
	t.Helper()
	RequireSupport(t)
	realTsgo, err := exec.LookPath("tsgo")
	if err != nil {
		t.Skip("tsgo not on PATH")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "tsgo.pid")
	wrapper := filepath.Join(dir, "tsgo-wrapper.sh")
	script := "#!/bin/sh\necho $$ > \"$CGRAPH_TSGO_PID_FILE\"\nexec \"$CGRAPH_REAL_TSGO\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("writing tsgo wrapper: %v", err)
	}
	if cfg.ProjectRoot == "" {
		cfg.ProjectRoot = FixtureRoot()
	}
	if cfg.Telemetry == "" {
		cfg.Telemetry = filepath.Join(dir, "telemetry.jsonl")
	}
	if cfg.SessionID == "" {
		cfg.SessionID = "eval"
	}
	args := []string{
		"--project-root", cfg.ProjectRoot,
		"--jsonl", cfg.Telemetry,
		"--session-id", cfg.SessionID,
		"--graph-mode", "graph",
		"--tsgo", wrapper,
	}
	if cfg.ControlSocket != "" {
		args = append(args, "--control-socket", cfg.ControlSocket)
	}
	stderr := &lockedBuffer{}
	cmd := exec.Command(cfg.Binary, args...)
	cmd.Env = append(os.Environ(),
		"CGRAPH_REAL_TSGO="+realTsgo,
		"CGRAPH_TSGO_PID_FILE="+pidFile,
	)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("opening daemon stdout: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening daemon stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}
	d := &Daemon{Cmd: cmd, Stdin: stdin, Stdout: stdout, PIDFile: pidFile, stderr: stderr, wait: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		d.mu.Lock()
		d.exitErr = err
		d.mu.Unlock()
		close(d.wait)
	}()
	t.Cleanup(func() { d.Cleanup(t) })
	return d
}

// RequireSupport skips daemon evals on platforms where the Unix control socket is unavailable.
func RequireSupport(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("daemon eval tests require Unix sockets and signals")
	}
	probe := filepath.Join(t.TempDir(), "control-support.sock")
	ln, err := net.Listen("unix", probe)
	if err != nil {
		t.Skipf("Unix sockets unavailable: %v", err)
	}
	_ = ln.Close()
	_ = os.Remove(probe)
}

// Stderr returns all daemon diagnostics captured so far.
func (d *Daemon) Stderr() string { return d.stderr.String() }

// ExitError returns the result from Cmd.Wait after the process exits.
func (d *Daemon) ExitError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exitErr
}

// WaitForExit waits a bounded amount of time for the daemon.
func (d *Daemon) WaitForExit(timeout time.Duration) (error, bool) {
	select {
	case <-d.wait:
		return d.ExitError(), true
	case <-time.After(timeout):
		return nil, false
	}
}

// WaitForPID waits for and parses the wrapped tsgo child's PID.
func (d *Daemon) WaitForPID(t *testing.T) int {
	t.Helper()
	if err := d.WaitForFile(d.PIDFile, ShortWait); err != nil {
		t.Fatalf("waiting for tsgo PID: %v (stderr=%s)", err, d.Stderr())
	}
	data, err := os.ReadFile(d.PIDFile)
	if err != nil {
		t.Fatalf("reading PID file %s: %v", d.PIDFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid PID file %s: %q", d.PIDFile, data)
	}
	d.mu.Lock()
	d.childPID = pid
	d.mu.Unlock()
	return pid
}

// WaitForFile polls for a path while also detecting early daemon exit.
func (d *Daemon) WaitForFile(path string, timeout time.Duration) error {
	return Poll(timeout, func() (bool, error) {
		if _, err := os.Stat(path); err == nil {
			return true, nil
		}
		select {
		case <-d.wait:
			return false, fmt.Errorf("daemon exited: %v", d.ExitError())
		default:
			return false, nil
		}
	})
}

// WaitForSocket polls until the daemon accepts a control connection.
func (d *Daemon) WaitForSocket(path string, timeout time.Duration) error {
	return Poll(timeout, func() (bool, error) {
		select {
		case <-d.wait:
			return false, fmt.Errorf("daemon exited: %v", d.ExitError())
		default:
		}
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true, nil
		}
		return false, nil
	})
}

// AcceptedIdleConnection proves the daemon accepted this exact client before
// returning it for shutdown assertions.
func AcceptedIdleConnection(t *testing.T, socket string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, ShortWait)
	if err != nil {
		t.Fatalf("connecting idle control client: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(ShortWait))
	if _, err := fmt.Fprintln(conn, "unknown command"); err != nil {
		_ = conn.Close()
		t.Fatalf("proving idle control client acceptance: %v", err)
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("reading idle control response: %v", err)
	}
	if response != "err unknown\n" {
		_ = conn.Close()
		t.Fatalf("idle control response = %q", response)
	}
	return conn
}

// AssertConnectionClosed accepts orderly EOF and platform-specific reset errors.
func AssertConnectionClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(ShortWait))
	var one [1]byte
	n, err := conn.Read(one[:])
	if n != 0 || (!errors.Is(err, io.EOF) && !IsClosedConnError(err)) {
		t.Errorf("idle control connection was not closed: n=%d err=%v", n, err)
	}
}

// Poll evaluates condition until it succeeds or timeout elapses.
func Poll(timeout time.Duration, condition func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := condition()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("deadline exceeded")
		}
		wait := min(PollInterval, time.Until(deadline))
		timer := time.NewTimer(wait)
		<-timer.C
	}
}

// AssertPIDGone verifies that a recorded child process has stopped.
func AssertPIDGone(t *testing.T, pid int) {
	t.Helper()
	if err := Poll(ShortWait, func() (bool, error) { return !pidExists(pid), nil }); err != nil {
		t.Errorf("process PID %d did not disappear: %v", pid, err)
	}
}

// Cleanup closes stdin, waits for the daemon, and kills leaked processes as a fallback.
func (d *Daemon) Cleanup(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	if d.cleaned {
		d.mu.Unlock()
		return
	}
	d.cleaned = true
	pid := d.childPID
	d.mu.Unlock()
	if d.Stdin != nil {
		_ = d.Stdin.Close()
	}
	if _, ok := d.WaitForExit(ShortWait); !ok {
		t.Errorf("daemon did not exit during cleanup; forcing termination")
		if d.Cmd.Process != nil {
			_ = d.Cmd.Process.Kill()
		}
		if pid != 0 {
			_ = killProcess(pid)
		}
		_, _ = d.WaitForExit(ShortWait)
	}
	if pid != 0 {
		AssertPIDGone(t, pid)
	}
}

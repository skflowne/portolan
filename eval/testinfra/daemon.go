// Package testinfra provides the shared real-daemon harness used by eval tests.
package testinfra

import (
	"bufio"
	"context"
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

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// BuildDaemon builds portoland once for a package's TestMain.
func BuildDaemon() (string, func(), error) {
	if runtime.GOOS == "windows" {
		return "", func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "portoland-eval-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	bin := filepath.Join(tmp, "portoland")
	build := exec.Command("go", "build", "-o", bin, "./cmd/portoland")
	build.Dir = ModuleRoot()
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("building portoland: %w", err)
	}
	return bin, cleanup, nil
}

// Config describes one real daemon process.
type Config struct {
	Binary        string
	ProjectRoot   string
	Telemetry     string
	SessionID     string
	GraphMode     string
	MaxResults    int
	ControlSocket string
	Env           map[string]string
}

func controlledDaemonEnv(overrides map[string]string, omitted ...string) []string {
	controlled := make(map[string]struct{}, len(overrides)+len(omitted))
	for key := range overrides {
		controlled[key] = struct{}{}
	}
	for _, key := range omitted {
		controlled[key] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		_, overridden := controlled[key]
		if overridden || strings.HasPrefix(key, "OTEL_EXPORTER_OTLP_") || strings.HasPrefix(key, "OTEL_BSP_") || key == "OTEL_SDK_DISABLED" {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

// Daemon owns one portoland process and its wrapped tsgo child.
type Daemon struct {
	Cmd     *exec.Cmd
	Stdin   io.WriteCloser
	Stdout  io.ReadCloser
	PIDFile string

	stderr     *lockedBuffer
	stdout     *lockedBuffer
	stdoutDone chan struct{}
	wait       chan struct{}
	mu         sync.Mutex
	exitErr    error
	childPID   int
	cleaned    bool
}

// NewDaemon configures and starts a daemon with a PID-recording tsgo wrapper.
func NewDaemon(t *testing.T, cfg Config) *Daemon {
	t.Helper()
	RequireSupport(t)
	realTsgo, err := resolveTsgo()
	if err != nil {
		if os.Getenv(RequireTsgoEnv) == "1" {
			t.Fatalf("required analyzer unavailable: %v", err)
		}
		t.Skipf("compatible analyzer unavailable: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "tsgo.pid")
	wrapper := filepath.Join(dir, "tsgo-wrapper.sh")
	script := "#!/bin/sh\necho $$ > \"$PORTOLAN_TSGO_PID_FILE\"\nexec \"$PORTOLAN_REAL_TSGO\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("writing tsgo wrapper: %v", err)
	}
	if cfg.ProjectRoot == "" {
		cfg.ProjectRoot = FixtureRoot()
	}
	if cfg.Telemetry == "" {
		cfg.Telemetry = filepath.Join(dir, "telemetry.jsonl")
	}
	args := []string{
		"--project-root", cfg.ProjectRoot,
		"--jsonl", cfg.Telemetry,
		"--session-id", cfg.SessionID,
		"--graph-mode", cfg.GraphMode,
		"--tsgo", wrapper,
	}
	if cfg.MaxResults > 0 {
		args = append(args, "--max-results", strconv.Itoa(cfg.MaxResults))
	}
	if cfg.ControlSocket != "" {
		args = append(args, "--control-socket", cfg.ControlSocket)
	}
	stderr := &lockedBuffer{}
	cmd := exec.Command(cfg.Binary, args...)
	envOverrides := make(map[string]string, len(cfg.Env)+2)
	for key, value := range cfg.Env {
		envOverrides[key] = value
	}
	envOverrides["PORTOLAN_REAL_TSGO"] = realTsgo
	envOverrides["PORTOLAN_TSGO_PID_FILE"] = pidFile
	cmd.Env = controlledDaemonEnv(envOverrides)
	cmd.Stderr = stderr
	stdoutSource, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening daemon stdout pipe: %v", err)
	}
	cmd.Stdout = stdoutWriter
	stdoutCapture := &lockedBuffer{}
	stdout, stdoutForward := io.Pipe()
	stdoutDone := make(chan struct{})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening daemon stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdoutSource.Close()
		_ = stdoutWriter.Close()
		t.Fatalf("starting daemon: %v", err)
	}
	_ = stdoutWriter.Close()
	go captureProcessStdout(stdoutSource, stdoutForward, stdoutCapture, stdoutDone)
	d := &Daemon{Cmd: cmd, Stdin: stdin, Stdout: stdout, PIDFile: pidFile, stderr: stderr, stdout: stdoutCapture, stdoutDone: stdoutDone, wait: make(chan struct{})}
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

// ConnectMCP initializes an MCP client session over the daemon's stdio pipes.
func ConnectMCP(t *testing.T, d *Daemon, name string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.IOTransport{Reader: d.Stdout, Writer: d.Stdin}, nil)
	if err != nil {
		t.Fatalf("connecting to daemon: %v (stderr=%s)", err, d.Stderr())
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
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
	if !d.FinishStdout(ShortWait) {
		t.Errorf("daemon stdout capture did not reach EOF")
	}
	if pid != 0 {
		AssertPIDGone(t, pid)
	}
}

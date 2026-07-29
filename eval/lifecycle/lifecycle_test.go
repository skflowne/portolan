// Package lifecycle exercises the real daemon's process and control-socket
// lifecycle with the real tsgo language server.
package lifecycle

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skflowne/portolan/eval/testinfra"
)

var daemonBin string

func TestMain(m *testing.M) {
	var cleanup func()
	var err error
	daemonBin, cleanup, err = testinfra.BuildDaemon()
	if err != nil {
		panic("lifecycle: " + err.Error())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func newDaemon(t *testing.T, socket string) *testinfra.Daemon {
	t.Helper()
	return testinfra.NewDaemon(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		SessionID:     "lifecycle",
		GraphMode:     "graph",
		ControlSocket: socket,
	})
}

func startDaemon(t *testing.T, socket string) (*testinfra.Daemon, int) {
	t.Helper()
	d := newDaemon(t, socket)
	if err := d.WaitForSocket(socket, testinfra.ShortWait); err != nil {
		t.Fatalf("waiting for daemon readiness: %v (stderr=%s)", err, d.Stderr())
	}
	return d, d.WaitForPID(t)
}

func startConnectedDaemon(t *testing.T, socket string) (*testinfra.Daemon, *mcp.ClientSession, int) {
	t.Helper()
	d, childPID := startDaemon(t, socket)
	return d, testinfra.ConnectMCP(t, d, "lifecycle"), childPID
}

func assertPathGone(t *testing.T, path string) {
	t.Helper()
	if err := testinfra.Poll(testinfra.ShortWait, func() (bool, error) {
		_, err := os.Lstat(path)
		return os.IsNotExist(err), nil
	}); err != nil {
		t.Errorf("path %s did not disappear: %v", path, err)
	}
}

func controlCommand(t *testing.T, socket, command string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, testinfra.ShortWait)
	if err != nil {
		t.Fatalf("dialing control socket: %v", err)
	}
	defer conn.Close()
	return commandOnConn(t, conn, command)
}

func commandOnConn(t *testing.T, conn net.Conn, command string) string {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(testinfra.ShortWait))
	if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
		t.Fatalf("writing control command: %v", err)
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading control response: %v", err)
	}
	return response
}

func shutdownViaStdin(t *testing.T, d *testinfra.Daemon) {
	t.Helper()
	started := time.Now()
	if err := d.Stdin.Close(); err != nil {
		t.Fatalf("closing daemon stdin: %v", err)
	}
	requireSuccessfulExit(t, d, "stdin disconnect", started)
}

func requireSuccessfulExit(t *testing.T, d *testinfra.Daemon, trigger string, started time.Time) {
	t.Helper()
	err, ok := d.WaitForExit(testinfra.ShortWait)
	if !ok {
		t.Fatalf("daemon did not exit after %s", trigger)
	}
	if err != nil {
		t.Fatalf("daemon exited unsuccessfully after %s: %v (stderr=%s)", trigger, err, d.Stderr())
	}
	if elapsed := time.Since(started); elapsed > testinfra.ShortWait {
		t.Fatalf("%s shutdown took too long: %v", trigger, elapsed)
	}
}

func TestDaemonRejectsInvalidTelemetryDimensionsBeforeRuntimeConstruction(t *testing.T) {
	testinfra.RequireSupport(t)
	tests := []struct {
		name      string
		dimension []string
		wantError string
	}{
		{name: "graph mode", dimension: []string{"--session-id", "lifecycle", "--graph-mode", "invalid"}, wantError: "--graph-mode"},
		{name: "session ID", dimension: []string{"--session-id=", "--graph-mode", "graph"}, wantError: "--session-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "tsgo-started")
			wrapper := filepath.Join(dir, "tsgo-wrapper.sh")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch \"$PORTOLAN_START_MARKER\"\nexit 1\n"), 0o755); err != nil {
				t.Fatalf("writing tsgo marker wrapper: %v", err)
			}
			telemetryPath := filepath.Join(dir, "telemetry.jsonl")
			socketPath := filepath.Join(dir, "control.sock")
			args := []string{
				"--project-root", testinfra.FixtureRoot(),
				"--jsonl", telemetryPath,
				"--control-socket", socketPath,
				"--tsgo", wrapper,
			}
			args = append(args, tt.dimension...)
			cmd := exec.Command(daemonBin, args...)
			cmd.Env = append(environmentWithoutTelemetryDimensions(), "PORTOLAN_START_MARKER="+marker)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("daemon accepted invalid telemetry dimensions: %s", output)
			}
			if !strings.Contains(string(output), tt.wantError) {
				t.Fatalf("startup error %q does not identify %s", output, tt.wantError)
			}
			for _, path := range []string{marker, telemetryPath, socketPath} {
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Errorf("runtime artifact %s exists after configuration rejection: %v", path, statErr)
				}
			}
		})
	}
}

func environmentWithoutTelemetryDimensions() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "PORTOLAN_SESSION_ID" && key != "PORTOLAN_GRAPH_MODE" {
			env = append(env, entry)
		}
	}
	return env
}

func TestMCPStdinDisconnectShutsDownEverything(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	d, sess, childPID := startConnectedDaemon(t, socket)
	if got := controlCommand(t, socket, "sync file.ts"); got != "ok generation=1\n" {
		t.Fatalf("sync response = %q", got)
	}
	if got := controlCommand(t, socket, "unknown command"); got != "err unknown\n" {
		t.Fatalf("unknown response = %q", got)
	}
	idle := testinfra.AcceptedIdleConnection(t, socket)
	defer idle.Close()

	started := time.Now()
	if err := sess.Close(); err != nil {
		t.Fatalf("closing MCP stdin: %v", err)
	}
	requireSuccessfulExit(t, d, "MCP disconnect", started)
	testinfra.AssertPIDGone(t, childPID)
	assertPathGone(t, socket)
	testinfra.AssertConnectionClosed(t, idle)
}

func TestSIGTERMWithIdleControlClient(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	d, _, childPID := startConnectedDaemon(t, socket)
	idle := testinfra.AcceptedIdleConnection(t, socket)
	defer idle.Close()

	started := time.Now()
	if err := testinfra.Terminate(d.Cmd.Process); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	requireSuccessfulExit(t, d, "SIGTERM", started)
	testinfra.AssertConnectionClosed(t, idle)
	testinfra.AssertPIDGone(t, childPID)
	assertPathGone(t, socket)
}

func TestDuplicateLiveSocketStartupCannotStealOwnership(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	first, firstSession, _ := startConnectedDaemon(t, socket)
	originalInfo, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("stat original socket: %v", err)
	}

	second := newDaemon(t, socket)
	secondPID := second.WaitForPID(t)
	secondClient := mcp.NewClient(&mcp.Implementation{Name: "duplicate", Version: "0.0.1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, connectErr := secondClient.Connect(ctx, &mcp.IOTransport{Reader: second.Stdout, Writer: second.Stdin}, nil)
	cancel()
	if connectErr == nil {
		t.Fatal("duplicate daemon unexpectedly connected")
	}
	exitErr, ok := second.WaitForExit(testinfra.ShortWait)
	if !ok {
		t.Fatalf("duplicate daemon did not exit promptly")
	}
	if exitErr == nil {
		t.Fatalf("duplicate daemon exited successfully")
	}
	stderr := second.Stderr()
	if !strings.Contains(stderr, socket) || !strings.Contains(stderr, "already owned") {
		t.Fatalf("duplicate startup error did not identify socket ownership: %s", stderr)
	}
	testinfra.AssertPIDGone(t, secondPID)

	info, err := os.Lstat(socket)
	if err != nil || !os.SameFile(originalInfo, info) {
		t.Fatalf("original socket identity changed: before=%v after=%v err=%v", originalInfo, info, err)
	}
	if got := controlCommand(t, socket, "sync file.ts"); got != "ok generation=1\n" {
		t.Fatalf("original sync response = %q", got)
	}
	if got := controlCommand(t, socket, "not-a-command"); got != "err unknown\n" {
		t.Fatalf("original unknown response = %q", got)
	}
	if _, ok := first.WaitForExit(50 * time.Millisecond); ok {
		t.Fatalf("original daemon exited while duplicate started: %v", first.ExitError())
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, listErr := firstSession.ListTools(listCtx, nil)
	listCancel()
	if listErr != nil {
		t.Fatalf("original daemon was disrupted: %v", listErr)
	}
	if len(result.Tools) != 3 {
		t.Fatalf("original daemon was disrupted: tools=%d", len(result.Tools))
	}
	shutdownViaStdin(t, first)
	assertPathGone(t, socket)
}

func TestCleanupCannotRemoveReplacementSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	d, _ := startDaemon(t, socket)
	if err := os.Remove(socket); err != nil {
		t.Fatalf("unlinking daemon socket: %v", err)
	}
	replacement, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("binding replacement socket: %v", err)
	}
	defer replacement.Close()
	replacementInfo, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("stat replacement socket: %v", err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := replacement.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	shutdownViaStdin(t, d)
	info, err := os.Lstat(socket)
	if err != nil || !os.SameFile(replacementInfo, info) {
		t.Fatalf("daemon cleanup removed or changed replacement socket: %v", err)
	}
	conn, err := net.DialTimeout("unix", socket, testinfra.ShortWait)
	if err != nil {
		t.Fatalf("dialing replacement socket: %v", err)
	}
	defer conn.Close()
	select {
	case acceptedConn := <-accepted:
		if acceptedConn == nil {
			t.Fatalf("replacement listener did not receive connection")
		}
		_ = acceptedConn.Close()
	case <-time.After(testinfra.ShortWait):
		t.Fatalf("replacement listener did not receive connection")
	}
}

func TestStaleSocketIsReclaimedSafely(t *testing.T) {
	testinfra.RequireSupport(t)
	socket := filepath.Join(t.TempDir(), "control.sock")
	createStaleSocket(t, socket)
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("stale socket pathname was unexpectedly removed: %v", err)
	}

	d, _ := startDaemon(t, socket)
	if got := controlCommand(t, socket, "sync file.ts"); got != "ok generation=1\n" {
		t.Fatalf("sync response = %q", got)
	}
	shutdownViaStdin(t, d)
	assertPathGone(t, socket)
}

func TestNonSocketPathsAreNeverTreatedAsStale(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T, path string) func(t *testing.T)
	}{
		{
			name: "regular file",
			make: func(t *testing.T, path string) func(t *testing.T) {
				content := []byte("do not remove")
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatalf("creating regular file: %v", err)
				}
				return func(t *testing.T) {
					got, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(got, content) {
						t.Errorf("regular file changed: %v", err)
					}
				}
			},
		},
		{
			name: "symlink",
			make: func(t *testing.T, path string) func(t *testing.T) {
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
					t.Fatalf("creating symlink target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("creating symlink: %v", err)
				}
				return func(t *testing.T) {
					got, err := os.Readlink(path)
					if err != nil || got != target {
						t.Errorf("symlink changed: got=%q err=%v", got, err)
					}
				}
			},
		},
		{
			name: "non-empty directory",
			make: func(t *testing.T, path string) func(t *testing.T) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("creating directory: %v", err)
				}
				child := filepath.Join(path, "keep")
				if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
					t.Fatalf("creating directory child: %v", err)
				}
				return func(t *testing.T) {
					if _, err := os.Stat(child); err != nil {
						t.Errorf("directory contents changed: %v", err)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "control.sock")
			verify := tc.make(t, socket)
			d := newDaemon(t, socket)
			childPID := d.WaitForPID(t)
			exitErr, ok := d.WaitForExit(testinfra.ShortWait)
			if !ok {
				t.Fatalf("daemon did not reject non-socket path promptly")
			}
			if exitErr == nil {
				t.Fatalf("daemon accepted non-socket path")
			}
			if !strings.Contains(d.Stderr(), socket) || !strings.Contains(d.Stderr(), "not a unix socket") {
				t.Fatalf("startup error was not clear: %s", d.Stderr())
			}
			verify(t)
			testinfra.AssertPIDGone(t, childPID)
		})
	}
}

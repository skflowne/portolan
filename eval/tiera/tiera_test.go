// Package tiera is the Phase 0 Tier A gate: a retrieval-correctness harness
// that drives the real cgraphd daemon over MCP against a pinned TypeScript fixture.
package tiera

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skflowne/code-graph-harness/eval/testinfra"
	"github.com/skflowne/code-graph-harness/internal/tools"
)

var daemonBin string

func TestMain(m *testing.M) {
	var cleanup func()
	var err error
	daemonBin, cleanup, err = testinfra.BuildDaemon()
	if err != nil {
		panic("tiera: " + err.Error())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type daemonProcess struct {
	proc   *testinfra.Daemon
	sess   *mcp.ClientSession
	jsonl  string
	socket string
	pid    int
}

func startDaemon(t *testing.T, sessionID, socket string) *daemonProcess {
	t.Helper()
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "telemetry.jsonl")
	proc := testinfra.NewDaemon(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		Telemetry:     jsonl,
		SessionID:     sessionID,
		ControlSocket: socket,
	})
	client := mcp.NewClient(&mcp.Implementation{Name: sessionID, Version: "0.0.1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.IOTransport{Reader: proc.Stdout, Writer: proc.Stdin}, nil)
	if err != nil {
		t.Fatalf("connecting to daemon: %v (stderr=%s)", err, proc.Stderr())
	}
	d := &daemonProcess{proc: proc, sess: sess, jsonl: jsonl, socket: socket, pid: proc.WaitForPID(t)}
	t.Cleanup(func() { _ = sess.Close() })
	return d
}

func startLifecycleDaemon(t *testing.T) *daemonProcess {
	t.Helper()
	return startDaemon(t, "lifecycle", filepath.Join(t.TempDir(), "control.sock"))
}

func waitForCommand(t *testing.T, d *daemonProcess) {
	t.Helper()
	if _, ok := d.proc.WaitForExit(testinfra.ShortWait); !ok {
		t.Fatal("daemon did not exit promptly")
	}
}

func session(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	d := startDaemon(t, "tiera", filepath.Join(t.TempDir(), "control.sock"))
	return d.sess, d.jsonl
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	return testinfra.FixtureRoot()
}

// callInto calls a tool and decodes its structured output into out.
func callInto(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: call error: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool reported protocol error: %+v", name, res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshaling structured content: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: decoding output: %v (raw=%s)", name, err, raw)
	}
}

func TestTierA(t *testing.T) {
	sess, jsonl := session(t)
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")
	mainTS := filepath.Join(fixtureRoot(t), "src", "main.ts")

	// The daemon must advertise exactly the three Phase 0 tools.
	t.Run("tools_list", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lt, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		got := map[string]bool{}
		for _, tl := range lt.Tools {
			got[tl.Name] = true
		}
		for _, want := range []string{"find_definition", "find_references", "get_outline"} {
			if !got[want] {
				t.Errorf("tool %q not advertised (got %v)", want, keys(got))
			}
		}
	})

	// get_outline(geometry.ts) must surface the declared top-level symbols.
	t.Run("outline_geometry", func(t *testing.T) {
		var out tools.GetOutlineOutput
		callInto(t, sess, "get_outline", map[string]any{"file": geometry}, &out)
		if !out.Found {
			t.Fatalf("expected symbols, got none: %s", out.Message)
		}
		names := map[string]bool{}
		for _, s := range out.Symbols {
			names[s.Name] = true
		}
		for _, want := range []string{"Shape", "Circle", "Rectangle", "totalArea"} {
			if !names[want] {
				t.Errorf("outline missing %q (got %v)", want, keys(names))
			}
		}
		assertFresh(t, out.Freshness.Stale)
	})

	// find_references(geometry.ts, Circle) must include the cross-file uses in
	// main.ts — the core proof that reference resolution spans files.
	t.Run("references_cross_file", func(t *testing.T) {
		var out tools.FindReferencesOutput
		callInto(t, sess, "find_references", map[string]any{"file": geometry, "symbol": "Circle"}, &out)
		if !out.Found {
			t.Fatalf("expected references to Circle, got none: %s", out.Message)
		}
		var files []string
		crossFile := false
		for _, l := range out.Locations {
			files = append(files, l.File)
			if strings.HasSuffix(filepath.ToSlash(l.File), "src/main.ts") {
				crossFile = true
			}
		}
		if !crossFile {
			t.Errorf("expected a reference in src/main.ts; got files %v", files)
		}
		assertFresh(t, out.Freshness.Stale)
	})

	// find_definition(geometry.ts, totalArea) resolves the declaration to a
	// location back in geometry.ts.
	t.Run("definition_totalArea", func(t *testing.T) {
		var out tools.FindDefinitionOutput
		callInto(t, sess, "find_definition", map[string]any{"file": geometry, "symbol": "totalArea"}, &out)
		if !out.Found {
			t.Fatalf("expected a definition, got none: %s", out.Message)
		}
		if got := out.Locations[0].File; !strings.HasSuffix(filepath.ToSlash(got), "src/geometry.ts") {
			t.Errorf("expected definition in src/geometry.ts, got %s", got)
		}
		assertFresh(t, out.Freshness.Stale)
	})

	// get_outline(main.ts) surfaces the consumer's own declarations.
	t.Run("outline_main", func(t *testing.T) {
		var out tools.GetOutlineOutput
		callInto(t, sess, "get_outline", map[string]any{"file": mainTS}, &out)
		names := map[string]bool{}
		for _, s := range out.Symbols {
			names[s.Name] = true
		}
		for _, want := range []string{"shapes", "report"} {
			if !names[want] {
				t.Errorf("main.ts outline missing %q (got %v)", want, keys(names))
			}
		}
	})

	// Phase 0 exit criterion: every call is logged. After the calls above,
	// the JSONL stream must contain one line per tool invocation.
	t.Run("every_call_logged", func(t *testing.T) {
		// Close the session first so the daemon flushes and exits.
		_ = sess.Close()
		// Give the OS a moment; poll the file.
		var lines []map[string]any
		deadline := time.Now().Add(5 * time.Second)
		// 4 tool calls above (2 outlines + 1 refs + 1 def); ListTools is not a
		// tool call and emits no event. Break as soon as they've all landed.
		for time.Now().Before(deadline) {
			lines = readJSONL(t, jsonl)
			if len(lines) >= 4 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(lines) < 4 {
			t.Fatalf("expected >=4 telemetry events (one per tool call), got %d: %v", len(lines), lines)
		}
		seen := map[string]int{}
		for _, l := range lines {
			if tool, ok := l["tool"].(string); ok {
				seen[tool]++
			}
			if _, ok := l["ts"]; !ok {
				t.Errorf("telemetry event missing timestamp: %v", l)
			}
		}
		for _, want := range []string{"get_outline", "find_references", "find_definition"} {
			if seen[want] == 0 {
				t.Errorf("no telemetry event for tool %q (saw %v)", want, seen)
			}
		}
	})
}

func TestDaemonStdinEOFStopsProvider(t *testing.T) {
	d := startLifecycleDaemon(t)
	started := time.Now()
	if err := d.sess.Close(); err != nil {
		t.Fatalf("closing MCP stdin: %v", err)
	}
	waitForCommand(t, d)
	if elapsed := time.Since(started); elapsed > testinfra.ShortWait {
		t.Fatalf("MCP disconnect took too long: %v", elapsed)
	}
	testinfra.AssertPIDGone(t, d.pid)
}

func TestDaemonSIGTERMStopsWithIdleControlClient(t *testing.T) {
	d := startLifecycleDaemon(t)
	conn := testinfra.AcceptedIdleConnection(t, d.socket)
	defer conn.Close()

	started := time.Now()
	if err := testinfra.Terminate(d.proc.Cmd.Process); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	waitForCommand(t, d)
	if elapsed := time.Since(started); elapsed > testinfra.ShortWait {
		t.Fatalf("SIGTERM shutdown took too long: %v", elapsed)
	}
	testinfra.AssertConnectionClosed(t, conn)
	testinfra.AssertPIDGone(t, d.pid)
	_ = d.sess.Close()
}

func TestDuplicateDaemonLeavesOriginalFunctional(t *testing.T) {
	first := startLifecycleDaemon(t)
	secondProc := testinfra.NewDaemon(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		SessionID:     "duplicate",
		ControlSocket: first.socket,
	})
	secondPID := secondProc.WaitForPID(t)
	secondClient := mcp.NewClient(&mcp.Implementation{Name: "duplicate", Version: "0.0.1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, err := secondClient.Connect(ctx, &mcp.IOTransport{Reader: secondProc.Stdout, Writer: secondProc.Stdin}, nil)
	cancel()
	if err == nil {
		t.Fatal("duplicate daemon unexpectedly connected")
	}
	if waitErr, ok := secondProc.WaitForExit(testinfra.ShortWait); !ok || waitErr == nil {
		t.Fatalf("duplicate daemon exit: err=%v exited=%v", waitErr, ok)
	}
	if !strings.Contains(secondProc.Stderr(), "already owned") {
		t.Fatalf("duplicate startup error was not clear: %s", secondProc.Stderr())
	}
	testinfra.AssertPIDGone(t, secondPID)

	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer listCancel()
	result, err := first.sess.ListTools(listCtx, nil)
	if err != nil || len(result.Tools) != 3 {
		t.Fatalf("original daemon was disrupted: tools=%d err=%v", len(result.Tools), err)
	}
}

func assertFresh(t *testing.T, stale bool) {
	t.Helper()
	if stale {
		t.Errorf("Phase 0 results must never be stale (no barrier yet), got stale=true")
	}
}

func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("malformed JSONL line: %q: %v", line, err)
			continue
		}
		out = append(out, m)
	}
	return out
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

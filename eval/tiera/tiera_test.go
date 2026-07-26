// Package tiera is the Phase 0 Tier A gate: a retrieval-correctness harness
// that drives the real portoland daemon over MCP against a pinned TypeScript fixture.
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
	"github.com/skflowne/portolan/eval/testinfra"
	"github.com/skflowne/portolan/internal/tools"
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
	sess  *mcp.ClientSession
	jsonl string
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
	sess := testinfra.ConnectMCP(t, proc, sessionID)
	proc.WaitForPID(t)
	return &daemonProcess{sess: sess, jsonl: jsonl}
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
		if len(lines) != 4 {
			t.Fatalf("expected exactly 4 telemetry events (one per completed tool call), got %d: %v", len(lines), lines)
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
		wantCounts := map[string]int{"get_outline": 2, "find_references": 1, "find_definition": 1}
		for tool, want := range wantCounts {
			if seen[tool] != want {
				t.Errorf("telemetry count for %q = %d, want %d (saw %v)", tool, seen[tool], want, seen)
			}
		}
	})
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

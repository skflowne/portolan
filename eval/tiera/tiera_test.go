// Package tiera verifies retrieval correctness by driving the real portoland
// daemon over MCP against a pinned TypeScript fixture.
package tiera

import (
	"context"
	"os"
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
		panic("tiera: " + err.Error())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type daemonProcess struct {
	proc  *testinfra.Daemon
	sess  *mcp.ClientSession
	jsonl string
}

func startDaemon(t *testing.T, sessionID, socket string) *daemonProcess {
	t.Helper()
	return startDaemonWithConfig(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		SessionID:     sessionID,
		GraphMode:     "graph",
		ControlSocket: socket,
	})
}

func startDaemonWithConfig(t *testing.T, cfg testinfra.Config) *daemonProcess {
	t.Helper()
	dir := t.TempDir()
	if cfg.Telemetry == "" {
		cfg.Telemetry = filepath.Join(dir, "telemetry.jsonl")
	}
	proc := testinfra.NewDaemon(t, cfg)
	sess := testinfra.ConnectMCP(t, proc, cfg.SessionID)
	proc.WaitForPID(t)
	return &daemonProcess{proc: proc, sess: sess, jsonl: cfg.Telemetry}
}

func stopDaemon(t *testing.T, daemon *daemonProcess) {
	t.Helper()
	_ = daemon.sess.Close()
	_ = daemon.proc.Stdin.Close()
	if err, ok := daemon.proc.WaitForExit(testinfra.ShortWait); !ok {
		t.Fatal("daemon did not exit after MCP completion")
	} else if err != nil {
		t.Fatalf("daemon exit: %v (stderr=%s)", err, daemon.proc.Stderr())
	}
	if !daemon.proc.FinishStdout(time.Second) {
		t.Fatal("daemon stdout capture did not reach EOF")
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	return testinfra.FixtureRoot()
}

func callInto(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, want, out any) {
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
	if err := decodeStructuredOutput(name, res.StructuredContent, want, out); err != nil {
		t.Fatalf("%s structured output: %v", name, err)
	}
}

// callTextRaw drives a compact-text tool and holds the transport invariant:
// exactly one text content item and no structured duplicate.
func callTextRaw(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
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
	if res.StructuredContent != nil {
		t.Fatalf("%s duplicated its result as structured content: %+v", name, res.StructuredContent)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s content = %+v, want exactly one item", name, res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s content item type = %T, want *mcp.TextContent", name, res.Content[0])
	}
	return text.Text
}

// callText makes an exact text response checkout-independent for pinned files.
func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	normalized := fixtureRelativeText(t, callTextRaw(t, sess, name, args))
	if strings.Contains(normalized, fixtureRoot(t)) {
		t.Fatalf("%s response retains an absolute fixture path: %q", name, normalized)
	}
	return normalized
}

func assertOutlineText(t *testing.T, sess *mcp.ClientSession, file, pinned string) {
	t.Helper()
	got := callText(t, sess, "get_outline", map[string]any{"file": file})
	if want := expectedText(t, pinned); got != want {
		t.Fatalf("%s text =\n%s\n\nwant\n%s", pinned, got, want)
	}
}

func TestTierA(t *testing.T) {
	daemon := startDaemon(t, "tiera", filepath.Join(t.TempDir(), "control.sock"))
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")
	mainTS := filepath.Join(fixtureRoot(t), "src", "main.ts")
	emptyTS := filepath.Join(fixtureRoot(t), "src", "empty.ts")
	want := loadPinnedContract(t)
	var got pinnedContract

	t.Run("tools_list", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		listed, err := daemon.sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		got := make(map[string]bool, len(listed.Tools))
		for _, tool := range listed.Tools {
			got[tool.Name] = true
		}
		for _, expected := range []string{"find_definition", "find_references", "get_outline"} {
			if !got[expected] {
				t.Errorf("tool %q not advertised", expected)
			}
		}
	})

	assertOutlineText(t, daemon.sess, geometry, "outline_geometry")
	references := callTextRaw(t, daemon.sess, "find_references", map[string]any{"file": geometry, "symbol": "Circle"})
	if wantReferences := expectedAbsoluteText(t, "references_circle"); references != wantReferences {
		t.Fatalf("references_circle text =\n%s\n\nwant\n%s", references, wantReferences)
	}
	callInto(t, daemon.sess, "find_definition", map[string]any{"file": geometry, "symbol": "totalArea"}, expectedStructuredOutput(t, want, "definition_total_area"), &got.DefinitionTotalArea)
	// A parameter property's selection range is a nested, non-top-level one:
	// the position every member navigation is issued at.
	callInto(t, daemon.sess, "find_definition", map[string]any{"file": geometry, "symbol": "radius"}, expectedStructuredOutput(t, want, "definition_radius"), &got.DefinitionRadius)
	assertOutlineText(t, daemon.sess, mainTS, "outline_main")
	assertOutlineText(t, daemon.sess, emptyTS, "outline_empty")
	assertOutlineText(t, daemon.sess, "relative.ts", "outline_invalid_file")
	normalizeContractPaths(t, &got)
	if err := validatePinnedContract(got, want); err != nil {
		t.Fatal(err)
	}

	stopDaemon(t, daemon)
	events, err := testinfra.WaitForQuiescentJSONL(daemon.jsonl, len(want.Telemetry), testinfra.ShortWait, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTelemetry(events, want.Telemetry); err != nil {
		t.Fatal(err)
	}
	assertProtocolOnlyStdout(t, daemon.proc.StdoutBytes())
}

func TestTierAOutlineTruncation(t *testing.T) {
	const cap = 4
	dir := t.TempDir()
	daemon := startDaemonWithConfig(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		SessionID:     "tiera-capped",
		GraphMode:     "graph",
		ControlSocket: filepath.Join(dir, "control.sock"),
		MaxResults:    cap,
	})
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")
	assertOutlineText(t, daemon.sess, geometry, "outline_geometry_capped")

	stopDaemon(t, daemon)
	wantEvent := expectedEvent{Tool: "get_outline", SessionID: "tiera-capped", GraphMode: "graph", ResultSize: cap, Truncated: true}
	events, err := testinfra.WaitForQuiescentJSONL(daemon.jsonl, 1, testinfra.ShortWait, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTelemetry(events, []expectedEvent{wantEvent}); err != nil {
		t.Fatal(err)
	}
	assertProtocolOnlyStdout(t, daemon.proc.StdoutBytes())
}

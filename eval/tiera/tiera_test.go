// Package tiera verifies retrieval correctness by driving the real portoland
// daemon over MCP against a pinned TypeScript fixture.
package tiera

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	daemon := startDaemon(t, "tiera", filepath.Join(t.TempDir(), "control.sock"))
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")
	mainTS := filepath.Join(fixtureRoot(t), "src", "main.ts")
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

	callInto(t, daemon.sess, "get_outline", map[string]any{"file": geometry}, &got.OutlineGeometry)
	callInto(t, daemon.sess, "find_references", map[string]any{"file": geometry, "symbol": "Circle"}, &got.ReferencesCircle)
	callInto(t, daemon.sess, "find_definition", map[string]any{"file": geometry, "symbol": "totalArea"}, &got.DefinitionTotalArea)
	callInto(t, daemon.sess, "get_outline", map[string]any{"file": mainTS}, &got.OutlineMain)
	callInto(t, daemon.sess, "get_outline", map[string]any{"file": "relative.ts"}, &got.OutlineInvalidFile)
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
	var got pinnedContract
	callInto(t, daemon.sess, "get_outline", map[string]any{"file": geometry}, &got.OutlineGeometry)
	normalizeContractPaths(t, &got)

	want := pinnedContract{OutlineGeometry: loadPinnedContract(t).OutlineGeometry}
	want.OutlineGeometry.Symbols = want.OutlineGeometry.Symbols[:cap]
	want.OutlineGeometry.Truncated = true
	if err := validatePinnedContract(got, want); err != nil {
		t.Fatal(err)
	}

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

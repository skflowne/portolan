package tiera

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skflowne/portolan/eval/testinfra"
)

func TestGenerationPropagatesFromControlSyncToMCP(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	d := startDaemon(t, "generation-propagation", socket)
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")

	before := callText(t, d.sess, "get_outline", map[string]any{"file": geometry})
	assertFreshnessStaysInternal(t, before)

	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatalf("connecting to control socket: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting control socket deadline: %v", err)
	}
	if _, err := fmt.Fprintln(conn, "sync", geometry); err != nil {
		t.Fatalf("sending sync command: %v", err)
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading sync response: %v", err)
	}
	if response != "ok generation=1\n" {
		t.Fatalf("sync response = %q", response)
	}

	after := callText(t, d.sess, "get_outline", map[string]any{"file": geometry})
	assertFreshnessStaysInternal(t, after)
	// A bumped generation reaches telemetry below but must not perturb the
	// agent-facing text of an unchanged file.
	if after != before {
		t.Fatalf("outline text changed with the generation:\n%s\n\nwas\n%s", after, before)
	}

	stopDaemon(t, d)
	events, err := testinfra.WaitForQuiescentJSONL(d.jsonl, 2, testinfra.ShortWait, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []expectedEvent{
		{Tool: "get_outline", SessionID: "generation-propagation", GraphMode: "graph", ResultSize: 13},
		{Tool: "get_outline", SessionID: "generation-propagation", GraphMode: "graph", ResultSize: 13, Generation: 1},
	}
	if err := validateTelemetry(events, wantEvents); err != nil {
		t.Fatal(err)
	}
	assertProtocolOnlyStdout(t, d.proc.StdoutBytes())
}

func assertFreshnessStaysInternal(t *testing.T, text string) {
	t.Helper()
	if want := expectedText(t, "outline_geometry"); text != want {
		t.Fatalf("outline text =\n%s\n\nwant\n%s", text, want)
	}
	for _, token := range []string{"generation", "stale"} {
		if strings.Contains(strings.ToLower(text), token) {
			t.Errorf("routine outline text exposes %q: %q", token, text)
		}
	}
}

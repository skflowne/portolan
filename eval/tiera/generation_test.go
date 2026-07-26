package tiera

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/tools"
)

func TestGenerationPropagatesFromControlSyncToMCP(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	d := startDaemon(t, "generation-propagation", socket)
	geometry := filepath.Join(fixtureRoot(t), "src", "geometry.ts")

	var before tools.GetOutlineOutput
	callInto(t, d.sess, "get_outline", map[string]any{"file": geometry}, &before)
	assertOutlineGeneration(t, before, 0)

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

	var after tools.GetOutlineOutput
	callInto(t, d.sess, "get_outline", map[string]any{"file": geometry}, &after)
	assertOutlineGeneration(t, after, 1)
}

func assertOutlineGeneration(t *testing.T, out tools.GetOutlineOutput, want uint64) {
	t.Helper()
	if !out.Found || out.Error != "" {
		t.Fatalf("get_outline failed: found=%v error=%q message=%q", out.Found, out.Error, out.Message)
	}
	if out.Freshness.Generation != want {
		t.Errorf("generation = %d, want %d", out.Freshness.Generation, want)
	}
	if out.Freshness.Stale {
		t.Error("Phase 0 result must not be stale")
	}
}

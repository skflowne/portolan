package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skflowne/code-graph-harness/internal/core"
	"github.com/skflowne/code-graph-harness/internal/tools"
)

func testTools(t *testing.T) *tools.Tools {
	t.Helper()
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.Symbol{
			file: {
				{Name: "DoThing", Kind: "function", File: file,
					Range: core.Range{Start: core.Position{Line: 1}, End: core.Position{Line: 2}},
					SelRange: core.Range{
						Start: core.Position{Line: 1, Character: 5},
						End:   core.Position{Line: 1, Character: 12},
					},
				},
			},
		},
		Definitions: map[string][]core.Location{
			file: {{File: file, Range: core.Range{Start: core.Position{Line: 0}, End: core.Position{Line: 0, Character: 3}}}},
		},
	}
	return tools.New(provider, &core.GenerationCounter{}, core.NopLogger{}, core.Config{SessionID: "test", GraphMode: "graph"})
}

// TestNewServer_ConstructsAndRegistersTools asserts the server builds without
// error and lists exactly the three expected tools.
func TestNewServer_ConstructsAndRegistersTools(t *testing.T) {
	srv := NewServer(testTools(t))
	if srv == nil {
		t.Fatal("expected non-nil server")
	}

	clientTransport, serverTransport := sdk.NewInMemoryTransports()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverSessionCh := make(chan *sdk.ServerSession, 1)
	go func() {
		ss, err := srv.Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Errorf("server connect: %v", err)
			return
		}
		serverSessionCh <- ss
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	ss := <-serverSessionCh
	defer ss.Close()

	listRes, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range listRes.Tools {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}
	}
	for _, want := range []string{"find_definition", "find_references", "get_outline"} {
		if !names[want] {
			t.Errorf("expected tool %q to be registered, got %+v", want, names)
		}
	}
}

// TestNewServer_FindDefinitionRoundTrip drives a full tools/call round trip
// over the SDK's in-memory transport and asserts the structured output
// matches what internal/tools.FindDefinition produces directly.
func TestNewServer_FindDefinitionRoundTrip(t *testing.T) {
	srv := NewServer(testTools(t))
	clientTransport, serverTransport := sdk.NewInMemoryTransports()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_, _ = srv.Connect(ctx, serverTransport, nil)
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "find_definition",
		Arguments: map[string]any{"file": "/repo/main.go", "symbol": "DoThing"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out tools.FindDefinitionOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v (raw=%s)", err, raw)
	}
	if !out.Found {
		t.Fatalf("expected Found=true, got %+v", out)
	}
	if len(out.Locations) != 1 || out.Locations[0].File != "/repo/main.go" {
		t.Fatalf("unexpected locations: %+v", out.Locations)
	}
}

func TestControlSocket_SyncBumpsGenerationAndReplies(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-test.sock")

	gen := &core.GenerationCounter{}
	cs := NewControlSocket(sockPath, gen)

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

	if _, err := conn.Write([]byte("sync foo.ts\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply != "ok generation=1\n" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if gen.Current().Generation != 1 {
		t.Fatalf("expected generation counter to have bumped to 1, got %d", gen.Current().Generation)
	}
}

func TestControlSocket_CancellationClosesIdleClientAndWaits(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-idle.sock")
	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	cancel()
	waitDone := make(chan struct{})
	go func() {
		cs.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancellation")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		t.Fatal("expected cancellation to close idle client")
	}
}

func TestControlSocket_DuplicateDoesNotDisruptFirst(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-duplicate.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := NewControlSocket(sockPath, &core.GenerationCounter{})
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second := NewControlSocket(sockPath, &core.GenerationCounter{})
	if err := second.Start(ctx); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("expected clear duplicate error, got %v", err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("first listener was disrupted: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("sync file.ts\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if reply, err := bufio.NewReader(conn).ReadString('\n'); err != nil || reply != "ok generation=1\n" {
		t.Fatalf("unexpected reply %q (%v)", reply, err)
	}
	cancel()
	first.Wait()
}

func TestControlSocket_UnknownCommand(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-test2.sock")

	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
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

	if _, err := conn.Write([]byte("bogus\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply != "err unknown\n" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestControlSocket_RejectsAndPreservesRegularFile(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-regular.sock")
	contents := []byte("not a socket")
	if err := os.WriteFile(sockPath, contents, 0o644); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cs.Start(ctx); err == nil {
		t.Fatal("expected regular file to be rejected")
	}
	got, err := os.ReadFile(sockPath)
	if err != nil {
		t.Fatalf("regular file was removed: %v", err)
	}
	if string(got) != string(contents) {
		t.Fatalf("regular file changed: %q", got)
	}
}

func TestControlSocket_RecoversActualStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-stale.sock")
	old, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close seed listener: %v", err)
	}

	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start should recover stale socket: %v", err)
	}
	cancel()
	cs.Wait()
}

func TestControlSocket_CleanupPreservesReplacementSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "cgraphd-replacement.sock")
	cs := NewControlSocket(sockPath, &core.GenerationCounter{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()
	// Remove the daemon's pathname while its listener remains open, then
	// install a replacement before Wait runs ownership-aware cleanup.
	if err := os.Remove(sockPath); err != nil {
		t.Fatalf("unlink daemon socket: %v", err)
	}
	var replacement net.Listener
	var err error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		replacement, err = net.Listen("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if replacement == nil {
		t.Fatalf("could not bind replacement socket: %v", err)
	}
	defer replacement.Close()
	cs.Wait()
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("cleanup removed replacement socket: %v", err)
	}
}

func TestSocketPath_DerivesFromProjectRootWhenUnset(t *testing.T) {
	p1 := SocketPath(core.Config{ProjectRoot: "/home/user/proj-a"})
	p2 := SocketPath(core.Config{ProjectRoot: "/home/user/proj-b"})
	if p1 == p2 {
		t.Fatalf("expected distinct paths for distinct project roots, got %q for both", p1)
	}
	p3 := SocketPath(core.Config{ProjectRoot: "/home/user/proj-a", ControlSocket: "/tmp/explicit.sock"})
	if p3 != "/tmp/explicit.sock" {
		t.Fatalf("expected explicit ControlSocket to win, got %q", p3)
	}
}

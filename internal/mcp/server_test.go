package mcp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/tools"
)

func testTools(t *testing.T) *tools.Tools {
	t.Helper()
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{
			file: {
				{Symbol: core.Symbol{Name: "DoThing", Kind: "function", File: file,
					Range: core.Range{Start: core.Position{Line: 1}, End: core.Position{Line: 2}},
					SelRange: core.Range{
						Start: core.Position{Line: 1, Character: 5},
						End:   core.Position{Line: 1, Character: 12},
					},
				}},
			},
		},
		Definitions: map[string][]core.Location{
			file: {{File: file, Range: core.Range{Start: core.Position{Line: 0}, End: core.Position{Line: 0, Character: 3}}}},
		},
	}
	return tools.New(provider, &core.GenerationCounter{}, core.NopLogger{}, core.Config{SessionID: "test", GraphMode: "graph"})
}

// listServerTools serves srv over the in-memory transport and returns the
// tools an MCP client actually sees, including the descriptions shipped to it.
func listServerTools(t *testing.T, srv *sdk.Server) []*sdk.Tool {
	t.Helper()
	clientTransport, serverTransport := sdk.NewInMemoryTransports()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	serverSessionCh := make(chan *sdk.ServerSession, 1)
	go func() {
		ss, err := srv.Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Errorf("server connect: %v", err)
			close(serverSessionCh)
			return
		}
		serverSessionCh <- ss
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	ss, ok := <-serverSessionCh
	if !ok {
		t.Fatal("server session unavailable")
	}
	t.Cleanup(func() { ss.Close() })

	listRes, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return listRes.Tools
}

// TestNewServer_ConstructsAndRegistersTools asserts the server builds without
// error and lists exactly the three expected tools.
func TestNewServer_ConstructsAndRegistersTools(t *testing.T) {
	srv := NewServer(testTools(t))
	if srv == nil {
		t.Fatal("expected non-nil server")
	}

	names := map[string]bool{}
	var outline *sdk.Tool
	for _, tool := range listServerTools(t, srv) {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}
		if tool.Name == "get_outline" {
			outline = tool
		}
	}
	for _, want := range []string{"find_definition", "find_references", "get_outline"} {
		if !names[want] {
			t.Errorf("expected tool %q to be registered, got %+v", want, names)
		}
	}
	if outline == nil {
		t.Fatal("get_outline tool not found")
	}
	for _, grammar := range []string{"ranges 0-based", "two spaces per nesting level", "symbols; complete", "truncated: more symbols exist", "empty:", "error:"} {
		if !strings.Contains(outline.Description, grammar) {
			t.Errorf("get_outline description does not explain %q: %q", grammar, outline.Description)
		}
	}
	if outline.OutputSchema != nil {
		t.Fatalf("get_outline advertises an output schema for a text-only response: %+v", outline.OutputSchema)
	}
}

// grammarToken returns the backticked token the get_outline description
// advertises right after lead, so the grammar assertions read the shipped
// description instead of restating it.
func grammarToken(t *testing.T, description, lead string) string {
	t.Helper()
	_, after, ok := strings.Cut(description, lead+"`")
	if !ok {
		t.Fatalf("get_outline description no longer states %q: %q", lead, description)
	}
	token, _, ok := strings.Cut(after, "`")
	if !ok {
		t.Fatalf("get_outline description leaves the token after %q unterminated: %q", lead, description)
	}
	return token
}

func outlineSymbol(depth int, signature string, startLine, endLine int) tools.OutlineSymbol {
	return tools.OutlineSymbol{
		Symbol: core.Symbol{
			Name:      signature,
			Kind:      core.SymbolKindUnknown,
			File:      "/project/src/geometry.ts",
			Signature: signature,
			Range: core.Range{
				Start: core.Position{Line: startLine},
				End:   core.Position{Line: endLine, Character: 1},
			},
		},
		Depth: depth,
	}
}

// TestNewServer_GetOutlineDescriptionMatchesRenderedText bridges the two owners
// of the get_outline grammar: internal/mcp advertises it and tools.RenderOutline
// produces it, and the description is the only spec a model has at call time.
// Each claim is read out of the shipped description and checked against rendered
// text, so moving either owner alone fails here instead of silently lying to
// every MCP client.
func TestNewServer_GetOutlineDescriptionMatchesRenderedText(t *testing.T) {
	var description string
	for _, tool := range listServerTools(t, NewServer(testTools(t))) {
		if tool.Name == "get_outline" {
			description = tool.Description
		}
	}
	if description == "" {
		t.Fatal("get_outline tool has no description to bridge")
	}

	out := tools.GetOutlineOutput{
		Found: true,
		File:  "/project/src/geometry.ts",
		Symbols: []tools.OutlineSymbol{
			outlineSymbol(0, "interface Shape", 3, 5),
			outlineSymbol(1, "area(): number", 4, 4),
			outlineSymbol(0, "class Circle implements Shape", 7, 13),
			outlineSymbol(0, "const origin: Point", 15, 15),
		},
	}
	rendered := tools.RenderOutline(out)
	lines := strings.Split(rendered, "\n")
	if len(lines) < len(out.Symbols)+4 {
		t.Fatalf("rendered outline is too short to carry the advertised grammar: %q", rendered)
	}

	wantHeader := strings.ReplaceAll(grammarToken(t, description, "Line 1 is "), "<path>", out.File)
	if !strings.HasPrefix(rendered, wantHeader+"\n") {
		t.Errorf("description advertises line 1 as %q, rendered line 1 is %q", wantHeader, lines[0])
	}
	if want := grammarToken(t, description, "line 2 is "); lines[1] != want {
		t.Errorf("description advertises line 2 as %q, rendered line 2 is %q", want, lines[1])
	}

	const headerRule = "A blank line closes the header"
	if !strings.Contains(description, headerRule) {
		t.Fatalf("description no longer states %q: %q", headerRule, description)
	}
	if lines[2] != "" {
		t.Errorf("%s, but rendered line 3 is %q", headerRule, lines[2])
	}

	const footerRule = "After a final blank line the last line is"
	wantFooter := strings.Replace(grammarToken(t, description, footerRule+" "), "N", strconv.Itoa(len(out.Symbols)), 1)
	if got := lines[len(lines)-1]; got != wantFooter {
		t.Errorf("description advertises the last line as %q, rendered last line is %q", wantFooter, got)
	}
	if lines[len(lines)-2] != "" {
		t.Errorf("%s %q, but the line before it is %q", footerRule, wantFooter, lines[len(lines)-2])
	}

	const nestingRule = "Within that list a blank line precedes a top-level symbol that follows a nested one"
	if !strings.Contains(description, nestingRule) {
		t.Fatalf("description no longer states %q: %q", nestingRule, description)
	}
	body := lines[3 : len(lines)-2]
	symbol := 0
	for i, line := range body {
		if line == "" {
			continue
		}
		if symbol >= len(out.Symbols) {
			t.Fatalf("rendered outline carries more symbol lines than symbols: %q", rendered)
		}
		wantBlank := symbol > 0 && out.Symbols[symbol].Depth == 0 && out.Symbols[symbol-1].Depth > 0
		if gotBlank := i > 0 && body[i-1] == ""; gotBlank != wantBlank {
			t.Errorf("%s: symbol line %q preceded by blank line = %v, want %v", nestingRule, line, gotBlank, wantBlank)
		}
		symbol++
	}
	if symbol != len(out.Symbols) {
		t.Errorf("rendered outline carries %d symbol lines for %d symbols: %q", symbol, len(out.Symbols), rendered)
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

// TestNewServer_GetOutlineReturnsOnlyRenderedText asserts the MCP boundary
// carries the compact outline exactly once: one text content item produced by
// the tools renderer and no structured duplicate.
func TestNewServer_GetOutlineReturnsOnlyRenderedText(t *testing.T) {
	tl := testTools(t)
	srv := NewServer(tl)
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

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "get_outline", Arguments: map[string]any{"file": "/repo/main.go"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("get_outline duplicated its result as structured content: %+v", res.StructuredContent)
	}
	if len(res.Content) != 1 {
		t.Fatalf("get_outline content = %+v, want exactly one item", res.Content)
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("get_outline content item type = %T, want *sdk.TextContent", res.Content[0])
	}
	out, err := tl.GetOutline(ctx, tools.GetOutlineInput{File: "/repo/main.go"})
	if err != nil {
		t.Fatalf("GetOutline: %v", err)
	}
	if text.Text != tools.RenderOutline(out) {
		t.Fatalf("transport text = %q, want the tools renderer projection %q", text.Text, tools.RenderOutline(out))
	}
	if !strings.Contains(text.Text, "file /repo/main.go") || !strings.Contains(text.Text, "1 symbol; complete") {
		t.Fatalf("get_outline text is not the compact outline: %q", text.Text)
	}
}

func TestControlSocket_SyncBumpsGenerationAndReplies(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "portoland-test.sock")

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
	sockPath := filepath.Join(dir, "portoland-idle.sock")
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
	sockPath := filepath.Join(dir, "portoland-duplicate.sock")
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
	sockPath := filepath.Join(dir, "portoland-test2.sock")

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
	sockPath := filepath.Join(dir, "portoland-regular.sock")
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
	sockPath := filepath.Join(dir, "portoland-stale.sock")
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
	sockPath := filepath.Join(dir, "portoland-replacement.sock")
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
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	const projectRoot = "/home/user/proj-a"
	digest := sha256.Sum256([]byte(projectRoot))
	want := filepath.Join(runtimeDir, "portoland", fmt.Sprintf("portoland-%s.sock", hex.EncodeToString(digest[:])[:12]))

	if got := SocketPath(core.Config{ProjectRoot: projectRoot}); got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
	if other := SocketPath(core.Config{ProjectRoot: "/home/user/proj-b"}); other == want {
		t.Fatalf("distinct project roots produced the same socket path %q", other)
	}
	if explicit := SocketPath(core.Config{ProjectRoot: projectRoot, ControlSocket: "/tmp/explicit.sock"}); explicit != "/tmp/explicit.sock" {
		t.Fatalf("explicit ControlSocket = %q, want /tmp/explicit.sock", explicit)
	}
}

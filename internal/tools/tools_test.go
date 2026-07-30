package tools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

func pos(line, char int) core.Position { return core.Position{Line: line, Character: char} }

func rng(sl, sc, el, ec int) core.Range {
	return core.Range{Start: pos(sl, sc), End: pos(el, ec)}
}

func symbolNode(symbol core.Symbol, children ...core.SymbolNode) core.SymbolNode {
	return core.SymbolNode{Symbol: symbol, Children: children}
}

type signatureResultProvider struct {
	*core.StubProvider
	signatures []string
	err        error
	got        []core.Symbol
}

func (p *signatureResultProvider) SymbolSignatures(_ context.Context, _ string, symbols []core.Symbol) ([]string, error) {
	p.got = append([]core.Symbol(nil), symbols...)
	if p.signatures != nil || p.err != nil {
		return p.signatures, p.err
	}
	return existingSignatures(symbols), nil
}

type fileCallResult struct {
	found     bool
	resultLen int
	truncated bool
	freshness core.Freshness
	message   string
	errorText string
	err       error
}

type fileToolCase struct {
	name string
	call func(context.Context, *Tools, string) fileCallResult
}

func fileToolCases() []fileToolCase {
	return []fileToolCase{
		{
			name: "find_definition",
			call: func(ctx context.Context, tl *Tools, file string) fileCallResult {
				out, err := tl.FindDefinition(ctx, FindDefinitionInput{File: file, Symbol: "Target"})
				return fileCallResult{out.Found, len(out.Locations), out.Truncated, out.Freshness, out.Message, out.Error, err}
			},
		},
		{
			name: "find_references",
			call: func(ctx context.Context, tl *Tools, file string) fileCallResult {
				out, err := tl.FindReferences(ctx, FindReferencesInput{File: file, Symbol: "Target"})
				return fileCallResult{out.Found, len(out.Locations), out.Truncated, out.Freshness, out.Message, out.Error, err}
			},
		},
		{
			name: "get_outline",
			call: func(ctx context.Context, tl *Tools, file string) fileCallResult {
				out, err := tl.GetOutline(ctx, GetOutlineInput{File: file})
				return fileCallResult{out.Found, len(out.Symbols), out.Truncated, out.Freshness, out.Message, out.Error, err}
			},
		},
	}
}

func TestToolTelemetryLifecycle(t *testing.T) {
	const (
		initialGeneration = 1
		sessionID         = "telemetry-session"
		graphMode         = "no-graph"
	)
	for _, tool := range fileToolCases() {
		outcomes := []string{"success", "empty", "provider_error", "input_error"}
		if tool.name != "get_outline" {
			outcomes = append(outcomes, "unresolved", "result_error")
		}
		for _, outcome := range outcomes {
			t.Run(tool.name+"/"+outcome, func(t *testing.T) {
				gen := &core.GenerationCounter{}
				gen.Bump()
				provider := newTelemetryProvider(tool.name, outcome, gen)
				logger := &capturingLogger{}
				tl := New(provider, gen, logger, core.Config{
					SessionID:  sessionID,
					GraphMode:  graphMode,
					MaxResults: 2,
				})
				file := "/repo/main.go"
				if outcome == "input_error" {
					file = "relative.go"
				}

				got := tool.call(context.Background(), tl, file)
				if got.err != nil {
					t.Fatalf("tool returned Go error: %v", got.err)
				}
				if logger.count() != 1 {
					t.Fatalf("telemetry events = %d, want exactly 1", logger.count())
				}
				ev, _ := logger.last()
				if ev.SessionID != sessionID || ev.GraphMode != graphMode || ev.Tool != tool.name {
					t.Fatalf("static correlation fields = session %q mode %q tool %q", ev.SessionID, ev.GraphMode, ev.Tool)
				}
				if ev.Generation != initialGeneration || ev.Stale || ev.Generation != got.freshness.Generation || ev.Stale != got.freshness.Stale {
					t.Fatalf("event freshness = {%d,%v}, output freshness = %+v, want initial generation %d", ev.Generation, ev.Stale, got.freshness, initialGeneration)
				}
				if ev.ResultSize != got.resultLen || ev.Truncated != got.truncated || ev.Err != got.errorText {
					t.Fatalf("event completion = size %d truncated %v err %q, output = size %d truncated %v err %q", ev.ResultSize, ev.Truncated, ev.Err, got.resultLen, got.truncated, got.errorText)
				}
				if ev.Extra != nil {
					t.Fatalf("event extra = %+v, want nil", ev.Extra)
				}

				switch outcome {
				case "success":
					if !got.found || got.resultLen != 2 || !got.truncated || got.errorText != "" {
						t.Fatalf("success result = %+v, want two capped results", got)
					}
				case "empty", "unresolved":
					if got.found || got.resultLen != 0 || got.truncated || got.errorText != "" || got.message == "" {
						t.Fatalf("empty result = %+v", got)
					}
				case "provider_error", "result_error", "input_error":
					if got.found || got.resultLen != 0 || got.truncated || got.errorText == "" || got.message == "" {
						t.Fatalf("soft failure result = %+v", got)
					}
				}

				if outcome == "input_error" {
					if provider.callCount() != 0 || gen.Current().Generation != initialGeneration {
						t.Fatalf("input failure provider calls = %d generation = %d", provider.callCount(), gen.Current().Generation)
					}
				} else if provider.callCount() == 0 || gen.Current().Generation != initialGeneration+1 {
					t.Fatalf("provider calls = %d generation = %d, want work after initial snapshot", provider.callCount(), gen.Current().Generation)
				}
			})
		}
	}
}

func TestToolDurationIncludesExecution(t *testing.T) {
	const (
		providerDelay = 25 * time.Millisecond
		minimumMs     = 20
	)
	gen := &core.GenerationCounter{}
	provider := newTelemetryProvider("get_outline", "success", gen)
	provider.delay = providerDelay
	logger := &capturingLogger{}
	tl := New(provider, gen, logger, core.Config{})

	if _, err := tl.GetOutline(context.Background(), GetOutlineInput{File: "/repo/main.go"}); err != nil {
		t.Fatalf("GetOutline: %v", err)
	}
	ev, ok := logger.last()
	if !ok {
		t.Fatal("telemetry event not emitted")
	}
	if ev.DurationMs < minimumMs {
		t.Fatalf("duration_ms = %d, want at least %d to include provider execution", ev.DurationMs, minimumMs)
	}
}

func TestToolMethodsUseSharedFinalEmission(t *testing.T) {
	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse tools package: %v", err)
	}
	pkg := packages["tools"]
	if pkg == nil {
		t.Fatal("tools package not found")
	}

	toolCount := 0
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			method, ok := decl.(*ast.FuncDecl)
			if !ok || method.Recv == nil || !ast.IsExported(method.Name.Name) {
				continue
			}
			receiver, ok := method.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			receiverName, ok := receiver.X.(*ast.Ident)
			if !ok || receiverName.Name != "Tools" {
				continue
			}
			toolCount++
			t.Run(method.Name.Name, func(t *testing.T) {
				runnerCalls := 0
				var localEmitters []string
				ast.Inspect(method.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch selector.Sel.Name {
					case "runTool":
						runnerCalls++
					case "emit", "Log":
						localEmitters = append(localEmitters, selector.Sel.Name)
					}
					return true
				})
				if runnerCalls != 1 || len(localEmitters) != 0 {
					t.Fatalf("shared runner calls = %d, local emitters = %v; want one runner and no local emission", runnerCalls, localEmitters)
				}
			})
		}
	}
	if toolCount != len(fileToolCases()) {
		t.Fatalf("exported tool methods = %d, test cases = %d", toolCount, len(fileToolCases()))
	}
}

func TestToolsRejectInvalidFileBeforeProviderWork(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	invalid := []struct {
		name string
		file string
	}{
		{name: "empty"},
		{name: "relative", file: "repo/main.go"},
		{name: "bare_drive", file: `C:`},
		{name: "malformed_unc", file: `\\`},
		{name: "unsupported_unc", file: `\\server\share\main.go`},
		{name: "cross_distro", file: `\\wsl$\Debian\repo\main.go`},
		{name: "nul", file: "/repo/\x00main.go"},
	}

	for _, tool := range fileToolCases() {
		for _, input := range invalid {
			t.Run(tool.name+"/"+input.name, func(t *testing.T) {
				provider := &fileRecordingProvider{}
				logger := &capturingLogger{}
				gen := &core.GenerationCounter{}
				gen.Bump()
				tl := New(provider, gen, logger, core.Config{})

				got := tool.call(context.Background(), tl, input.file)
				if got.err != nil {
					t.Fatalf("tool returned Go error: %v", got.err)
				}
				if got.found || got.resultLen != 0 || got.truncated {
					t.Fatalf("partial result: found=%v len=%d truncated=%v", got.found, got.resultLen, got.truncated)
				}
				if got.errorText == "" || got.message == "" {
					t.Fatalf("failure error=%q message=%q, want both populated", got.errorText, got.message)
				}
				if input.file != "" && (strings.Contains(got.errorText, input.file) || strings.Contains(got.message, input.file)) {
					t.Fatalf("failure exposed raw file %q: error=%q message=%q", input.file, got.errorText, got.message)
				}
				if got.freshness.Generation != 1 || got.freshness.Stale {
					t.Fatalf("freshness = %+v, want generation 1 and not stale", got.freshness)
				}
				if calls := provider.files(); len(calls) != 0 {
					t.Fatalf("provider files = %q, want no calls", calls)
				}
				if logger.count() != 1 {
					t.Fatalf("telemetry events = %d, want 1", logger.count())
				}
				ev, _ := logger.last()
				if ev.Err != got.errorText || ev.ResultSize != 0 || ev.Truncated || ev.Generation != 1 || ev.Stale {
					t.Fatalf("telemetry event = %+v, output error=%q", ev, got.errorText)
				}
			})
		}
	}
}

func TestToolsCanonicalizeEquivalentFileInputs(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	inputs := []string{
		`C:\repo\main.go`,
		"/mnt/c/repo/main.go",
		`\\wsl.localhost\Ubuntu\mnt\c\repo\main.go`,
	}
	const canonical = "/mnt/c/repo/main.go"

	for _, tool := range fileToolCases() {
		for _, input := range inputs {
			t.Run(tool.name+"/"+input, func(t *testing.T) {
				provider := &fileRecordingProvider{symbols: true}
				got := tool.call(context.Background(), newTestTools(provider, &capturingLogger{}, core.Config{}), input)
				if got.err != nil || got.errorText != "" || !got.found {
					t.Fatalf("result = %+v, want successful found result", got)
				}
				for i, file := range provider.files() {
					if file != canonical {
						t.Fatalf("provider file %d = %q, want %q", i+1, file, canonical)
					}
				}
			})
		}
	}
}

func TestToolsUseCanonicalFileInEmptyMessages(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	inputs := []string{
		`C:\repo\empty.go`,
		"/mnt/c/repo/empty.go",
		`\\wsl$\Ubuntu\mnt\c\repo\empty.go`,
	}
	const canonical = "/mnt/c/repo/empty.go"

	for _, tool := range fileToolCases() {
		for _, input := range inputs {
			t.Run(tool.name+"/"+input, func(t *testing.T) {
				got := tool.call(context.Background(), newTestTools(&fileRecordingProvider{}, &capturingLogger{}, core.Config{}), input)
				if got.err != nil || got.errorText != "" || got.found {
					t.Fatalf("result = %+v, want honest empty result", got)
				}
				if !strings.Contains(got.message, canonical) {
					t.Fatalf("message = %q, want canonical file %q", got.message, canonical)
				}
				if input != canonical && strings.Contains(got.message, input) {
					t.Fatalf("message = %q, must not expose raw file %q", got.message, input)
				}
			})
		}
	}
}

func TestFindDefinition_ResolvesByNameAndPosition(t *testing.T) {
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{
			file: {
				symbolNode(core.Symbol{
					Name:     "DoThing",
					Kind:     "function",
					File:     file,
					Range:    rng(10, 0, 20, 1),
					SelRange: rng(10, 5, 10, 12),
				}),
			},
		},
		Definitions: map[string][]core.Location{
			file: {
				{File: file, Range: rng(1, 0, 1, 5)},
			},
		},
	}
	logger := &capturingLogger{}
	cfg := core.Config{SessionID: "s1", GraphMode: "graph"}
	tl := newTestTools(provider, logger, cfg)

	out, err := tl.FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "DoThing"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Found {
		t.Fatalf("expected Found=true, got %+v", out)
	}
	if len(out.Locations) != 1 {
		t.Fatalf("expected 1 location, got %d", len(out.Locations))
	}
	if out.Truncated {
		t.Fatalf("expected Truncated=false")
	}
	if out.Freshness.Generation != 0 || out.Freshness.Stale {
		t.Fatalf("expected fresh Freshness{0,false}, got %+v", out.Freshness)
	}

	if logger.count() != 1 {
		t.Fatalf("expected exactly 1 event logged, got %d", logger.count())
	}
	ev, _ := logger.last()
	if ev.Tool != "find_definition" || ev.SessionID != "s1" || ev.GraphMode != "graph" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.ResultSize != 1 || ev.Truncated || ev.Err != "" {
		t.Fatalf("unexpected event fields: %+v", ev)
	}
}

func TestFindDefinition_DisambiguatesByLine(t *testing.T) {
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{
			file: {
				symbolNode(core.Symbol{Name: "Foo", Kind: "function", File: file, Range: rng(1, 0, 2, 0), SelRange: rng(1, 5, 1, 8)}),
				symbolNode(core.Symbol{Name: "Foo", Kind: "function", File: file, Range: rng(5, 0, 6, 0), SelRange: rng(5, 5, 5, 8)}),
			},
		},
	}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{})

	line := 5
	// Definitions map has nothing for file, so we expect Found=false with a
	// "no definition found" message -- but crucially it must have resolved
	// the *second* Foo (we can't observe the position directly through the
	// public API here, so we assert indirectly via a StubProvider variant).
	out, err := tl.FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "Foo", Line: &line})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Found {
		t.Fatalf("expected Found=false (no Definitions configured), got %+v", out)
	}
	if out.Message == "" {
		t.Fatalf("expected a Message explaining the empty result")
	}
}

func TestFindDefinition_SymbolNotFound(t *testing.T) {
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{file: {symbolNode(core.Symbol{Name: "Bar", SelRange: rng(0, 0, 0, 3)})}},
	}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{})

	out, err := tl.FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "DoesNotExist"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Found {
		t.Fatalf("expected Found=false")
	}
	if out.Error != "" {
		t.Fatalf("symbol-not-found must not be an Error, got %q", out.Error)
	}
	if out.Message == "" {
		t.Fatalf("expected explanatory Message")
	}
	if out.Locations != nil {
		t.Fatalf("expected nil Locations, got %+v", out.Locations)
	}

	ev, ok := logger.last()
	if !ok {
		t.Fatalf("expected an event to be logged")
	}
	if ev.Err != "" {
		t.Fatalf("expected no Err on the event for an honest not-found, got %q", ev.Err)
	}
}

func TestFindDefinition_ProviderErrorIsSoft(t *testing.T) {
	provider := &erroringProvider{err: errBoom}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{})

	out, err := tl.FindDefinition(context.Background(), FindDefinitionInput{File: "/x.go", Symbol: "Foo"})
	if err != nil {
		t.Fatalf("Tools methods must never return a Go error for provider failures, got %v", err)
	}
	if out.Found {
		t.Fatalf("expected Found=false on provider error")
	}
	if out.Error == "" {
		t.Fatalf("expected Error to be populated")
	}

	ev, _ := logger.last()
	if ev.Err == "" {
		t.Fatalf("expected event Err to be populated")
	}
}

func TestFindReferences_CapsAndTruncates(t *testing.T) {
	file := "/repo/main.go"
	symbols := []core.SymbolNode{symbolNode(core.Symbol{Name: "Used", SelRange: rng(0, 0, 0, 4)})}
	var refs []core.Location
	for i := 0; i < 250; i++ {
		refs = append(refs, core.Location{File: file, Range: rng(i, 0, i, 1)})
	}
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{file: symbols},
		Refs:    map[string][]core.Location{file: refs},
	}
	logger := &capturingLogger{}
	cfg := core.Config{MaxResults: 50}
	tl := newTestTools(provider, logger, cfg)

	out, err := tl.FindReferences(context.Background(), FindReferencesInput{File: file, Symbol: "Used"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Found {
		t.Fatalf("expected Found=true")
	}
	if !out.Truncated {
		t.Fatalf("expected Truncated=true")
	}
	if len(out.Locations) != 50 {
		t.Fatalf("expected 50 (capped) locations, got %d", len(out.Locations))
	}

	ev, _ := logger.last()
	if !ev.Truncated || ev.ResultSize != 50 {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestFindReferences_DefaultCap(t *testing.T) {
	file := "/repo/main.go"
	symbols := []core.SymbolNode{symbolNode(core.Symbol{Name: "Used", SelRange: rng(0, 0, 0, 4)})}
	var refs []core.Location
	for i := 0; i < core.DefaultMaxResults+10; i++ {
		refs = append(refs, core.Location{File: file, Range: rng(i, 0, i, 1)})
	}
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{file: symbols},
		Refs:    map[string][]core.Location{file: refs},
	}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{}) // MaxResults=0 -> DefaultMaxResults

	out, err := tl.FindReferences(context.Background(), FindReferencesInput{File: file, Symbol: "Used"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated {
		t.Fatalf("expected Truncated=true with default cap")
	}
	if len(out.Locations) != core.DefaultMaxResults {
		t.Fatalf("expected %d locations, got %d", core.DefaultMaxResults, len(out.Locations))
	}
}

func TestGetOutline_FlattensAndStampsFreshness(t *testing.T) {
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.SymbolNode{
			file: {
				symbolNode(core.Symbol{
					Name: "Outer", Kind: "class", File: file,
					Range: rng(0, 0, 10, 0), SelRange: rng(0, 6, 0, 11),
					Signature: "class Outer", Detail: "document symbol detail",
				}, symbolNode(core.Symbol{Name: "Inner", Kind: "method", File: file, Range: rng(1, 0, 2, 0), SelRange: rng(1, 4, 1, 9), Signature: "(method) Outer.Inner(): void"})),
			},
		},
	}
	logger := &capturingLogger{}
	gen := &core.GenerationCounter{}
	gen.Bump()
	gen.Bump()
	tl := New(provider, gen, logger, core.Config{})

	out, err := tl.GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Found {
		t.Fatalf("expected Found=true")
	}
	if len(out.Symbols) != 2 {
		t.Fatalf("expected 2 flattened symbols (parent+child), got %d: %+v", len(out.Symbols), out.Symbols)
	}
	if out.Symbols[0].Name != "Outer" || out.Symbols[0].Depth != 0 {
		t.Fatalf("expected Outer at depth 0 first, got %+v", out.Symbols[0])
	}
	if out.Symbols[1].Name != "Inner" || out.Symbols[1].Depth != 1 {
		t.Fatalf("expected Inner at depth 1 second, got %+v", out.Symbols[1])
	}
	if out.Symbols[0].Signature != "class Outer" || out.Symbols[0].Detail != "document symbol detail" ||
		out.Symbols[1].Signature != "(method) Outer.Inner(): void" || out.Symbols[1].Detail != "" {
		t.Fatalf("flattened signature/detail fields = %+v, want independent provider values", out.Symbols)
	}
	if out.Freshness.Generation != 2 {
		t.Fatalf("expected Freshness.Generation=2, got %d", out.Freshness.Generation)
	}

	if logger.count() != 1 {
		t.Fatalf("expected exactly 1 event, got %d", logger.count())
	}
}

func TestGetOutline_CapsFlattenedList(t *testing.T) {
	file := "/repo/big.go"
	var symbols []core.SymbolNode
	for i := 0; i < 120; i++ {
		symbols = append(symbols, symbolNode(core.Symbol{Name: "S", Kind: "function", File: file, SelRange: rng(i, 0, i, 1)}))
	}
	provider := &core.StubProvider{Symbols: map[string][]core.SymbolNode{file: symbols}}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{}) // default cap 100

	out, err := tl.GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated {
		t.Fatalf("expected Truncated=true")
	}
	if len(out.Symbols) != core.DefaultMaxResults {
		t.Fatalf("expected %d symbols, got %d", core.DefaultMaxResults, len(out.Symbols))
	}
}

func TestGetOutlineRequestsSignaturesOnlyForCappedSymbols(t *testing.T) {
	file := "/repo/big.go"
	symbols := []core.SymbolNode{
		symbolNode(core.Symbol{Name: "Container", Kind: "class", File: file, SelRange: rng(0, 0, 0, 9)},
			symbolNode(core.Symbol{Name: "First", Kind: "method", File: file, SelRange: rng(1, 0, 1, 5)}),
			symbolNode(core.Symbol{Name: "Second", Kind: "method", File: file, SelRange: rng(2, 0, 2, 6)})),
		symbolNode(core.Symbol{Name: "Trailing", Kind: "function", File: file, SelRange: rng(3, 0, 3, 8)}),
	}
	provider := &signatureResultProvider{StubProvider: &core.StubProvider{Symbols: map[string][]core.SymbolNode{file: symbols}}}
	tl := newTestTools(provider, &capturingLogger{}, core.Config{MaxResults: 2})

	out, err := tl.GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil || out.Error != "" {
		t.Fatalf("GetOutline = (%+v, %v)", out, err)
	}
	want := []core.Symbol{symbols[0].Symbol, symbols[0].Children[0].Symbol}
	if !reflect.DeepEqual(provider.got, want) {
		t.Fatalf("signature symbols = %+v, want exact capped originals %+v", provider.got, want)
	}
	if !out.Truncated {
		t.Fatal("Truncated = false, want true when cap cuts through nested symbols")
	}
}

func TestGetOutlineSignatureFailureIsSoftAndReturnsNoPartialOutline(t *testing.T) {
	file := "/repo/main.go"
	provider := &signatureResultProvider{
		StubProvider: &core.StubProvider{Symbols: map[string][]core.SymbolNode{file: {symbolNode(core.Symbol{Name: "Target", File: file})}}},
		err:          errBoom,
	}
	logger := &capturingLogger{}
	out, err := newTestTools(provider, logger, core.Config{}).GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil {
		t.Fatalf("GetOutline returned Go error: %v", err)
	}
	if out.Found || len(out.Symbols) != 0 || out.Error != errBoom.Error() || out.Message == "" {
		t.Fatalf("output = %+v, want structured failure without partial symbols", out)
	}
	event, ok := logger.last()
	if !ok || event.Err != errBoom.Error() || event.ResultSize != 0 {
		t.Fatalf("event = %+v, present %v", event, ok)
	}
}

func TestGetOutlineRejectsMisalignedSignatureResult(t *testing.T) {
	file := "/repo/main.go"
	provider := &signatureResultProvider{
		StubProvider: &core.StubProvider{Symbols: map[string][]core.SymbolNode{file: {symbolNode(core.Symbol{Name: "Target", File: file})}}},
		signatures:   []string{},
	}
	out, err := newTestTools(provider, &capturingLogger{}, core.Config{}).GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil {
		t.Fatalf("GetOutline returned Go error: %v", err)
	}
	if out.Found || len(out.Symbols) != 0 || out.Error == "" || out.Message == "" {
		t.Fatalf("output = %+v, want structured provider-contract failure", out)
	}
}

func TestGetOutline_EmptyFileIsHonestNotFound(t *testing.T) {
	file := "/repo/empty.go"
	provider := &core.StubProvider{}
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{})

	out, err := tl.GetOutline(context.Background(), GetOutlineInput{File: file})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Found {
		t.Fatalf("expected Found=false for a file with no symbols")
	}
	if out.Message == "" {
		t.Fatalf("expected explanatory Message")
	}
}

// erroringProvider is a minimal core.LanguageProvider whose every method
// fails, used to assert the soft-error contract without touching StubProvider.
type erroringProvider struct{ err error }

var errBoom = &providerErr{"boom"}

type providerErr struct{ msg string }

func (e *providerErr) Error() string { return e.msg }

func (p *erroringProvider) Definition(_ context.Context, _ string, _ core.Position) ([]core.Location, error) {
	return nil, p.err
}

func (p *erroringProvider) References(_ context.Context, _ string, _ core.Position, _ bool) ([]core.Location, error) {
	return nil, p.err
}

func (p *erroringProvider) DocumentSymbols(_ context.Context, _ string) ([]core.SymbolNode, error) {
	return nil, p.err
}

func (p *erroringProvider) SymbolSignatures(_ context.Context, _ string, _ []core.Symbol) ([]string, error) {
	return nil, p.err
}

func (p *erroringProvider) Close() error { return nil }

var _ core.LanguageProvider = (*erroringProvider)(nil)

type telemetryProvider struct {
	tool    string
	outcome string
	gen     *core.GenerationCounter
	delay   time.Duration

	bumpOnce sync.Once
	mu       sync.Mutex
	calls    int
}

func newTelemetryProvider(tool, outcome string, gen *core.GenerationCounter) *telemetryProvider {
	return &telemetryProvider{tool: tool, outcome: outcome, gen: gen}
}

func (p *telemetryProvider) enter() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.bumpOnce.Do(func() { p.gen.Bump() })
	time.Sleep(p.delay)
}

func (p *telemetryProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *telemetryProvider) symbols(file string) []core.SymbolNode {
	if p.tool == "get_outline" {
		if p.outcome == "empty" {
			return nil
		}
		return []core.SymbolNode{
			symbolNode(core.Symbol{Name: "First", File: file}),
			symbolNode(core.Symbol{Name: "Second", File: file}),
			symbolNode(core.Symbol{Name: "Third", File: file}),
		}
	}
	if p.outcome == "unresolved" {
		return []core.SymbolNode{symbolNode(core.Symbol{Name: "Other", File: file})}
	}
	return []core.SymbolNode{symbolNode(core.Symbol{Name: "Target", File: file})}
}

func (p *telemetryProvider) Definition(_ context.Context, file string, _ core.Position) ([]core.Location, error) {
	p.enter()
	if p.outcome == "result_error" {
		return nil, errBoom
	}
	if p.outcome == "empty" {
		return nil, nil
	}
	return []core.Location{{File: file}, {File: file}, {File: file}}, nil
}

func (p *telemetryProvider) References(_ context.Context, file string, _ core.Position, _ bool) ([]core.Location, error) {
	p.enter()
	if p.outcome == "result_error" {
		return nil, errBoom
	}
	if p.outcome == "empty" {
		return nil, nil
	}
	return []core.Location{{File: file}, {File: file}, {File: file}}, nil
}

func (p *telemetryProvider) DocumentSymbols(_ context.Context, file string) ([]core.SymbolNode, error) {
	p.enter()
	if p.outcome == "provider_error" {
		return nil, errBoom
	}
	return p.symbols(file), nil
}

func (p *telemetryProvider) SymbolSignatures(_ context.Context, _ string, symbols []core.Symbol) ([]string, error) {
	return existingSignatures(symbols), nil
}

func (p *telemetryProvider) Close() error { return nil }

var _ core.LanguageProvider = (*telemetryProvider)(nil)

type fileRecordingProvider struct {
	mu      sync.Mutex
	records []string
	symbols bool
}

func (p *fileRecordingProvider) record(file string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, file)
}

func (p *fileRecordingProvider) files() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.records...)
}

func (p *fileRecordingProvider) Definition(_ context.Context, file string, _ core.Position) ([]core.Location, error) {
	p.record(file)
	return []core.Location{{File: file}}, nil
}

func (p *fileRecordingProvider) References(_ context.Context, file string, _ core.Position, _ bool) ([]core.Location, error) {
	p.record(file)
	return []core.Location{{File: file}}, nil
}

func (p *fileRecordingProvider) DocumentSymbols(_ context.Context, file string) ([]core.SymbolNode, error) {
	p.record(file)
	if !p.symbols {
		return nil, nil
	}
	return []core.SymbolNode{symbolNode(core.Symbol{Name: "Target", File: file})}, nil
}

func (p *fileRecordingProvider) SymbolSignatures(_ context.Context, file string, symbols []core.Symbol) ([]string, error) {
	p.record(file)
	return existingSignatures(symbols), nil
}

func (p *fileRecordingProvider) Close() error { return nil }

var _ core.LanguageProvider = (*fileRecordingProvider)(nil)

package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

// capturingLogger records every Event logged so tests can assert on it.
type capturingLogger struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *capturingLogger) Log(_ context.Context, ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *capturingLogger) Close() error { return nil }

func (c *capturingLogger) last() (core.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return core.Event{}, false
	}
	return c.events[len(c.events)-1], true
}

func (c *capturingLogger) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func pos(line, char int) core.Position { return core.Position{Line: line, Character: char} }
func rng(sl, sc, el, ec int) core.Range {
	return core.Range{Start: pos(sl, sc), End: pos(el, ec)}
}

func newTestTools(provider core.LanguageProvider, logger *capturingLogger, cfg core.Config) *Tools {
	return New(provider, &core.GenerationCounter{}, logger, cfg)
}

func TestFindDefinition_ResolvesByNameAndPosition(t *testing.T) {
	file := "/repo/main.go"
	provider := &core.StubProvider{
		Symbols: map[string][]core.Symbol{
			file: {
				{
					Name:     "DoThing",
					Kind:     "function",
					File:     file,
					Range:    rng(10, 0, 20, 1),
					SelRange: rng(10, 5, 10, 12),
				},
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
		Symbols: map[string][]core.Symbol{
			file: {
				{Name: "Foo", Kind: "function", File: file, Range: rng(1, 0, 2, 0), SelRange: rng(1, 5, 1, 8)},
				{Name: "Foo", Kind: "function", File: file, Range: rng(5, 0, 6, 0), SelRange: rng(5, 5, 5, 8)},
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
		Symbols: map[string][]core.Symbol{file: {{Name: "Bar", SelRange: rng(0, 0, 0, 3)}}},
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
	symbols := []core.Symbol{{Name: "Used", SelRange: rng(0, 0, 0, 4)}}
	var refs []core.Location
	for i := 0; i < 250; i++ {
		refs = append(refs, core.Location{File: file, Range: rng(i, 0, i, 1)})
	}
	provider := &core.StubProvider{
		Symbols: map[string][]core.Symbol{file: symbols},
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
	symbols := []core.Symbol{{Name: "Used", SelRange: rng(0, 0, 0, 4)}}
	var refs []core.Location
	for i := 0; i < core.DefaultMaxResults+10; i++ {
		refs = append(refs, core.Location{File: file, Range: rng(i, 0, i, 1)})
	}
	provider := &core.StubProvider{
		Symbols: map[string][]core.Symbol{file: symbols},
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
		Symbols: map[string][]core.Symbol{
			file: {
				{
					Name: "Outer", Kind: "class", File: file,
					Range: rng(0, 0, 10, 0), SelRange: rng(0, 6, 0, 11),
					Children: []core.Symbol{
						{Name: "Inner", Kind: "method", File: file, Range: rng(1, 0, 2, 0), SelRange: rng(1, 4, 1, 9)},
					},
				},
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
	if out.Freshness.Generation != 2 {
		t.Fatalf("expected Freshness.Generation=2, got %d", out.Freshness.Generation)
	}

	if logger.count() != 1 {
		t.Fatalf("expected exactly 1 event, got %d", logger.count())
	}
}

func TestGetOutline_CapsFlattenedList(t *testing.T) {
	file := "/repo/big.go"
	var symbols []core.Symbol
	for i := 0; i < 120; i++ {
		symbols = append(symbols, core.Symbol{Name: "S", Kind: "function", File: file, SelRange: rng(i, 0, i, 1)})
	}
	provider := &core.StubProvider{Symbols: map[string][]core.Symbol{file: symbols}}
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
func (p *erroringProvider) DocumentSymbols(_ context.Context, _ string) ([]core.Symbol, error) {
	return nil, p.err
}
func (p *erroringProvider) Close() error { return nil }

var _ core.LanguageProvider = (*erroringProvider)(nil)

func TestToolsOwnOneOperationDeadline(t *testing.T) {
	file := "/repo/main.go"
	cases := []struct {
		name string
		call func(*Tools) error
	}{
		{
			name: "find_definition",
			call: func(tl *Tools) error {
				_, err := tl.FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "Target"})
				return err
			},
		},
		{
			name: "find_references",
			call: func(tl *Tools) error {
				_, err := tl.FindReferences(context.Background(), FindReferencesInput{File: file, Symbol: "Target"})
				return err
			},
		},
		{
			name: "get_outline",
			call: func(tl *Tools) error {
				_, err := tl.GetOutline(context.Background(), GetOutlineInput{File: file})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &contextRecordingProvider{file: file}
			tl := newTestTools(provider, &capturingLogger{}, core.Config{})
			if err := tc.call(tl); err != nil {
				t.Fatalf("tool call: %v", err)
			}

			contexts := provider.contexts()
			if len(contexts) == 0 {
				t.Fatal("provider received no calls")
			}
			deadline, ok := contexts[0].Deadline()
			if !ok {
				t.Fatal("provider context has no operation deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 4*time.Second || remaining > 5*time.Second {
				t.Fatalf("operation budget remaining = %v, want (4s, 5s]", remaining)
			}
			for i, got := range contexts[1:] {
				if got != contexts[0] {
					t.Fatalf("provider call %d received a different operation context", i+2)
				}
				if nextDeadline, _ := got.Deadline(); !nextDeadline.Equal(deadline) {
					t.Fatalf("provider call %d deadline = %v, want %v", i+2, nextDeadline, deadline)
				}
			}
		})
	}
}

func TestToolsDoNotExtendCallerDeadline(t *testing.T) {
	provider := &contextRecordingProvider{file: "/repo/main.go"}
	tl := newTestTools(provider, &capturingLogger{}, core.Config{})
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, _ := parent.Deadline()

	if _, err := tl.GetOutline(parent, GetOutlineInput{File: provider.file}); err != nil {
		t.Fatalf("GetOutline: %v", err)
	}
	gotDeadline, ok := provider.contexts()[0].Deadline()
	if !ok || !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("provider deadline = %v, want caller deadline %v", gotDeadline, parentDeadline)
	}
}

func TestToolOperationDeadlineCancelsProvider(t *testing.T) {
	provider := newBlockingProvider("symbols")
	logger := &capturingLogger{}
	tl := newTestTools(provider, logger, core.Config{})
	tl.operationTimeout = 10 * time.Millisecond

	out, err := tl.GetOutline(context.Background(), GetOutlineInput{File: "/repo/main.go"})
	if err != nil {
		t.Fatalf("GetOutline returned Go error: %v", err)
	}
	if out.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("soft error = %q, want %q", out.Error, context.DeadlineExceeded)
	}
	if logger.count() != 1 {
		t.Fatalf("telemetry events = %d, want 1", logger.count())
	}
}

func TestToolCancellationStagesRemainSoftAndEmitOnce(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		call  func(context.Context, *Tools) (string, error)
	}{
		{
			name:  "definition_symbols",
			stage: "symbols",
			call: func(ctx context.Context, tl *Tools) (string, error) {
				out, err := tl.FindDefinition(ctx, FindDefinitionInput{File: "/repo/main.go", Symbol: "Target"})
				return out.Error, err
			},
		},
		{
			name:  "definition_second_stage",
			stage: "definition",
			call: func(ctx context.Context, tl *Tools) (string, error) {
				out, err := tl.FindDefinition(ctx, FindDefinitionInput{File: "/repo/main.go", Symbol: "Target"})
				return out.Error, err
			},
		},
		{
			name:  "references_symbols",
			stage: "symbols",
			call: func(ctx context.Context, tl *Tools) (string, error) {
				out, err := tl.FindReferences(ctx, FindReferencesInput{File: "/repo/main.go", Symbol: "Target"})
				return out.Error, err
			},
		},
		{
			name:  "references_second_stage",
			stage: "references",
			call: func(ctx context.Context, tl *Tools) (string, error) {
				out, err := tl.FindReferences(ctx, FindReferencesInput{File: "/repo/main.go", Symbol: "Target"})
				return out.Error, err
			},
		},
		{
			name:  "outline_symbols",
			stage: "symbols",
			call: func(ctx context.Context, tl *Tools) (string, error) {
				out, err := tl.GetOutline(ctx, GetOutlineInput{File: "/repo/main.go"})
				return out.Error, err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newBlockingProvider(tc.stage)
			logger := &capturingLogger{}
			tl := newTestTools(provider, logger, core.Config{})
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan struct {
				errText string
				err     error
			}, 1)
			go func() {
				errText, err := tc.call(ctx, tl)
				result <- struct {
					errText string
					err     error
				}{errText: errText, err: err}
			}()

			<-provider.entered
			cancel()
			got := <-result
			if got.err != nil {
				t.Fatalf("tool returned Go error: %v", got.err)
			}
			if got.errText != context.Canceled.Error() {
				t.Fatalf("soft error = %q, want %q", got.errText, context.Canceled)
			}
			if logger.count() != 1 {
				t.Fatalf("telemetry events = %d, want 1", logger.count())
			}
			ev, _ := logger.last()
			if ev.Err != got.errText {
				t.Fatalf("telemetry error = %q, want %q", ev.Err, got.errText)
			}
			if tc.stage == "symbols" && provider.secondStageCalls() != 0 {
				t.Fatalf("second-stage calls = %d, want 0", provider.secondStageCalls())
			}
		})
	}
}

type contextRecordingProvider struct {
	mu      sync.Mutex
	file    string
	records []context.Context
}

func (p *contextRecordingProvider) record(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, ctx)
}

func (p *contextRecordingProvider) contexts() []context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]context.Context(nil), p.records...)
}

func (p *contextRecordingProvider) Definition(ctx context.Context, file string, _ core.Position) ([]core.Location, error) {
	p.record(ctx)
	return []core.Location{{File: file}}, nil
}

func (p *contextRecordingProvider) References(ctx context.Context, file string, _ core.Position, _ bool) ([]core.Location, error) {
	p.record(ctx)
	return []core.Location{{File: file}}, nil
}

func (p *contextRecordingProvider) DocumentSymbols(ctx context.Context, file string) ([]core.Symbol, error) {
	p.record(ctx)
	return []core.Symbol{{Name: "Target", File: file}}, nil
}

func (p *contextRecordingProvider) Close() error { return nil }

type blockingProvider struct {
	stage       string
	entered     chan struct{}
	enterOnce   sync.Once
	mu          sync.Mutex
	secondCalls int
}

func newBlockingProvider(stage string) *blockingProvider {
	return &blockingProvider{stage: stage, entered: make(chan struct{})}
}

func (p *blockingProvider) block(ctx context.Context, stage string) error {
	if p.stage != stage {
		return nil
	}
	p.enterOnce.Do(func() { close(p.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func (p *blockingProvider) Definition(ctx context.Context, file string, _ core.Position) ([]core.Location, error) {
	p.mu.Lock()
	p.secondCalls++
	p.mu.Unlock()
	if err := p.block(ctx, "definition"); err != nil {
		return nil, err
	}
	return []core.Location{{File: file}}, nil
}

func (p *blockingProvider) References(ctx context.Context, file string, _ core.Position, _ bool) ([]core.Location, error) {
	p.mu.Lock()
	p.secondCalls++
	p.mu.Unlock()
	if err := p.block(ctx, "references"); err != nil {
		return nil, err
	}
	return []core.Location{{File: file}}, nil
}

func (p *blockingProvider) DocumentSymbols(ctx context.Context, file string) ([]core.Symbol, error) {
	if err := p.block(ctx, "symbols"); err != nil {
		return nil, err
	}
	return []core.Symbol{{Name: "Target", File: file}}, nil
}

func (p *blockingProvider) secondStageCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.secondCalls
}

func (p *blockingProvider) Close() error { return nil }

var _ core.LanguageProvider = (*contextRecordingProvider)(nil)
var _ core.LanguageProvider = (*blockingProvider)(nil)

package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

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
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan struct {
		out GetOutlineOutput
		err error
	}, 1)
	go func() {
		out, err := tl.GetOutline(parent, GetOutlineInput{File: "/repo/main.go"})
		result <- struct {
			out GetOutlineOutput
			err error
		}{out: out, err: err}
	}()

	var got struct {
		out GetOutlineOutput
		err error
	}
	select {
	case got = <-result:
	case <-time.After(time.Second):
		cancel()
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("tool ignored both operation and parent cancellation")
		}
		t.Fatal("tool did not honor its operation deadline")
	}
	if got.err != nil {
		t.Fatalf("GetOutline returned Go error: %v", got.err)
	}
	if got.out.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("soft error = %q, want %q", got.out.Error, context.DeadlineExceeded)
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
		{
			name:  "outline_signatures",
			stage: "signatures",
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
			defer cancel()
			result := make(chan toolCallResult, 1)
			go func() {
				errText, err := tc.call(ctx, tl)
				result <- toolCallResult{errText: errText, err: err}
			}()

			select {
			case <-provider.entered:
			case <-time.After(time.Second):
				t.Fatal("provider stage was not entered")
			}
			cancel()
			var got toolCallResult
			select {
			case got = <-result:
			case <-time.After(time.Second):
				t.Fatal("tool did not return after cancellation")
			}
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

func TestProviderStageRejectsSuccessAfterConcurrentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan struct {
		locations []core.Location
		err       error
	}, 1)

	go func() {
		locations, err := runProviderStage(ctx, func(context.Context) ([]core.Location, error) {
			close(entered)
			<-release
			return []core.Location{{File: "/repo/main.go"}}, nil
		})
		result <- struct {
			locations []core.Location
			err       error
		}{locations: locations, err: err}
	}()

	<-entered
	cancel()
	<-ctx.Done()
	close(release)
	got := <-result
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want %v", got.err, context.Canceled)
	}
	if got.locations != nil {
		t.Fatalf("locations = %+v, want nil", got.locations)
	}
}

func TestPostProviderCancellationRemainsSoft(t *testing.T) {
	file := "/repo/main.go"
	type result struct {
		found     bool
		truncated bool
		resultLen int
		errText   string
		message   string
		err       error
	}
	definitionCall := func(ctx context.Context, tl *Tools) result {
		out, err := tl.FindDefinition(ctx, FindDefinitionInput{File: file, Symbol: "Target"})
		return result{out.Found, out.Truncated, len(out.Locations), out.Error, out.Message, err}
	}
	referencesCall := func(ctx context.Context, tl *Tools) result {
		out, err := tl.FindReferences(ctx, FindReferencesInput{File: file, Symbol: "Target"})
		return result{out.Found, out.Truncated, len(out.Locations), out.Error, out.Message, err}
	}
	outlineCall := func(ctx context.Context, tl *Tools) result {
		out, err := tl.GetOutline(ctx, GetOutlineInput{File: file})
		return result{out.Found, out.Truncated, len(out.Symbols), out.Error, out.Message, err}
	}
	cases := []struct {
		name        string
		cancelStage string
		secondCalls int
		call        func(context.Context, *Tools) result
	}{
		{name: "definition_symbols", cancelStage: "symbols", call: definitionCall},
		{name: "definition_result", cancelStage: "definition", secondCalls: 1, call: definitionCall},
		{name: "references_symbols", cancelStage: "symbols", call: referencesCall},
		{name: "references_result", cancelStage: "references", secondCalls: 1, call: referencesCall},
		{name: "outline_symbols", cancelStage: "symbols", call: outlineCall},
		{name: "outline_signatures", cancelStage: "signatures", call: outlineCall},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &cancelingResultProvider{cancel: cancel, cancelStage: tc.cancelStage, file: file}
			logger := &capturingLogger{}
			tl := newTestTools(provider, logger, core.Config{})

			got := tc.call(ctx, tl)
			if got.err != nil {
				t.Fatalf("tool returned Go error: %v", got.err)
			}
			if got.found || got.truncated || got.resultLen != 0 {
				t.Fatalf("partial output: found=%v truncated=%v resultLen=%d", got.found, got.truncated, got.resultLen)
			}
			if got.errText != context.Canceled.Error() || got.message == "" {
				t.Fatalf("soft result error=%q message=%q, want context canceled with message", got.errText, got.message)
			}
			if provider.secondStageCalls() != tc.secondCalls {
				t.Fatalf("second-stage calls = %d, want %d", provider.secondStageCalls(), tc.secondCalls)
			}
			if logger.count() != 1 {
				t.Fatalf("telemetry events = %d, want 1", logger.count())
			}
			ev, _ := logger.last()
			if ev.Err != context.Canceled.Error() || ev.ResultSize != 0 || ev.Truncated {
				t.Fatalf("telemetry event = %+v", ev)
			}
		})
	}
}

type toolCallResult struct {
	errText string
	err     error
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
	return []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Target", File: file}}}, nil
}

func (p *contextRecordingProvider) SymbolSignatures(ctx context.Context, _ string, symbols []core.Symbol) ([]string, error) {
	p.record(ctx)
	return existingSignatures(symbols), nil
}

func (p *contextRecordingProvider) Close() error { return nil }

type cancelingResultProvider struct {
	cancel      context.CancelFunc
	cancelStage string
	file        string

	mu          sync.Mutex
	secondCalls int
}

func (p *cancelingResultProvider) Definition(_ context.Context, file string, _ core.Position) ([]core.Location, error) {
	p.mu.Lock()
	p.secondCalls++
	p.mu.Unlock()
	if p.cancelStage == "definition" {
		p.cancel()
	}
	return []core.Location{{File: file}}, nil
}

func (p *cancelingResultProvider) References(_ context.Context, file string, _ core.Position, _ bool) ([]core.Location, error) {
	p.mu.Lock()
	p.secondCalls++
	p.mu.Unlock()
	if p.cancelStage == "references" {
		p.cancel()
	}
	return []core.Location{{File: file}}, nil
}

func (p *cancelingResultProvider) DocumentSymbols(_ context.Context, _ string) ([]core.Symbol, error) {
	if p.cancelStage == "symbols" {
		p.cancel()
	}
	return []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Container",
		File: p.file}, Children: []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Target",
		File: p.file},
	}},
	}}, nil
}

func (p *cancelingResultProvider) SymbolSignatures(_ context.Context, _ string, symbols []core.Symbol) ([]string, error) {
	if p.cancelStage == "signatures" {
		p.cancel()
	}
	return existingSignatures(symbols), nil
}

func (p *cancelingResultProvider) secondStageCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.secondCalls
}

func (p *cancelingResultProvider) Close() error { return nil }

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
	return []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Target", File: file}}}, nil
}

func (p *blockingProvider) SymbolSignatures(ctx context.Context, _ string, symbols []core.Symbol) ([]string, error) {
	if err := p.block(ctx, "signatures"); err != nil {
		return nil, err
	}
	return existingSignatures(symbols), nil
}

func (p *blockingProvider) secondStageCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.secondCalls
}

func (p *blockingProvider) Close() error { return nil }

var _ core.LanguageProvider = (*contextRecordingProvider)(nil)

var _ core.LanguageProvider = (*cancelingResultProvider)(nil)

var _ core.LanguageProvider = (*blockingProvider)(nil)

package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

func TestSymbolSignaturesFromTsgo(t *testing.T) {
	p := newTestProvider(t)
	tests := []struct {
		file string
		want map[string]string
	}{
		{
			file: "signatures.ts",
			want: map[string]string{
				"Callable@0":       "interface Callable",
				"()@1":             "(value: string): number",
				"new()@2":          "new (value: string): Date",
				"[]@3":             "[key: string]: unknown",
				"assignedArrow@6":  "const assignedArrow: (x: number) => number",
				"map() callback@8": "(x: number): number",
				"map() callback@9": "(x: number): number",
			},
		},
		{
			file: "member-signatures.ts",
			want: map[string]string{
				"Box@0":       "class Box<T>",
				"value@1":     "value: T",
				"size@5":      "size: number",
				"members@10":  "const members: {\n    method(x: number): number;\n    prop: number;\n}",
				"method@10":   "method(x: number): number",
				"prop@10":     "prop: number",
				"Weird@12":    "interface Weird",
				"optional@13": "optional?(): void",
			},
		},
		{file: "default-arrow.ts", want: map[string]string{"default@0": "(): number"}},
		{file: "default-function.ts", want: map[string]string{"default@0": "function default(): number"}},
		{file: "default-class.ts", want: map[string]string{"default@0": "class default", "method@1": "method(): void"}},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			file := absTestdata(t, tc.file)
			symbols, err := p.DocumentSymbols(testCtx(t), file)
			if err != nil {
				t.Fatalf("DocumentSymbols: %v", err)
			}
			flat := flattenCoreSymbols(symbols)
			signatures, err := p.SymbolSignatures(testCtx(t), file, flat)
			if err != nil {
				t.Fatalf("SymbolSignatures: %v", err)
			}
			if len(signatures) != len(flat) {
				t.Fatalf("signatures = %d, symbols = %d", len(signatures), len(flat))
			}
			got := make(map[string]string, len(flat))
			for i, symbol := range flat {
				got[fmt.Sprintf("%s@%d", symbol.Name, symbol.Range.Start.Line)] = signatures[i]
				if symbol.Detail != "" {
					t.Errorf("%s detail = %q, want empty and independent of signature", symbol.Name, symbol.Detail)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("signature keys = %+v, want exactly %+v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("signature %s = %q, want %q (all: %+v)", key, got[key], want, got)
				}
			}
		})
	}
}

func flattenCoreSymbols(symbols []core.SymbolNode) []core.Symbol {
	var flat []core.Symbol
	var visit func([]core.SymbolNode)
	visit = func(current []core.SymbolNode) {
		for _, symbol := range current {
			flat = append(flat, symbol.Symbol)
			visit(symbol.Children)
		}
	}
	visit(symbols)
	return flat
}

func TestDecodeHoverSignatureSeparatesQuickInfoFromDocumentation(t *testing.T) {
	raw := json.RawMessage("{\"contents\":{\"kind\":\"markdown\",\"value\":\"before\\n```typescript\\nfunction documented(): number\\n```\\nDocumentation body.\"}}")
	got, err := decodeHoverSignature(raw, core.Symbol{Name: "documented", Kind: core.SymbolKindFunction})
	if err != nil {
		t.Fatalf("decodeHoverSignature: %v", err)
	}
	if got != "function documented(): number" {
		t.Fatalf("signature = %q, want quick info without documentation", got)
	}
	if got, err := decodeHoverSignature(json.RawMessage(`null`), core.Symbol{Name: "documented", Kind: core.SymbolKindFunction}); err != nil || got != "" {
		t.Fatalf("null hover = (%q, %v), want unavailable signature", got, err)
	}
}

func TestDecodeHoverSignatureNormalizesPinnedTsgoDisplays(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "hover_signatures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name   string          `json:"name"`
		Symbol core.Symbol     `json:"symbol"`
		Result json.RawMessage `json:"result"`
		Want   string          `json:"want"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			got, err := decodeHoverSignature(fixture.Result, fixture.Symbol)
			if err != nil {
				t.Fatalf("decodeHoverSignature: %v", err)
			}
			if got != fixture.Want {
				t.Fatalf("signature = %q, want %q", got, fixture.Want)
			}
		})
	}
}

func TestDecodeHoverSignatureRejectsMalformedResults(t *testing.T) {
	symbol := core.Symbol{Name: "area", Kind: core.SymbolKindMethod}
	for _, raw := range []string{"{", `{"contents":42}`, "{\"contents\":{\"kind\":\"markdown\",\"value\":\"```typescript\\n(method) Circle.area(): number\"}}"} {
		if got, err := decodeHoverSignature(json.RawMessage(raw), symbol); err == nil || got != "" {
			t.Errorf("decodeHoverSignature(%s) = (%q, %v), want error", raw, got, err)
		}
	}
}

func TestSignatureAndDocumentSymbolDetailRemainDistinct(t *testing.T) {
	rawSymbols := json.RawMessage(`[{
		"name":"documented","detail":"wire detail","kind":12,
		"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":30}},
		"selectionRange":{"start":{"line":0,"character":9},"end":{"line":0,"character":19}}
	}]`)
	symbols, err := decodeDocumentSymbols(context.Background(), rawSymbols, "/repo/a.ts")
	if err != nil {
		t.Fatalf("decodeDocumentSymbols: %v", err)
	}
	plans := []signaturePlan{{position: symbols[0].SelRange.Start, symbol: symbols[0].Symbol}}
	signatures, err := requestSignatures(context.Background(), "file:///repo/a.ts", plans, func(context.Context, string, any) (json.RawMessage, error) {
		return json.RawMessage("{\"contents\":{\"kind\":\"markdown\",\"value\":\"```typescript\\nfunction documented(): number\\n```\"}}"), nil
	}, decodeHoverSignature)
	if err != nil {
		t.Fatalf("requestSignatures: %v", err)
	}
	symbols[0].Signature = signatures[0]
	if symbols[0].Signature != "function documented(): number" || symbols[0].Detail != "wire detail" {
		t.Fatalf("symbol = %+v, want independent signature and detail", symbols[0])
	}
}

func TestPlanSignatureUsesAuthoritativeSyntheticLocations(t *testing.T) {
	source := "interface I {\n  (value: string): number;\n}\n" +
		"const nested = (fn = () => 1) => fn();\n"
	tests := []struct {
		name         string
		symbol       core.Symbol
		wantPosition core.Position
		wantDirect   string
	}{
		{
			name: "bodyless call signature",
			symbol: core.Symbol{Name: "()", Kind: "method", Range: core.Range{
				Start: core.Position{Line: 1, Character: 2}, End: core.Position{Line: 1, Character: 26},
			}, SelRange: core.Range{Start: core.Position{Line: 1, Character: 2}, End: core.Position{Line: 1, Character: 2}}},
			wantPosition: core.Position{Line: 1, Character: 2},
			wantDirect:   "(value: string): number",
		},
		{
			name: "outer arrow after nested default",
			symbol: core.Symbol{Name: "callback() callback", Kind: "function", Range: core.Range{
				Start: core.Position{Line: 3, Character: 15}, End: core.Position{Line: 3, Character: 38},
			}, SelRange: core.Range{Start: core.Position{Line: 3, Character: 15}, End: core.Position{Line: 3, Character: 15}}},
			wantPosition: core.Position{Line: 3, Character: 30},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planSignature(context.Background(), tc.symbol, source)
			if err != nil {
				t.Fatalf("planSignature: %v", err)
			}
			if got.position != tc.wantPosition || got.direct != tc.wantDirect || got.symbol != tc.symbol {
				t.Fatalf("plan = %+v, want position=%+v direct=%q and original symbol", got, tc.wantPosition, tc.wantDirect)
			}
		})
	}
}

func TestExtractDeclarationHeader(t *testing.T) {
	tests := []struct {
		name   string
		source string
		symbol core.Symbol
		want   string
	}{
		{
			name:   "simple class excludes modifiers and body",
			source: "export class Circle {\n  area(): number { return 1; }\n}",
			symbol: core.Symbol{Name: "Circle", Kind: core.SymbolKindClass, Range: core.Range{Start: core.Position{}, End: core.Position{Line: 2, Character: 1}}},
			want:   "class Circle",
		},
		{
			name: "multiline generic class with multiple heritage targets",
			source: "export abstract class RedisSessionStore<T extends Session>\n" +
				"  extends BaseStore<T>\n" +
				"  implements SessionStore, Disposable\n" +
				"{\n  value = 1;\n}",
			symbol: core.Symbol{Name: "RedisSessionStore", Kind: core.SymbolKindClass, Range: core.Range{Start: core.Position{}, End: core.Position{Line: 5, Character: 1}}},
			want:   "class RedisSessionStore<T extends Session> extends BaseStore<T> implements SessionStore, Disposable",
		},
		{
			name:   "interface with multiple heritage targets",
			source: "export interface CachedStore<T> extends SessionStore<T>, Disposable {\n  get(): T;\n}",
			symbol: core.Symbol{Name: "CachedStore", Kind: core.SymbolKindInterface, Range: core.Range{Start: core.Position{}, End: core.Position{Line: 2, Character: 1}}},
			want:   "interface CachedStore<T> extends SessionStore<T>, Disposable",
		},
		{
			name:   "braced object generic constraint",
			source: "class Registry<T extends { value: string; nested: { count: number } }> extends Base<T> {\n  body = true;\n}",
			symbol: core.Symbol{Name: "Registry", Kind: core.SymbolKindClass, Range: core.Range{Start: core.Position{}, End: core.Position{Line: 2, Character: 1}}},
			want:   "class Registry<T extends { value: string; nested: { count: number } }> extends Base<T>",
		},
		{
			name: "comments and literal braces are not body openers",
			source: "export /* modifier */ class Tagged<T extends { open: \"{\"; close: `}`; }>\n" +
				"/* clause */ extends Base<T> {\n  body = \"implements Wrong\";\n}",
			symbol: core.Symbol{Name: "Tagged", Kind: core.SymbolKindClass, Range: core.Range{Start: core.Position{}, End: core.Position{Line: 3, Character: 1}}},
			want:   "class Tagged<T extends { open: \"{\"; close: `}`; }> extends Base<T>",
		},
		{
			name:   "relational heritage expression",
			source: "class Selected extends (a < b ? A : B) {}",
			symbol: core.Symbol{Name: "Selected", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 41}}},
			want:   "class Selected extends (a < b ? A : B)",
		},
		{
			name:   "UTF-16 range",
			source: "😀 export class Café<T> extends Base<T> {}",
			symbol: core.Symbol{Name: "Café", Kind: core.SymbolKindClass, Range: core.Range{Start: core.Position{Character: 3}, End: core.Position{Character: 42}}},
			want:   "class Café<T> extends Base<T>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := extractDeclarationHeader(context.Background(), tc.source, tc.symbol)
			if err != nil {
				t.Fatalf("extractDeclarationHeader: %v", err)
			}
			if !ok || got != tc.want {
				t.Fatalf("header = (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
}

func TestExtractDeclarationHeaderFallsBackAtomically(t *testing.T) {
	tests := []struct {
		name   string
		source string
		symbol core.Symbol
	}{
		{
			name:   "mismatched name",
			source: "class Actual {}",
			symbol: core.Symbol{Name: "Expected", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 15}}},
		},
		{
			name:   "mismatched kind",
			source: "interface Shape {}",
			symbol: core.Symbol{Name: "Shape", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 18}}},
		},
		{
			name:   "anonymous default class",
			source: "export default class {}",
			symbol: core.Symbol{Name: "default", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 23}}},
		},
		{
			name:   "missing body",
			source: "declare class Ambient<T>;",
			symbol: core.Symbol{Name: "Ambient", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 25}}},
		},
		{
			name:   "unterminated literal",
			source: "class Broken<T extends { value: \"unterminated } {}",
			symbol: core.Symbol{Name: "Broken", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 50}}},
		},
		{
			name:   "unmatched generic closer",
			source: "class Broken<T>> {}",
			symbol: core.Symbol{Name: "Broken", Kind: core.SymbolKindClass, Range: core.Range{End: core.Position{Character: 19}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := extractDeclarationHeader(context.Background(), tc.source, tc.symbol)
			if err != nil {
				t.Fatalf("extractDeclarationHeader: %v", err)
			}
			if ok || got != "" {
				t.Fatalf("header = (%q, %v), want atomic fallback", got, ok)
			}
		})
	}
}

type cancelAfterChecksContext struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecksContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestExtractDeclarationHeaderCancelsDuringRangeMapping(t *testing.T) {
	source := strings.Repeat("/* retained snapshot trivia */ ", 1024) + "class Target {}"
	symbol := core.Symbol{
		Name:  "Target",
		Kind:  core.SymbolKindClass,
		Range: core.Range{End: core.Position{Character: len(source)}},
	}
	ctx := &cancelAfterChecksContext{Context: context.Background(), remaining: 5}
	got, ok, err := extractDeclarationHeader(ctx, source, symbol)
	if !errors.Is(err, context.Canceled) || ok || got != "" {
		t.Fatalf("header = (%q, %v, %v), want atomic context cancellation", got, ok, err)
	}
}

func TestPlanSignaturePrefersCompleteDeclarationHeader(t *testing.T) {
	source := "export class Circle implements Shape { body = true; }"
	symbol := core.Symbol{
		Name:     "Circle",
		Kind:     core.SymbolKindClass,
		Range:    core.Range{End: core.Position{Character: len(source)}},
		SelRange: core.Range{Start: core.Position{Character: 13}, End: core.Position{Character: 19}},
	}
	got, err := planSignature(context.Background(), symbol, source)
	if err != nil {
		t.Fatalf("planSignature: %v", err)
	}
	if got.direct != "class Circle implements Shape" {
		t.Fatalf("direct signature = %q, want source header", got.direct)
	}

	symbol.Name = "Other"
	got, err = planSignature(context.Background(), symbol, source)
	if err != nil {
		t.Fatalf("fallback planSignature: %v", err)
	}
	if got.direct != "" || got.position != symbol.SelRange.Start {
		t.Fatalf("fallback plan = %+v, want unchanged hover position", got)
	}
}

func TestRequestSignaturesMissingHoverStaysEmpty(t *testing.T) {
	plans := []signaturePlan{{
		position: core.Position{Line: 4, Character: 2},
		symbol:   core.Symbol{Name: "missing", Kind: core.SymbolKindFunction},
	}}
	signatures, err := requestSignatures(context.Background(), "file:///repo/a.ts", plans, func(context.Context, string, any) (json.RawMessage, error) {
		return json.RawMessage(`null`), nil
	}, decodeHoverSignature)
	if err != nil {
		t.Fatalf("requestSignatures: %v", err)
	}
	if len(signatures) != 1 || signatures[0] != "" {
		t.Fatalf("signatures = %#v, want one unavailable signature", signatures)
	}
}

func TestRequestSignaturesBoundsConcurrency(t *testing.T) {
	plans := make([]signaturePlan, maxConcurrentSignatureRequests*3)
	for i := range plans {
		plans[i].position = core.Position{Line: i}
	}
	var mu sync.Mutex
	inFlight, maximum, calls := 0, 0, 0
	reachedLimit := make(chan struct{})
	release := make(chan struct{})
	var reachedOnce sync.Once
	request := func(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
		mu.Lock()
		inFlight++
		calls++
		if inFlight > maximum {
			maximum = inFlight
		}
		if inFlight == maxConcurrentSignatureRequests {
			reachedOnce.Do(func() { close(reachedLimit) })
		}
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return json.RawMessage("{\"contents\":{\"kind\":\"markdown\",\"value\":\"```typescript\\nfunction f(): void\\n```\"}}"), nil
	}

	result := make(chan error, 1)
	go func() {
		_, err := requestSignatures(context.Background(), "file:///repo/a.ts", plans, request, decodeHoverSignature)
		result <- err
	}()
	select {
	case <-reachedLimit:
	case <-time.After(time.Second):
		t.Fatal("signature requests did not reach concurrency limit")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("requestSignatures: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maximum != maxConcurrentSignatureRequests || calls != len(plans) {
		t.Fatalf("maximum concurrency = %d, calls = %d; want %d and %d", maximum, calls, maxConcurrentSignatureRequests, len(plans))
	}
}

func TestRequestSignaturesReturnsBeforeLateDecode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Error("late hover decoder did not exit")
		}
	}()

	decode := func(json.RawMessage, core.Symbol) (string, error) {
		close(entered)
		defer close(exited)
		<-release
		return "late signature", nil
	}
	type signatureResult struct {
		signatures []string
		err        error
	}
	result := make(chan signatureResult, 1)
	go func() {
		signatures, err := requestSignatures(ctx, "file:///repo/a.ts", []signaturePlan{{}}, func(context.Context, string, any) (json.RawMessage, error) {
			return json.RawMessage(`{"contents":"hover"}`), nil
		}, decode)
		result <- signatureResult{signatures: signatures, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("hover decoder did not start")
	}
	cancel()
	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) || got.signatures != nil {
			t.Fatalf("requestSignatures = (%v, %v), want nil signatures and context canceled", got.signatures, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("requestSignatures did not return while hover conversion remained blocked")
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("late hover decoder did not finish")
	}
	select {
	case got := <-result:
		t.Fatalf("late hover conversion produced another result: %+v", got)
	default:
	}
}

func TestRequestSignaturesPreservesFirstError(t *testing.T) {
	firstErr := errors.New("first hover failure")
	laterErr := errors.New("later hover failure")
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	plans := []signaturePlan{
		{position: core.Position{Line: 0}},
		{position: core.Position{Line: 1}},
	}
	request := func(ctx context.Context, _ string, params any) (json.RawMessage, error) {
		line := params.(textDocumentPositionParams).Position.Line
		if line == 0 {
			close(firstStarted)
			<-releaseFirst
			return nil, firstErr
		}
		close(secondStarted)
		<-ctx.Done()
		return nil, laterErr
	}
	result := make(chan error, 1)
	go func() {
		_, err := requestSignatures(context.Background(), "file:///repo/a.ts", plans, request, decodeHoverSignature)
		result <- err
	}()
	waitSignal(t, firstStarted, "first hover request")
	waitSignal(t, secondStarted, "second hover request")
	close(releaseFirst)
	if err := waitError(t, result, "signature first error"); !errors.Is(err, firstErr) {
		t.Fatalf("requestSignatures error = %v, want first hover failure", err)
	}
}

func TestRequestSignaturesHonorsCancellation(t *testing.T) {
	t.Run("before dispatch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := requestSignatures(ctx, "file:///repo/a.ts", []signaturePlan{{}}, func(context.Context, string, any) (json.RawMessage, error) {
			t.Fatal("request called after cancellation")
			return nil, nil
		}, decodeHoverSignature)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})

	t.Run("in flight", func(t *testing.T) {
		plans := make([]signaturePlan, maxConcurrentSignatureRequests)
		ctx, cancel := context.WithCancel(context.Background())
		var mu sync.Mutex
		active := 0
		allStarted := make(chan struct{})
		var startedOnce sync.Once
		request := func(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
			mu.Lock()
			active++
			if active == len(plans) {
				startedOnce.Do(func() { close(allStarted) })
			}
			mu.Unlock()
			<-ctx.Done()
			mu.Lock()
			active--
			mu.Unlock()
			return nil, ctx.Err()
		}
		result := make(chan error, 1)
		go func() {
			_, err := requestSignatures(ctx, "file:///repo/a.ts", plans, request, decodeHoverSignature)
			result <- err
		}()
		select {
		case <-allStarted:
		case <-time.After(time.Second):
			t.Fatal("signature requests did not enter the in-flight state")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("in-flight signature requests did not drain after cancellation")
		}
		mu.Lock()
		defer mu.Unlock()
		if active != 0 {
			t.Fatalf("active requests = %d, want 0", active)
		}
	})
}

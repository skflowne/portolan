package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

func definitionLocation(file string, sl, sc, el, ec int) core.Location {
	return core.Location{
		File: file,
		Range: core.Range{
			Start: core.Position{Line: sl, Character: sc},
			End:   core.Position{Line: el, Character: ec},
		},
	}
}

func definitionSymbol(file, name string, full, selection core.Range) core.SymbolNode {
	return core.SymbolNode{Symbol: core.Symbol{
		Name: name, File: file, Range: full, SelRange: selection,
	}}
}

func TestEnrichDefinitionSourcesPreservesInputOrderAndLoadsEachFileOnce(t *testing.T) {
	const (
		fileA   = "/repo/a.ts"
		fileB   = "/repo/b.ts"
		sourceA = "export function first(): string {\r\n  return `héllo`;\r\n}\r\n\r\nexport function second(): number {\r\n  return 2;\r\n}\r\n"
		sourceB = "export const other = () => {\n  return 3;\n};\n"
	)
	firstRange := core.Range{Start: core.Position{}, End: core.Position{Line: 2, Character: 1}}
	firstSelection := core.Range{Start: core.Position{Character: 16}, End: core.Position{Character: 21}}
	secondRange := core.Range{Start: core.Position{Line: 4}, End: core.Position{Line: 6, Character: 1}}
	secondSelection := core.Range{Start: core.Position{Line: 4, Character: 16}, End: core.Position{Line: 4, Character: 22}}
	otherRange := core.Range{Start: core.Position{}, End: core.Position{Line: 2, Character: 2}}
	otherSelection := core.Range{Start: core.Position{Character: 13}, End: core.Position{Character: 18}}
	locations := []core.Location{
		{File: fileA, Range: secondSelection},
		{File: fileB, Range: otherRange},
		{File: fileA, Range: firstSelection},
	}

	loads := make(map[string]int)
	var loadsMu sync.Mutex
	load := func(_ context.Context, file string) (definitionSnapshot, error) {
		loadsMu.Lock()
		loads[file]++
		loadsMu.Unlock()
		switch file {
		case fileA:
			return definitionSnapshot{source: sourceA, symbols: []core.SymbolNode{
				definitionSymbol(fileA, "first", firstRange, firstSelection),
				definitionSymbol(fileA, "second", secondRange, secondSelection),
			}}, nil
		case fileB:
			return definitionSnapshot{source: sourceB, symbols: []core.SymbolNode{
				definitionSymbol(fileB, "other", otherRange, otherSelection),
			}}, nil
		default:
			return definitionSnapshot{}, errors.New("unexpected file")
		}
	}

	got, err := enrichDefinitionSources(context.Background(), locations, load)
	if err != nil {
		t.Fatalf("enrichDefinitionSources: %v", err)
	}
	want := []core.Definition{
		{Target: locations[0], DeclarationRange: secondRange, Source: "export function second(): number {\r\n  return 2;\r\n}"},
		{Target: locations[1], DeclarationRange: otherRange, Source: sourceB[:len(sourceB)-1]},
		{Target: locations[2], DeclarationRange: firstRange, Source: "export function first(): string {\r\n  return `héllo`;\r\n}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definitions = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(loads, map[string]int{fileA: 1, fileB: 1}) {
		t.Fatalf("loads = %v, want one per distinct target", loads)
	}
}

func TestEnrichDefinitionSourcesReturnsAtomicMappingAndExtractionErrors(t *testing.T) {
	file := "/repo/a.ts"
	validSelection := core.Range{Start: core.Position{Character: 6}, End: core.Position{Character: 8}}
	validRange := core.Range{Start: core.Position{}, End: core.Position{Character: 11}}
	valid := definitionLocation(file, 0, 6, 0, 8)

	tests := []struct {
		name      string
		locations []core.Location
		snapshot  definitionSnapshot
		wantError string
	}{
		{
			name:      "missing exact match after valid target",
			locations: []core.Location{valid, definitionLocation(file, 4, 0, 4, 3)},
			snapshot: definitionSnapshot{source: "const ok=1;", symbols: []core.SymbolNode{
				definitionSymbol(file, "ok", validRange, validSelection),
			}},
			wantError: "does not match a declaration",
		},
		{
			name:      "ambiguous exact match",
			locations: []core.Location{valid},
			snapshot: definitionSnapshot{source: "const ok=1;", symbols: []core.SymbolNode{
				definitionSymbol(file, "ok", validRange, validSelection),
				definitionSymbol(file, "ok", validRange, validSelection),
			}},
			wantError: "matches multiple declarations",
		},
		{
			name:      "range outside retained source",
			locations: []core.Location{valid},
			snapshot: definitionSnapshot{source: "const ok=1;", symbols: []core.SymbolNode{
				definitionSymbol(file, "ok", core.Range{Start: core.Position{}, End: core.Position{Line: 3}}, validSelection),
			}},
			wantError: "extracting declaration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := enrichDefinitionSources(context.Background(), tc.locations, func(context.Context, string) (definitionSnapshot, error) {
				return tc.snapshot, nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantError)
			}
			if got != nil {
				t.Fatalf("definitions = %#v, want nil on atomic failure", got)
			}
		})
	}
}

func TestEnrichDefinitionSourcesRejectsCancellationAfterLoaderCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	location := definitionLocation("/repo/a.ts", 0, 6, 0, 8)
	selection := location.Range
	full := core.Range{Start: core.Position{}, End: core.Position{Character: 11}}
	got, err := enrichDefinitionSources(ctx, []core.Location{location}, func(context.Context, string) (definitionSnapshot, error) {
		cancel()
		return definitionSnapshot{
			source:  "const ok=1;",
			symbols: []core.SymbolNode{definitionSymbol(location.File, "ok", full, selection)},
		}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if got != nil {
		t.Fatalf("definitions = %#v, want nil after cancellation", got)
	}
}

func TestEnrichDefinitionSourcesBoundsDistinctFileWork(t *testing.T) {
	const files = maxConcurrentProviderRequests + 3
	locations := make([]core.Location, files)
	for i := range locations {
		locations[i] = definitionLocation(filepath.Join("/repo", string(rune('a'+i))+".ts"), 0, 6, 0, 8)
	}
	var mu sync.Mutex
	active, peak := 0, 0
	release := make(chan struct{})
	entered := make(chan struct{}, maxConcurrentProviderRequests)
	load := func(_ context.Context, file string) (definitionSnapshot, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		selection := core.Range{Start: core.Position{Character: 6}, End: core.Position{Character: 8}}
		return definitionSnapshot{
			source:  "const ok=1;",
			symbols: []core.SymbolNode{definitionSymbol(file, "ok", core.Range{Start: core.Position{}, End: core.Position{Character: 11}}, selection)},
		}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := enrichDefinitionSources(context.Background(), locations, load)
		done <- err
	}()
	for range maxConcurrentProviderRequests {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("bounded workers did not start")
		}
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("more than the configured number of target files loaded concurrently")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("enrichDefinitionSources: %v", err)
	}
	if peak != maxConcurrentProviderRequests {
		t.Fatalf("peak loaders = %d, want %d", peak, maxConcurrentProviderRequests)
	}
}

func TestDefinitionSourcesUsesRetainedDidOpenSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tsgo subprocess")
	}
	root := t.TempDir()
	original := "export function greet(name: string): string {\r\n  return `Hello ${name} ☃`;\r\n}\r\n"
	files := map[string]string{
		"a.ts":          original,
		"b.ts":          "import { greet } from \"./a\";\nexport const result = greet(\"World\");\n",
		"tsconfig.json": `{"compilerOptions":{"strict":true,"noEmit":true},"include":["*.ts"]}`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	p, err := New(core.Config{ProjectRoot: root})
	if err != nil {
		t.Fatalf("lsp.New: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Provider.Close: %v", err)
		}
	})

	aFile := filepath.Join(root, "a.ts")
	bFile := filepath.Join(root, "b.ts")
	if _, err := p.DocumentSymbols(testCtx(t), aFile); err != nil {
		t.Fatalf("opening target snapshot: %v", err)
	}
	locations, err := p.Definition(testCtx(t), bFile, core.Position{Line: 1, Character: 25})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("locations = %#v, want one", locations)
	}
	if err := os.WriteFile(aFile, []byte("export const changed = true;\n"), 0o600); err != nil {
		t.Fatalf("change target on disk: %v", err)
	}

	definitions, err := p.DefinitionSources(testCtx(t), locations)
	if err != nil {
		t.Fatalf("DefinitionSources: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("definitions = %#v, want one", definitions)
	}
	wantSource := strings.TrimSuffix(original, "\r\n")
	if definitions[0].Target != locations[0] || definitions[0].Source != wantSource || definitions[0].DeclarationRange.Start != (core.Position{}) || definitions[0].DeclarationRange.End != (core.Position{Line: 2, Character: 1}) {
		t.Fatalf("definition = %#v, want target %#v, retained source %q, range [0:0-2:1]", definitions[0], locations[0], wantSource)
	}
}

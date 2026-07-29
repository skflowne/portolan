package lsp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

// newTestProvider spawns a real tsgo --lsp -stdio subprocess rooted at
// testdata, and registers a cleanup to close it. Tests degrade to a skip if
// tsgo isn't on PATH (it is expected to be, in the harness's environment).
func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	if _, err := exec.LookPath("tsgo"); err != nil {
		t.Skip("tsgo not found on PATH; skipping LSP integration test")
	}

	root, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolving testdata dir: %v", err)
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
	return p
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func absTestdata(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("resolving %s: %v", name, err)
	}
	return abs
}

// TestDocumentSymbols asserts DocumentSymbols finds the `greet` function
// declared in a.ts, with the expected kind.
func TestDocumentSymbols(t *testing.T) {
	p := newTestProvider(t)
	aFile := absTestdata(t, "a.ts")

	syms, err := p.DocumentSymbols(testCtx(t), aFile)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) == 0 {
		t.Fatalf("expected at least one symbol in %s, got none", aFile)
	}

	var found *core.Symbol
	for i := range syms {
		if syms[i].Name == "greet" {
			found = &syms[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected to find symbol %q, got %+v", "greet", syms)
	}
	if found.Kind != "function" {
		t.Errorf("expected kind %q for greet, got %q", "function", found.Kind)
	}
	if found.File != aFile {
		t.Errorf("expected symbol File %q, got %q", aFile, found.File)
	}
}

// TestDefinition asserts that resolving the use-site of `greet` in b.ts
// (inside the call console.log(greet("World"))) lands on its declaration in
// a.ts, line 0.
func TestDocumentSymbolsUseUTF16CharacterOffsets(t *testing.T) {
	p := newTestProvider(t)
	file := absTestdata(t, "unicode-position.ts")

	symbols, err := p.DocumentSymbols(testCtx(t), file)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	for _, symbol := range symbols {
		if symbol.Name == "target" {
			if symbol.SelRange.Start != (core.Position{Line: 0, Character: 20}) {
				t.Fatalf("target selection start = %+v, want UTF-16 position {Line:0 Character:20}", symbol.SelRange.Start)
			}
			return
		}
	}
	t.Fatalf("target symbol not found in %+v", symbols)
}

func TestDefinition(t *testing.T) {
	p := newTestProvider(t)
	aFile := absTestdata(t, "a.ts")
	bFile := absTestdata(t, "b.ts")

	// Line 3: `  console.log(greet("World"));` — "greet" spans chars 14-18.
	locs, err := p.Definition(testCtx(t), bFile, core.Position{Line: 3, Character: 16})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) == 0 {
		t.Fatalf("expected at least one definition location, got none")
	}

	found := false
	for _, l := range locs {
		if l.File == aFile && l.Range.Start.Line == 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a definition in %s at line 0, got %+v", aFile, locs)
	}
}

// TestReferences asserts that querying references from the declaration site
// of `greet` in a.ts finds both the declaration itself (includeDeclaration:
// true) and the use-site in b.ts.
func TestReferences(t *testing.T) {
	p := newTestProvider(t)
	aFile := absTestdata(t, "a.ts")
	bFile := absTestdata(t, "b.ts")

	// Line 0: `export function greet(name: string): string {` — "greet" spans
	// chars 16-20.
	locs, err := p.References(testCtx(t), aFile, core.Position{Line: 0, Character: 18}, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(locs) == 0 {
		t.Fatalf("expected references, got none")
	}

	var foundDecl, foundUse bool
	for _, l := range locs {
		if l.File == aFile {
			foundDecl = true
		}
		if l.File == bFile {
			foundUse = true
		}
	}
	if !foundDecl {
		t.Errorf("expected declaration reference in %s, got %+v", aFile, locs)
	}
	if !foundUse {
		t.Errorf("expected use-site reference in %s, got %+v", bFile, locs)
	}
}

func TestDefinitionPreservesEscapedCanonicalPathOverLSP(t *testing.T) {
	if _, err := exec.LookPath("tsgo"); err != nil {
		t.Skip("tsgo not found on PATH; skipping LSP integration test")
	}

	root := filepath.Join(t.TempDir(), "project space ☃")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create project: %v", err)
	}
	files := map[string]string{
		"a.ts":          "export function greet(name: string): string {\n  return name;\n}\n",
		"b.ts":          "import { greet } from \"./a\";\n\nexport const result = greet(\"World\");\n",
		"tsconfig.json": `{"compilerOptions":{"strict":true,"noEmit":true},"include":["*.ts"]}`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	p, err := New(core.Config{ProjectRoot: root + "/./"})
	if err != nil {
		t.Fatalf("lsp.New: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Provider.Close: %v", err)
		}
	})

	bFile := filepath.Join(root, "nested", "..", "b.ts")
	locations, err := p.Definition(testCtx(t), bFile, core.Position{Line: 2, Character: 24})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	wantFile := filepath.Join(root, "a.ts")
	for _, location := range locations {
		if location.File == wantFile && location.Range.Start.Line == 0 {
			return
		}
	}
	t.Fatalf("expected canonical definition in %q, got %+v", wantFile, locations)
}

// TestDefinitionNoResult asserts the "honest null" contract: querying a
// position with no symbol (a blank line) returns (nil, nil), not an error.
func TestDefinitionNoResult(t *testing.T) {
	p := newTestProvider(t)
	bFile := absTestdata(t, "b.ts")

	// Line 1 of b.ts is blank.
	locs, err := p.Definition(testCtx(t), bFile, core.Position{Line: 1, Character: 0})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if locs != nil {
		t.Errorf("expected nil locations on a blank line, got %+v", locs)
	}
}

// TestConcurrentQueries drives Definition, References, and DocumentSymbols
// from many goroutines against a single shared Provider, asserting the
// concurrency-safety the core.LanguageProvider contract requires (no panics,
// no hangs, no cross-request data corruption).
func TestConcurrentQueries(t *testing.T) {
	p := newTestProvider(t)
	aFile := absTestdata(t, "a.ts")
	bFile := absTestdata(t, "b.ts")

	const workers = 20
	var wg sync.WaitGroup
	errCh := make(chan error, workers*3)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := testCtx(t)

			if _, err := p.DocumentSymbols(ctx, aFile); err != nil {
				errCh <- err
				return
			}
			locs, err := p.Definition(ctx, bFile, core.Position{Line: 3, Character: 16})
			if err != nil {
				errCh <- err
				return
			}
			if len(locs) == 0 {
				errCh <- errors.New("expected definition locations, got none")
				return
			}
			if _, err := p.References(ctx, aFile, core.Position{Line: 0, Character: 18}, true); err != nil {
				errCh <- err
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent query failed: %v", err)
	}
}

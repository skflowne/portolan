package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

// FindReferencesInput is the input schema for find_references.
type FindReferencesInput struct {
	File   string `json:"file" jsonschema:"absolute path of the source file containing the symbol"`
	Symbol string `json:"symbol" jsonschema:"the symbol name to find references to (function, method, type, variable, etc.)"`
	Line   *int   `json:"line,omitempty" jsonschema:"optional 0-based line number, used to disambiguate when the symbol name occurs more than once in the file"`
}

// FindReferencesOutput is the output schema for find_references.
type FindReferencesOutput struct {
	// Found is true iff at least one reference location was returned. Both
	// "symbol name did not resolve" and "resolved but has no references" are
	// honest, non-error results: Found is false and Message explains why.
	Found     bool            `json:"found"`
	Locations []core.Location `json:"locations"`
	Truncated bool            `json:"truncated"`
	Freshness core.Freshness  `json:"freshness"`
	Message   string          `json:"message,omitempty"`
	// Error is set only when the underlying provider call itself failed
	// (a soft error — the call never panics or returns a Go error for this).
	Error string `json:"error,omitempty"`
}

// FindReferences resolves input.Symbol to a Position via DocumentSymbols,
// then calls provider.References with includeDeclaration=true. See the
// package doc for the shared found/error/cap/freshness/telemetry contract.
func (t *Tools) FindReferences(ctx context.Context, in FindReferencesInput) (FindReferencesOutput, error) {
	ctx, cancel := t.operationContext(ctx)
	defer cancel()

	start := time.Now()
	fresh := t.Gen.Current()
	out := FindReferencesOutput{Freshness: fresh}
	ev := core.Event{
		SessionID:  t.Cfg.SessionID,
		GraphMode:  t.Cfg.GraphMode,
		Tool:       "find_references",
		Generation: fresh.Generation,
		Stale:      fresh.Stale,
	}

	file := t.normFile(in.File)
	symbols, err := t.Provider.DocumentSymbols(ctx, file)
	if err != nil {
		out.Error = err.Error()
		out.Message = fmt.Sprintf("failed to load symbols for %s", file)
		t.emit(ctx, &ev, start, 0, false, err.Error())
		return out, nil
	}

	pos, ok := resolveSymbolPosition(symbols, in.Symbol, in.Line)
	if !ok {
		out.Message = fmt.Sprintf("symbol %q not found in %s", in.Symbol, file)
		t.emit(ctx, &ev, start, 0, false, "")
		return out, nil
	}

	locs, err := t.Provider.References(ctx, file, pos, true)
	if err != nil {
		out.Error = err.Error()
		out.Message = fmt.Sprintf("provider error resolving references to %q", in.Symbol)
		t.emit(ctx, &ev, start, 0, false, err.Error())
		return out, nil
	}
	if len(locs) == 0 {
		out.Message = fmt.Sprintf("no references found for %q", in.Symbol)
		t.emit(ctx, &ev, start, 0, false, "")
		return out, nil
	}

	truncated := false
	if cap := t.Cfg.Cap(); len(locs) > cap {
		locs = locs[:cap]
		truncated = true
	}

	out.Found = true
	out.Locations = locs
	out.Truncated = truncated
	t.emit(ctx, &ev, start, len(locs), truncated, "")
	return out, nil
}

package tools

import (
	"context"
	"fmt"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/tools/render"
)

// FindReferencesInput is the input schema for find_references.
type FindReferencesInput struct {
	File   string `json:"file" jsonschema:"absolute path of the source file containing the symbol"`
	Symbol string `json:"symbol" jsonschema:"the symbol name to find references to (function, method, type, variable, etc.)"`
	Line   *int   `json:"line,omitempty" jsonschema:"optional 0-based line number, used to disambiguate when the symbol name occurs more than once in the file"`
}

// FindReferencesOutput is the authoritative typed internal retrieval result,
// not an advertised MCP output schema. RenderReferences owns its agent-facing
// projection.
type FindReferencesOutput struct {
	// Found is true iff at least one reference location was returned. Both
	// "symbol name did not resolve" and "resolved but has no references" are
	// honest, non-error results: Found is false and Message explains why.
	Found bool `json:"found"`
	// File is the canonical source file containing the requested symbol.
	File            string          `json:"file,omitempty"`
	Locations       []core.Location `json:"locations"`
	TotalReferences int             `json:"total_references"`
	Truncated       bool            `json:"truncated"`
	Freshness       core.Freshness  `json:"freshness"`
	Message         string          `json:"message,omitempty"`
	// Error is set for input-validation or provider failures. Both are soft:
	// the call never panics or returns a Go error for them.
	Error string `json:"error,omitempty"`
}

// FindReferences resolves input.Symbol to a Position via DocumentSymbols,
// then calls provider.References with includeDeclaration=true. See the
// package doc for the shared found/error/cap/freshness/telemetry contract.
func (t *Tools) FindReferences(ctx context.Context, in FindReferencesInput) (FindReferencesOutput, error) {
	var out FindReferencesOutput
	t.runTool(ctx, "find_references", &out, func(ctx context.Context) {
		file, failure := validateFile(in.File)
		if failure != nil {
			out.Error = failure.err
			out.Message = failure.message
			return
		}
		out.File = file
		symbols, err := runProviderStage(ctx, func(ctx context.Context) ([]core.SymbolNode, error) {
			return t.Provider.DocumentSymbols(ctx, file)
		})
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("failed to load symbols for %s", file)
			return
		}

		pos, ok, err := resolveSymbolPosition(ctx, symbols, in.Symbol, in.Line)
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("operation canceled while resolving symbol %q", in.Symbol)
			return
		}
		if !ok {
			out.Message = fmt.Sprintf("symbol %q not found in %s", in.Symbol, file)
			return
		}

		locs, err := runProviderStage(ctx, func(ctx context.Context) ([]core.Location, error) {
			return t.Provider.References(ctx, file, pos, true)
		})
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("provider error resolving references to %q", in.Symbol)
			return
		}
		if len(locs) == 0 {
			out.Message = fmt.Sprintf("no references found for %q", in.Symbol)
			return
		}

		out.TotalReferences = len(locs)
		groups, err := render.GroupLocations(ctx, locs)
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("operation canceled while grouping references to %q", in.Symbol)
			return
		}
		if cap := t.Cfg.Cap(); len(groups) > cap {
			groups = groups[:cap]
			out.Truncated = true
		}
		out.Locations, err = flattenLocationGroups(ctx, groups)
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("operation canceled while grouping references to %q", in.Symbol)
			out.Truncated = false
			return
		}
		out.Found = true
	})
	return out, nil
}

func flattenLocationGroups(ctx context.Context, groups []render.LocationGroup) ([]core.Location, error) {
	locations := make([]core.Location, 0)
	for _, group := range groups {
		for _, location := range group.Locations {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			locations = append(locations, location)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return locations, nil
}

func (o *FindReferencesOutput) setFreshness(fresh core.Freshness) {
	o.Freshness = fresh
}

func (o *FindReferencesOutput) telemetryFields() (int, bool, string) {
	return len(o.Locations), o.Truncated, o.Error
}

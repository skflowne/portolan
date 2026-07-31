package tools

import (
	"context"
	"fmt"

	"github.com/skflowne/portolan/internal/core"
)

// GetOutlineInput is the input schema for get_outline.
type GetOutlineInput struct {
	File string `json:"file" jsonschema:"absolute path of the source file to outline"`
}

// OutlineSymbol projects one canonical symbol into a bounded flat outline.
// Depth is derived from SymbolNode hierarchy and is not part of symbol identity.
type OutlineSymbol struct {
	core.Symbol
	Depth int `json:"depth"`
}

// GetOutlineOutput is the output schema for get_outline.
type GetOutlineOutput struct {
	// Found is true iff the file produced at least one symbol. An empty
	// file (or one the provider has no symbols for) is an honest, non-error
	// result: Found is false and Message explains why.
	Found bool `json:"found"`
	// File is the canonical path the outline describes, recorded once the
	// caller-supplied path clears the tools normalization boundary. It is the
	// authoritative file identity for the result, including when no symbol
	// carries it.
	File      string          `json:"file,omitempty"`
	Symbols   []OutlineSymbol `json:"symbols"`
	Truncated bool            `json:"truncated"`
	Freshness core.Freshness  `json:"freshness"`
	Message   string          `json:"message,omitempty"`
	// Error is set for input-validation or provider failures. Both are soft:
	// the call never panics or returns a Go error for them.
	Error string `json:"error,omitempty"`
}

// GetOutline returns the flattened outline of input.File. See the package
// doc for the shared found/error/cap/freshness/telemetry contract, and
// OutlineSymbol's doc for the flattening/Depth shape decision.
func (t *Tools) GetOutline(ctx context.Context, in GetOutlineInput) (GetOutlineOutput, error) {
	var out GetOutlineOutput
	t.runTool(ctx, "get_outline", &out, func(ctx context.Context) {
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
		if len(symbols) == 0 {
			out.Message = fmt.Sprintf("no symbols found in %s", file)
			return
		}

		flat, err := flattenSymbols(ctx, symbols, t.Cfg.Cap())
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("operation canceled while shaping outline for %s", file)
			return
		}
		signatures, err := runProviderStage(ctx, func(ctx context.Context) ([]string, error) {
			return t.Provider.SymbolSignatures(ctx, file, flat.originals)
		})
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("failed to load symbol signatures for %s", file)
			return
		}
		if len(signatures) != len(flat.outline) {
			out.Error = fmt.Sprintf("provider returned %d signatures for %d symbols", len(signatures), len(flat.outline))
			out.Message = fmt.Sprintf("failed to load symbol signatures for %s", file)
			return
		}
		for i := range flat.outline {
			flat.outline[i].Signature = signatures[i]
		}

		out.Found = true
		out.Symbols = flat.outline
		out.Truncated = flat.truncated
	})
	return out, nil
}

func (o *GetOutlineOutput) setFreshness(fresh core.Freshness) {
	o.Freshness = fresh
}

func (o *GetOutlineOutput) telemetryFields() (int, bool, string) {
	return len(o.Symbols), o.Truncated, o.Error
}

type flattenedSymbols struct {
	outline   []OutlineSymbol
	originals []core.Symbol
	truncated bool
}

// flattenSymbols walks symbols depth-first and stops once it can prove the
// configured result cap is truncated.
func flattenSymbols(ctx context.Context, symbols []core.SymbolNode, cap int) (flattenedSymbols, error) {
	if err := ctx.Err(); err != nil {
		return flattenedSymbols{}, err
	}
	flat := flattenedSymbols{
		outline:   make([]OutlineSymbol, 0, min(len(symbols), cap)),
		originals: make([]core.Symbol, 0, min(len(symbols), cap)),
	}
	var walk func([]core.SymbolNode, int) (bool, error)
	walk = func(syms []core.SymbolNode, depth int) (bool, error) {
		for i := range syms {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if len(flat.outline) == cap {
				return true, nil
			}
			s := syms[i]
			flat.outline = append(flat.outline, OutlineSymbol{Symbol: s.Symbol, Depth: depth})
			flat.originals = append(flat.originals, s.Symbol)
			if len(s.Children) > 0 {
				truncated, err := walk(s.Children, depth+1)
				if err != nil || truncated {
					return truncated, err
				}
			}
		}
		return false, nil
	}
	truncated, err := walk(symbols, 0)
	if err != nil {
		return flattenedSymbols{}, err
	}
	flat.truncated = truncated
	return flat, nil
}

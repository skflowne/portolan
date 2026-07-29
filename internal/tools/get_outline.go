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

// OutlineSymbol is one flattened entry in a get_outline result. It carries
// the same fields as core.Symbol except Children: the tree is flattened
// depth-first (parent immediately followed by its children) and Depth
// records nesting level (0 = top-level) so callers can reconstruct
// indentation without an unbounded, cap-defeating nested shape.
type OutlineSymbol struct {
	Name      string          `json:"name"`
	Kind      core.SymbolKind `json:"kind"`
	File      string          `json:"file"`
	Range     core.Range      `json:"range"`
	SelRange  core.Range      `json:"selRange"`
	Signature string          `json:"signature,omitempty"`
	Detail    string          `json:"detail,omitempty"`
	Depth     int             `json:"depth"`
}

// GetOutlineOutput is the output schema for get_outline.
type GetOutlineOutput struct {
	// Found is true iff the file produced at least one symbol. An empty
	// file (or one the provider has no symbols for) is an honest, non-error
	// result: Found is false and Message explains why.
	Found     bool            `json:"found"`
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
		symbols, err := t.Provider.DocumentSymbols(ctx, file)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("failed to load symbols for %s", file)
			return
		}
		if len(symbols) == 0 {
			out.Message = fmt.Sprintf("no symbols found in %s", file)
			return
		}

		flat, truncated, err := flattenSymbols(ctx, symbols, t.Cfg.Cap())
		if err != nil {
			out.Error = err.Error()
			out.Message = fmt.Sprintf("operation canceled while shaping outline for %s", file)
			return
		}

		out.Found = true
		out.Symbols = flat
		out.Truncated = truncated
	})
	return out, nil
}

func (o *GetOutlineOutput) setFreshness(fresh core.Freshness) {
	o.Freshness = fresh
}

func (o *GetOutlineOutput) telemetryFields() (int, bool, string) {
	return len(o.Symbols), o.Truncated, o.Error
}

// flattenSymbols walks symbols depth-first and stops once it can prove the
// configured result cap is truncated.
func flattenSymbols(ctx context.Context, symbols []core.Symbol, cap int) ([]OutlineSymbol, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	out := make([]OutlineSymbol, 0, min(len(symbols), cap))
	var walk func([]core.Symbol, int) (bool, error)
	walk = func(syms []core.Symbol, depth int) (bool, error) {
		for _, s := range syms {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if len(out) == cap {
				return true, nil
			}
			out = append(out, OutlineSymbol{
				Name:      s.Name,
				Kind:      s.Kind,
				File:      s.File,
				Range:     s.Range,
				SelRange:  s.SelRange,
				Signature: s.Signature,
				Detail:    s.Detail,
				Depth:     depth,
			})
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
		return nil, false, err
	}
	return out, truncated, nil
}

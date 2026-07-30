package tools

import (
	"context"

	"github.com/skflowne/portolan/internal/core"
)

// resolveSymbolPosition walks the (possibly nested) symbol tree returned by
// DocumentSymbols looking for a symbol named name. If line is non-nil, a
// symbol whose SelRange.Start.Line matches it is preferred (disambiguating
// overloaded/shadowed names); otherwise the first match encountered in a
// depth-first, parent-before-children walk is used.
//
// Returns the SelRange.Start position (the name/selection range, per
// core.Symbol's doc comment — this is what LSP definition/references
// requests expect, not the full symbol Range) and whether any match was
// found at all.
func resolveSymbolPosition(ctx context.Context, symbols []core.SymbolNode, name string, line *int) (core.Position, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.Position{}, false, err
	}
	var first *core.SymbolNode
	var lineMatch *core.SymbolNode

	var walk func([]core.SymbolNode) (bool, error)
	walk = func(syms []core.SymbolNode) (bool, error) {
		for i := range syms {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			s := &syms[i]
			if s.Name == name {
				if first == nil {
					first = s
				}
				if line == nil {
					return true, nil
				}
				if s.SelRange.Start.Line == *line {
					lineMatch = s
					return true, nil
				}
			}
			if len(s.Children) > 0 {
				done, err := walk(s.Children)
				if err != nil || done {
					return done, err
				}
			}
		}
		return false, nil
	}
	if _, err := walk(symbols); err != nil {
		return core.Position{}, false, err
	}

	if lineMatch != nil {
		return lineMatch.SelRange.Start, true, nil
	}
	if first != nil {
		return first.SelRange.Start, true, nil
	}
	return core.Position{}, false, nil
}

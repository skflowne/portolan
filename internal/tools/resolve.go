package tools

import "github.com/skflowne/portolan/internal/core"

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
func resolveSymbolPosition(symbols []core.Symbol, name string, line *int) (core.Position, bool) {
	var first *core.Symbol
	var lineMatch *core.Symbol

	var walk func([]core.Symbol)
	walk = func(syms []core.Symbol) {
		for i := range syms {
			s := &syms[i]
			if s.Name == name {
				if first == nil {
					first = s
				}
				if line != nil && lineMatch == nil && s.SelRange.Start.Line == *line {
					lineMatch = s
				}
			}
			if len(s.Children) > 0 {
				walk(s.Children)
			}
		}
	}
	walk(symbols)

	if lineMatch != nil {
		return lineMatch.SelRange.Start, true
	}
	if first != nil {
		return first.SelRange.Start, true
	}
	return core.Position{}, false
}

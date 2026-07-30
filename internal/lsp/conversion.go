package lsp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/skflowne/portolan/internal/core"
)

func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	return string(raw) == "null"
}

func decodeDocumentSymbols(ctx context.Context, raw json.RawMessage, file string) ([]core.SymbolNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}
	return runFiniteWork(ctx, nil, func() ([]core.SymbolNode, error) {
		var syms []lspDocumentSymbol
		if err := json.Unmarshal(raw, &syms); err != nil {
			return nil, fmt.Errorf("lsp: decoding documentSymbol result: %w", err)
		}
		if len(syms) == 0 {
			return nil, nil
		}

		out := make([]core.SymbolNode, 0, len(syms))
		for _, symbol := range syms {
			converted, err := symbol.toCoreSymbol(ctx, file)
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	})
}

// decodeLocations handles null, Location, Location[], and LocationLink[].
func decodeLocations(ctx context.Context, raw json.RawMessage) ([]core.Location, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}
	return runFiniteWork(ctx, nil, func() ([]core.Location, error) {
		var list []rawLocation
		if err := json.Unmarshal(raw, &list); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			var single rawLocation
			if err2 := json.Unmarshal(raw, &single); err2 != nil {
				return nil, fmt.Errorf("lsp: decoding location result: %w", err)
			}
			list = []rawLocation{single}
		}
		if len(list) == 0 {
			return nil, nil
		}

		out := make([]core.Location, 0, len(list))
		for _, rawLocation := range list {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			location, ok, err := rawLocation.toLocation()
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, location)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	})
}

func (rl rawLocation) toLocation() (core.Location, bool, error) {
	uri := rl.URI
	rng := rl.Range
	if uri == "" && rl.TargetURI != "" {
		uri = rl.TargetURI
		if rl.TargetSelectionRange != nil {
			rng = rl.TargetSelectionRange
		} else {
			rng = rl.TargetRange
		}
	}
	if uri == "" || rng == nil {
		return core.Location{}, false, nil
	}
	path, err := pathFromURI(uri)
	if err != nil {
		return core.Location{}, false, err
	}
	convertedRange, err := rng.toCoreRange()
	if err != nil {
		return core.Location{}, false, fmt.Errorf("lsp: invalid location range: %w", err)
	}
	return core.Location{File: path, Range: convertedRange}, true, nil
}

func (p lspPosition) toCorePosition() (core.Position, error) {
	position := core.Position{Line: p.Line, Character: p.Character}
	if err := position.Validate(); err != nil {
		return core.Position{}, err
	}
	return position, nil
}

func toLSPPosition(position core.Position) (lspPosition, error) {
	if err := position.Validate(); err != nil {
		return lspPosition{}, err
	}
	return lspPosition{Line: position.Line, Character: position.Character}, nil
}

func (r lspRange) toCoreRange() (core.Range, error) {
	start, err := r.Start.toCorePosition()
	if err != nil {
		return core.Range{}, err
	}
	end, err := r.End.toCorePosition()
	if err != nil {
		return core.Range{}, err
	}
	converted := core.Range{Start: start, End: end}
	if err := converted.Validate(); err != nil {
		return core.Range{}, err
	}
	return converted, nil
}

func (s lspDocumentSymbol) toCoreSymbol(ctx context.Context, file string) (core.SymbolNode, error) {
	if err := ctx.Err(); err != nil {
		return core.SymbolNode{}, err
	}
	var children []core.SymbolNode
	if len(s.Children) > 0 {
		children = make([]core.SymbolNode, 0, len(s.Children))
		for _, child := range s.Children {
			converted, err := child.toCoreSymbol(ctx, file)
			if err != nil {
				return core.SymbolNode{}, err
			}
			children = append(children, converted)
		}
	}
	fullRange, err := s.Range.toCoreRange()
	if err != nil {
		return core.SymbolNode{}, fmt.Errorf("lsp: invalid range for symbol %q: %w", s.Name, err)
	}
	selectionRange, err := s.SelectionRange.toCoreRange()
	if err != nil {
		return core.SymbolNode{}, fmt.Errorf("lsp: invalid selection range for symbol %q: %w", s.Name, err)
	}
	if !fullRange.Contains(selectionRange) {
		return core.SymbolNode{}, fmt.Errorf("lsp: selection range for symbol %q lies outside its full range", s.Name)
	}
	return core.SymbolNode{
		Symbol: core.Symbol{
			Name:     s.Name,
			Kind:     symbolKindName(s.Kind),
			File:     file,
			Range:    fullRange,
			SelRange: selectionRange,
			Detail:   s.Detail,
		},
		Children: children,
	}, nil
}

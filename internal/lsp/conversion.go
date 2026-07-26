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

func decodeDocumentSymbols(ctx context.Context, raw json.RawMessage, file string) ([]core.Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}
	return runContextWork(ctx, func() ([]core.Symbol, error) {
		var syms []lspDocumentSymbol
		if err := json.Unmarshal(raw, &syms); err != nil {
			return nil, fmt.Errorf("lsp: decoding documentSymbol result: %w", err)
		}
		if len(syms) == 0 {
			return nil, nil
		}

		out := make([]core.Symbol, 0, len(syms))
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
	return runContextWork(ctx, func() ([]core.Location, error) {
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
	return core.Location{File: path, Range: rng.toCoreRange()}, true, nil
}

func (r lspRange) toCoreRange() core.Range {
	return core.Range{
		Start: core.Position{Line: r.Start.Line, Character: r.Start.Character},
		End:   core.Position{Line: r.End.Line, Character: r.End.Character},
	}
}

func (s lspDocumentSymbol) toCoreSymbol(ctx context.Context, file string) (core.Symbol, error) {
	if err := ctx.Err(); err != nil {
		return core.Symbol{}, err
	}
	var children []core.Symbol
	if len(s.Children) > 0 {
		children = make([]core.Symbol, 0, len(s.Children))
		for _, child := range s.Children {
			converted, err := child.toCoreSymbol(ctx, file)
			if err != nil {
				return core.Symbol{}, err
			}
			children = append(children, converted)
		}
	}
	return core.Symbol{
		Name:     s.Name,
		Kind:     core.SymbolKind(symbolKindName(s.Kind)),
		File:     file,
		Range:    s.Range.toCoreRange(),
		SelRange: s.SelectionRange.toCoreRange(),
		Detail:   s.Detail,
		Children: children,
	}, nil
}

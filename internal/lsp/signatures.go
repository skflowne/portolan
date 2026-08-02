package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/skflowne/portolan/internal/core"
)

type signaturePlan struct {
	position core.Position
	direct   string
	symbol   core.Symbol
}

func (p *Provider) SymbolSignatures(ctx context.Context, file string, symbols []core.Symbol) ([]string, error) {
	if len(symbols) == 0 {
		return []string{}, nil
	}
	canonicalFile, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}

	var source string
	if needsSignatureSource(symbols) {
		var ok bool
		source, ok = p.openedSource(canonicalFile)
		if !ok {
			return nil, fmt.Errorf("lsp: opened source unavailable for %s", canonicalFile)
		}
	}
	plans := make([]signaturePlan, len(symbols))
	for i := range symbols {
		plans[i], err = planSignature(ctx, symbols[i], source)
		if err != nil {
			return nil, err
		}
	}
	return requestSignatures(ctx, uri, plans, p.transport.call, decodeHoverSignature)
}

func needsSignatureSource(symbols []core.Symbol) bool {
	for _, symbol := range symbols {
		if symbol.Kind == core.SymbolKindClass || symbol.Kind == core.SymbolKindInterface ||
			symbol.SelRange.Start == symbol.SelRange.End && symbol.Name != "constructor" {
			return true
		}
	}
	return false
}

func planSignature(ctx context.Context, symbol core.Symbol, source string) (signaturePlan, error) {
	plan := signaturePlan{position: symbol.SelRange.Start, symbol: symbol}
	if symbol.Kind == core.SymbolKindClass || symbol.Kind == core.SymbolKindInterface {
		header, ok, err := extractDeclarationHeader(ctx, source, symbol)
		if err != nil {
			return signaturePlan{}, fmt.Errorf("lsp: extracting %s declaration header: %w", symbol.Name, err)
		}
		if ok {
			plan.direct = header
			return plan, nil
		}
	}
	if symbol.SelRange.Start != symbol.SelRange.End || symbol.Name == "constructor" {
		return plan, nil
	}

	switch symbol.Name {
	case "()", "new()", "[]":
		text, err := textInRange(ctx, source, symbol.Range)
		if err != nil {
			return signaturePlan{}, fmt.Errorf("lsp: extracting %s signature: %w", symbol.Name, err)
		}
		plan.direct = strings.TrimSuffix(strings.TrimSpace(text), ";")
		return plan, nil
	}

	var tokens []string
	switch symbol.Kind {
	case core.SymbolKindFunction:
		tokens = []string{"function", "=>"}
	case core.SymbolKindClass:
		tokens = []string{"class"}
	default:
		return plan, nil
	}
	position, ok, err := findDeclarationToken(ctx, source, symbol.Range, tokens)
	if err != nil {
		return signaturePlan{}, fmt.Errorf("lsp: locating %s signature: %w", symbol.Name, err)
	}
	if ok {
		plan.position = position
	}
	return plan, nil
}

type hoverDecoder func(json.RawMessage, core.Symbol) (string, error)

func requestSignatures(ctx context.Context, uri string, plans []signaturePlan, request requestFunc, decode hoverDecoder) ([]string, error) {
	signatures := make([]string, len(plans))
	positions := make([]lspPosition, len(plans))
	var indexes []int
	for i, plan := range plans {
		if plan.direct != "" {
			signatures[i] = plan.direct
		} else {
			position, err := toLSPPosition(plan.position)
			if err != nil {
				return nil, fmt.Errorf("lsp: invalid hover position: %w", err)
			}
			positions[i] = position
			indexes = append(indexes, i)
		}
	}
	if len(indexes) == 0 {
		return signatures, nil
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	workerCount := min(len(indexes), maxConcurrentProviderRequests)
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if workCtx.Err() != nil {
					return
				}
				raw, err := request(workCtx, "textDocument/hover", textDocumentPositionParams{
					TextDocument: textDocumentIdentifier{URI: uri},
					Position:     positions[index],
				})
				if err == nil {
					var signature string
					signature, err = runFiniteWork(workCtx, nil, func() (string, error) {
						return decode(raw, plans[index].symbol)
					})
					if err == nil {
						signatures[index] = signature
					}
				}
				if err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	for _, index := range indexes {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			break
		}
		if workCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return signatures, nil
}

func textInRange(ctx context.Context, source string, r core.Range) (string, error) {
	start, err := byteOffsetContext(ctx, source, r.Start)
	if err != nil {
		return "", err
	}
	end, err := byteOffsetContext(ctx, source, r.End)
	if err != nil {
		return "", err
	}
	if end < start {
		return "", fmt.Errorf("range end precedes start")
	}
	return source[start:end], nil
}

func findDeclarationToken(ctx context.Context, source string, r core.Range, tokens []string) (core.Position, bool, error) {
	start, err := byteOffsetContext(ctx, source, r.Start)
	if err != nil {
		return core.Position{}, false, err
	}
	end, err := byteOffsetContext(ctx, source, r.End)
	if err != nil {
		return core.Position{}, false, err
	}
	if end < start {
		return core.Position{}, false, fmt.Errorf("range end precedes start")
	}
	offset, ok, err := declarationTokenOffset(ctx, source, start, end, tokens)
	if err != nil {
		return core.Position{}, false, err
	}
	if ok {
		return positionAt(source, offset), true, nil
	}
	return core.Position{}, false, nil
}

func byteOffset(source string, position core.Position) (int, error) {
	return byteOffsetContext(context.Background(), source, position)
}

func byteOffsetContext(ctx context.Context, source string, position core.Position) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := position.Validate(); err != nil {
		return 0, err
	}
	line, offset := 0, 0
	for line < position.Line && offset < len(source) {
		if source[offset] == '\n' {
			line++
		}
		offset++
		if offset%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
	}
	if line != position.Line {
		return 0, fmt.Errorf("line %d is outside source", position.Line)
	}
	units := 0
	for offset < len(source) && source[offset] != '\n' && units < position.Character {
		r, size := utf8.DecodeRuneInString(source[offset:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		units += utf16.RuneLen(r)
		offset += size
		if offset%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if units != position.Character {
		return 0, fmt.Errorf("character %d is outside line %d", position.Character, position.Line)
	}
	return offset, nil
}

func positionAt(source string, offset int) core.Position {
	position := core.Position{}
	for i := 0; i < offset; {
		r, size := utf8.DecodeRuneInString(source[i:offset])
		if r == '\n' {
			position.Line++
			position.Character = 0
		} else {
			position.Character += utf16.RuneLen(r)
		}
		i += size
	}
	return position
}

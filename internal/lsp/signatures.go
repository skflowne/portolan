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

const maxConcurrentSignatureRequests = 8

type signaturePlan struct {
	position core.Position
	direct   string
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
		plans[i], err = planSignature(symbols[i], source)
		if err != nil {
			return nil, err
		}
	}
	return requestSignatures(ctx, uri, plans, p.transport.call, decodeHoverSignature)
}

func needsSignatureSource(symbols []core.Symbol) bool {
	for _, symbol := range symbols {
		if symbol.SelRange.Start == symbol.SelRange.End && symbol.Name != "constructor" {
			return true
		}
	}
	return false
}

func planSignature(symbol core.Symbol, source string) (signaturePlan, error) {
	plan := signaturePlan{position: symbol.SelRange.Start}
	if symbol.SelRange.Start != symbol.SelRange.End || symbol.Name == "constructor" {
		return plan, nil
	}

	switch symbol.Name {
	case "()", "new()", "[]":
		text, err := textInRange(source, symbol.Range)
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
	position, ok, err := findDeclarationToken(source, symbol.Range, tokens)
	if err != nil {
		return signaturePlan{}, fmt.Errorf("lsp: locating %s signature: %w", symbol.Name, err)
	}
	if ok {
		plan.position = position
	}
	return plan, nil
}

type hoverDecoder func(json.RawMessage) (string, error)

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
	workerCount := min(len(indexes), maxConcurrentSignatureRequests)
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
						return decode(raw)
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

func decodeHoverSignature(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var hover hoverResult
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", fmt.Errorf("lsp: decoding hover result: %w", err)
	}
	return decodeHoverContents(hover.Contents)
}

func decodeHoverContents(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var markup markupContent
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		if markup.Kind == "markdown" {
			return markdownCode(markup.Value), nil
		}
		return strings.TrimSpace(markup.Value), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		for _, item := range items {
			value, err := decodeHoverContents(item)
			if err != nil {
				return "", err
			}
			if value != "" {
				return value, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("lsp: unsupported hover contents: %s", raw)
}

func markdownCode(markdown string) string {
	start := strings.Index(markdown, "```")
	if start < 0 {
		return strings.TrimSpace(markdown)
	}
	lineEnd := strings.IndexByte(markdown[start:], '\n')
	if lineEnd < 0 {
		return strings.TrimSpace(markdown)
	}
	contentStart := start + lineEnd + 1
	end := strings.Index(markdown[contentStart:], "```")
	if end < 0 {
		return strings.TrimSpace(markdown)
	}
	return strings.TrimSpace(markdown[contentStart : contentStart+end])
}

func textInRange(source string, r core.Range) (string, error) {
	start, err := byteOffset(source, r.Start)
	if err != nil {
		return "", err
	}
	end, err := byteOffset(source, r.End)
	if err != nil {
		return "", err
	}
	if end < start {
		return "", fmt.Errorf("range end precedes start")
	}
	return source[start:end], nil
}

func findDeclarationToken(source string, r core.Range, tokens []string) (core.Position, bool, error) {
	start, err := byteOffset(source, r.Start)
	if err != nil {
		return core.Position{}, false, err
	}
	end, err := byteOffset(source, r.End)
	if err != nil {
		return core.Position{}, false, err
	}
	if end < start {
		return core.Position{}, false, fmt.Errorf("range end precedes start")
	}
	if offset, ok := declarationTokenOffset(source, start, end, tokens); ok {
		return positionAt(source, offset), true, nil
	}
	return core.Position{}, false, nil
}

func declarationTokenOffset(source string, start, end int, tokens []string) (int, bool) {
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
	for i := start; i < end; {
		switch source[i] {
		case '/', '\'', '"', '`':
			next := skipLiteralOrComment(source, i, end)
			if next > i {
				i = next
				continue
			}
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{':
			braceDepth++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
		}
		if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
			for _, token := range tokens {
				if strings.HasPrefix(source[i:end], token) && tokenBoundary(source, i, token) {
					return i, true
				}
			}
		}
		_, size := utf8.DecodeRuneInString(source[i:end])
		if size == 0 {
			break
		}
		i += size
	}
	return 0, false
}

func skipLiteralOrComment(source string, start, end int) int {
	if source[start] == '/' && start+1 < end {
		switch source[start+1] {
		case '/':
			if newline := strings.IndexByte(source[start+2:end], '\n'); newline >= 0 {
				return start + 2 + newline + 1
			}
			return end
		case '*':
			if close := strings.Index(source[start+2:end], "*/"); close >= 0 {
				return start + 2 + close + 2
			}
			return end
		default:
			return start
		}
	}
	quote := source[start]
	if quote != '\'' && quote != '"' && quote != '`' {
		return start
	}
	for i := start + 1; i < end; i++ {
		if source[i] == '\\' {
			i++
			continue
		}
		if source[i] == quote {
			return i + 1
		}
	}
	return end
}

func tokenBoundary(source string, start int, token string) bool {
	if token == "=>" {
		return true
	}
	beforeOK := start == 0 || !isIdentifierByte(source[start-1])
	end := start + len(token)
	afterOK := end == len(source) || !isIdentifierByte(source[end])
	return beforeOK && afterOK
}

func isIdentifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= utf8.RuneSelf
}

func byteOffset(source string, position core.Position) (int, error) {
	if err := position.Validate(); err != nil {
		return 0, err
	}
	line, offset := 0, 0
	for line < position.Line && offset < len(source) {
		if source[offset] == '\n' {
			line++
		}
		offset++
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

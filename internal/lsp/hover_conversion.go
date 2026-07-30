package lsp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/skflowne/portolan/internal/core"
)

func decodeHoverSignature(raw json.RawMessage, symbol core.Symbol) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var hover hoverResult
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", fmt.Errorf("lsp: decoding hover result: %w", err)
	}
	display, err := decodeHoverContents(hover.Contents)
	if err != nil {
		return "", err
	}
	return normalizeTSGoSignature(display, symbol), nil
}

func decodeHoverContents(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var markup markupContent
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		if markup.Kind == "markdown" {
			return markdownCode(markup.Value)
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

func markdownCode(markdown string) (string, error) {
	start := strings.Index(markdown, "```")
	if start < 0 {
		return strings.TrimSpace(markdown), nil
	}
	lineEnd := strings.IndexByte(markdown[start:], '\n')
	if lineEnd < 0 {
		return "", fmt.Errorf("lsp: malformed fenced hover content")
	}
	contentStart := start + lineEnd + 1
	end := strings.Index(markdown[contentStart:], "```")
	if end < 0 {
		return "", fmt.Errorf("lsp: malformed fenced hover content")
	}
	return strings.TrimSpace(markdown[contentStart : contentStart+end]), nil
}

func normalizeTSGoSignature(display string, symbol core.Symbol) string {
	switch symbol.Kind {
	case core.SymbolKindMethod:
		return normalizeTSGoMember(display, symbol.Name, "(method) ", "(<")
	case core.SymbolKindProperty:
		return normalizeTSGoMember(display, symbol.Name, "(property) ", ":?!")
	case core.SymbolKindConstructor:
		if symbol.Name == "constructor" {
			return normalizeTSGoConstructor(display)
		}
	case core.SymbolKindFunction:
		if symbol.Name == "default" || strings.HasSuffix(symbol.Name, " callback") {
			return normalizeTSGoAnonymousFunction(display)
		}
	}
	return display
}

func normalizeTSGoMember(display, name, prefix, allowedTail string) string {
	if name == "" || !strings.HasPrefix(display, prefix) {
		return display
	}
	rest := strings.TrimPrefix(display, prefix)
	separator := strings.LastIndex(rest, "."+name)
	if separator <= 0 {
		return display
	}
	qualifier := rest[:separator]
	tail := rest[separator+1+len(name):]
	if strings.TrimSpace(qualifier) != qualifier || strings.ContainsAny(qualifier, "\r\n") || tail == "" || !strings.ContainsRune(allowedTail, rune(tail[0])) {
		return display
	}
	return name + tail
}

func normalizeTSGoConstructor(display string) string {
	const prefix = "constructor "
	if !strings.HasPrefix(display, prefix) {
		return display
	}
	rest := strings.TrimPrefix(display, prefix)
	open := strings.IndexByte(rest, '(')
	close := strings.LastIndexByte(rest, ')')
	if open <= 0 || close <= open {
		return display
	}
	className := rest[:open]
	if strings.TrimSpace(className) != className || strings.ContainsAny(className, " \t\r\n") {
		return display
	}
	suffix := rest[close+1:]
	if suffix != "" && suffix != ": "+className {
		return display
	}
	return "constructor" + rest[open:close+1]
}

func normalizeTSGoAnonymousFunction(display string) string {
	const prefix = "function (Anonymous function)"
	if !strings.HasPrefix(display, prefix) {
		return display
	}
	tail := strings.TrimPrefix(display, prefix)
	if !strings.HasPrefix(tail, "(") {
		return display
	}
	return tail
}

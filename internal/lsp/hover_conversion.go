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
	if strings.HasPrefix(display, "(method) ") {
		if symbol.Kind == core.SymbolKindMethod {
			return normalizeTSGoMember(display, symbol.Name, "(method) ", "(?<")
		}
		return ""
	}
	if strings.HasPrefix(display, "(property) ") {
		if symbol.Kind == core.SymbolKindProperty {
			return normalizeTSGoMember(display, symbol.Name, "(property) ", ":?!")
		}
		return ""
	}
	if strings.HasPrefix(display, "(accessor) ") {
		if symbol.Kind == core.SymbolKindProperty {
			return normalizeTSGoMember(display, symbol.Name, "(accessor) ", ":")
		}
		return ""
	}
	if strings.HasPrefix(display, "constructor ") {
		if symbol.Kind == core.SymbolKindConstructor && symbol.Name == "constructor" {
			return normalizeTSGoConstructor(display)
		}
		return ""
	}
	if strings.HasPrefix(display, "function (Anonymous function)") {
		if symbol.Kind == core.SymbolKindFunction && (symbol.Name == "default" || strings.HasSuffix(symbol.Name, " callback")) {
			return normalizeTSGoAnonymousFunction(display)
		}
		return ""
	}
	return display
}

func normalizeTSGoMember(display, name, prefix, allowedTail string) string {
	if name == "" {
		return ""
	}
	rest := strings.TrimPrefix(display, prefix)
	var tail string
	if strings.HasPrefix(rest, name) {
		tail = rest[len(name):]
	} else {
		separator := strings.LastIndex(rest, "."+name)
		if separator <= 0 {
			return ""
		}
		qualifier := rest[:separator]
		if strings.TrimSpace(qualifier) != qualifier || strings.ContainsAny(qualifier, "\r\n") {
			return ""
		}
		tail = rest[separator+1+len(name):]
	}
	if tail == "" || !strings.ContainsRune(allowedTail, rune(tail[0])) {
		return ""
	}
	return name + tail
}

func normalizeTSGoConstructor(display string) string {
	const prefix = "constructor "
	rest := strings.TrimPrefix(display, prefix)
	open := strings.IndexByte(rest, '(')
	close := strings.LastIndexByte(rest, ')')
	if open <= 0 || close <= open {
		return ""
	}
	className := rest[:open]
	if strings.TrimSpace(className) != className || strings.ContainsAny(className, " \t\r\n") {
		return ""
	}
	suffix := rest[close+1:]
	if suffix != "" && suffix != ": "+className {
		return ""
	}
	return "constructor" + rest[open:close+1]
}

func normalizeTSGoAnonymousFunction(display string) string {
	const prefix = "function (Anonymous function)"
	tail := strings.TrimPrefix(display, prefix)
	if !strings.HasPrefix(tail, "(") {
		return ""
	}
	return tail
}

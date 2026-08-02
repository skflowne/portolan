// Package render provides deterministic compact-text projections of canonical
// navigation atoms for tool-specific assemblers.
package render

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/skflowne/portolan/internal/core"
)

// RangeConvention states the coordinate convention once per assembled output.
const RangeConvention = "ranges 0-based"

// Position renders a zero-based UTF-16 position.
func Position(position core.Position) string {
	return strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Character)
}

// Range renders a zero-based half-open range.
func Range(range_ core.Range) string {
	return Position(range_.Start) + "-" + Position(range_.End)
}

// SymbolLine renders one declaration line at the supplied hierarchy depth.
func SymbolLine(symbol core.Symbol, depth int) string {
	declaration := symbol.Signature
	if declaration == "" {
		parts := make([]string, 0, 2)
		if symbol.Kind != "" && symbol.Kind != core.SymbolKindUnknown {
			parts = append(parts, string(symbol.Kind))
		}
		if symbol.Name != "" {
			parts = append(parts, symbol.Name)
		}
		declaration = strings.Join(parts, " ")
	}

	indent := ""
	if depth > 0 {
		indent = strings.Repeat("  ", depth)
	}
	if declaration == "" {
		return indent + "[" + Range(symbol.Range) + "]"
	}
	return indent + inlineText(declaration) + " [" + Range(symbol.Range) + "]"
}

// Inline escapes control characters so untrusted text cannot add response
// lines or terminal control sequences.
func Inline(text string) string {
	return inlineText(text)
}

func inlineText(text string) string {
	var rendered strings.Builder
	for _, r := range text {
		switch r {
		case '\a':
			rendered.WriteString(`\a`)
		case '\b':
			rendered.WriteString(`\b`)
		case '\t':
			rendered.WriteString(`\t`)
		case '\n':
			rendered.WriteString(`\n`)
		case '\v':
			rendered.WriteString(`\v`)
		case '\f':
			rendered.WriteString(`\f`)
		case '\r':
			rendered.WriteString(`\r`)
		default:
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				fmt.Fprintf(&rendered, `\u%04x`, r)
				continue
			}
			rendered.WriteRune(r)
		}
	}
	return rendered.String()
}

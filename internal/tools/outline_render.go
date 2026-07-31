package tools

import (
	"strings"

	"github.com/skflowne/portolan/internal/tools/render"
)

// RenderOutline projects a completed get_outline result into the single
// agent-facing text response. It is the only assembler of that text: the
// typed result keeps selection ranges and freshness for behavior, provider
// navigation, telemetry, and tests, and none of them reach this projection.
//
// The grammar is one file header, one range convention line, one line per
// symbol indented two spaces per hierarchy depth, and one footer that
// distinguishes a complete outline from a capped one. Soft errors and honest
// empties replace the whole body with their own marker so an agent can tell
// the four states apart without parsing counts.
func RenderOutline(out GetOutlineOutput) string {
	if out.Error != "" {
		return render.Error(out.Message + ": " + out.Error)
	}
	if !out.Found {
		return render.Empty(out.Message)
	}

	var rendered strings.Builder
	rendered.WriteString(render.FileLine(out.File))
	rendered.WriteByte('\n')
	rendered.WriteString(render.RangeConvention)
	rendered.WriteByte('\n')
	for i, symbol := range out.Symbols {
		// A blank line reopens the reading frame only where nesting closed,
		// keeping flat outlines dense.
		if i == 0 || (symbol.Depth == 0 && out.Symbols[i-1].Depth > 0) {
			rendered.WriteByte('\n')
		}
		rendered.WriteString(render.SymbolLine(symbol.Symbol, symbol.Depth))
		rendered.WriteByte('\n')
	}
	rendered.WriteByte('\n')
	rendered.WriteString(render.Footer(len(out.Symbols), "symbol", "symbols", out.Truncated))
	return rendered.String()
}

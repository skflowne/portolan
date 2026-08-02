package tools

import (
	"strings"

	"github.com/skflowne/portolan/internal/tools/render"
)

// RenderDefinition is the sole assembler of find_definition's agent-facing
// response. Narrow provider target ranges and freshness remain in the typed
// result and do not reach this projection.
func RenderDefinition(in FindDefinitionInput, out FindDefinitionOutput) string {
	if out.Error != "" {
		return render.Error(out.Message + ": " + out.Error)
	}
	if !out.Found {
		return render.Empty(out.Message)
	}

	var rendered strings.Builder
	for i, definition := range out.Definitions {
		if i > 0 {
			rendered.WriteString("\n\n")
		}
		rendered.WriteString("definition for ")
		rendered.WriteString(render.Inline(in.Symbol))
		rendered.WriteString(" — ")
		rendered.WriteString(render.Inline(definition.Target.File))
		rendered.WriteString(" [")
		rendered.WriteString(render.Range(definition.DeclarationRange))
		rendered.WriteString("]\n\n")
		rendered.WriteString(render.FencedSource(definition.Source))
	}
	if len(out.Definitions) > 0 {
		rendered.WriteString("\n\n")
	}
	footer := render.Footer(len(out.Definitions), "definition", "definitions", out.Truncated)
	if rendered.Len() == 0 {
		return footer
	}
	rendered.WriteString(footer)
	return rendered.String()
}

package tools

import (
	"context"
	"strconv"
	"strings"

	"github.com/skflowne/portolan/internal/tools/render"
)

// RenderReferences projects a completed find_references result into its sole
// agent-facing text representation.
func RenderReferences(out FindReferencesOutput, in FindReferencesInput) string {
	if out.File == "" {
		return render.Error(out.Message + ": " + out.Error)
	}

	groups, _ := render.GroupLocations(context.Background(), out.Locations)
	var rendered strings.Builder
	rendered.WriteString("references ")
	rendered.WriteString(render.InlineText(in.Symbol))
	rendered.WriteString(" — ")
	rendered.WriteString(render.InlineText(out.File))
	if in.Line != nil {
		rendered.WriteByte(':')
		rendered.WriteString(strconv.Itoa(*in.Line))
	}
	rendered.WriteByte('\n')
	rendered.WriteString(render.RangeConvention)
	rendered.WriteString("\n\n")

	switch {
	case out.Error != "":
		rendered.WriteString(render.Error(out.Message + ": " + out.Error))
		return rendered.String()
	case !out.Found:
		rendered.WriteString(render.Empty(out.Message))
	default:
		rendered.WriteString(render.LocationGroups(groups))
	}
	rendered.WriteString("\n\n")
	rendered.WriteString(referencesFooter(len(out.Locations), len(groups), out.TotalReferences, out.Truncated))
	return rendered.String()
}

func referencesFooter(references, files, total int, truncated bool) string {
	footer := render.Count(references, "reference", "references") + " across " +
		render.Count(files, "file", "files")
	if !truncated {
		return footer + "; complete"
	}
	omitted := total - references
	if omitted == 1 {
		return footer + "; truncated: 1 more reference exists"
	}
	return footer + "; truncated: " + strconv.Itoa(omitted) + " more references exist"
}

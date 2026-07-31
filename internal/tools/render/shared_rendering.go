package render

import (
	"strconv"
	"strings"

	"github.com/skflowne/portolan/internal/core"
)

type locationGroup struct {
	file   string
	ranges []core.Range
}

// Locations groups locations by first file appearance while retaining provider
// range order within each file.
func Locations(locations []core.Location) string {
	groups := make([]locationGroup, 0)
	groupIndexes := make(map[string]int)
	for _, location := range locations {
		index, exists := groupIndexes[location.File]
		if !exists {
			index = len(groups)
			groupIndexes[location.File] = index
			groups = append(groups, locationGroup{file: location.File})
		}
		groups[index].ranges = append(groups[index].ranges, location.Range)
	}

	var rendered strings.Builder
	for groupIndex, group := range groups {
		if groupIndex > 0 {
			rendered.WriteByte('\n')
		}
		rendered.WriteString(inlineText(group.file))
		rendered.WriteString(" [")
		for rangeIndex, range_ := range group.ranges {
			if rangeIndex > 0 {
				rendered.WriteString(", ")
			}
			rendered.WriteString(Range(range_))
		}
		rendered.WriteByte(']')
	}
	return rendered.String()
}

// FileLine names the single file an assembled projection describes, so
// per-symbol lines never repeat it.
func FileLine(file string) string {
	return "file " + inlineText(file)
}

// Empty renders an honest-empty state marker.
func Empty(message string) string {
	return "empty: " + inlineText(message)
}

// Error renders a soft-error state marker.
func Error(message string) string {
	return "error: " + inlineText(message)
}

// Count renders a count with its singular or plural noun.
func Count(count int, singular, plural string) string {
	noun := plural
	if count == 1 {
		noun = singular
	}
	return strconv.Itoa(count) + " " + inlineText(noun)
}

// Footer renders completion or semantic truncation without inventing an
// omitted-result count.
func Footer(count int, singular, plural string, truncated bool) string {
	footer := Count(count, singular, plural)
	if truncated {
		return footer + "; truncated: more " + inlineText(plural) + " exist"
	}
	return footer + "; complete"
}

package render

import (
	"strconv"
	"strings"

	"github.com/skflowne/portolan/internal/core"
)

// Locations projects locations into first-seen file lines while retaining
// provider range order within each file. It does not select or cap locations.
func Locations(locations []core.Location) string {
	var rendered strings.Builder
	groupIndexes := make(map[string]int)
	groups := make([][]core.Location, 0)
	files := make([]string, 0)
	for _, location := range locations {
		index, exists := groupIndexes[location.File]
		if !exists {
			index = len(files)
			groupIndexes[location.File] = index
			files = append(files, location.File)
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], location)
	}
	for groupIndex, group := range groups {
		if groupIndex > 0 {
			rendered.WriteByte('\n')
		}
		rendered.WriteString(InlineText(files[groupIndex]))
		rendered.WriteString(" [")
		for rangeIndex, location := range group {
			if rangeIndex > 0 {
				rendered.WriteString(", ")
			}
			rendered.WriteString(Range(location.Range))
		}
		rendered.WriteByte(']')
	}
	return rendered.String()
}

// FileLine names the single file an assembled projection describes, so
// per-symbol lines never repeat it.
func FileLine(file string) string {
	return "file " + InlineText(file)
}

// Empty renders an honest-empty state marker.
func Empty(message string) string {
	return "empty: " + InlineText(message)
}

// Error renders a soft-error state marker.
func Error(message string) string {
	return "error: " + InlineText(message)
}

// Count renders a count with its singular or plural noun.
func Count(count int, singular, plural string) string {
	noun := plural
	if count == 1 {
		noun = singular
	}
	return strconv.Itoa(count) + " " + InlineText(noun)
}

// Footer renders completion or semantic truncation without inventing an
// omitted-result count.
func Footer(count int, singular, plural string, truncated bool) string {
	footer := Count(count, singular, plural)
	if truncated {
		return footer + "; truncated: more " + InlineText(plural) + " exist"
	}
	return footer + "; complete"
}

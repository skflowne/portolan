package render

import (
	"context"
	"strconv"
	"strings"

	"github.com/skflowne/portolan/internal/core"
)

// LocationGroup is one first-seen file and its complete provider locations.
type LocationGroup struct {
	File      string
	Locations []core.Location
}

// GroupLocations groups locations by first file appearance while retaining
// complete locations and provider order within each file.
func GroupLocations(ctx context.Context, locations []core.Location) ([]LocationGroup, error) {
	groups := make([]LocationGroup, 0)
	groupIndexes := make(map[string]int)
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, exists := groupIndexes[location.File]
		if !exists {
			index = len(groups)
			groupIndexes[location.File] = index
			groups = append(groups, LocationGroup{File: location.File})
		}
		groups[index].Locations = append(groups[index].Locations, location)
	}
	return groups, nil
}

// Locations groups locations by first file appearance while retaining provider
// range order within each file.
func Locations(locations []core.Location) string {
	groups, _ := GroupLocations(context.Background(), locations)

	var rendered strings.Builder
	for groupIndex, group := range groups {
		if groupIndex > 0 {
			rendered.WriteByte('\n')
		}
		rendered.WriteString(inlineText(group.File))
		rendered.WriteString(" [")
		for rangeIndex, location := range group.Locations {
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

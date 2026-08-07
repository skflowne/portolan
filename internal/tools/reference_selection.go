package tools

import (
	"context"

	"github.com/skflowne/portolan/internal/core"
)

// referenceSelection is the completed file-group cap decision for one provider
// result. Its locations are grouped by first-seen file and preserve provider
// order within each retained file.
type referenceSelection struct {
	Locations       []core.Location
	TotalReferences int
	RetainedFiles   int
	Truncated       bool
}

// selectReferenceLocations retains every provider location from the first cap
// first-seen canonical files.
func selectReferenceLocations(ctx context.Context, locations []core.Location, cap int) (referenceSelection, error) {
	groups := make([][]core.Location, 0)
	groupIndexes := make(map[string]int)
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return referenceSelection{}, err
		}
		index, exists := groupIndexes[location.File]
		if !exists {
			index = len(groups)
			groupIndexes[location.File] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], location)
	}

	retainedFiles := min(cap, len(groups))
	retained := make([]core.Location, 0, len(locations))
	for _, group := range groups[:retainedFiles] {
		for _, location := range group {
			if err := ctx.Err(); err != nil {
				return referenceSelection{}, err
			}
			retained = append(retained, location)
		}
	}
	if err := ctx.Err(); err != nil {
		return referenceSelection{}, err
	}
	return referenceSelection{
		Locations:       retained,
		TotalReferences: len(locations),
		RetainedFiles:   retainedFiles,
		Truncated:       len(groups) > retainedFiles,
	}, nil
}

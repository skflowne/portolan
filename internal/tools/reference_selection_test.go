package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestSelectReferenceLocationsRetainsCompleteFirstSeenFileGroups(t *testing.T) {
	locations := []core.Location{
		{File: "/repo/a.go", Range: rng(1, 0, 1, 1)},
		{File: "/repo/b.go", Range: rng(2, 0, 2, 1)},
		{File: "/repo/a.go", Range: rng(3, 0, 3, 1)},
		{File: "/repo/c.go", Range: rng(4, 0, 4, 1)},
		{File: "/repo/b.go", Range: rng(5, 0, 5, 1)},
	}
	want := []core.Location{locations[0], locations[2], locations[1], locations[4]}

	selection, err := selectReferenceLocations(context.Background(), locations, 2)
	if err != nil {
		t.Fatalf("selectReferenceLocations() error = %v", err)
	}
	if !reflect.DeepEqual(selection.Locations, want) {
		t.Fatalf("locations = %+v, want %+v", selection.Locations, want)
	}
	if selection.TotalReferences != len(locations) {
		t.Fatalf("total references = %d, want %d", selection.TotalReferences, len(locations))
	}
	if selection.RetainedFiles != 2 {
		t.Fatalf("retained files = %d, want 2", selection.RetainedFiles)
	}
	if omitted := selection.TotalReferences - len(selection.Locations); omitted != 1 {
		t.Fatalf("omitted references = %d, want 1", omitted)
	}
	if !selection.Truncated {
		t.Fatal("selection that omits a file group must be truncated")
	}
}

func TestSelectReferenceLocationsReturnsNoPartialSelectionAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	selection, err := selectReferenceLocations(ctx, []core.Location{{File: "/repo/a.go"}}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("selectReferenceLocations() error = %v, want %v", err, context.Canceled)
	}
	if selection.Locations != nil || selection.TotalReferences != 0 || selection.RetainedFiles != 0 || selection.Truncated {
		t.Fatalf("selectReferenceLocations() selection = %+v, want zero selection", selection)
	}
}

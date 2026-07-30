package render_test

import (
	"testing"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/tools/render"
)

func TestLocationsPreservesFirstFileAppearanceAndProviderRangeOrder(t *testing.T) {
	locations := []core.Location{
		{File: "/project/a.ts", Range: testRange(7, 13, 7, 19)},
		{File: "/project/b.ts", Range: testRange(3, 9, 3, 15)},
		{File: "/project/a.ts", Range: testRange(6, 6, 6, 12)},
		{File: "/project/b.ts", Range: testRange(8, 6, 8, 12)},
		{File: "/project/a.ts", Range: testRange(9, 2, 10, 4)},
	}
	want := "/project/a.ts [7:13-7:19, 6:6-6:12, 9:2-10:4]\n" +
		"/project/b.ts [3:9-3:15, 8:6-8:12]"

	if got := render.Locations(locations); got != want {
		t.Fatalf("Locations() = %q, want %q", got, want)
	}
}

func TestLocationsRendersZeroWidthRangesAndEscapesFileText(t *testing.T) {
	locations := []core.Location{
		{File: "/project/café 🚀\n\x1b[31m.ts", Range: testRange(4, 6, 4, 6)},
	}
	want := `/project/café 🚀\n\u001b[31m.ts [4:6-4:6]`

	if got := render.Locations(locations); got != want {
		t.Fatalf("Locations() = %q, want %q", got, want)
	}
}

func TestLocationsRendersNoLinesForNoLocations(t *testing.T) {
	if got := render.Locations(nil); got != "" {
		t.Fatalf("Locations(nil) = %q, want empty text", got)
	}
}

func TestStateMarkersAreInlineSafe(t *testing.T) {
	if got, want := render.Empty("symbol missing\ntry grep"), `empty: symbol missing\ntry grep`; got != want {
		t.Fatalf("Empty() = %q, want %q", got, want)
	}
	if got, want := render.Error("provider \x1b[31mfailed\x1b[0m\u2028retry"), `error: provider \u001b[31mfailed\u001b[0m\u2028retry`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestCountUsesSingularAndPluralNouns(t *testing.T) {
	tests := []struct {
		count int
		want  string
	}{
		{count: 0, want: "0 references"},
		{count: 1, want: "1 reference"},
		{count: 2, want: "2 references"},
	}

	for _, tt := range tests {
		if got := render.Count(tt.count, "reference", "references"); got != tt.want {
			t.Errorf("Count(%d) = %q, want %q", tt.count, got, tt.want)
		}
	}
}

func TestFooterDistinguishesCompleteAndSemanticTruncation(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		truncated bool
		want      string
	}{
		{name: "singular complete", count: 1, want: "1 reference; complete"},
		{name: "plural complete", count: 2, want: "2 references; complete"},
		{name: "empty complete", count: 0, want: "0 references; complete"},
		{name: "singular truncated", count: 1, truncated: true, want: "1 reference; truncated: more references exist"},
		{name: "plural truncated", count: 2, truncated: true, want: "2 references; truncated: more references exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := render.Footer(tt.count, "reference", "references", tt.truncated); got != tt.want {
				t.Fatalf("Footer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func testRange(startLine, startCharacter, endLine, endCharacter int) core.Range {
	return core.Range{
		Start: core.Position{Line: startLine, Character: startCharacter},
		End:   core.Position{Line: endLine, Character: endCharacter},
	}
}

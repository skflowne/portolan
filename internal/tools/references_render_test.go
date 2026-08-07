package tools

import (
	"strings"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestRenderReferencesComposesGroupedRangesAndOptionalLine(t *testing.T) {
	line := 7
	out := FindReferencesOutput{
		Found:           true,
		File:            "/project/src/geometry.ts",
		TotalReferences: 4,
		RetainedFiles:   2,
		Locations: []core.Location{
			{File: "/project/src/geometry.ts", Range: rng(7, 13, 7, 19)},
			{File: "/project/src/main.ts", Range: rng(3, 9, 3, 15)},
			{File: "/project/src/main.ts", Range: rng(6, 6, 6, 12)},
			{File: "/project/src/geometry.ts", Range: rng(9, 2, 9, 2)},
		},
		Freshness: core.Freshness{Generation: 37, Stale: true},
	}
	in := FindReferencesInput{File: `C:\project\src\geometry.ts`, Symbol: "Circle", Line: &line}
	want := "references Circle — /project/src/geometry.ts:7\n" +
		"ranges 0-based\n" +
		"\n" +
		"/project/src/geometry.ts [7:13-7:19, 9:2-9:2]\n" +
		"/project/src/main.ts [3:9-3:15, 6:6-6:12]\n" +
		"\n" +
		"4 references across 2 files; complete"

	got := RenderReferences(out, in)
	if got != want {
		t.Fatalf("RenderReferences() =\n%s\nwant\n%s", got, want)
	}
	for _, withheld := range []string{in.File, "generation", "37", "stale"} {
		if strings.Contains(got, withheld) {
			t.Errorf("routine reference text leaks %q: %q", withheld, got)
		}
	}
}

func TestRenderReferencesReportsExactOmittedReferenceCount(t *testing.T) {
	out := FindReferencesOutput{
		Found:           true,
		File:            "/project/src/geometry.ts",
		TotalReferences: 17,
		RetainedFiles:   2,
		Truncated:       true,
		Locations: []core.Location{
			{File: "/project/src/a.ts", Range: rng(1, 0, 1, 1)},
			{File: "/project/src/a.ts", Range: rng(2, 0, 2, 1)},
			{File: "/project/src/a.ts", Range: rng(3, 0, 3, 1)},
			{File: "/project/src/a.ts", Range: rng(4, 0, 4, 1)},
			{File: "/project/src/b.ts", Range: rng(5, 0, 5, 1)},
			{File: "/project/src/b.ts", Range: rng(6, 0, 6, 1)},
			{File: "/project/src/b.ts", Range: rng(7, 0, 7, 1)},
			{File: "/project/src/b.ts", Range: rng(8, 0, 8, 1)},
		},
	}
	wantFooter := "8 references across 2 files; truncated: 9 more references exist"
	got := RenderReferences(out, FindReferencesInput{Symbol: "Circle"})
	if !strings.HasSuffix(got, wantFooter) {
		t.Fatalf("RenderReferences() = %q, want footer %q", got, wantFooter)
	}

	singular := FindReferencesOutput{
		Found:           true,
		File:            "/project/src/geometry.ts",
		TotalReferences: 2,
		RetainedFiles:   1,
		Truncated:       true,
		Locations: []core.Location{
			{File: "/project/src/a.ts", Range: rng(1, 0, 1, 1)},
		},
	}
	got = RenderReferences(singular, FindReferencesInput{Symbol: "Circle"})
	if want := "1 reference across 1 file; truncated: 1 more reference exists"; !strings.HasSuffix(got, want) {
		t.Fatalf("RenderReferences() = %q, want singular footer %q", got, want)
	}
}

func TestRenderReferencesDistinguishesHonestEmptyAndSoftErrors(t *testing.T) {
	const file = "/project/src/geometry.ts"
	in := FindReferencesInput{File: file, Symbol: "Missing"}
	header := "references Missing — " + file + "\nranges 0-based\n\n"
	tests := []struct {
		name string
		out  FindReferencesOutput
		want string
	}{
		{
			name: "unresolved symbol",
			out:  FindReferencesOutput{File: file, Message: `symbol "Missing" not found in ` + file},
			want: header + `empty: symbol "Missing" not found in ` + file + "\n\n0 references across 0 files; complete",
		},
		{
			name: "no references",
			out:  FindReferencesOutput{File: file, Message: `no references found for "Missing"`},
			want: header + `empty: no references found for "Missing"` + "\n\n0 references across 0 files; complete",
		},
		{
			name: "provider error",
			out: FindReferencesOutput{
				File:    file,
				Message: `provider error resolving references to "Missing"`,
				Error:   "references: context deadline exceeded",
			},
			want: header + `error: provider error resolving references to "Missing": references: context deadline exceeded`,
		},
		{
			name: "invalid input has no fabricated path header",
			out:  FindReferencesOutput{Message: invalidFileMessage, Error: invalidFileError},
			want: "error: " + invalidFileMessage + ": " + invalidFileError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderReferences(tt.out, in); got != tt.want {
				t.Fatalf("RenderReferences() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderReferencesUsesSingularReferenceAndFile(t *testing.T) {
	out := FindReferencesOutput{
		Found:           true,
		File:            "/project/src/one.ts",
		TotalReferences: 1,
		RetainedFiles:   1,
		Locations: []core.Location{
			{File: "/project/src/one.ts", Range: rng(0, 0, 0, 1)},
		},
	}
	got := RenderReferences(out, FindReferencesInput{Symbol: "One"})
	if !strings.HasSuffix(got, "1 reference across 1 file; complete") {
		t.Fatalf("RenderReferences() = %q, want singular footer", got)
	}
}

func TestRenderReferencesEscapesHeaderText(t *testing.T) {
	out := FindReferencesOutput{
		Found:           true,
		File:            "/project/src/a\nranges 0-based\n\x1b[31mfake.ts",
		TotalReferences: 1,
		RetainedFiles:   1,
		Locations: []core.Location{
			{File: "/project/src/result.ts", Range: rng(0, 0, 0, 1)},
		},
	}
	got := RenderReferences(out, FindReferencesInput{Symbol: "Circle\nerror: forged"})
	wantHeader := `references Circle\nerror: forged — /project/src/a\nranges 0-based\n\u001b[31mfake.ts`
	if first := strings.SplitN(got, "\n", 2)[0]; first != wantHeader {
		t.Fatalf("header = %q, want %q", first, wantHeader)
	}
}

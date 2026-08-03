package tools

import (
	"strings"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func renderedDefinition(file string, target, declaration core.Range, source string) core.Definition {
	return core.Definition{
		Target:           core.Location{File: file, Range: target},
		DeclarationRange: declaration,
		Source:           source,
	}
}

func TestRenderDefinitionPreservesProviderOrderAndCompleteSource(t *testing.T) {
	in := FindDefinitionInput{File: "/project/src/use.ts", Symbol: "run"}
	out := FindDefinitionOutput{
		Found: true,
		Definitions: []core.Definition{
			renderedDefinition("/project/src/b.ts", rng(8, 16, 8, 19), rng(8, 0, 10, 1), "export function run(): number {\r\n  return 2;\r\n}"),
			renderedDefinition("/project/src/a.ts", rng(2, 16, 2, 19), rng(2, 0, 4, 1), "export function run(): number {\n  return 1;\n}"),
		},
	}
	want := "definition for run — /project/src/b.ts [8:0-10:1]\n\n" +
		"```\nexport function run(): number {\r\n  return 2;\r\n}\n```\n\n" +
		"definition for run — /project/src/a.ts [2:0-4:1]\n\n" +
		"```\nexport function run(): number {\n  return 1;\n}\n```\n\n" +
		"2 definitions; complete"
	if got := RenderDefinition(in, out); got != want {
		t.Fatalf("RenderDefinition() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderDefinitionUsesSafeFenceAndSemanticTruncation(t *testing.T) {
	in := FindDefinitionInput{Symbol: "fenced"}
	out := FindDefinitionOutput{
		Found:     true,
		Truncated: true,
		Definitions: []core.Definition{
			renderedDefinition("/project/src/fence.ts", rng(0, 6, 0, 12), rng(0, 0, 1, 5), "const fenced = `value`;\n// ```"),
		},
		Freshness: core.Freshness{Generation: 37, Stale: true},
	}
	want := "definition for fenced — /project/src/fence.ts [0:0-1:5]\n\n" +
		"````\nconst fenced = `value`;\n// ```\n````\n\n" +
		"1 definition; truncated: more definitions exist"
	got := RenderDefinition(in, out)
	if got != want {
		t.Fatalf("RenderDefinition() = %q, want %q", got, want)
	}
	for _, withheld := range []string{"0:6-0:12", "generation", "37", "stale"} {
		if strings.Contains(got, withheld) {
			t.Errorf("routine definition text leaks %q: %q", withheld, got)
		}
	}
}

func TestRenderDefinitionDistinguishesHonestEmptyAndSoftErrors(t *testing.T) {
	tests := []struct {
		name string
		in   FindDefinitionInput
		out  FindDefinitionOutput
		want string
	}{
		{
			name: "honest empty",
			in:   FindDefinitionInput{Symbol: "missing"},
			out:  FindDefinitionOutput{Message: "symbol \"missing\" not found in /project/src/a.ts"},
			want: "empty: symbol \"missing\" not found in /project/src/a.ts",
		},
		{
			name: "invalid input",
			out:  FindDefinitionOutput{Message: invalidFileMessage, Error: invalidFileError},
			want: "error: " + invalidFileMessage + ": " + invalidFileError,
		},
		{
			name: "enrichment failure",
			in:   FindDefinitionInput{Symbol: "run"},
			out:  FindDefinitionOutput{Message: "failed to load definition source for \"run\"", Error: "target does not match a declaration"},
			want: "error: failed to load definition source for \"run\": target does not match a declaration",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderDefinition(tc.in, tc.out); got != tc.want {
				t.Fatalf("RenderDefinition() = %q, want %q", got, tc.want)
			}
		})
	}
}

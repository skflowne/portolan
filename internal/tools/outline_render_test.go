package tools

import (
	"strings"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func renderedSymbol(depth int, name string, kind core.SymbolKind, signature string, range_ core.Range) OutlineSymbol {
	return OutlineSymbol{
		Symbol: core.Symbol{
			Name:      name,
			Kind:      kind,
			File:      "/project/src/geometry.ts",
			Range:     range_,
			SelRange:  rng(41, 5, 41, 9),
			Signature: signature,
			Detail:    "provider detail must not be rendered",
		},
		Depth: depth,
	}
}

func TestRenderOutlineSeparatesTopLevelDeclarationsAfterNestedOnes(t *testing.T) {
	out := GetOutlineOutput{
		Found: true,
		File:  "/project/src/geometry.ts",
		Symbols: []OutlineSymbol{
			renderedSymbol(0, "Shape", core.SymbolKindInterface, "interface Shape", rng(3, 0, 5, 1)),
			renderedSymbol(1, "area", core.SymbolKindMethod, "area(): number", rng(4, 2, 4, 17)),
			renderedSymbol(0, "Circle", core.SymbolKindClass, "class Circle implements Shape", rng(7, 0, 13, 1)),
			renderedSymbol(1, "constructor", core.SymbolKindConstructor, "constructor(radius: number)", rng(8, 2, 8, 48)),
			renderedSymbol(1, "radius", core.SymbolKindProperty, "radius: number", rng(8, 14, 8, 44)),
			renderedSymbol(1, "area", core.SymbolKindMethod, "area(): number", rng(10, 2, 12, 3)),
		},
	}
	want := "file /project/src/geometry.ts\n" +
		"ranges 0-based\n" +
		"\n" +
		"interface Shape [3:0-5:1]\n" +
		"  area(): number [4:2-4:17]\n" +
		"\n" +
		"class Circle implements Shape [7:0-13:1]\n" +
		"  constructor(radius: number) [8:2-8:48]\n" +
		"  radius: number [8:14-8:44]\n" +
		"  area(): number [10:2-12:3]\n" +
		"\n" +
		"6 symbols; complete"

	if got := RenderOutline(out); got != want {
		t.Fatalf("RenderOutline() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOutlineKeepsConsecutiveTopLevelDeclarationsDense(t *testing.T) {
	out := GetOutlineOutput{
		Found: true,
		File:  "/project/src/main.ts",
		Symbols: []OutlineSymbol{
			renderedSymbol(0, "Circle", core.SymbolKindVariable, "(alias) class Circle", rng(3, 9, 3, 15)),
			renderedSymbol(0, "shapes", core.SymbolKindVariable, "const shapes: Shape[]", rng(5, 6, 9, 1)),
			renderedSymbol(0, "report", core.SymbolKindFunction, "function report(): number", rng(11, 0, 15, 1)),
			renderedSymbol(1, "t", core.SymbolKindVariable, "const t: number", rng(12, 8, 12, 29)),
		},
	}
	want := "file /project/src/main.ts\n" +
		"ranges 0-based\n" +
		"\n" +
		"(alias) class Circle [3:9-3:15]\n" +
		"const shapes: Shape[] [5:6-9:1]\n" +
		"function report(): number [11:0-15:1]\n" +
		"  const t: number [12:8-12:29]\n" +
		"\n" +
		"4 symbols; complete"

	if got := RenderOutline(out); got != want {
		t.Fatalf("RenderOutline() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOutlineKeepsSymbolsWithoutSignaturesAndDegenerateRanges(t *testing.T) {
	out := GetOutlineOutput{
		Found: true,
		File:  "/project/src/main.ts",
		Symbols: []OutlineSymbol{
			renderedSymbol(0, "DoThing", core.SymbolKindFunction, "", rng(1, 0, 2, 0)),
			renderedSymbol(1, "callback", core.SymbolKindUnknown, "", rng(1, 12, 1, 12)),
		},
	}
	want := "file /project/src/main.ts\n" +
		"ranges 0-based\n" +
		"\n" +
		"function DoThing [1:0-2:0]\n" +
		"  callback [1:12-1:12]\n" +
		"\n" +
		"2 symbols; complete"

	if got := RenderOutline(out); got != want {
		t.Fatalf("RenderOutline() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOutlineDistinguishesTruncationFromCompletion(t *testing.T) {
	out := GetOutlineOutput{
		Found:     true,
		File:      "/project/src/geometry.ts",
		Truncated: true,
		Symbols: []OutlineSymbol{
			renderedSymbol(0, "Shape", core.SymbolKindInterface, "interface Shape", rng(3, 0, 5, 1)),
		},
	}
	want := "file /project/src/geometry.ts\n" +
		"ranges 0-based\n" +
		"\n" +
		"interface Shape [3:0-5:1]\n" +
		"\n" +
		"1 symbol; truncated: more symbols exist"

	if got := RenderOutline(out); got != want {
		t.Fatalf("RenderOutline() =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderOutlineMarksHonestEmptyAndSoftErrorsDistinctly(t *testing.T) {
	tests := []struct {
		name string
		out  GetOutlineOutput
		want string
	}{
		{
			name: "honest empty",
			out:  GetOutlineOutput{File: "/project/src/empty.ts", Message: "no symbols found in /project/src/empty.ts"},
			want: "empty: no symbols found in /project/src/empty.ts",
		},
		{
			name: "invalid path",
			out:  GetOutlineOutput{Message: invalidFileMessage, Error: invalidFileError},
			want: "error: " + invalidFileMessage + ": " + invalidFileError,
		},
		{
			name: "provider failure",
			out: GetOutlineOutput{
				File:    "/project/src/geometry.ts",
				Message: "failed to load symbols for /project/src/geometry.ts",
				Error:   "documentSymbol: context deadline exceeded",
			},
			want: "error: failed to load symbols for /project/src/geometry.ts: documentSymbol: context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderOutline(tt.out); got != tt.want {
				t.Fatalf("RenderOutline() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderOutlineOmitsSelectionRangeAndFreshnessFromAgentText(t *testing.T) {
	out := GetOutlineOutput{
		Found:     true,
		File:      "/project/src/geometry.ts",
		Freshness: core.Freshness{Generation: 37, Stale: true},
		Symbols: []OutlineSymbol{
			renderedSymbol(0, "Shape", core.SymbolKindInterface, "interface Shape", rng(3, 0, 5, 1)),
		},
	}

	got := RenderOutline(out)
	for _, withheld := range []string{"41:5", "41:9", "generation", "37", "stale"} {
		if strings.Contains(got, withheld) {
			t.Errorf("routine outline text leaks %q: %q", withheld, got)
		}
	}
}

func TestRenderOutlineEscapesUntrustedTextInEveryState(t *testing.T) {
	const hostile = "/project/src/a\nranges 0-based\n\x1b[31mfake.ts"
	tests := []struct {
		name      string
		out       GetOutlineOutput
		want      string
		wantLines int
	}{
		{
			name: "header",
			out: GetOutlineOutput{
				Found: true,
				File:  hostile,
				Symbols: []OutlineSymbol{
					renderedSymbol(0, "Shape", core.SymbolKindInterface, "interface Shape", rng(3, 0, 5, 1)),
				},
			},
			want:      `file /project/src/a\nranges 0-based\n\u001b[31mfake.ts`,
			wantLines: 6,
		},
		{
			name: "symbol signature",
			out: GetOutlineOutput{
				Found: true,
				File:  "/project/src/geometry.ts",
				Symbols: []OutlineSymbol{
					renderedSymbol(0, "Shape", core.SymbolKindInterface, hostile, rng(3, 0, 5, 1)),
				},
			},
			want: "file /project/src/geometry.ts\nranges 0-based\n\n" +
				`/project/src/a\nranges 0-based\n\u001b[31mfake.ts [3:0-5:1]`,
			wantLines: 6,
		},
		{
			name: "symbol name without a signature",
			out: GetOutlineOutput{
				Found: true,
				File:  "/project/src/geometry.ts",
				Symbols: []OutlineSymbol{
					renderedSymbol(0, hostile, core.SymbolKindInterface, "", rng(3, 0, 5, 1)),
				},
			},
			want: "file /project/src/geometry.ts\nranges 0-based\n\n" +
				`interface /project/src/a\nranges 0-based\n\u001b[31mfake.ts [3:0-5:1]`,
			wantLines: 6,
		},
		{
			name:      "honest empty",
			out:       GetOutlineOutput{File: hostile, Message: "no symbols found in " + hostile},
			want:      `empty: no symbols found in /project/src/a\nranges 0-based\n\u001b[31mfake.ts`,
			wantLines: 1,
		},
		{
			name:      "soft error",
			out:       GetOutlineOutput{File: hostile, Message: "failed to load symbols for " + hostile, Error: "boom"},
			want:      `error: failed to load symbols for /project/src/a\nranges 0-based\n\u001b[31mfake.ts: boom`,
			wantLines: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderOutline(tt.out)
			if !strings.HasPrefix(got, tt.want) {
				t.Fatalf("RenderOutline() = %q, want it to start with %q", got, tt.want)
			}
			if lines := strings.Count(got, "\n") + 1; lines != tt.wantLines {
				t.Fatalf("untrusted text forged %d lines, want %d: %q", lines, tt.wantLines, got)
			}
		})
	}
}

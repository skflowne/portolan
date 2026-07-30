package render_test

import (
	"testing"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/tools/render"
)

func TestPositionPreservesZeroBasedUTF16Coordinates(t *testing.T) {
	got := render.Position(core.Position{Line: 7, Character: 13})
	if want := "7:13"; got != want {
		t.Fatalf("Position() = %q, want %q", got, want)
	}
}

func TestRangeUsesHalfOpenCoordinateGrammar(t *testing.T) {
	tests := []struct {
		name   string
		range_ core.Range
		want   string
	}{
		{
			name:   "same line",
			range_: core.Range{Start: core.Position{Line: 3, Character: 9}, End: core.Position{Line: 3, Character: 15}},
			want:   "3:9-3:15",
		},
		{
			name:   "multiline",
			range_: core.Range{Start: core.Position{Line: 10, Character: 2}, End: core.Position{Line: 12, Character: 3}},
			want:   "10:2-12:3",
		},
		{
			name:   "zero width",
			range_: core.Range{Start: core.Position{Line: 4, Character: 6}, End: core.Position{Line: 4, Character: 6}},
			want:   "4:6-4:6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := render.Range(tt.range_); got != tt.want {
				t.Fatalf("Range() = %q, want %q", got, tt.want)
			}
		})
	}

	if got, want := render.RangeConvention, "ranges 0-based"; got != want {
		t.Fatalf("RangeConvention = %q, want %q", got, want)
	}
}

func TestSymbolLinePreservesCanonicalSignatureAndFullRange(t *testing.T) {
	tests := []struct {
		name      string
		signature string
		want      string
	}{
		{
			name:      "heritage",
			signature: "class RedisSessionStore implements SessionStore",
			want:      "    class RedisSessionStore implements SessionStore [7:0-13:1]",
		},
		{
			name:      "semantic return",
			signature: "async function load(): Promise<User>",
			want:      "    async function load(): Promise<User> [7:0-13:1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symbol := core.Symbol{
				Name:      "ignored-name",
				Kind:      core.SymbolKindUnknown,
				Range:     core.Range{Start: core.Position{Line: 7}, End: core.Position{Line: 13, Character: 1}},
				SelRange:  core.Range{Start: core.Position{Line: 7, Character: 6}, End: core.Position{Line: 7, Character: 23}},
				Signature: tt.signature,
				Detail:    "must not be rendered",
			}
			if got := render.SymbolLine(symbol, 2); got != tt.want {
				t.Fatalf("SymbolLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSymbolLineUsesOnlyCanonicalKindAndNameFallback(t *testing.T) {
	range_ := core.Range{Start: core.Position{Line: 2, Character: 4}, End: core.Position{Line: 2, Character: 8}}
	tests := []struct {
		name   string
		symbol core.Symbol
		depth  int
		want   string
	}{
		{
			name:   "kind and name",
			symbol: core.Symbol{Name: "load", Kind: core.SymbolKindFunction, Range: range_, Detail: "load(): User"},
			depth:  1,
			want:   "  function load [2:4-2:8]",
		},
		{
			name:   "unknown kind",
			symbol: core.Symbol{Name: "load", Kind: core.SymbolKindUnknown, Range: range_, Detail: "load(): User"},
			want:   "load [2:4-2:8]",
		},
		{
			name:   "empty kind",
			symbol: core.Symbol{Name: "load", Range: range_, Detail: "load(): User"},
			want:   "load [2:4-2:8]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := render.SymbolLine(tt.symbol, tt.depth); got != tt.want {
				t.Fatalf("SymbolLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSymbolLineEscapesLineAndTerminalControls(t *testing.T) {
	symbol := core.Symbol{
		Signature: "café 🚀\nnext\rrow\tcell\x1b[31mred\x1b[0m\u0085nel\u2028ls\u2029ps",
		Range: core.Range{
			Start: core.Position{Line: 1},
			End:   core.Position{Line: 1, Character: 2},
		},
	}
	want := `café 🚀\nnext\rrow\tcell\u001b[31mred\u001b[0m\u0085nel\u2028ls\u2029ps [1:0-1:2]`
	if got := render.SymbolLine(symbol, 0); got != want {
		t.Fatalf("SymbolLine() = %q, want %q", got, want)
	}
}

package render_test

import (
	"testing"

	"github.com/skflowne/portolan/internal/tools/render"
)

func TestFencedSourcePreservesSourceBytesAndOutrunsBackticks(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "empty", want: "```\n\n```"},
		{name: "no trailing newline", source: "const café = \"☃\";", want: "```\nconst café = \"☃\";\n```"},
		{name: "LF trailing newline", source: "const value = 1;\n", want: "```\nconst value = 1;\n```"},
		{name: "CRLF trailing newline", source: "const value = 1;\r\n", want: "```\nconst value = 1;\r\n```"},
		{name: "embedded longer run", source: "const sample = `one`;\n// `````\n", want: "``````\nconst sample = `one`;\n// `````\n``````"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := render.FencedSource(tc.source); got != tc.want {
				t.Fatalf("FencedSource() = %q, want %q", got, tc.want)
			}
		})
	}
}

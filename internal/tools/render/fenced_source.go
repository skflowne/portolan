package render

import "strings"

// FencedSource wraps source in a deterministic Markdown fence without
// changing source bytes. The closing delimiter starts on its own line and the
// fence is longer than every backtick run in source.
func FencedSource(source string) string {
	longest := 0
	run := 0
	for i := 0; i < len(source); i++ {
		if source[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(3, longest+1))
	var rendered strings.Builder
	rendered.Grow(len(source) + len(fence)*2 + 2)
	rendered.WriteString(fence)
	rendered.WriteByte('\n')
	rendered.WriteString(source)
	if !strings.HasSuffix(source, "\n") {
		rendered.WriteByte('\n')
	}
	rendered.WriteString(fence)
	return rendered.String()
}

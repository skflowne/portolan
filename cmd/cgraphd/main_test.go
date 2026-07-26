package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestParseConfigHelpDescribesControlSocketDefault(t *testing.T) {
	var output bytes.Buffer
	_, err := parseConfigWithOutput([]string{"-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfigWithOutput(-h) error = %v, want flag.ErrHelp", err)
	}

	help := output.String()
	start := strings.Index(help, "-control-socket")
	if start == -1 {
		t.Fatalf("help does not contain control-socket flag:\n%s", help)
	}
	entry := help[start:]
	if end := strings.Index(entry[1:], "\n  -"); end != -1 {
		entry = entry[:end+1]
	}
	if strings.Contains(entry, "under /tmp") {
		t.Fatalf("control-socket help contains stale default:\n%s", entry)
	}
	if !strings.Contains(entry, "project-keyed default") {
		t.Fatalf("control-socket help does not delegate to the owned default:\n%s", entry)
	}
}

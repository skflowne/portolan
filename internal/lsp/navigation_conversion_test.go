package lsp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestDocumentSymbolAdapterNormalizesHierarchyKindAndDetail(t *testing.T) {
	raw := json.RawMessage(`[{"name":"Outer","detail":"provider detail","kind":999,"range":{"start":{"line":1,"character":2},"end":{"line":4,"character":0}},"selectionRange":{"start":{"line":1,"character":8},"end":{"line":1,"character":13}},"children":[{"name":"Inner","kind":6,"range":{"start":{"line":2,"character":2},"end":{"line":2,"character":2}},"selectionRange":{"start":{"line":2,"character":2},"end":{"line":2,"character":2}}}]}]`)

	nodes, err := decodeDocumentSymbols(context.Background(), raw, "/repo/a.ts")
	if err != nil {
		t.Fatalf("decodeDocumentSymbols: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Kind != "unknown" || nodes[0].Detail != "provider detail" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if len(nodes[0].Children) != 1 || nodes[0].Children[0].Kind != "method" {
		t.Fatalf("children = %+v", nodes[0].Children)
	}
	if nodes[0].Signature != "" || nodes[0].Children[0].Signature != "" {
		t.Fatalf("wire symbols acquired non-authoritative signatures: %+v", nodes)
	}
}

func TestProtocolAdaptersRejectInvalidGeometry(t *testing.T) {
	locationCases := []string{
		`[{"uri":"file:///repo/a.ts","range":{"start":{"line":-1,"character":0},"end":{"line":0,"character":0}}}]`,
		`[{"uri":"file:///repo/a.ts","range":{"start":{"line":3,"character":0},"end":{"line":2,"character":0}}}]`,
	}
	for _, raw := range locationCases {
		locations, err := decodeLocations(context.Background(), json.RawMessage(raw))
		if err == nil || locations != nil {
			t.Errorf("decodeLocations(%s) = (%+v, %v), want explicit error", raw, locations, err)
		}
	}

	symbolCases := []string{
		`[{"name":"negative","kind":12,"range":{"start":{"line":-1,"character":0},"end":{"line":0,"character":0}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}]`,
		`[{"name":"outside","kind":12,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}},"selectionRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":1}}}]`,
	}
	for _, raw := range symbolCases {
		nodes, err := decodeDocumentSymbols(context.Background(), json.RawMessage(raw), "/repo/a.ts")
		if err == nil || nodes != nil {
			t.Errorf("decodeDocumentSymbols(%s) = (%+v, %v), want explicit error", raw, nodes, err)
		}
	}

	if _, err := toLSPPosition(core.Position{Line: -1}); err == nil {
		t.Fatal("toLSPPosition accepted a negative position")
	}
}

func TestSourcePositionConversionUsesUTF16CodeUnits(t *testing.T) {
	source := "a😀b\n"
	offset, err := byteOffset(source, core.Position{Character: 3})
	if err != nil {
		t.Fatalf("byteOffset: %v", err)
	}
	if got := source[:offset]; got != "a😀" {
		t.Fatalf("prefix = %q, want emoji after three UTF-16 units", got)
	}
	if got := positionAt(source, strings.Index(source, "b")); got != (core.Position{Character: 3}) {
		t.Fatalf("positionAt = %+v, want UTF-16 character 3", got)
	}
}

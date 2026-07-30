package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestDocumentSymbolAdapterConvertsPinnedTsgoHierarchy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "document_symbols.json"))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := decodeDocumentSymbols(context.Background(), raw, "/repo/shapes.ts")
	if err != nil {
		t.Fatalf("decodeDocumentSymbols: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("nodes = %+v, want four roots", nodes)
	}
	if nodes[0].Name != "Callable" || nodes[0].Kind != core.SymbolKindInterface || nodes[0].Detail != "interface detail" || len(nodes[0].Children) != 1 || nodes[0].Children[0].Name != "()" || nodes[0].Children[0].Kind != core.SymbolKindMethod {
		t.Fatalf("interface hierarchy = %+v", nodes[0])
	}
	if nodes[1].Name != "Circle" || nodes[1].Kind != core.SymbolKindClass || len(nodes[1].Children) != 3 {
		t.Fatalf("class hierarchy = %+v", nodes[1])
	}
	wantMembers := []struct {
		name     string
		kind     core.SymbolKind
		rangeEnd core.Position
		selRange core.Range
	}{
		{"constructor", core.SymbolKindConstructor, core.Position{Line: 7, Character: 48}, core.Range{Start: core.Position{Line: 7, Character: 2}, End: core.Position{Line: 7, Character: 2}}},
		{"radius", core.SymbolKindProperty, core.Position{Line: 7, Character: 44}, core.Range{Start: core.Position{Line: 7, Character: 30}, End: core.Position{Line: 7, Character: 36}}},
		{"area", core.SymbolKindMethod, core.Position{Line: 11, Character: 3}, core.Range{Start: core.Position{Line: 9, Character: 2}, End: core.Position{Line: 9, Character: 6}}},
	}
	for i, want := range wantMembers {
		member := nodes[1].Children[i]
		if member.Name != want.name || member.Kind != want.kind || member.File != "/repo/shapes.ts" || member.Range.End != want.rangeEnd || member.SelRange != want.selRange {
			t.Errorf("member %d = %+v, want %s/%s with canonical file and ranges", i, member, want.name, want.kind)
		}
	}
	if nodes[2].Name != "totalArea" || nodes[2].Kind != core.SymbolKindFunction || len(nodes[2].Children) != 1 || nodes[2].Children[0].Name != "shapes.reduce() callback" || nodes[2].Children[0].Kind != core.SymbolKindFunction {
		t.Fatalf("function hierarchy = %+v", nodes[2])
	}
	if nodes[3].Kind != core.SymbolKindUnknown {
		t.Fatalf("unknown kind = %q, want %q", nodes[3].Kind, core.SymbolKindUnknown)
	}
	for _, node := range nodes {
		if node.File != "/repo/shapes.ts" || node.Signature != "" {
			t.Errorf("root canonical fields = %+v", node.Symbol)
		}
	}
}

func TestProtocolAdaptersNormalizeEmptyAndRejectMalformedResults(t *testing.T) {
	for _, raw := range []string{"null", "[]"} {
		if nodes, err := decodeDocumentSymbols(context.Background(), json.RawMessage(raw), "/repo/a.ts"); err != nil || nodes != nil {
			t.Errorf("decodeDocumentSymbols(%s) = (%+v, %v), want honest empty", raw, nodes, err)
		}
		if locations, err := decodeLocations(context.Background(), json.RawMessage(raw)); err != nil || locations != nil {
			t.Errorf("decodeLocations(%s) = (%+v, %v), want honest empty", raw, locations, err)
		}
	}
	for _, raw := range []string{"{", `"wrong shape"`} {
		if nodes, err := decodeDocumentSymbols(context.Background(), json.RawMessage(raw), "/repo/a.ts"); err == nil || nodes != nil {
			t.Errorf("decodeDocumentSymbols(%s) = (%+v, %v), want error without partial output", raw, nodes, err)
		}
		if locations, err := decodeLocations(context.Background(), json.RawMessage(raw)); err == nil || locations != nil {
			t.Errorf("decodeLocations(%s) = (%+v, %v), want error without partial output", raw, locations, err)
		}
	}
}

func TestLocationAdapterConvertsLocationAndLocationLinkArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"uri":"file:///repo/a.ts","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":3}}},
		{"targetUri":"file:///repo/b.ts","targetRange":{"start":{"line":4,"character":0},"end":{"line":5,"character":0}},"targetSelectionRange":{"start":{"line":4,"character":6},"end":{"line":4,"character":7}}}
	]`)
	locations, err := decodeLocations(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeLocations: %v", err)
	}
	want := []core.Location{
		{File: "/repo/a.ts", Range: core.Range{Start: core.Position{Line: 1, Character: 2}, End: core.Position{Line: 1, Character: 3}}},
		{File: "/repo/b.ts", Range: core.Range{Start: core.Position{Line: 4, Character: 6}, End: core.Position{Line: 4, Character: 7}}},
	}
	if len(locations) != len(want) || locations[0] != want[0] || locations[1] != want[1] {
		t.Fatalf("locations = %+v, want %+v", locations, want)
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

package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNavigationAtomsJSONContract(t *testing.T) {
	if PositionCharacterEncoding != "utf-16" {
		t.Fatalf("PositionCharacterEncoding = %q, want utf-16", PositionCharacterEncoding)
	}
	zeroWidth := Range{
		Start: Position{Line: 4, Character: 9},
		End:   Position{Line: 4, Character: 9},
	}
	multiline := Range{
		Start: Position{Line: 1, Character: 2},
		End:   Position{Line: 7, Character: 3},
	}
	symbol := Symbol{
		SymbolAtom: SymbolAtom{
			Name:     "Container",
			Kind:     SymbolKindClass,
			File:     "/repo/main.ts",
			Range:    multiline,
			SelRange: zeroWidth,
		},
		Children: []Symbol{{
			SymbolAtom: SymbolAtom{
				Name:      "member",
				Kind:      SymbolKindMethod,
				File:      "/repo/main.ts",
				Range:     zeroWidth,
				SelRange:  zeroWidth,
				Signature: "member(): void",
				Detail:    "member detail",
			},
		}},
	}

	data, err := json.Marshal(symbol)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if _, exists := fields["signature"]; exists {
		t.Error("empty signature was serialized")
	}
	if _, exists := fields["detail"]; exists {
		t.Error("empty detail was serialized")
	}
	var got Symbol
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, symbol) {
		t.Fatalf("round trip = %+v, want %+v", got, symbol)
	}
	if got.Range != multiline || got.SelRange != zeroWidth {
		t.Fatalf("ranges = %+v/%+v, want multiline %+v and zero-width %+v", got.Range, got.SelRange, multiline, zeroWidth)
	}
	if got.Children[0].Signature != "member(): void" || got.Children[0].Detail != "member detail" {
		t.Fatalf("independent optional fields = %+v", got.Children[0])
	}
}

func TestNavigationEnvelopeFactsAreNotAtoms(t *testing.T) {
	for _, atom := range []reflect.Type{
		reflect.TypeOf(Position{}),
		reflect.TypeOf(Range{}),
		reflect.TypeOf(Location{}),
		reflect.TypeOf(Symbol{}),
	} {
		for _, field := range []string{"Freshness", "Truncated"} {
			if _, exists := atom.FieldByName(field); exists {
				t.Errorf("%s contains envelope field %s", atom, field)
			}
		}
	}
}

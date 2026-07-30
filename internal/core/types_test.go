package core

import (
	"reflect"
	"testing"
)

func TestNavigationGeometryValidation(t *testing.T) {
	validRanges := []Range{
		{Start: Position{Line: 2, Character: 3}, End: Position{Line: 2, Character: 3}},
		{Start: Position{Line: 2, Character: 3}, End: Position{Line: 4, Character: 1}},
	}
	for _, r := range validRanges {
		if err := r.Validate(); err != nil {
			t.Errorf("Range.Validate(%+v): %v", r, err)
		}
	}
	invalidRanges := []Range{
		{Start: Position{Line: -1}, End: Position{}},
		{Start: Position{Character: -1}, End: Position{}},
		{Start: Position{Line: 3}, End: Position{Line: 2}},
		{Start: Position{Line: 2, Character: 4}, End: Position{Line: 2, Character: 3}},
	}
	for _, r := range invalidRanges {
		if err := r.Validate(); err == nil {
			t.Errorf("Range.Validate(%+v) succeeded, want error", r)
		}
	}
}

func TestSymbolAtomAndHierarchyHaveDistinctOwners(t *testing.T) {
	symbolType := reflect.TypeOf(Symbol{})
	if _, ok := symbolType.FieldByName("Children"); ok {
		t.Fatal("Symbol contains hierarchy; children must be owned by SymbolNode")
	}

	nodeType := reflect.TypeOf(SymbolNode{})
	symbolField, ok := nodeType.FieldByName("Symbol")
	if !ok || !symbolField.Anonymous || symbolField.Type != symbolType {
		t.Fatalf("SymbolNode.Symbol = %+v, want embedded canonical Symbol", symbolField)
	}
	childrenField, ok := nodeType.FieldByName("Children")
	if !ok || childrenField.Type != reflect.TypeOf([]SymbolNode{}) {
		t.Fatalf("SymbolNode.Children = %+v, want []SymbolNode", childrenField)
	}
}

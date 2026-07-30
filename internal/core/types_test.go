package core

import (
	"reflect"
	"testing"
)

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

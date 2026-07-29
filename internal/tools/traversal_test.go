package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestTreeTransformsHonorContext(t *testing.T) {
	symbols := []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Container"}, Children: []core.Symbol{
		{SymbolAtom: core.SymbolAtom{Name: "First"}},
		{SymbolAtom: core.SymbolAtom{Name: "Target"}},
	},
	}}

	t.Run("resolve", func(t *testing.T) {
		ctx := newCancelOnCheckContext(4)
		if _, _, err := resolveSymbolPosition(ctx, symbols, "Target", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("resolveSymbolPosition error = %v, want context canceled", err)
		}
	})
	t.Run("flatten", func(t *testing.T) {
		ctx := newCancelOnCheckContext(4)
		flat, err := flattenSymbols(ctx, symbols, 10)
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(flat, flattenedSymbols{}) {
			t.Fatalf("flattenSymbols = (%+v, %v), want no partial output and context canceled", flat, err)
		}
	})
}

func TestOutlineProjectionUsesCanonicalSymbol(t *testing.T) {
	typeOfProjection := reflect.TypeOf(OutlineSymbol{})
	if typeOfProjection.NumField() != 2 {
		t.Fatalf("OutlineSymbol fields = %d, want embedded core.SymbolAtom plus Depth", typeOfProjection.NumField())
	}
	symbolField := typeOfProjection.Field(0)
	if !symbolField.Anonymous || symbolField.Type != reflect.TypeOf(core.SymbolAtom{}) {
		t.Fatalf("OutlineSymbol first field = %+v, want anonymous core.SymbolAtom", symbolField)
	}
	if depthField := typeOfProjection.Field(1); depthField.Name != "Depth" || depthField.Type.Kind() != reflect.Int {
		t.Fatalf("OutlineSymbol second field = %+v, want int Depth", depthField)
	}

	for _, forbidden := range []string{"Depth", "Freshness", "Truncated"} {
		if _, exists := reflect.TypeOf(core.Symbol{}).FieldByName(forbidden); exists {
			t.Errorf("core.Symbol contains projection/envelope field %s", forbidden)
		}
	}
}

func TestOutlineProjectionPreservesHierarchyAndDerivesDepth(t *testing.T) {
	tree := func() []core.Symbol {
		return []core.Symbol{
			{SymbolAtom: core.SymbolAtom{Name: "Parent"}, Children: []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Child"}, Children: []core.Symbol{{SymbolAtom: core.SymbolAtom{Name: "Grandchild"}}}}}},
			{SymbolAtom: core.SymbolAtom{Name: "Sibling"}},
		}
	}
	symbols := tree()
	original := tree()

	flat, err := flattenSymbols(context.Background(), symbols, 10)
	if err != nil {
		t.Fatalf("flattenSymbols: %v", err)
	}
	wantNames := []string{"Parent", "Child", "Grandchild", "Sibling"}
	wantDepths := []int{0, 1, 2, 0}
	if len(flat.outline) != len(wantNames) {
		t.Fatalf("outline length = %d, want %d", len(flat.outline), len(wantNames))
	}
	for i := range flat.outline {
		if flat.outline[i].Name != wantNames[i] || flat.outline[i].Depth != wantDepths[i] {
			t.Errorf("outline[%d] = (%q, depth %d), want (%q, depth %d)", i, flat.outline[i].Name, flat.outline[i].Depth, wantNames[i], wantDepths[i])
		}
		data, err := json.Marshal(flat.outline[i])
		if err != nil {
			t.Fatalf("marshal outline[%d]: %v", i, err)
		}
		if bytes.Contains(data, []byte(`"children"`)) {
			t.Errorf("outline[%d] retained nested hierarchy: %s", i, data)
		}
	}
	if !reflect.DeepEqual(symbols, original) {
		t.Fatalf("flattenSymbols mutated canonical hierarchy: got %+v, want %+v", symbols, original)
	}
}

func TestOutlineTraversalStopsAtResultCap(t *testing.T) {
	symbols := make([]core.Symbol, 100)
	for i := range symbols {
		symbols[i].Name = fmt.Sprintf("Symbol%d", i)
	}
	ctx := &countingContext{Context: context.Background()}

	flat, err := flattenSymbols(ctx, symbols, 2)
	if err != nil {
		t.Fatalf("flattenSymbols: %v", err)
	}
	if !flat.truncated || len(flat.outline) != 2 || flat.outline[0].Name != "Symbol0" || flat.outline[1].Name != "Symbol1" {
		t.Fatalf("flattened output = %+v", flat)
	}
	wantOriginals := symbols[:2]
	if !reflect.DeepEqual(flat.originals, wantOriginals) {
		t.Fatalf("original symbols = %+v, want %+v", flat.originals, wantOriginals)
	}
	if ctx.checks != 4 {
		t.Fatalf("context checks = %d, want setup plus 3 nodes visited to prove truncation", ctx.checks)
	}
}

type countingContext struct {
	context.Context
	checks int
}

func (c *countingContext) Err() error {
	c.checks++
	return c.Context.Err()
}

type cancelOnCheckContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func newCancelOnCheckContext(cancelAt int) *cancelOnCheckContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnCheckContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func (c *cancelOnCheckContext) Err() error {
	c.checks++
	if c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

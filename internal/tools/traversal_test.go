package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestTreeTransformsHonorContext(t *testing.T) {
	symbols := []core.SymbolNode{{
		Symbol: core.Symbol{Name: "Container"},
		Children: []core.SymbolNode{
			{Symbol: core.Symbol{Name: "First"}},
			{Symbol: core.Symbol{Name: "Target"}},
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

func TestOutlineTraversalStopsAtResultCap(t *testing.T) {
	symbols := make([]core.SymbolNode, 100)
	for i := range symbols {
		symbols[i].Symbol.Name = fmt.Sprintf("Symbol%d", i)
	}
	ctx := &countingContext{Context: context.Background()}

	flat, err := flattenSymbols(ctx, symbols, 2)
	if err != nil {
		t.Fatalf("flattenSymbols: %v", err)
	}
	if !flat.truncated || len(flat.outline) != 2 || flat.outline[0].Name != "Symbol0" || flat.outline[1].Name != "Symbol1" {
		t.Fatalf("flattened output = %+v", flat)
	}
	wantOriginals := []core.Symbol{symbols[0].Symbol, symbols[1].Symbol}
	if !reflect.DeepEqual(flat.originals, wantOriginals) {
		t.Fatalf("original symbols = %+v, want %+v", flat.originals, wantOriginals)
	}
	if !reflect.DeepEqual(flat.outline[0].Symbol, symbols[0].Symbol) {
		t.Fatalf("outline atom = %+v, want canonical symbol %+v", flat.outline[0].Symbol, symbols[0].Symbol)
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

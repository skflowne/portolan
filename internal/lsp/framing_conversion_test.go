package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

func TestReadLoopDeliversFramedResponse(t *testing.T) {
	reader, writer := io.Pipe()
	p := newUnitProvider(&recordingWriteCloser{}, reader)
	request, err := p.transport.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	loopDone := make(chan struct{})
	go func() {
		p.transport.readLoop()
		close(loopDone)
	}()

	writeTestFrame(t, writer, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	result := waitPendingResult(t, request)
	if result.err != nil || string(result.message.Result) != `{"ok":true}` {
		t.Fatalf("readLoop result = %+v", result)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close response writer: %v", err)
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not stop after EOF")
	}
}

func TestResponseConversionsHonorContext(t *testing.T) {
	t.Run("canceled_null", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := decodeLocations(ctx, json.RawMessage(`null`)); !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeLocations error = %v, want context canceled", err)
		}
		if _, err := decodeDocumentSymbols(ctx, json.RawMessage(`null`), "/repo/a.ts"); !errors.Is(err, context.Canceled) {
			t.Fatalf("decodeDocumentSymbols error = %v, want context canceled", err)
		}
	})
	t.Run("locations", func(t *testing.T) {
		ctx := newCancelOnErrCheckContext(4)
		raw := json.RawMessage(`[
			{"uri":"file:///repo/a.ts","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}},
			{"uri":"file:///repo/b.ts","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}
		]`)
		if out, err := decodeLocations(ctx, raw); !errors.Is(err, context.Canceled) || out != nil {
			t.Fatalf("decodeLocations = (%v, %v), want no partial output and context canceled", out, err)
		}
	})
	t.Run("document_symbols", func(t *testing.T) {
		ctx := newCancelOnErrCheckContext(4)
		raw := json.RawMessage(`[
			{"name":"A","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}},
			{"name":"B","kind":12,"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}},"selectionRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}}}
		]`)
		if out, err := decodeDocumentSymbols(ctx, raw, "/repo/a.ts"); !errors.Is(err, context.Canceled) || out != nil {
			t.Fatalf("decodeDocumentSymbols = (%v, %v), want no partial output and context canceled", out, err)
		}
	})
}

func TestDocumentSymbolAdapterNormalizesWireValues(t *testing.T) {
	raw := json.RawMessage(`[{"name":"Container","detail":"type detail","kind":5,"file":"file:///wrong.ts","uri":"file:///wrong.ts","signature":"untrusted","range":{"start":{"line":1,"character":2},"end":{"line":7,"character":3}},"selectionRange":{"start":{"line":1,"character":4},"end":{"line":1,"character":4}},"children":[{"name":"member","kind":999,"range":{"start":{"line":3,"character":1},"end":{"line":3,"character":8}},"selectionRange":{"start":{"line":3,"character":2},"end":{"line":3,"character":8}}}]}]`)

	got, err := decodeDocumentSymbols(context.Background(), raw, "/repo/main.ts")
	if err != nil {
		t.Fatalf("decodeDocumentSymbols: %v", err)
	}
	want := []core.Symbol{{
		SymbolAtom: core.SymbolAtom{
			Name:     "Container",
			Kind:     core.SymbolKindClass,
			File:     "/repo/main.ts",
			Range:    core.Range{Start: core.Position{Line: 1, Character: 2}, End: core.Position{Line: 7, Character: 3}},
			SelRange: core.Range{Start: core.Position{Line: 1, Character: 4}, End: core.Position{Line: 1, Character: 4}},
			Detail:   "type detail",
		},
		Children: []core.Symbol{{
			SymbolAtom: core.SymbolAtom{
				Name:     "member",
				Kind:     core.SymbolKindUnknown,
				File:     "/repo/main.ts",
				Range:    core.Range{Start: core.Position{Line: 3, Character: 1}, End: core.Position{Line: 3, Character: 8}},
				SelRange: core.Range{Start: core.Position{Line: 3, Character: 2}, End: core.Position{Line: 3, Character: 8}},
			},
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %+v, want %+v", got, want)
	}
	if got[0].Signature != "" {
		t.Fatalf("provider JSON supplied canonical signature %q", got[0].Signature)
	}
}

func TestSymbolKindAdapterCoversLSPVocabulary(t *testing.T) {
	want := []core.SymbolKind{
		core.SymbolKindFile, core.SymbolKindModule, core.SymbolKindNamespace, core.SymbolKindPackage,
		core.SymbolKindClass, core.SymbolKindMethod, core.SymbolKindProperty, core.SymbolKindField,
		core.SymbolKindConstructor, core.SymbolKindEnum, core.SymbolKindInterface, core.SymbolKindFunction,
		core.SymbolKindVariable, core.SymbolKindConstant, core.SymbolKindString, core.SymbolKindNumber,
		core.SymbolKindBoolean, core.SymbolKindArray, core.SymbolKindObject, core.SymbolKindKey,
		core.SymbolKindNull, core.SymbolKindEnumMember, core.SymbolKindStruct, core.SymbolKindEvent,
		core.SymbolKindOperator, core.SymbolKindTypeParameter,
	}
	for i, expected := range want {
		if got := symbolKindName(i + 1); got != expected {
			t.Errorf("symbolKindName(%d) = %q, want %q", i+1, got, expected)
		}
	}
	for _, unknown := range []int{0, 27, 999} {
		if got := symbolKindName(unknown); got != core.SymbolKindUnknown {
			t.Errorf("symbolKindName(%d) = %q, want unknown", unknown, got)
		}
	}
}

func writeTestFrame(t *testing.T, writer io.Writer, body string) {
	t.Helper()
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

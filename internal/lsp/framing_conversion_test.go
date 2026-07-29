package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
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

func writeTestFrame(t *testing.T, writer io.Writer, body string) {
	t.Helper()
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

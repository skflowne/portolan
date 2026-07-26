package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// call sends a JSON-RPC request and consumes only the caller's operation
// context. A dispatched request canceled by that context gets a best-effort
// JSON-RPC cancellation notification.
func (t *transport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	request, err := t.register(key)
	if err != nil {
		return nil, err
	}

	dispatched, writeErr := t.writeMessage(ctx, writeExternal, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if writeErr != nil {
		t.complete(key, pendingResult{err: fmt.Errorf("writing request: %w", writeErr)})
	}

	result, canceled := t.waitPending(ctx, key, request)
	if canceled && dispatched {
		t.sendCancellation(id)
	}
	if result.err != nil {
		return nil, fmt.Errorf("lsp: %s: %w", method, result.err)
	}
	if result.message == nil {
		return nil, fmt.Errorf("lsp: %s: connection closed", method)
	}
	if result.message.Error != nil {
		return nil, fmt.Errorf("lsp: %s: server error %d: %s", method, result.message.Error.Code, result.message.Error.Message)
	}
	return result.message.Result, nil
}

func (t *transport) sendCancellation(id int64) {
	go func() {
		timeout := t.cancellationWriteTimeout
		if timeout <= 0 {
			timeout = defaultCancellationWriteTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, err := t.writeMessage(ctx, writeExternal, rpcNotification{
			JSONRPC: "2.0",
			Method:  "$/cancelRequest",
			Params:  cancelParams{ID: id},
		})
		if t.observeCancellation != nil {
			t.observeCancellation(err)
		}
	}()
}

func (t *transport) notify(ctx context.Context, method string, params any) error {
	if _, err := t.writeMessage(ctx, writeExternal, rpcNotification{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
		return fmt.Errorf("lsp: %s: %w", method, err)
	}
	return nil
}

// readLoop demultiplexes responses and routes server requests through the
// transport's protocol-write policy.
func (t *transport) readLoop() {
	for {
		data, err := readFrame(t.output)
		if err != nil {
			extra := t.stderr.String()
			cause := fmt.Errorf("lsp: connection closed: %w", err)
			if extra != "" {
				cause = fmt.Errorf("%w (stderr: %s)", cause, extra)
			}
			t.readerFailed(cause)
			return
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			t.respondMethodNotFound(msg.ID, msg.Method)
		case msg.Method != "":
		case len(msg.ID) > 0:
			message := msg
			t.deliverResponse(string(msg.ID), &message)
		}
	}
}

func (t *transport) respondMethodNotFound(id json.RawMessage, method string) {
	go func() {
		timeout := t.internalWriteTimeout
		if timeout <= 0 {
			timeout = defaultInternalWriteTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, _ = t.writeMessage(ctx, writeServerResponse, rpcErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)},
		})
	}()
}

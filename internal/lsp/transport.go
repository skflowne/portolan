package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// readFrame reads one LSP message off r: a block of "Key: Value\r\n" headers
// terminated by a blank line, followed by exactly Content-Length bytes of
// JSON body.
func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if strings.EqualFold(key, "Content-Length") {
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", val, err)
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, errors.New("lsp: message missing Content-Length header")
	}
	buf := make([]byte, contentLength)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func newWriteGate() chan struct{} {
	return make(chan struct{}, 1)
}

func (p *Provider) lockWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Provider) unlockWrite() {
	<-p.writeGate
}

func marshalMessage(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("lsp: marshaling message: %w", err)
	}
	return data, nil
}

// writeMessage admits and writes an external request or notification while
// holding the write gate, so Close cannot place shutdown ahead of an admitted
// frame.
func (p *Provider) writeMessage(v any) error {
	data, err := marshalMessage(v)
	if err != nil {
		return err
	}
	if err := p.lockWrite(context.Background()); err != nil {
		return err
	}
	defer p.unlockWrite()
	if err := p.lifecycle.admitExternalWrite(); err != nil {
		return err
	}
	return p.writeFrameLocked(data)
}

func (p *Provider) writeInternalMessage(v any) error {
	data, err := marshalMessage(v)
	if err != nil {
		return err
	}
	if err := p.lockWrite(context.Background()); err != nil {
		return err
	}
	defer p.unlockWrite()
	return p.writeFrameLocked(data)
}

func (p *Provider) writeFrameLocked(data []byte) error {
	if _, err := fmt.Fprintf(p.stdin, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err := p.stdin.Write(data)
	return err
}

// call sends a JSON-RPC request and blocks until a matching response arrives,
// ctx is done, or the per-request timeout elapses. It never hangs the caller
// past that bound.
func (p *Provider) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	request, err := p.lifecycle.register(key)
	if err != nil {
		return nil, err
	}

	if err := p.writeMessage(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		p.lifecycle.complete(key, pendingResult{err: fmt.Errorf("writing request: %w", err)})
	}

	reqCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	result := p.waitPending(reqCtx, key, request)
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

// notify sends a JSON-RPC notification (no response expected).
func (p *Provider) notify(method string, params any) error {
	if err := p.writeMessage(rpcNotification{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
		return fmt.Errorf("lsp: %s: %w", method, err)
	}
	return nil
}

// readLoop is the single background reader: it demuxes incoming frames by
// JSON-RPC id into per-request lifecycle entries. It runs for the
// lifetime of the subprocess and exits (marking the provider closed) on any
// read/decode error, most commonly EOF when the process exits.
func (p *Provider) readLoop() {
	for {
		data, err := readFrame(p.stdoutR)
		if err != nil {
			extra := p.stderrBuf.String()
			cause := fmt.Errorf("lsp: connection closed: %w", err)
			if extra != "" {
				cause = fmt.Errorf("%w (stderr: %s)", cause, extra)
			}
			p.shutdownPending(cause)
			return
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // malformed frame; skip rather than crash the loop
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			// Server-initiated request (e.g. tsgo's
			// client/registerCapability, sent with a *string* id like
			// "ts1"). We don't implement any of these — reply
			// MethodNotFound so tsgo isn't left waiting on a response that
			// never comes, which would otherwise stall its request queue.
			p.respondMethodNotFound(msg.ID, msg.Method)
		case msg.Method != "":
			// Notification from the server (e.g. window/logMessage,
			// textDocument/publishDiagnostics). Nothing to do with it yet.
		case len(msg.ID) > 0:
			m := msg
			p.lifecycle.deliverResponse(string(msg.ID), &m)
		}
	}
}

func (p *Provider) respondMethodNotFound(id json.RawMessage, method string) {
	_ = p.writeMessage(rpcErrorResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)},
	})
}

// shutdownPending marks the provider closed and releases every goroutine
// currently blocked in call() with cerr.
func (p *Provider) shutdownPending(cerr error) {
	p.lifecycle.shutdown(cerr)
}

// stderrBuffer captures the subprocess's recent stderr output so transport
// errors (e.g. tsgo crashing) can be reported with useful context.
type stderrBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const stderrBufferCap = 8192

func newStderrBuffer() *stderrBuffer { return &stderrBuffer{} }

func (b *stderrBuffer) drain(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.buf = append(b.buf, buf[:n]...)
			if len(b.buf) > stderrBufferCap {
				b.buf = b.buf[len(b.buf)-stderrBufferCap:]
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *stderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

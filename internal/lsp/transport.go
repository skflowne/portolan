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
	"time"
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

const (
	internalWriteTimeout     = time.Second
	cancellationWriteTimeout = 100 * time.Millisecond
)

type frameWriteResult struct {
	written int
	err     error
}

func frameBytes(data []byte) []byte {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	frame := make([]byte, 0, len(header)+len(data))
	frame = append(frame, header...)
	return append(frame, data...)
}

func writeFull(w io.Writer, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		n, err := w.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// writeMessage admits and writes one complete external frame. Cancellation
// while waiting for the gate leaves the transport usable; cancellation once a
// frame write starts makes the stream unusable and aborts the provider.
func (p *Provider) writeMessage(ctx context.Context, v any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	data, err := marshalMessage(v)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := p.lockWrite(ctx); err != nil {
		return false, err
	}
	defer p.unlockWrite()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := p.lifecycle.admitExternalWrite(); err != nil {
		return false, err
	}
	return p.writeFrameLocked(ctx, data)
}

func (p *Provider) writeInternalMessage(ctx context.Context, v any) (bool, error) {
	data, err := marshalMessage(v)
	if err != nil {
		return false, err
	}
	if err := p.lockWrite(ctx); err != nil {
		return false, err
	}
	defer p.unlockWrite()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return p.writeFrameLocked(ctx, data)
}

func (p *Provider) writeFrameLocked(ctx context.Context, data []byte) (bool, error) {
	frame := frameBytes(data)
	result := make(chan frameWriteResult, 1)
	go func() {
		written, err := writeFull(p.stdin, frame)
		result <- frameWriteResult{written: written, err: err}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			p.abortTransport(fmt.Errorf("writing LSP frame after %d bytes: %w", got.written, got.err))
			return false, got.err
		}
		return true, nil
	case <-ctx.Done():
		select {
		case got := <-result:
			if got.err == nil {
				return true, nil
			}
			p.abortTransport(fmt.Errorf("writing LSP frame after %d bytes: %w", got.written, got.err))
			return false, got.err
		default:
			p.abortTransport(ctx.Err())
			return false, ctx.Err()
		}
	}
}

func (p *Provider) closeInput() error {
	p.stdinCloseOnce.Do(func() {
		p.stdinCloseErr = p.stdin.Close()
	})
	return p.stdinCloseErr
}

func (p *Provider) abortTransport(cause error) {
	p.abortOnce.Do(func() {
		p.lifecycle.shutdown(cause)
		_ = p.closeInput()
		if p.killProcess != nil {
			_ = p.killProcess()
		}
	})
}

// call sends a JSON-RPC request and consumes only the caller's operation
// context. A dispatched request canceled by that context gets a best-effort
// JSON-RPC cancellation notification.
func (p *Provider) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	request, err := p.lifecycle.register(key)
	if err != nil {
		return nil, err
	}

	dispatched, writeErr := p.writeMessage(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if writeErr != nil {
		p.lifecycle.complete(key, pendingResult{err: fmt.Errorf("writing request: %w", writeErr)})
	}

	result, canceled := p.waitPending(ctx, key, request)
	if canceled && dispatched {
		p.sendCancellation(id)
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

func (p *Provider) sendCancellation(id int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancellationWriteTimeout)
		defer cancel()
		_, _ = p.writeMessage(ctx, rpcNotification{
			JSONRPC: "2.0",
			Method:  "$/cancelRequest",
			Params:  cancelParams{ID: id},
		})
	}()
}

// notify sends a JSON-RPC notification (no response expected).
func (p *Provider) notify(ctx context.Context, method string, params any) error {
	if _, err := p.writeMessage(ctx, rpcNotification{JSONRPC: "2.0", Method: method, Params: params}); err != nil {
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), internalWriteTimeout)
		defer cancel()
		_, _ = p.writeMessage(ctx, rpcErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)},
		})
	}()
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

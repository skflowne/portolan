package lsp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var errProviderClosed = errors.New("lsp: provider closed")

type providerState uint8

const (
	providerOpen providerState = iota
	providerClosing
	providerClosed
)

type pendingResult struct {
	message *jsonrpcMessage
	err     error
}

type pendingRequest struct {
	done   chan struct{}
	once   sync.Once
	result pendingResult
}

func newPendingRequest() *pendingRequest {
	return &pendingRequest{done: make(chan struct{})}
}

func (r *pendingRequest) complete(result pendingResult) bool {
	completed := false
	r.once.Do(func() {
		r.result = result
		completed = true
		close(r.done)
	})
	return completed
}

type transportLifecycle struct {
	mu      sync.Mutex
	state   providerState
	connErr error
	pending map[string]*pendingRequest
}

func newTransportLifecycle() transportLifecycle {
	return transportLifecycle{pending: make(map[string]*pendingRequest)}
}

func (l *transportLifecycle) register(key string) (*pendingRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != providerOpen {
		return nil, l.connectionErrorLocked()
	}
	r := newPendingRequest()
	l.pending[key] = r
	return r, nil
}

func (l *transportLifecycle) admitExternalWrite() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == providerOpen {
		return nil
	}
	return l.connectionErrorLocked()
}

func (l *transportLifecycle) complete(key string, result pendingResult) bool {
	l.mu.Lock()
	r, ok := l.pending[key]
	if ok {
		delete(l.pending, key)
	}
	l.mu.Unlock()
	if !ok {
		return false
	}
	return r.complete(result)
}

func (l *transportLifecycle) deliverResponse(key string, message *jsonrpcMessage) bool {
	return l.complete(key, pendingResult{message: message})
}

func (l *transportLifecycle) beginClose(key string) (*pendingRequest, bool) {
	l.mu.Lock()
	if l.state != providerOpen {
		l.mu.Unlock()
		return nil, false
	}
	l.state = providerClosing
	l.connErr = errProviderClosed

	waiting := make([]*pendingRequest, 0, len(l.pending))
	for id, request := range l.pending {
		waiting = append(waiting, request)
		delete(l.pending, id)
	}
	shutdown := newPendingRequest()
	l.pending[key] = shutdown
	l.mu.Unlock()

	for _, request := range waiting {
		request.complete(pendingResult{err: errProviderClosed})
	}
	return shutdown, true
}

func (l *transportLifecycle) shutdown(cause error) {
	if cause == nil {
		cause = errors.New("lsp: connection closed")
	}

	l.mu.Lock()
	if l.state == providerClosed {
		l.mu.Unlock()
		return
	}
	if l.state == providerOpen || l.connErr == nil {
		l.connErr = cause
	}
	l.state = providerClosed
	waiting := make([]*pendingRequest, 0, len(l.pending))
	for id, request := range l.pending {
		waiting = append(waiting, request)
		delete(l.pending, id)
	}
	l.mu.Unlock()

	for _, request := range waiting {
		request.complete(pendingResult{err: cause})
	}
}

func (l *transportLifecycle) connectionErrorLocked() error {
	if l.connErr != nil {
		return l.connErr
	}
	return errProviderClosed
}

func (p *Provider) waitPending(ctx context.Context, key string, request *pendingRequest) pendingResult {
	select {
	case <-request.done:
	case <-ctx.Done():
		p.lifecycle.complete(key, pendingResult{err: ctx.Err()})
		<-request.done
	}
	return request.result
}

// Close releases pending callers, performs the best-effort LSP shutdown
// handshake, and stops the subprocess. Concurrent calls share one cleanup.
func (p *Provider) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.close()
	})
	return p.closeErr
}

func (p *Provider) close() error {
	id := p.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	shutdownMessage := rpcRequest{JSONRPC: "2.0", ID: id, Method: "shutdown", Params: nil}
	data, marshalErr := marshalMessage(shutdownMessage)

	// Holding writeMu across admission closure and the shutdown frame ensures
	// every admitted external frame precedes shutdown.
	p.writeMu.Lock()
	shutdown, started := p.lifecycle.beginClose(key)
	var writeErr error
	if started {
		if marshalErr != nil {
			writeErr = marshalErr
		} else {
			writeErr = p.writeFrameLocked(data)
		}
	}
	p.writeMu.Unlock()

	if started {
		if writeErr != nil {
			p.lifecycle.complete(key, pendingResult{err: fmt.Errorf("writing shutdown request: %w", writeErr)})
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		p.waitPending(ctx, key, shutdown)
		cancel()
		_ = p.writeInternalMessage(rpcNotification{JSONRPC: "2.0", Method: "exit", Params: nil})
	}

	_ = p.stdin.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(exitWait):
			_ = p.cmd.Process.Kill()
			<-done
		}
	}

	p.lifecycle.shutdown(errProviderClosed)
	return nil
}

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

	waiting := l.drainPendingLocked()
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
	waiting := l.drainPendingLocked()
	l.mu.Unlock()

	for _, request := range waiting {
		request.complete(pendingResult{err: cause})
	}
}

func (l *transportLifecycle) drainPendingLocked() []*pendingRequest {
	waiting := make([]*pendingRequest, 0, len(l.pending))
	for id, request := range l.pending {
		waiting = append(waiting, request)
		delete(l.pending, id)
	}
	return waiting
}

func (l *transportLifecycle) connectionErrorLocked() error {
	if l.connErr != nil {
		return l.connErr
	}
	return errProviderClosed
}

func (p *Provider) waitPending(ctx context.Context, key string, request *pendingRequest) (pendingResult, bool) {
	select {
	case <-request.done:
		return request.result, false
	case <-ctx.Done():
		canceled := p.lifecycle.complete(key, pendingResult{err: ctx.Err()})
		<-request.done
		return request.result, canceled
	}
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
	shutdownTimeout := p.shutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	exitWait := p.exitWait
	if exitWait <= 0 {
		exitWait = defaultExitWait
	}
	killWait := p.killWait
	if killWait <= 0 {
		killWait = defaultKillWait
	}
	totalCtx, totalCancel := context.WithTimeout(context.Background(), shutdownTimeout+exitWait+killWait)
	defer totalCancel()
	ctx, cancel := context.WithTimeout(totalCtx, shutdownTimeout)
	defer cancel()

	id := p.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	shutdownMessage := rpcRequest{JSONRPC: "2.0", ID: id, Method: "shutdown", Params: nil}
	data, marshalErr := marshalMessage(ctx, shutdownMessage)

	var shutdown *pendingRequest
	var started bool
	var writeErr error
	if err := p.lockWrite(ctx); err != nil {
		p.abortTransport(err)
	} else {
		shutdown, started = p.lifecycle.beginClose(key)
		if started {
			if marshalErr != nil {
				writeErr = marshalErr
			} else {
				_, writeErr = p.writeFrameLocked(ctx, data)
			}
		}
		p.unlockWrite()
	}

	if started {
		if writeErr != nil {
			p.lifecycle.complete(key, pendingResult{err: fmt.Errorf("writing shutdown request: %w", writeErr)})
		}
		_, _ = p.waitPending(ctx, key, shutdown)
		_, _ = p.writeInternalMessage(ctx, rpcNotification{JSONRPC: "2.0", Method: "exit", Params: nil})
	}

	_ = p.closeInput()
	p.waitForProcessContext(totalCtx, exitWait, killWait)
	p.lifecycle.shutdown(errProviderClosed)
	return nil
}

func (p *Provider) waitForProcess(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultExitWait
	}
	killWait := p.killWait
	if killWait <= 0 {
		killWait = defaultKillWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+killWait)
	defer cancel()
	p.waitForProcessContext(ctx, timeout, killWait)
}

func (p *Provider) waitForProcessContext(ctx context.Context, gracefulWait, killWait time.Duration) {
	if p.waitProcess == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- p.waitProcess() }()
	timer := time.NewTimer(gracefulWait)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	case <-ctx.Done():
	}
	if p.killProcess != nil {
		_ = p.killProcess()
	}
	killTimer := time.NewTimer(killWait)
	defer killTimer.Stop()
	select {
	case <-done:
	case <-killTimer.C:
	case <-ctx.Done():
	}
}

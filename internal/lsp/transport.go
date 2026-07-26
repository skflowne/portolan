package lsp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errProviderClosed = errors.New("lsp: provider closed")

type transportState uint8

const (
	transportOpen transportState = iota
	transportClosing
	transportClosed
	transportAborted
)

type writePolicy uint8

const (
	writeExternal writePolicy = iota
	writeServerResponse
	writeShutdown
	writeExit
)

const (
	defaultInternalWriteTimeout     = time.Second
	defaultCancellationWriteTimeout = 100 * time.Millisecond
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

type transportConfig struct {
	input       io.WriteCloser
	output      io.Reader
	stderr      *stderrBuffer
	killProcess func() error
	waitProcess func() error
}

// transport owns the JSON-RPC connection from write admission through process
// reaping. Provider owns language operations and delegates all transport state.
type transport struct {
	mu      sync.Mutex
	state   transportState
	connErr error
	pending map[string]*pendingRequest

	input     io.WriteCloser
	output    *bufio.Reader
	stderr    *stderrBuffer
	writeGate chan struct{}
	nextID    atomic.Int64

	closeOnce     sync.Once
	closeErr      error
	inputOnce     sync.Once
	inputCloseErr error
	killOnce      sync.Once
	waitOnce      sync.Once
	processDone   chan struct{}
	killProcess   func() error
	waitProcess   func() error

	internalWriteTimeout     time.Duration
	cancellationWriteTimeout time.Duration
	shutdownTimeout          time.Duration
	exitWait                 time.Duration
	killWait                 time.Duration
	afterFrameDispatch       func()
	observeCancellation      func(error)
}

func newTransport(cfg transportConfig) *transport {
	output := cfg.output
	if output == nil {
		output = strings.NewReader("")
	}
	stderr := cfg.stderr
	if stderr == nil {
		stderr = newStderrBuffer()
	}
	return &transport{
		pending:                  make(map[string]*pendingRequest),
		input:                    cfg.input,
		output:                   bufio.NewReader(output),
		stderr:                   stderr,
		writeGate:                make(chan struct{}, 1),
		processDone:              make(chan struct{}),
		killProcess:              cfg.killProcess,
		waitProcess:              cfg.waitProcess,
		internalWriteTimeout:     defaultInternalWriteTimeout,
		cancellationWriteTimeout: defaultCancellationWriteTimeout,
		shutdownTimeout:          defaultShutdownTimeout,
		exitWait:                 defaultExitWait,
		killWait:                 defaultKillWait,
	}
}

func (t *transport) lockWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case t.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *transport) unlockWrite() {
	<-t.writeGate
}

func (t *transport) register(key string) (*pendingRequest, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != transportOpen {
		return nil, t.connectionErrorLocked()
	}
	request := newPendingRequest()
	t.pending[key] = request
	return request, nil
}

func (t *transport) isOpen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state == transportOpen
}

func (t *transport) admitWrite(policy writePolicy) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	allowed := t.state == transportOpen
	switch policy {
	case writeServerResponse:
		allowed = t.state == transportOpen || t.state == transportClosing
	case writeShutdown, writeExit:
		allowed = t.state == transportClosing
	}
	if allowed {
		return nil
	}
	return t.connectionErrorLocked()
}

func (t *transport) complete(key string, result pendingResult) bool {
	t.mu.Lock()
	request, ok := t.pending[key]
	if ok {
		delete(t.pending, key)
	}
	t.mu.Unlock()
	if !ok {
		return false
	}
	return request.complete(result)
}

func (t *transport) deliverResponse(key string, message *jsonrpcMessage) bool {
	return t.complete(key, pendingResult{message: message})
}

func (t *transport) beginClose(key string) (*pendingRequest, bool) {
	t.mu.Lock()
	if t.state != transportOpen {
		t.mu.Unlock()
		return nil, false
	}
	t.state = transportClosing
	t.connErr = errProviderClosed
	waiting := t.drainPendingLocked()
	shutdown := newPendingRequest()
	t.pending[key] = shutdown
	t.mu.Unlock()

	completePending(waiting, errProviderClosed)
	return shutdown, true
}

func (t *transport) finishClose() {
	t.mu.Lock()
	if t.state != transportClosing {
		t.mu.Unlock()
		return
	}
	t.state = transportClosed
	waiting := t.drainPendingLocked()
	t.mu.Unlock()
	completePending(waiting, errProviderClosed)
}

func (t *transport) readerFailed(cause error) {
	if cause == nil {
		cause = errors.New("lsp: connection closed")
	}

	t.mu.Lock()
	if t.state == transportClosing {
		waiting := t.drainPendingLocked()
		t.mu.Unlock()
		completePending(waiting, cause)
		return
	}
	t.mu.Unlock()
	t.abort(cause)
}

func (t *transport) drainPendingLocked() []*pendingRequest {
	waiting := make([]*pendingRequest, 0, len(t.pending))
	for id, request := range t.pending {
		waiting = append(waiting, request)
		delete(t.pending, id)
	}
	return waiting
}

func completePending(waiting []*pendingRequest, cause error) {
	for _, request := range waiting {
		request.complete(pendingResult{err: cause})
	}
}

func (t *transport) connectionErrorLocked() error {
	if t.connErr != nil {
		return t.connErr
	}
	return errProviderClosed
}

func (t *transport) waitPending(ctx context.Context, key string, request *pendingRequest) (pendingResult, bool) {
	select {
	case <-request.done:
		return request.result, false
	case <-ctx.Done():
		canceled := t.complete(key, pendingResult{err: ctx.Err()})
		<-request.done
		return request.result, canceled
	}
}

func (t *transport) writeMessage(ctx context.Context, policy writePolicy, message any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	data, err := marshalMessage(ctx, message)
	if err != nil {
		return false, err
	}
	if err := t.lockWrite(ctx); err != nil {
		return false, err
	}
	defer t.unlockWrite()
	return t.writeAdmittedFrameLocked(ctx, policy, data)
}

func (t *transport) writeAdmittedFrameLocked(ctx context.Context, policy writePolicy, data []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := t.admitWrite(policy); err != nil {
		return false, err
	}
	return t.writeFrameLocked(ctx, data)
}

func (t *transport) closeInput() error {
	t.inputOnce.Do(func() {
		if t.input != nil {
			t.inputCloseErr = t.input.Close()
		}
	})
	return t.inputCloseErr
}

func (t *transport) kill() {
	t.killOnce.Do(func() {
		if t.killProcess != nil {
			_ = t.killProcess()
		}
	})
}

func (t *transport) abort(cause error) {
	if cause == nil {
		cause = errors.New("lsp: connection closed")
	}

	t.mu.Lock()
	if t.state == transportClosed || t.state == transportAborted {
		t.mu.Unlock()
		return
	}
	t.state = transportAborted
	t.connErr = cause
	waiting := t.drainPendingLocked()
	t.mu.Unlock()

	completePending(waiting, cause)
	_ = t.closeInput()
	t.kill()
	t.startProcessWait()
}

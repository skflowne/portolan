package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func newUnitProvider(stdin io.WriteCloser, stdout io.Reader) *Provider {
	if stdout == nil {
		stdout = strings.NewReader("")
	}
	return &Provider{
		transport: newTransport(transportConfig{input: stdin, output: stdout}),
	}
}

func waitPendingResult(t *testing.T, request *pendingRequest) pendingResult {
	t.Helper()
	select {
	case <-request.done:
		return request.result
	case <-time.After(time.Second):
		t.Fatal("pending request did not complete")
		return pendingResult{}
	}
}

func waitForPendingCount(t *testing.T, p *Provider, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.transport.mu.Lock()
		got := len(p.transport.pending)
		p.transport.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending request count did not reach %d", want)
}

func waitError(t *testing.T, results <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(time.Second):
		t.Fatalf("%s did not complete", operation)
		return nil
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

type cancelOnErrCheckContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func newCancelOnErrCheckContext(cancelAt int) *cancelOnErrCheckContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnErrCheckContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func (c *cancelOnErrCheckContext) Err() error {
	c.checks++
	if c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

type recordingWriteCloser struct {
	mu              sync.Mutex
	bodies          [][]byte
	bodyEvents      chan string
	shutdownStarted chan struct{}
	releaseShutdown chan struct{}
	shutdownOnce    sync.Once
	closeCount      int
}

func newRecordingWriteCloser() *recordingWriteCloser {
	return &recordingWriteCloser{
		bodyEvents:      make(chan string, 16),
		shutdownStarted: make(chan struct{}),
		releaseShutdown: make(chan struct{}),
	}
}

func (w *recordingWriteCloser) Write(data []byte) (int, error) {
	bodyData := data
	if split := bytes.Index(data, []byte("\r\n\r\n")); split >= 0 {
		bodyData = data[split+4:]
	}
	if len(bodyData) == 0 || bodyData[0] != '{' {
		return len(data), nil
	}
	body := append([]byte(nil), bodyData...)
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.bodies = append(w.bodies, body)
	w.mu.Unlock()
	w.bodyEvents <- envelope.Method
	if envelope.Method == "shutdown" {
		w.shutdownOnce.Do(func() { close(w.shutdownStarted) })
		<-w.releaseShutdown
	}
	return len(data), nil
}

func (w *recordingWriteCloser) Close() error {
	w.mu.Lock()
	w.closeCount++
	w.mu.Unlock()
	return nil
}

func (w *recordingWriteCloser) waitForMethod(t *testing.T, method string) {
	t.Helper()
	select {
	case got := <-w.bodyEvents:
		if got != method {
			t.Fatalf("written method = %q, want %q", got, method)
		}
	case <-time.After(time.Second):
		t.Fatalf("method %q was not written", method)
	}
}

func (w *recordingWriteCloser) waitForShutdown(t *testing.T) {
	t.Helper()
	select {
	case <-w.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown write did not start")
	}
}

func (w *recordingWriteCloser) messageBodies() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	bodies := make([][]byte, len(w.bodies))
	for i, body := range w.bodies {
		bodies[i] = append([]byte(nil), body...)
	}
	return bodies
}

func (w *recordingWriteCloser) methods() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	methods := make([]string, 0, len(w.bodies))
	for _, body := range w.bodies {
		var envelope struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		methods = append(methods, envelope.Method)
	}
	return methods
}

func (w *recordingWriteCloser) closeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeCount
}

type closeBlockingWriteCloser struct {
	started   chan struct{}
	release   chan struct{}
	returned  chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func newCloseBlockingWriteCloser() *closeBlockingWriteCloser {
	return &closeBlockingWriteCloser{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
}

func (w *closeBlockingWriteCloser) Write(data []byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	close(w.returned)
	return 0, io.ErrClosedPipe
}

func (w *closeBlockingWriteCloser) Close() error {
	w.mu.Lock()
	w.closes++
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.release) })
	return nil
}

func (w *closeBlockingWriteCloser) closeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

var _ io.WriteCloser = (*recordingWriteCloser)(nil)

var _ io.WriteCloser = (*closeBlockingWriteCloser)(nil)

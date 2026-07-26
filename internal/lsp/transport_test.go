package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		stdin:     stdin,
		stdoutR:   bufio.NewReader(stdout),
		lifecycle: newTransportLifecycle(),
		stderrBuf: newStderrBuffer(),
		timeout:   time.Second,
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

func TestPendingResponseTargetSurvivesConcurrentShutdown(t *testing.T) {
	p := newUnitProvider(&recordingWriteCloser{}, nil)
	request, err := p.lifecycle.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	cause := errors.New("connection failed")
	response := &jsonrpcMessage{Result: json.RawMessage(`{"ok":true}`)}
	start := make(chan struct{})
	var delivered bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		delivered = p.lifecycle.deliverResponse("1", response)
	}()
	go func() {
		defer wg.Done()
		<-start
		p.shutdownPending(cause)
	}()
	close(start)
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("response delivery and shutdown did not finish")
	}

	result := waitPendingResult(t, request)
	if delivered {
		if result.message != response || result.err != nil {
			t.Fatalf("response won with result %+v", result)
		}
	} else if !errors.Is(result.err, cause) || result.message != nil {
		t.Fatalf("shutdown won with result %+v", result)
	}
}

func TestPendingResponseDeliveredAtMostOnce(t *testing.T) {
	p := newUnitProvider(&recordingWriteCloser{}, nil)
	request, err := p.lifecycle.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	first := &jsonrpcMessage{Result: json.RawMessage(`1`)}
	second := &jsonrpcMessage{Result: json.RawMessage(`2`)}

	if !p.lifecycle.deliverResponse("1", first) {
		t.Fatal("first response did not complete request")
	}
	if p.lifecycle.deliverResponse("1", second) {
		t.Fatal("duplicate response completed request")
	}
	if result := waitPendingResult(t, request); result.message != first || result.err != nil {
		t.Fatalf("got terminal result %+v, want first response", result)
	}
}

func TestReadLoopDeliversFramedResponse(t *testing.T) {
	reader, writer := io.Pipe()
	p := newUnitProvider(&recordingWriteCloser{}, reader)
	request, err := p.lifecycle.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	loopDone := make(chan struct{})
	go func() {
		p.readLoop()
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

func TestReadLoopFailureReleasesPendingWithCause(t *testing.T) {
	reader, writer := io.Pipe()
	p := newUnitProvider(&recordingWriteCloser{}, reader)
	p.stderrBuf.buf = []byte("server exploded")
	request, err := p.lifecycle.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	loopDone := make(chan struct{})
	go func() {
		p.readLoop()
		close(loopDone)
	}()
	cause := errors.New("process pipe failed")
	if err := writer.CloseWithError(cause); err != nil {
		t.Fatalf("close response writer: %v", err)
	}

	result := waitPendingResult(t, request)
	if !errors.Is(result.err, cause) {
		t.Fatalf("pending error = %v, want cause %v", result.err, cause)
	}
	if !strings.Contains(result.err.Error(), "server exploded") {
		t.Fatalf("pending error %q does not preserve stderr", result.err)
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not stop after failure")
	}
}

func TestCallTimeoutRemovesPendingAndIgnoresLateResponse(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.timeout = 0

	if _, err := p.call(context.Background(), "first", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want deadline exceeded", err)
	}
	writer.waitForMethod(t, "first")
	if p.lifecycle.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`"late"`)}) {
		t.Fatal("late response completed timed-out request")
	}

	p.timeout = time.Second
	resultCh := make(chan pendingResult, 1)
	go func() {
		result, err := p.call(context.Background(), "second", nil)
		resultCh <- pendingResult{message: &jsonrpcMessage{Result: result}, err: err}
	}()
	writer.waitForMethod(t, "second")
	if !p.lifecycle.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`"fresh"`)}) {
		t.Fatal("second response did not complete request")
	}
	result := waitCallResult(t, resultCh)
	if result.err != nil || string(result.message.Result) != `"fresh"` {
		t.Fatalf("second call result = %+v", result)
	}
}

func TestCallWriteFailureRemovesPending(t *testing.T) {
	cause := errors.New("write failed")
	p := newUnitProvider(&failingWriteCloser{err: cause}, nil)

	if _, err := p.call(context.Background(), "broken", nil); !errors.Is(err, cause) {
		t.Fatalf("call error = %v, want cause %v", err, cause)
	}
	p.lifecycle.mu.Lock()
	pending := len(p.lifecycle.pending)
	p.lifecycle.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}
}

func TestCloseReleasesPendingAndSerializesShutdown(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)

	callErr := make(chan error, 1)
	go func() {
		_, err := p.call(context.Background(), "ordinary", nil)
		callErr <- err
	}()
	writer.waitForMethod(t, "ordinary")

	const closers = 5
	closeErrs := make(chan error, closers)
	for range closers {
		go func() { closeErrs <- p.Close() }()
	}
	writer.waitForShutdown(t)

	select {
	case err := <-callErr:
		if !errors.Is(err, errProviderClosed) {
			t.Fatalf("ordinary call error = %v, want provider closed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not promptly release ordinary call")
	}

	notifyErr := make(chan error, 1)
	go func() { notifyErr <- p.notify("queued", nil) }()
	close(writer.releaseShutdown)
	if !p.lifecycle.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("shutdown response did not complete internal request")
	}

	if err := waitError(t, notifyErr, "queued notification"); !errors.Is(err, errProviderClosed) {
		t.Fatalf("queued notification error = %v, want provider closed", err)
	}
	for range closers {
		if err := waitError(t, closeErrs, "Close"); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	methods := writer.methods()
	want := []string{"ordinary", "shutdown", "exit"}
	if fmt.Sprint(methods) != fmt.Sprint(want) {
		t.Fatalf("written methods = %v, want %v", methods, want)
	}
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin Close calls = %d, want 1", writer.closeCalls())
	}
}

func TestCloseAfterReaderFailureSkipsHandshake(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	cause := errors.New("reader failed")
	p.shutdownPending(cause)

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods after reader failure = %v, want none", methods)
	}
	if err := p.lifecycle.admitExternalWrite(); !errors.Is(err, cause) {
		t.Fatalf("preserved connection error = %v, want %v", err, cause)
	}
}

func waitCallResult(t *testing.T, results <-chan pendingResult) pendingResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("call did not complete")
		return pendingResult{}
	}
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

func writeTestFrame(t *testing.T, writer io.Writer, body string) {
	t.Helper()
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatalf("write frame: %v", err)
	}
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
	if len(data) == 0 || data[0] != '{' {
		return len(data), nil
	}
	body := append([]byte(nil), data...)
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

type failingWriteCloser struct {
	err error
}

func (w *failingWriteCloser) Write([]byte) (int, error) { return 0, w.err }
func (w *failingWriteCloser) Close() error              { return nil }

var _ io.WriteCloser = (*recordingWriteCloser)(nil)
var _ io.WriteCloser = (*failingWriteCloser)(nil)

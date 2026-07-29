package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFramePreparationCancellationDoesNotWrite(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	ctx := newCancelOnErrCheckContext(2)

	dispatched, err := p.transport.writeFrameLocked(ctx, []byte(`{"jsonrpc":"2.0","method":"prepared"}`))
	if !errors.Is(err, context.Canceled) || dispatched {
		t.Fatalf("writeFrameLocked = (%v, %v), want not dispatched and context canceled", dispatched, err)
	}
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
}

func TestCanceledSerializationDoesNotReachTransport(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	marshaler := &blockingJSONMarshaler{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.writeMessage(ctx, writeExternal, marshaler)
		result <- err
	}()

	waitSignal(t, marshaler.entered, "JSON serialization")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("writeMessage error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		close(marshaler.release)
		waitSignal(t, marshaler.exited, "JSON serialization cleanup")
		<-result
		t.Fatal("serialization held caller past cancellation")
	}
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}

	close(marshaler.release)
	waitSignal(t, marshaler.exited, "JSON serialization cleanup")
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("late serialization wrote methods %v", methods)
	}
}

func TestCallBlockedWriteHonorsContext(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	var kills atomic.Int32
	p.transport.killProcess = func() error {
		kills.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "blocked", nil)
		result <- err
	}()

	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call error = %v, want context canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		_ = writer.Close()
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("blocked call did not return after writer release")
		}
		t.Fatal("blocked write held caller past cancellation")
	}
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	if kills.Load() != 1 {
		t.Fatalf("process kill calls = %d, want 1", kills.Load())
	}
	waitSignal(t, writer.returned, "blocked writer exit")
	p.transport.mu.Lock()
	pending := len(p.transport.pending)
	p.transport.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}
}

func TestCallReturnsWhenWriterIgnoresTransportAbort(t *testing.T) {
	writer := newUninterruptibleWriteCloser()
	defer writer.releaseWrite()
	p := newUnitProvider(writer, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "uninterruptible", nil)
		result <- err
	}()
	waitSignal(t, writer.started, "uninterruptible write")
	cancel()
	if err := waitError(t, result, "uninterruptible call"); !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v, want context canceled", err)
	}
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	writer.releaseWrite()
	waitSignal(t, writer.returned, "uninterruptible writer cleanup")
}

func TestCanceledDispatchedCallSendsCancellation(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "work", nil)
		result <- err
	}()
	writer.waitForMethod(t, "work")
	cancel()

	if err := waitError(t, result, "canceled call"); !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v, want context canceled", err)
	}
	writer.waitForMethod(t, "$/cancelRequest")

	bodies := writer.messageBodies()
	if len(bodies) != 2 {
		t.Fatalf("written message count = %d, want 2", len(bodies))
	}
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(bodies[0], &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	var cancellation struct {
		JSONRPC string `json:"jsonrpc"`
		ID      *int64 `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			ID int64 `json:"id"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bodies[1], &cancellation); err != nil {
		t.Fatalf("decode cancellation: %v", err)
	}
	if cancellation.JSONRPC != "2.0" || cancellation.ID != nil || cancellation.Method != "$/cancelRequest" || cancellation.Params.ID != request.ID {
		t.Fatalf("cancellation = %+v, request id = %d", cancellation, request.ID)
	}
	if p.transport.deliverResponse(strconv.FormatInt(request.ID, 10), &jsonrpcMessage{Result: json.RawMessage(`"late"`)}) {
		t.Fatal("late response completed canceled request")
	}
}

func TestResponseWinningCancellationRaceDoesNotSendCancel(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "response-wins", nil)
		result <- err
	}()
	writer.waitForMethod(t, "response-wins")
	if !p.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("response was not delivered")
	}
	if err := waitError(t, result, "response-winning call"); err != nil {
		t.Fatalf("call: %v", err)
	}
	cancellation := make(chan error, 1)
	p.transport.observeCancellation = func(err error) { cancellation <- err }
	p.transport.cancellationWriteTimeout = 10 * time.Millisecond
	cancel()
	select {
	case err := <-cancellation:
		t.Fatalf("unexpected cancellation notification: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if methods := writer.methods(); len(methods) != 1 {
		t.Fatalf("written methods = %v, want request only", methods)
	}
}

func TestCanceledWriteGateAcquisitionDoesNotDispatch(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	if err := p.transport.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.transport.call(ctx, "blocked-at-gate", nil); !errors.Is(err, context.Canceled) {
		p.transport.unlockWrite()
		t.Fatalf("call error = %v, want context canceled", err)
	}
	p.transport.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	p.transport.mu.Lock()
	pending := len(p.transport.pending)
	p.transport.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}

	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(context.Background(), "usable", nil)
		result <- err
	}()
	writer.waitForMethod(t, "usable")
	if !p.transport.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("usable response was not delivered")
	}
	if err := waitError(t, result, "usable call"); err != nil {
		t.Fatalf("usable call: %v", err)
	}
}

func TestBlockedWriteGateAcquisitionHonorsCancellation(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	if err := p.transport.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "waiting-at-gate", nil)
		result <- err
	}()
	waitForPendingCount(t, p, 1)
	cancel()
	if err := waitError(t, result, "gate-blocked call"); !errors.Is(err, context.Canceled) {
		p.transport.unlockWrite()
		t.Fatalf("call error = %v, want context canceled", err)
	}
	p.transport.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	waitForPendingCount(t, p, 0)
}

func TestCancellationNotificationGateTimeoutIsDropped(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.transport.cancellationWriteTimeout = 10 * time.Millisecond
	observed := make(chan error, 1)
	p.transport.observeCancellation = func(err error) { observed <- err }
	if err := p.transport.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	p.transport.sendCancellation(7)
	if err := waitError(t, observed, "cancellation gate timeout"); !errors.Is(err, context.DeadlineExceeded) {
		p.transport.unlockWrite()
		t.Fatalf("cancellation error = %v, want deadline exceeded", err)
	}
	p.transport.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	if err := p.transport.admitWrite(writeExternal); err != nil {
		t.Fatalf("provider became unusable after cancellation gate timeout: %v", err)
	}
}

func TestCancellationNotificationBlockedWriteAbortsTransport(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.transport.cancellationWriteTimeout = 10 * time.Millisecond
	observed := make(chan error, 1)
	p.transport.observeCancellation = func(err error) { observed <- err }
	p.transport.sendCancellation(7)
	waitSignal(t, writer.started, "cancellation write")
	if err := waitError(t, observed, "blocked cancellation write"); err == nil {
		t.Fatal("blocked cancellation write returned no error")
	}
	waitSignal(t, writer.returned, "cancellation writer exit")
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	if err := p.transport.admitWrite(writeExternal); err == nil {
		t.Fatal("provider remained open after partial cancellation frame")
	}
}

func TestMethodNotFoundWriteIsAsynchronousAndBounded(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.transport.internalWriteTimeout = 10 * time.Millisecond
	returned := make(chan struct{})
	go func() {
		p.transport.respondMethodNotFound(json.RawMessage(`"server-1"`), "unsupported")
		close(returned)
	}()
	waitSignal(t, returned, "respondMethodNotFound return")
	waitSignal(t, writer.started, "MethodNotFound write")
	waitSignal(t, writer.returned, "MethodNotFound writer exit")
	if err := p.transport.admitWrite(writeExternal); err == nil {
		t.Fatal("provider remained open after blocked MethodNotFound frame")
	}
}

func TestServerResponseRemainsWritableWhileClosing(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	if _, started := p.transport.beginClose("1"); !started {
		t.Fatal("close transition did not start")
	}
	if err := p.transport.admitWrite(writeExternal); !errors.Is(err, errProviderClosed) {
		t.Fatalf("external write admission = %v, want provider closed", err)
	}

	p.transport.respondMethodNotFound(json.RawMessage(`"server-1"`), "unsupported")
	deadline := time.Now().Add(time.Second)
	for len(writer.messageBodies()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	bodies := writer.messageBodies()
	if len(bodies) != 1 {
		t.Fatalf("server response frame count = %d, want 1", len(bodies))
	}
}

func TestInternalWriteRejectedAfterAbort(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.transport.abort(errors.New("transport failed"))

	writes := []struct {
		name    string
		policy  writePolicy
		message any
	}{
		{name: "shutdown", policy: writeShutdown, message: rpcRequest{JSONRPC: "2.0", ID: 1, Method: "shutdown"}},
		{name: "exit", policy: writeExit, message: rpcNotification{JSONRPC: "2.0", Method: "exit"}},
	}
	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) {
			dispatched, err := p.transport.writeMessage(context.Background(), write.policy, write.message)
			if err == nil || dispatched {
				t.Fatalf("internal write after abort = (%v, %v), want rejected", dispatched, err)
			}
		})
	}
	if bodies := writer.messageBodies(); len(bodies) != 0 {
		t.Fatalf("written frame count = %d, want 0", len(bodies))
	}
}

func TestPartialFrameErrorTerminatesTransport(t *testing.T) {
	cause := errors.New("partial write failed")
	writer := &partialErrorWriteCloser{limit: 8, err: cause}
	p := newUnitProvider(writer, nil)

	if _, err := p.transport.call(context.Background(), "partial", nil); !errors.Is(err, cause) {
		t.Fatalf("call error = %v, want %v", err, cause)
	}
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	if _, err := p.transport.call(context.Background(), "later", nil); err == nil {
		t.Fatal("later call succeeded on corrupted transport")
	}
	if calls := writer.writeCalls(); calls != 1 {
		t.Fatalf("write calls = %d, want 1", calls)
	}
}

func TestShortWritesCompleteOneFrame(t *testing.T) {
	writer := &shortWriteCloser{limit: 3, complete: make(chan struct{})}
	p := newUnitProvider(writer, nil)
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(context.Background(), "short", nil)
		result <- err
	}()
	writer.waitForFrame(t)
	if !p.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("response was not delivered")
	}
	if err := waitError(t, result, "short-write call"); err != nil {
		t.Fatalf("call: %v", err)
	}
	frames := splitTestFrames(t, writer.bytes())
	if len(frames) != 1 {
		t.Fatalf("frame count = %d, want 1", len(frames))
	}
}

func TestCallWriteFailureRemovesPending(t *testing.T) {
	cause := errors.New("write failed")
	p := newUnitProvider(&failingWriteCloser{err: cause}, nil)

	if _, err := p.transport.call(context.Background(), "broken", nil); !errors.Is(err, cause) {
		t.Fatalf("call error = %v, want cause %v", err, cause)
	}
	p.transport.mu.Lock()
	pending := len(p.transport.pending)
	p.transport.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}
}

type blockingJSONMarshaler struct {
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (m *blockingJSONMarshaler) MarshalJSON() ([]byte, error) {
	close(m.entered)
	<-m.release
	close(m.exited)
	return []byte(`{"jsonrpc":"2.0","method":"blocked"}`), nil
}

type uninterruptibleWriteCloser struct {
	started     chan struct{}
	release     chan struct{}
	returned    chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	closes      int
}

func newUninterruptibleWriteCloser() *uninterruptibleWriteCloser {
	return &uninterruptibleWriteCloser{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
}

func (w *uninterruptibleWriteCloser) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	close(w.returned)
	return 0, io.ErrClosedPipe
}

func (w *uninterruptibleWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closes++
	return nil
}

func (w *uninterruptibleWriteCloser) releaseWrite() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *uninterruptibleWriteCloser) closeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

type partialErrorWriteCloser struct {
	mu     sync.Mutex
	limit  int
	err    error
	writes int
	closes int
}

func (w *partialErrorWriteCloser) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	n := min(w.limit, len(data))
	return n, w.err
}

func (w *partialErrorWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closes++
	return nil
}

func (w *partialErrorWriteCloser) closeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

func (w *partialErrorWriteCloser) writeCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

type shortWriteCloser struct {
	mu       sync.Mutex
	limit    int
	data     []byte
	complete chan struct{}
	once     sync.Once
}

func (w *shortWriteCloser) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := min(w.limit, len(data))
	w.data = append(w.data, data[:n]...)
	if bytes.Contains(w.data, []byte(`"method":"short"`)) {
		w.once.Do(func() { close(w.complete) })
	}
	return n, nil
}

func (w *shortWriteCloser) Close() error { return nil }

func (w *shortWriteCloser) waitForFrame(t *testing.T) {
	t.Helper()
	select {
	case <-w.complete:
	case <-time.After(time.Second):
		t.Fatal("short writer did not complete frame")
	}
}

func (w *shortWriteCloser) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data...)
}

func splitTestFrames(t *testing.T, data []byte) [][]byte {
	t.Helper()
	source := bytes.NewReader(data)
	reader := bufio.NewReader(source)
	var frames [][]byte
	for source.Len() > 0 || reader.Buffered() > 0 {
		frame, err := readFrame(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

type failingWriteCloser struct {
	err error
}

func (w *failingWriteCloser) Write([]byte) (int, error) { return 0, w.err }

func (w *failingWriteCloser) Close() error { return nil }

var _ io.WriteCloser = (*uninterruptibleWriteCloser)(nil)

var _ io.WriteCloser = (*partialErrorWriteCloser)(nil)

var _ io.WriteCloser = (*shortWriteCloser)(nil)

var _ io.WriteCloser = (*failingWriteCloser)(nil)

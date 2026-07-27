package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func transportStateAndCause(t *transport) (transportState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.connErr
}

func TestTransportPublishesFirstUnavailableCause(t *testing.T) {
	t.Run("graceful close", func(t *testing.T) {
		connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
		if _, started := connection.beginClose("1"); !started {
			t.Fatal("close transition did not start")
		}
		waitSignal(t, connection.unavailableDone(), "transport unavailability")
		if err := connection.unavailableError(); !errors.Is(err, errProviderClosed) {
			t.Fatalf("unavailable error = %v, want provider closed", err)
		}
	})

	t.Run("abort", func(t *testing.T) {
		connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
		cause := errors.New("reader failed")
		connection.abort(cause)
		waitSignal(t, connection.unavailableDone(), "transport unavailability")
		if err := connection.unavailableError(); !errors.Is(err, cause) {
			t.Fatalf("unavailable error = %v, want %v", err, cause)
		}
	})

	t.Run("abort after close", func(t *testing.T) {
		connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
		if _, started := connection.beginClose("1"); !started {
			t.Fatal("close transition did not start")
		}
		connection.abort(errors.New("shutdown write failed"))
		waitSignal(t, connection.unavailableDone(), "transport unavailability")
		if err := connection.unavailableError(); !errors.Is(err, errProviderClosed) {
			t.Fatalf("unavailable error after abort = %v, want first cause provider closed", err)
		}
	})
}

func TestTransportShutdownUsesWriteAdmission(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lifecycle.go: %v", err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "writeFrameLocked" {
			t.Error("shutdown bypasses lifecycle-aware write admission")
		}
		return true
	})
}

func TestTransportCloseTransitionOwnsPendingAndWriteAdmission(t *testing.T) {
	connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
	ordinary, err := connection.register("1")
	if err != nil {
		t.Fatalf("register ordinary request: %v", err)
	}
	if err := connection.admitWrite(writeShutdown); !errors.Is(err, errProviderClosed) {
		t.Fatalf("shutdown admission while open = %v, want provider closed", err)
	}
	shutdown, started := connection.beginClose("2")
	if !started {
		t.Fatal("close transition did not start")
	}
	if result := waitPendingResult(t, ordinary); !errors.Is(result.err, errProviderClosed) {
		t.Fatalf("ordinary request error = %v, want provider closed", result.err)
	}
	if err := connection.admitWrite(writeExternal); !errors.Is(err, errProviderClosed) {
		t.Fatalf("external admission while closing = %v, want provider closed", err)
	}
	if err := connection.admitWrite(writeServerResponse); err != nil {
		t.Fatalf("server response admission while closing: %v", err)
	}
	if err := connection.admitWrite(writeShutdown); err != nil {
		t.Fatalf("shutdown admission while closing: %v", err)
	}
	if err := connection.admitWrite(writeExit); err != nil {
		t.Fatalf("exit admission while closing: %v", err)
	}
	if !connection.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("shutdown response was not delivered")
	}
	if result := waitPendingResult(t, shutdown); result.err != nil || result.message == nil {
		t.Fatalf("shutdown result = %+v, want response", result)
	}

	connection.finishClose()
	state, cause := transportStateAndCause(connection)
	if state != transportClosed || !errors.Is(cause, errProviderClosed) {
		t.Fatalf("terminal transport = (%v, %v), want closed/provider closed", state, cause)
	}
	for _, policy := range []writePolicy{writeExternal, writeServerResponse, writeShutdown, writeExit} {
		if err := connection.admitWrite(policy); !errors.Is(err, errProviderClosed) {
			t.Fatalf("write policy %v after close = %v, want provider closed", policy, err)
		}
	}
}

type closeWindowWriter struct {
	*recordingWriteCloser
	closed          atomic.Bool
	closeStarted    chan struct{}
	releaseClose    chan struct{}
	writeAfterClose chan struct{}
	writeSignalOnce sync.Once
	writes          atomic.Int32
}

func newCloseWindowWriter() *closeWindowWriter {
	return &closeWindowWriter{
		recordingWriteCloser: newRecordingWriteCloser(),
		closeStarted:         make(chan struct{}),
		releaseClose:         make(chan struct{}),
		writeAfterClose:      make(chan struct{}),
	}
}

func (w *closeWindowWriter) Write(data []byte) (int, error) {
	w.writes.Add(1)
	if w.closed.Load() {
		w.writeSignalOnce.Do(func() { close(w.writeAfterClose) })
		return 0, io.ErrClosedPipe
	}
	return w.recordingWriteCloser.Write(data)
}

func (w *closeWindowWriter) Close() error {
	w.closed.Store(true)
	close(w.closeStarted)
	<-w.releaseClose
	return w.recordingWriteCloser.Close()
}

type transportWriteResult struct {
	dispatched bool
	err        error
}

type signalingJSONMarshaler struct {
	marshaled chan struct{}
}

func (m signalingJSONMarshaler) MarshalJSON() ([]byte, error) {
	close(m.marshaled)
	return []byte(`{"jsonrpc":"2.0","id":"server-1","error":{"code":-32601,"message":"method not found"}}`), nil
}

func TestTransportInputClosureEndsWriteAdmission(t *testing.T) {
	writer := newCloseWindowWriter()
	connection := newUnitProvider(writer, nil).transport
	if _, started := connection.beginClose("1"); !started {
		t.Fatal("close transition did not start")
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- connection.closeInput() }()
	waitSignal(t, writer.closeStarted, "input close start")

	responseResult := make(chan transportWriteResult, 1)
	go func() {
		dispatched, err := connection.writeMessage(context.Background(), writeServerResponse, rpcErrorResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`"server-1"`),
			Error:   rpcError{Code: -32601, Message: "method not found"},
		})
		responseResult <- transportWriteResult{dispatched: dispatched, err: err}
	}()

	var response transportWriteResult
	select {
	case response = <-responseResult:
	case <-writer.writeAfterClose:
		close(writer.releaseClose)
		<-closeResult
		response = <-responseResult
		t.Fatalf("server response reached closed input: (%v, %v)", response.dispatched, response.err)
	case <-time.After(time.Second):
		close(writer.releaseClose)
		<-closeResult
		t.Fatal("server response admission did not complete")
	}
	if response.dispatched || !errors.Is(response.err, errProviderClosed) {
		t.Fatalf("server response after input close = (%v, %v), want rejected/provider closed", response.dispatched, response.err)
	}
	close(writer.releaseClose)
	if err := <-closeResult; err != nil {
		t.Fatalf("close input: %v", err)
	}
	if got := writer.writes.Load(); got != 0 {
		t.Fatalf("frame write attempts = %d, want 0", got)
	}
	state, cause := transportStateAndCause(connection)
	if state != transportClosing || !errors.Is(cause, errProviderClosed) {
		t.Fatalf("transport after input close = (%v, %v), want closing/provider closed", state, cause)
	}
	connection.finishClose()
}

func TestTransportRejectsServerResponseAfterInputCloseBegins(t *testing.T) {
	writer := newCloseWindowWriter()
	connection := newUnitProvider(writer, nil).transport
	var kills atomic.Int32
	var waits atomic.Int32
	connection.killProcess = func() error {
		kills.Add(1)
		return nil
	}
	connection.waitProcess = func() error {
		waits.Add(1)
		return nil
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- connection.Close() }()
	writer.waitForShutdown(t)
	if !connection.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("shutdown response was not delivered")
	}
	close(writer.releaseShutdown)
	waitSignal(t, writer.closeStarted, "input close start")

	state, cause := transportStateAndCause(connection)
	if state != transportClosing || !errors.Is(cause, errProviderClosed) {
		t.Fatalf("transport during input close = (%v, %v), want closing/provider closed", state, cause)
	}

	message := signalingJSONMarshaler{marshaled: make(chan struct{})}
	responseResult := make(chan transportWriteResult, 1)
	go func() {
		dispatched, err := connection.writeMessage(context.Background(), writeServerResponse, message)
		responseResult <- transportWriteResult{dispatched: dispatched, err: err}
	}()
	waitSignal(t, message.marshaled, "server response marshal")
	close(writer.releaseClose)

	response := <-responseResult
	if response.dispatched || !errors.Is(response.err, errProviderClosed) {
		t.Fatalf("server response after input close = (%v, %v), want rejected/provider closed", response.dispatched, response.err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := writer.writes.Load(); got != 2 {
		t.Fatalf("frame write attempts = %d, want shutdown and exit only", got)
	}
	state, cause = transportStateAndCause(connection)
	if state != transportClosed || !errors.Is(cause, errProviderClosed) {
		t.Fatalf("terminal transport = (%v, %v), want closed/provider closed", state, cause)
	}
	if kills.Load() != 0 || waits.Load() != 1 {
		t.Fatalf("process cleanup calls: kill=%d wait=%d, want 0/1", kills.Load(), waits.Load())
	}
}

func TestTransportAbortArbitratesPendingAndRepeatedCleanup(t *testing.T) {
	writer := newRecordingWriteCloser()
	provider := newUnitProvider(writer, nil)
	connection := provider.transport
	var kills atomic.Int32
	var waits atomic.Int32
	waitStarted := make(chan struct{}, 1)
	connection.killProcess = func() error {
		kills.Add(1)
		return nil
	}
	connection.waitProcess = func() error {
		waits.Add(1)
		select {
		case waitStarted <- struct{}{}:
		default:
		}
		return nil
	}
	request, err := connection.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	cause := errors.New("wire failed")
	connection.abort(cause)
	if result := waitPendingResult(t, request); !errors.Is(result.err, cause) {
		t.Fatalf("pending error = %v, want %v", result.err, cause)
	}
	if writer.closeCalls() != 1 || kills.Load() != 1 {
		t.Fatalf("standalone abort cleanup: input=%d kill=%d, want 1 each", writer.closeCalls(), kills.Load())
	}
	waitSignal(t, waitStarted, "standalone abort process wait")

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_ = provider.Close()
				return
			}
			connection.abort(errors.New("later abort"))
		}(i)
	}
	wg.Wait()

	state, terminalCause := transportStateAndCause(connection)
	if state != transportAborted || !errors.Is(terminalCause, cause) {
		t.Fatalf("terminal transport = (%v, %v), want aborted/%v", state, terminalCause, cause)
	}
	for _, policy := range []writePolicy{writeExternal, writeServerResponse, writeShutdown, writeExit} {
		if err := connection.admitWrite(policy); !errors.Is(err, cause) {
			t.Fatalf("write policy %v after abort = %v, want %v", policy, err, cause)
		}
	}
	if writer.closeCalls() != 1 || kills.Load() != 1 || waits.Load() != 1 {
		t.Fatalf("cleanup calls: input=%d kill=%d wait=%d, want 1 each", writer.closeCalls(), kills.Load(), waits.Load())
	}
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("protocol methods after abort = %v, want none", methods)
	}
}

func TestTransportReaderFailureDuringCloseReleasesShutdown(t *testing.T) {
	connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
	shutdown, started := connection.beginClose("1")
	if !started {
		t.Fatal("close transition did not start")
	}
	cause := errors.New("reader failed")
	connection.readerFailed(cause)
	if result := waitPendingResult(t, shutdown); !errors.Is(result.err, cause) {
		t.Fatalf("shutdown error = %v, want %v", result.err, cause)
	}
	state, _ := transportStateAndCause(connection)
	if state != transportClosing {
		t.Fatalf("transport state = %v, want closing until process cleanup", state)
	}
	if err := connection.admitWrite(writeExternal); !errors.Is(err, errProviderClosed) {
		t.Fatalf("external admission after reader failure = %v, want provider closed", err)
	}
	if err := connection.admitWrite(writeServerResponse); err != nil {
		t.Fatalf("server response admission while closing: %v", err)
	}
	if err := connection.admitWrite(writeExit); err != nil {
		t.Fatalf("exit admission while closing: %v", err)
	}
	connection.finishClose()
	state, terminalCause := transportStateAndCause(connection)
	if state != transportClosed || !errors.Is(terminalCause, errProviderClosed) {
		t.Fatalf("terminal transport = (%v, %v), want closed/provider closed", state, terminalCause)
	}
}

func TestTransportAbortDuringCloseReleasesShutdownWithAbortCause(t *testing.T) {
	connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
	shutdown, started := connection.beginClose("1")
	if !started {
		t.Fatal("close transition did not start")
	}
	cause := errors.New("write failed")
	connection.abort(cause)
	if result := waitPendingResult(t, shutdown); !errors.Is(result.err, cause) {
		t.Fatalf("shutdown error = %v, want %v", result.err, cause)
	}
	connection.finishClose()
	state, terminalCause := transportStateAndCause(connection)
	if state != transportAborted || !errors.Is(terminalCause, cause) {
		t.Fatalf("terminal transport = (%v, %v), want aborted/%v", state, terminalCause, cause)
	}
}

func TestTransportProcessReapingBranches(t *testing.T) {
	t.Run("graceful exit avoids kill", func(t *testing.T) {
		connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
		var waits atomic.Int32
		var kills atomic.Int32
		connection.waitProcess = func() error {
			waits.Add(1)
			return nil
		}
		connection.killProcess = func() error {
			kills.Add(1)
			return nil
		}

		connection.waitForProcess(time.Second)
		connection.waitForProcess(time.Second)
		if waits.Load() != 1 || kills.Load() != 0 {
			t.Fatalf("process calls: wait=%d kill=%d, want 1/0", waits.Load(), kills.Load())
		}
	})

	t.Run("failed kill and blocked wait stay bounded", func(t *testing.T) {
		connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
		releaseWait := make(chan struct{})
		waitStarted := make(chan struct{})
		waitReturned := make(chan struct{})
		var waits atomic.Int32
		var kills atomic.Int32
		connection.waitProcess = func() error {
			waits.Add(1)
			close(waitStarted)
			<-releaseWait
			close(waitReturned)
			return nil
		}
		connection.killProcess = func() error {
			kills.Add(1)
			return errors.New("kill failed")
		}
		connection.killWait = time.Millisecond

		finished := make(chan struct{})
		go func() {
			connection.waitForProcess(time.Millisecond)
			connection.waitForProcess(time.Millisecond)
			close(finished)
		}()
		waitSignal(t, waitStarted, "process wait start")
		waitSignal(t, finished, "bounded failed-kill process waits")
		if waits.Load() != 1 || kills.Load() != 1 {
			t.Fatalf("process calls: wait=%d kill=%d, want 1/1", waits.Load(), kills.Load())
		}
		close(releaseWait)
		waitSignal(t, waitReturned, "process wait return")
	})

	t.Run("setup abort kills and reaps", func(t *testing.T) {
		writer := newRecordingWriteCloser()
		connection := newUnitProvider(writer, nil).transport
		releaseWait := make(chan struct{})
		var waits atomic.Int32
		var kills atomic.Int32
		connection.waitProcess = func() error {
			waits.Add(1)
			<-releaseWait
			return nil
		}
		connection.killProcess = func() error {
			kills.Add(1)
			close(releaseWait)
			return nil
		}

		cause := errors.New("setup failed")
		connection.abortAndWait(cause)
		state, terminalCause := transportStateAndCause(connection)
		if state != transportAborted || !errors.Is(terminalCause, cause) {
			t.Fatalf("terminal transport = (%v, %v), want aborted/%v", state, terminalCause, cause)
		}
		if writer.closeCalls() != 1 || waits.Load() != 1 || kills.Load() != 1 {
			t.Fatalf("cleanup calls: input=%d wait=%d kill=%d, want 1 each", writer.closeCalls(), waits.Load(), kills.Load())
		}
	})
}

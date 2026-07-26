package lsp

import (
	"encoding/json"
	"errors"
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

func TestTransportCloseTransitionOwnsPendingAndWriteAdmission(t *testing.T) {
	connection := newUnitProvider(newRecordingWriteCloser(), nil).transport
	ordinary, err := connection.register("1")
	if err != nil {
		t.Fatalf("register ordinary request: %v", err)
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
	for _, policy := range []writePolicy{writeExternal, writeServerResponse, writeExit} {
		if err := connection.admitWrite(policy); !errors.Is(err, errProviderClosed) {
			t.Fatalf("write policy %v after close = %v, want provider closed", policy, err)
		}
	}
}

func TestTransportAbortArbitratesPendingAndRepeatedCleanup(t *testing.T) {
	writer := newRecordingWriteCloser()
	provider := newUnitProvider(writer, nil)
	connection := provider.transport
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
	request, err := connection.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	cause := errors.New("wire failed")
	connection.abort(cause)
	if result := waitPendingResult(t, request); !errors.Is(result.err, cause) {
		t.Fatalf("pending error = %v, want %v", result.err, cause)
	}

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
	for _, policy := range []writePolicy{writeExternal, writeServerResponse, writeExit} {
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
		defer close(releaseWait)
		var waits atomic.Int32
		var kills atomic.Int32
		connection.waitProcess = func() error {
			waits.Add(1)
			<-releaseWait
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
		waitSignal(t, finished, "bounded failed-kill process waits")
		if waits.Load() != 1 || kills.Load() != 1 {
			t.Fatalf("process calls: wait=%d kill=%d, want 1/1", waits.Load(), kills.Load())
		}
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

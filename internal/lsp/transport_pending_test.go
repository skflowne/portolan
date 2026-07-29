package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPendingResponseTargetSurvivesConcurrentShutdown(t *testing.T) {
	p := newUnitProvider(&recordingWriteCloser{}, nil)
	request, err := p.transport.register("1")
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
		delivered = p.transport.deliverResponse("1", response)
	}()
	go func() {
		defer wg.Done()
		<-start
		p.transport.readerFailed(cause)
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
	request, err := p.transport.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	first := &jsonrpcMessage{Result: json.RawMessage(`1`)}
	second := &jsonrpcMessage{Result: json.RawMessage(`2`)}

	if !p.transport.deliverResponse("1", first) {
		t.Fatal("first response did not complete request")
	}
	if p.transport.deliverResponse("1", second) {
		t.Fatal("duplicate response completed request")
	}
	if result := waitPendingResult(t, request); result.message != first || result.err != nil {
		t.Fatalf("got terminal result %+v, want first response", result)
	}
}

func TestPendingResponseAndCancellationCompleteExactlyOnce(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := newUnitProvider(&recordingWriteCloser{}, nil)
		request, err := p.transport.register("1")
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan struct {
			result   pendingResult
			canceled bool
		}, 1)
		go func() {
			got, canceled := p.transport.waitPending(ctx, "1", request)
			result <- struct {
				result   pendingResult
				canceled bool
			}{result: got, canceled: canceled}
		}()

		start := make(chan struct{})
		response := &jsonrpcMessage{Result: json.RawMessage(`null`)}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cancel()
		}()
		go func() {
			defer wg.Done()
			<-start
			p.transport.deliverResponse("1", response)
		}()
		close(start)
		wg.Wait()

		select {
		case got := <-result:
			if got.canceled {
				if !errors.Is(got.result.err, context.Canceled) || got.result.message != nil {
					t.Fatalf("cancellation won with result %+v", got.result)
				}
			} else if got.result.message != response || got.result.err != nil {
				t.Fatalf("response won with result %+v", got.result)
			}
		case <-time.After(time.Second):
			t.Fatal("response/cancellation race did not finish")
		}
		waitForPendingCount(t, p, 0)
	}
}

func TestReadLoopFailureReleasesPendingWithCause(t *testing.T) {
	reader, writer := io.Pipe()
	p := newUnitProvider(&recordingWriteCloser{}, reader)
	p.transport.stderr.buf = []byte("server exploded")
	request, err := p.transport.register("1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	loopDone := make(chan struct{})
	go func() {
		p.transport.readLoop()
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

func TestCallDeadlineRemovesPendingAndIgnoresLateResponse(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := p.transport.call(ctx, "caller-budget", nil)
		result <- err
	}()
	writer.waitForMethod(t, "caller-budget")
	if err := waitError(t, result, "caller-budget call"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error = %v, want deadline exceeded", err)
	}
	if p.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("late response completed timed-out request")
	}
}

func TestCallTimeoutRemovesPendingAndIgnoresLateResponse(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	firstCtx, firstCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer firstCancel()

	if _, err := p.transport.call(firstCtx, "first", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want deadline exceeded", err)
	}
	if p.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`"late"`)}) {
		t.Fatal("late response completed timed-out request")
	}

	resultCh := make(chan pendingResult, 1)
	go func() {
		result, err := p.transport.call(context.Background(), "second", nil)
		resultCh <- pendingResult{message: &jsonrpcMessage{Result: result}, err: err}
	}()
	writer.waitForMethod(t, "second")
	if !p.transport.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`"fresh"`)}) {
		t.Fatal("second response did not complete request")
	}
	result := waitCallResult(t, resultCh)
	if result.err != nil || string(result.message.Result) != `"fresh"` {
		t.Fatalf("second call result = %+v", result)
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

package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newUnitProvider(stdin io.WriteCloser, stdout io.Reader) *Provider {
	if stdout == nil {
		stdout = strings.NewReader("")
	}
	return &Provider{
		stdin:                    stdin,
		stdoutR:                  bufio.NewReader(stdout),
		lifecycle:                newTransportLifecycle(),
		stderrBuf:                newStderrBuffer(),
		writeGate:                newWriteGate(),
		internalWriteTimeout:     defaultInternalWriteTimeout,
		cancellationWriteTimeout: defaultCancellationWriteTimeout,
		shutdownTimeout:          defaultShutdownTimeout,
		exitWait:                 defaultExitWait,
		killWait:                 defaultKillWait,
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

func TestPendingResponseAndCancellationCompleteExactlyOnce(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := newUnitProvider(&recordingWriteCloser{}, nil)
		request, err := p.lifecycle.register("1")
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan struct {
			result   pendingResult
			canceled bool
		}, 1)
		go func() {
			got, canceled := p.waitPending(ctx, "1", request)
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
			p.lifecycle.deliverResponse("1", response)
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

func TestCallWaitUsesCallerContextWithoutNestedDeadline(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	observed := make(chan context.Context, 1)
	p.observeRequestContext = func(ctx context.Context) { observed <- ctx }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := p.call(ctx, "caller-budget", nil)
		result <- err
	}()
	writer.waitForMethod(t, "caller-budget")
	select {
	case got := <-observed:
		if got != ctx {
			t.Fatal("call replaced the caller context")
		}
		gotDeadline, _ := got.Deadline()
		wantDeadline, _ := ctx.Deadline()
		if !gotDeadline.Equal(wantDeadline) {
			t.Fatalf("wait deadline = %v, want %v", gotDeadline, wantDeadline)
		}
	case <-time.After(time.Second):
		t.Fatal("call did not reach response wait")
	}
	if !p.lifecycle.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("response was not delivered")
	}
	if err := waitError(t, result, "caller-budget call"); err != nil {
		t.Fatalf("call: %v", err)
	}
}

func TestCallTimeoutRemovesPendingAndIgnoresLateResponse(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	firstCtx, firstCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer firstCancel()

	if _, err := p.call(firstCtx, "first", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first call error = %v, want deadline exceeded", err)
	}
	if p.lifecycle.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`"late"`)}) {
		t.Fatal("late response completed timed-out request")
	}

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

func TestCallBlockedWriteHonorsContext(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	var kills atomic.Int32
	p.killProcess = func() error {
		kills.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.call(ctx, "blocked", nil)
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
	p.lifecycle.mu.Lock()
	pending := len(p.lifecycle.pending)
	p.lifecycle.mu.Unlock()
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
		_, err := p.call(ctx, "uninterruptible", nil)
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
		_, err := p.call(ctx, "work", nil)
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
	if p.lifecycle.deliverResponse(strconv.FormatInt(request.ID, 10), &jsonrpcMessage{Result: json.RawMessage(`"late"`)}) {
		t.Fatal("late response completed canceled request")
	}
}

func TestResponseWinningCancellationRaceDoesNotSendCancel(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.call(ctx, "response-wins", nil)
		result <- err
	}()
	writer.waitForMethod(t, "response-wins")
	if !p.lifecycle.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("response was not delivered")
	}
	if err := waitError(t, result, "response-winning call"); err != nil {
		t.Fatalf("call: %v", err)
	}
	cancellation := make(chan error, 1)
	p.observeCancellation = func(err error) { cancellation <- err }
	p.cancellationWriteTimeout = 10 * time.Millisecond
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
	if err := p.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.call(ctx, "blocked-at-gate", nil); !errors.Is(err, context.Canceled) {
		p.unlockWrite()
		t.Fatalf("call error = %v, want context canceled", err)
	}
	p.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	p.lifecycle.mu.Lock()
	pending := len(p.lifecycle.pending)
	p.lifecycle.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}

	result := make(chan error, 1)
	go func() {
		_, err := p.call(context.Background(), "usable", nil)
		result <- err
	}()
	writer.waitForMethod(t, "usable")
	if !p.lifecycle.deliverResponse("2", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("usable response was not delivered")
	}
	if err := waitError(t, result, "usable call"); err != nil {
		t.Fatalf("usable call: %v", err)
	}
}

func TestBlockedWriteGateAcquisitionHonorsCancellation(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	if err := p.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := p.call(ctx, "waiting-at-gate", nil)
		result <- err
	}()
	waitForPendingCount(t, p, 1)
	cancel()
	if err := waitError(t, result, "gate-blocked call"); !errors.Is(err, context.Canceled) {
		p.unlockWrite()
		t.Fatalf("call error = %v, want context canceled", err)
	}
	p.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	waitForPendingCount(t, p, 0)
}

func TestCancellationNotificationGateTimeoutIsDropped(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.cancellationWriteTimeout = 10 * time.Millisecond
	observed := make(chan error, 1)
	p.observeCancellation = func(err error) { observed <- err }
	if err := p.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	p.sendCancellation(7)
	if err := waitError(t, observed, "cancellation gate timeout"); !errors.Is(err, context.DeadlineExceeded) {
		p.unlockWrite()
		t.Fatalf("cancellation error = %v, want deadline exceeded", err)
	}
	p.unlockWrite()
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}
	if err := p.lifecycle.admitExternalWrite(); err != nil {
		t.Fatalf("provider became unusable after cancellation gate timeout: %v", err)
	}
}

func TestCancellationNotificationBlockedWriteAbortsTransport(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.cancellationWriteTimeout = 10 * time.Millisecond
	observed := make(chan error, 1)
	p.observeCancellation = func(err error) { observed <- err }
	p.sendCancellation(7)
	waitSignal(t, writer.started, "cancellation write")
	if err := waitError(t, observed, "blocked cancellation write"); err == nil {
		t.Fatal("blocked cancellation write returned no error")
	}
	waitSignal(t, writer.returned, "cancellation writer exit")
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	if err := p.lifecycle.admitExternalWrite(); err == nil {
		t.Fatal("provider remained open after partial cancellation frame")
	}
}

func TestMethodNotFoundWriteIsAsynchronousAndBounded(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.internalWriteTimeout = 10 * time.Millisecond
	returned := make(chan struct{})
	go func() {
		p.respondMethodNotFound(json.RawMessage(`"server-1"`), "unsupported")
		close(returned)
	}()
	waitSignal(t, returned, "respondMethodNotFound return")
	waitSignal(t, writer.started, "MethodNotFound write")
	waitSignal(t, writer.returned, "MethodNotFound writer exit")
	if err := p.lifecycle.admitExternalWrite(); err == nil {
		t.Fatal("provider remained open after blocked MethodNotFound frame")
	}
}

func TestCloseTimesOutWaitingForWriteGate(t *testing.T) {
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.shutdownTimeout = 10 * time.Millisecond
	var kills atomic.Int32
	p.killProcess = func() error {
		kills.Add(1)
		return nil
	}
	if err := p.lockWrite(context.Background()); err != nil {
		t.Fatalf("hold write gate: %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- p.Close() }()
	if err := waitError(t, result, "Close waiting for gate"); err != nil {
		p.unlockWrite()
		t.Fatalf("Close: %v", err)
	}
	p.unlockWrite()
	if kills.Load() != 1 || writer.closeCalls() != 1 {
		t.Fatalf("kill calls = %d stdin close calls = %d, want 1 each", kills.Load(), writer.closeCalls())
	}
}

func TestCloseTimesOutDuringShutdownWrite(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.shutdownTimeout = 10 * time.Millisecond
	result := make(chan error, 1)
	go func() { result <- p.Close() }()
	waitSignal(t, writer.started, "shutdown write")
	if err := waitError(t, result, "Close during shutdown write"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitSignal(t, writer.returned, "shutdown writer exit")
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
}

func TestWaitForProcessKillsAndReapsWithinBounds(t *testing.T) {
	p := newUnitProvider(&recordingWriteCloser{}, nil)
	releaseWait := make(chan struct{})
	waitReturned := make(chan struct{})
	p.waitProcess = func() error {
		<-releaseWait
		close(waitReturned)
		return nil
	}
	p.killProcess = func() error {
		close(releaseWait)
		return nil
	}
	p.killWait = time.Second
	finished := make(chan struct{})
	go func() {
		p.waitForProcess(10 * time.Millisecond)
		close(finished)
	}()
	waitSignal(t, finished, "bounded process wait")
	waitSignal(t, waitReturned, "process reap")
}

func TestPartialFrameErrorTerminatesTransport(t *testing.T) {
	cause := errors.New("partial write failed")
	writer := &partialErrorWriteCloser{limit: 8, err: cause}
	p := newUnitProvider(writer, nil)

	if _, err := p.call(context.Background(), "partial", nil); !errors.Is(err, cause) {
		t.Fatalf("call error = %v, want %v", err, cause)
	}
	if writer.closeCalls() != 1 {
		t.Fatalf("stdin close calls = %d, want 1", writer.closeCalls())
	}
	if _, err := p.call(context.Background(), "later", nil); err == nil {
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
		_, err := p.call(context.Background(), "short", nil)
		result <- err
	}()
	writer.waitForFrame(t)
	if !p.lifecycle.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
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
	go func() { notifyErr <- p.notify(context.Background(), "queued", nil) }()
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

func waitForPendingCount(t *testing.T, p *Provider, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.lifecycle.mu.Lock()
		got := len(p.lifecycle.pending)
		p.lifecycle.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending request count did not reach %d", want)
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
func (w *failingWriteCloser) Close() error              { return nil }

var _ io.WriteCloser = (*recordingWriteCloser)(nil)
var _ io.WriteCloser = (*closeBlockingWriteCloser)(nil)
var _ io.WriteCloser = (*uninterruptibleWriteCloser)(nil)
var _ io.WriteCloser = (*partialErrorWriteCloser)(nil)
var _ io.WriteCloser = (*shortWriteCloser)(nil)
var _ io.WriteCloser = (*failingWriteCloser)(nil)

package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newOpenUnitProvider(reader func(context.Context, string) ([]byte, error), writer *recordingWriteCloser) *Provider {
	p := newUnitProvider(writer, nil)
	p.openFiles = make(map[string]*openTransition)
	p.readFile = reader
	return p
}

func TestFirstOpenCancellationDoesNotWaitForOrApplyStaleRead(t *testing.T) {
	writer := newRecordingWriteCloser()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	var attempts atomic.Int32
	p := newOpenUnitProvider(func(_ context.Context, _ string) ([]byte, error) {
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			close(firstDone)
			return []byte("stale"), nil
		}
		return []byte("fresh"), nil
	}, writer)

	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() { firstResult <- p.ensureOpen(ctx, "/repo/a.ts") }()
	waitSignal(t, firstStarted, "first file read")
	cancel()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first open error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		release()
		waitSignal(t, firstDone, "stale reader cleanup")
		t.Fatal("first open did not return after cancellation")
	}

	retry := make(chan error, 1)
	go func() { retry <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	select {
	case err := <-retry:
		if err != nil {
			t.Fatalf("retry open: %v", err)
		}
	case <-time.After(time.Second):
		release()
		waitSignal(t, firstDone, "stale reader cleanup")
		select {
		case <-retry:
		case <-time.After(time.Second):
			t.Fatal("retry remained blocked after stale reader cleanup")
		}
		t.Fatal("retry blocked behind stale first-open work")
	}
	writer.waitForMethod(t, "textDocument/didOpen")
	if got := attempts.Load(); got != 2 {
		t.Fatalf("read attempts = %d, want 2", got)
	}

	release()
	waitSignal(t, firstDone, "stale reader exit")
	if methods := writer.methods(); len(methods) != 1 {
		t.Fatalf("written methods after stale read completed = %v, want one didOpen", methods)
	}
	p.openMu.Lock()
	transition := p.openFiles["/repo/a.ts"]
	p.openMu.Unlock()
	if transition == nil || transition.err != nil {
		t.Fatalf("open transition = %+v, want permanent success", transition)
	}
}

func TestFirstOpensForDifferentFilesReadConcurrently(t *testing.T) {
	writer := newRecordingWriteCloser()
	entered := make(chan string, 2)
	releases := map[string]chan struct{}{
		"/repo/a.ts": make(chan struct{}),
		"/repo/b.ts": make(chan struct{}),
	}
	p := newOpenUnitProvider(func(_ context.Context, path string) ([]byte, error) {
		entered <- path
		<-releases[path]
		return []byte(path), nil
	}, writer)

	results := make(chan error, 2)
	go func() { results <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	go func() { results <- p.ensureOpen(context.Background(), "/repo/b.ts") }()
	seen := map[string]bool{}
	for range 2 {
		select {
		case path := <-entered:
			seen[path] = true
		case <-time.After(time.Second):
			for _, release := range releases {
				close(release)
			}
			t.Fatal("different-file reads did not overlap")
		}
	}
	for _, release := range releases {
		close(release)
	}
	for range 2 {
		if err := waitError(t, results, "different-file open"); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("entered reads = %v, want both files", seen)
	}
	if methods := writer.methods(); len(methods) != 2 {
		t.Fatalf("written methods = %v, want two didOpen notifications", methods)
	}
}

func TestConcurrentSameFileOpenIsCanonical(t *testing.T) {
	writer := newRecordingWriteCloser()
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	p := newOpenUnitProvider(func(_ context.Context, _ string) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte("source"), nil
	}, writer)

	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() { results <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	}
	waitSignal(t, started, "same-file read")
	close(release)
	for range callers {
		if err := waitError(t, results, "same-file open"); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("read count = %d, want 1", got)
	}
	if methods := writer.methods(); len(methods) != 1 {
		t.Fatalf("written methods = %v, want one didOpen", methods)
	}
}

func TestCanceledSameFileWaiterDoesNotCancelOwner(t *testing.T) {
	writer := newRecordingWriteCloser()
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	p := newOpenUnitProvider(func(_ context.Context, _ string) ([]byte, error) {
		reads.Add(1)
		close(started)
		<-release
		return []byte("source"), nil
	}, writer)

	owner := make(chan error, 1)
	go func() { owner <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	waitSignal(t, started, "owner read")
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- p.ensureOpen(waiterCtx, "/repo/a.ts") }()
	cancel()
	if err := waitError(t, waiter, "same-file waiter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context canceled", err)
	}
	close(release)
	if err := waitError(t, owner, "same-file owner"); err != nil {
		t.Fatalf("owner open: %v", err)
	}
	if reads.Load() != 1 || len(writer.methods()) != 1 {
		t.Fatalf("reads = %d methods = %v, want one canonical open", reads.Load(), writer.methods())
	}
}

func TestLiveSameFileWaitersRetryCanceledOwner(t *testing.T) {
	writer := newRecordingWriteCloser()
	ownerStarted := make(chan struct{})
	ownerReadDone := make(chan struct{})
	var reads atomic.Int32
	p := newOpenUnitProvider(func(ctx context.Context, _ string) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(ownerStarted)
			<-ctx.Done()
			close(ownerReadDone)
			return nil, ctx.Err()
		}
		return []byte("fresh source"), nil
	}, writer)

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	owner := make(chan error, 1)
	go func() { owner <- p.ensureOpen(ownerCtx, "/repo/a.ts") }()
	waitSignal(t, ownerStarted, "owner read")

	const waiterCount = 2
	waiters := make(chan error, waiterCount)
	waiterContexts := make([]*observedWaitContext, waiterCount)
	for i := range waiterCount {
		waiterContexts[i] = newObservedWaitContext()
		ctx := waiterContexts[i]
		go func() { waiters <- p.ensureOpen(ctx, "/repo/a.ts") }()
	}
	for _, ctx := range waiterContexts {
		waitSignal(t, ctx.waiting, "same-file transition wait")
	}

	cancelOwner()
	if err := waitError(t, owner, "canceled open owner"); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner error = %v, want context canceled", err)
	}
	for range waiterCount {
		if err := waitError(t, waiters, "live same-file waiter"); err != nil {
			t.Fatalf("waiter open: %v", err)
		}
	}
	waitSignal(t, ownerReadDone, "canceled owner read cleanup")
	if got := reads.Load(); got != 2 {
		t.Fatalf("read attempts = %d, want canceled owner plus one retry", got)
	}
	bodies := writer.messageBodies()
	if len(bodies) != 1 {
		t.Fatalf("written messages = %d, want one didOpen", len(bodies))
	}
	var notification struct {
		Method string        `json:"method"`
		Params didOpenParams `json:"params"`
	}
	if err := json.Unmarshal(bodies[0], &notification); err != nil {
		t.Fatalf("decode didOpen: %v", err)
	}
	item := notification.Params.TextDocument
	if notification.Method != "textDocument/didOpen" || item.URI != "file:///repo/a.ts" || item.Version != 1 || item.Text != "fresh source" {
		t.Fatalf("didOpen notification = %+v", notification)
	}
}

func TestSameFileWaitersDoNotRetryNonContextFailure(t *testing.T) {
	writer := newRecordingWriteCloser()
	ownerStarted := make(chan struct{})
	releaseOwner := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOwner) }) }
	defer release()
	errRead := errors.New("read failed")
	var reads atomic.Int32
	p := newOpenUnitProvider(func(_ context.Context, _ string) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(ownerStarted)
			<-releaseOwner
			return nil, errRead
		}
		return []byte("fresh source"), nil
	}, writer)

	owner := make(chan error, 1)
	go func() { owner <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	waitSignal(t, ownerStarted, "owner read")
	waiterCtx := newObservedWaitContext()
	waiter := make(chan error, 1)
	go func() { waiter <- p.ensureOpen(waiterCtx, "/repo/a.ts") }()
	waitSignal(t, waiterCtx.waiting, "same-file transition wait")
	release()

	if err := waitError(t, owner, "failed open owner"); !errors.Is(err, errRead) {
		t.Fatalf("owner error = %v, want %v", err, errRead)
	}
	if err := waitError(t, waiter, "failed open waiter"); !errors.Is(err, errRead) {
		t.Fatalf("waiter error = %v, want %v", err, errRead)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("read attempts = %d, want no automatic retry", got)
	}
	if methods := writer.methods(); len(methods) != 0 {
		t.Fatalf("written methods = %v, want none", methods)
	}

	if err := p.ensureOpen(context.Background(), "/repo/a.ts"); err != nil {
		t.Fatalf("later independent retry: %v", err)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("read attempts after independent retry = %d, want 2", got)
	}
	writer.waitForMethod(t, "textDocument/didOpen")
}

func TestSameFileWaiterDoesNotRetryAfterCanceledPartialDidOpen(t *testing.T) {
	writer := newCloseBlockingWriteCloser()
	defer writer.Close()
	p := newUnitProvider(writer, nil)
	p.openFiles = make(map[string]*openTransition)
	var reads atomic.Int32
	p.readFile = func(context.Context, string) ([]byte, error) {
		reads.Add(1)
		return []byte("source"), nil
	}

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	owner := make(chan error, 1)
	go func() { owner <- p.ensureOpen(ownerCtx, "/repo/a.ts") }()
	waitSignal(t, writer.started, "partial didOpen write")

	waiterCtx := newObservedWaitContext()
	waiter := make(chan error, 1)
	go func() { waiter <- p.ensureOpen(waiterCtx, "/repo/a.ts") }()
	waitSignal(t, waiterCtx.waiting, "same-file transition wait")
	cancelOwner()

	ownerErr := waitError(t, owner, "partial didOpen owner")
	if ownerErr == nil {
		t.Fatal("owner returned no error after partial didOpen cancellation")
	}
	waiterErr := waitError(t, waiter, "partial didOpen waiter")
	if waiterErr == nil || waiterErr.Error() != ownerErr.Error() {
		t.Fatalf("waiter error = %v, want owner transition error %v", waiterErr, ownerErr)
	}
	waitSignal(t, writer.returned, "partial didOpen writer cleanup")
	if got := reads.Load(); got != 1 {
		t.Fatalf("read attempts = %d, want no retry on closed transport", got)
	}
	if p.transport.isOpen() {
		t.Fatal("transport remained open after partial didOpen write")
	}
}

func TestCompletedDidOpenWinsCancellationRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := newRecordingWriteCloser()
	p := newUnitProvider(writer, nil)
	p.openFiles = make(map[string]*openTransition)
	p.readFile = func(context.Context, string) ([]byte, error) { return []byte("source"), nil }
	dispatched := make(chan struct{})
	releasePublication := make(chan struct{})
	p.transport.afterFrameDispatch = func() {
		close(dispatched)
		<-releasePublication
	}

	result := make(chan error, 1)
	go func() { result <- p.ensureOpen(ctx, "/repo/a.ts") }()
	waitSignal(t, dispatched, "didOpen dispatch")
	cancel()
	close(releasePublication)
	if err := waitError(t, result, "didOpen publication"); err != nil {
		t.Fatalf("first open: %v", err)
	}
	p.transport.afterFrameDispatch = nil
	if err := p.ensureOpen(context.Background(), "/repo/a.ts"); err != nil {
		t.Fatalf("cached open: %v", err)
	}
	if methods := writer.methods(); len(methods) != 1 {
		t.Fatalf("written methods = %v, want one canonical didOpen", methods)
	}
}

type blockedFirstOpenWaiters struct {
	provider     *Provider
	writer       *recordingWriteCloser
	leader       <-chan error
	follower     <-chan error
	readReturned <-chan struct{}
	releaseRead  func()
}

func startBlockedFirstOpenWaiters(t *testing.T) blockedFirstOpenWaiters {
	t.Helper()
	writer := newRecordingWriteCloser()
	readStarted := make(chan struct{})
	readReturned := make(chan struct{})
	releaseRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	t.Cleanup(release)

	p := newOpenUnitProvider(func(context.Context, string) ([]byte, error) {
		close(readStarted)
		<-releaseRead
		close(readReturned)
		return []byte("stale source"), nil
	}, writer)
	leader := make(chan error, 1)
	go func() { leader <- p.ensureOpen(context.Background(), "/repo/a.ts") }()
	waitSignal(t, readStarted, "first-open read")

	followerCtx := newObservedWaitContext()
	follower := make(chan error, 1)
	go func() { follower <- p.ensureOpen(followerCtx, "/repo/a.ts") }()
	waitSignal(t, followerCtx.waiting, "same-file transition wait")
	return blockedFirstOpenWaiters{
		provider: p, writer: writer, leader: leader, follower: follower,
		readReturned: readReturned, releaseRead: release,
	}
}

func assertNoDidOpen(t *testing.T, writer *recordingWriteCloser) {
	t.Helper()
	for _, method := range writer.methods() {
		if method == "textDocument/didOpen" {
			t.Fatal("didOpen written for canceled first-open work")
		}
	}
}

func TestCloseReleasesBlockedFirstOpenWaiters(t *testing.T) {
	blocked := startBlockedFirstOpenWaiters(t)
	blocked.provider.transport.shutdownTimeout = 5 * time.Second

	closeResult := make(chan error, 1)
	go func() { closeResult <- blocked.provider.Close() }()
	blocked.writer.waitForShutdown(t)
	var releaseShutdownOnce sync.Once
	releaseShutdown := func() { releaseShutdownOnce.Do(func() { close(blocked.writer.releaseShutdown) }) }
	defer func() {
		blocked.provider.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)})
		releaseShutdown()
	}()

	if err := waitError(t, blocked.leader, "first-open leader after close"); !errors.Is(err, errProviderClosed) {
		t.Fatalf("leader error = %v, want provider closed", err)
	}
	if err := waitError(t, blocked.follower, "first-open follower after close"); !errors.Is(err, errProviderClosed) {
		t.Fatalf("follower error = %v, want provider closed", err)
	}
	assertNoDidOpen(t, blocked.writer)

	blocked.releaseRead()
	waitSignal(t, blocked.readReturned, "blocked read cleanup")
	assertNoDidOpen(t, blocked.writer)

	if !blocked.provider.transport.deliverResponse("1", &jsonrpcMessage{Result: json.RawMessage(`null`)}) {
		t.Fatal("shutdown response was not delivered")
	}
	releaseShutdown()
	if err := waitError(t, closeResult, "provider close"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAbortReleasesBlockedFirstOpenWaiters(t *testing.T) {
	blocked := startBlockedFirstOpenWaiters(t)
	cause := errors.New("reader failed")
	blocked.provider.transport.abort(cause)

	if err := waitError(t, blocked.leader, "first-open leader after abort"); !errors.Is(err, cause) {
		t.Fatalf("leader error = %v, want %v", err, cause)
	}
	if err := waitError(t, blocked.follower, "first-open follower after abort"); !errors.Is(err, cause) {
		t.Fatalf("follower error = %v, want %v", err, cause)
	}
	assertNoDidOpen(t, blocked.writer)

	blocked.releaseRead()
	waitSignal(t, blocked.readReturned, "blocked read cleanup")
	assertNoDidOpen(t, blocked.writer)
}

func TestInitializeUsesDedicatedContextForRequestAndNotification(t *testing.T) {
	var requestCtx context.Context
	var notifyCtx context.Context
	request := func(ctx context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "initialize" {
			t.Fatalf("request method = %q, want initialize", method)
		}
		requestCtx = ctx
		return json.RawMessage(`{}`), nil
	}
	notify := func(ctx context.Context, method string, _ any) error {
		if method != "initialized" {
			t.Fatalf("notification method = %q, want initialized", method)
		}
		notifyCtx = ctx
		return nil
	}

	if err := initializeProvider("file:///repo", "repo", request, notify); err != nil {
		t.Fatalf("initializeProvider: %v", err)
	}
	if requestCtx == nil || notifyCtx != requestCtx {
		t.Fatal("initialize request and notification did not share one context")
	}
	deadline, ok := requestCtx.Deadline()
	if !ok {
		t.Fatal("initialize context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 5*time.Second || remaining > initializeTimeout {
		t.Fatalf("initialize budget remaining = %v, want (5s, %v]", remaining, initializeTimeout)
	}
}

type observedWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func newObservedWaitContext() *observedWaitContext {
	return &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func waitSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

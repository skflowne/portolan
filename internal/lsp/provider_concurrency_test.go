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
		if err := waitTestError(t, results, "different-file open"); err != nil {
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
		if err := waitTestError(t, results, "same-file open"); err != nil {
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
	if err := waitTestError(t, waiter, "same-file waiter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context canceled", err)
	}
	close(release)
	if err := waitTestError(t, owner, "same-file owner"); err != nil {
		t.Fatalf("owner open: %v", err)
	}
	if reads.Load() != 1 || len(writer.methods()) != 1 {
		t.Fatalf("reads = %d methods = %v, want one canonical open", reads.Load(), writer.methods())
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
	p.afterFrameDispatch = func() {
		close(dispatched)
		<-releasePublication
	}

	result := make(chan error, 1)
	go func() { result <- p.ensureOpen(ctx, "/repo/a.ts") }()
	waitSignal(t, dispatched, "didOpen dispatch")
	cancel()
	close(releasePublication)
	if err := waitTestError(t, result, "didOpen publication"); err != nil {
		t.Fatalf("first open: %v", err)
	}
	p.afterFrameDispatch = nil
	if err := p.ensureOpen(context.Background(), "/repo/a.ts"); err != nil {
		t.Fatalf("cached open: %v", err)
	}
	if methods := writer.methods(); len(methods) != 1 {
		t.Fatalf("written methods = %v, want one canonical didOpen", methods)
	}
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

func waitSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitTestError(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}

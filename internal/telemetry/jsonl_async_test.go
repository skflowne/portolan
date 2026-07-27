package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

type controlledSink struct {
	mu sync.Mutex

	writeStarted chan struct{}
	writeGate    chan struct{}
	closeStarted chan struct{}
	closeGate    chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once

	writeFn  func([]byte) (int, error)
	syncErr  error
	closeErr error
	writes   [][]byte
	syncs    int
	closes   int
}

func (s *controlledSink) Write(p []byte) (int, error) {
	if s.writeStarted != nil {
		s.startOnce.Do(func() { close(s.writeStarted) })
	}
	if s.writeGate != nil {
		<-s.writeGate
	}
	if s.writeFn != nil {
		return s.writeFn(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, bytes.Clone(p))
	return len(p), nil
}

func (s *controlledSink) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs++
	return s.syncErr
}

func (s *controlledSink) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	if s.closeStarted != nil {
		s.closeOnce.Do(func() { close(s.closeStarted) })
	}
	if s.closeGate != nil {
		<-s.closeGate
	}
	return s.closeErr
}

func (s *controlledSink) snapshot() ([][]byte, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writes := make([][]byte, len(s.writes))
	for i := range s.writes {
		writes[i] = bytes.Clone(s.writes[i])
	}
	return writes, s.syncs, s.closes
}

func testJSONL(sink jsonlSink, mutate func(*jsonlOptions)) *JSONLLogger {
	opts := defaultJSONLOptions()
	opts.capacity = 2
	opts.admissionTimeout = 10 * time.Millisecond
	opts.shutdownTimeout = 250 * time.Millisecond
	if mutate != nil {
		mutate(&opts)
	}
	return newJSONLLogger(sink, opts)
}

func TestJSONLProductionDefaults(t *testing.T) {
	opts := defaultJSONLOptions()
	if opts.capacity != 512 {
		t.Errorf("capacity = %d, want 512", opts.capacity)
	}
	if opts.admissionTimeout != 50*time.Millisecond {
		t.Errorf("admission timeout = %s, want 50ms", opts.admissionTimeout)
	}
	if opts.maxRecordBytes != 16*1024 {
		t.Errorf("record cap = %d, want 16384 bytes", opts.maxRecordBytes)
	}
	if opts.shutdownTimeout != 2*time.Second {
		t.Errorf("shutdown timeout = %s, want 2s", opts.shutdownTimeout)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForCondition(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func logEvent(logger *JSONLLogger, ctx context.Context, id string) {
	logger.Log(ctx, core.Event{SessionID: "session", GraphMode: "graph", Tool: id})
}

func logEventAsync(logger *JSONLLogger, ctx context.Context, id string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		logEvent(logger, ctx, id)
		close(done)
	}()
	return done
}

func requireReturned(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within the tool-path bound", what)
	}
}

func decodeTools(t *testing.T, writes [][]byte) []string {
	t.Helper()
	tools := make([]string, 0, len(writes))
	for _, line := range writes {
		if len(line) == 0 || line[len(line)-1] != '\n' {
			t.Fatalf("record is not a complete line: %q", line)
		}
		var ev core.Event
		if err := json.Unmarshal(bytes.TrimSpace(line), &ev); err != nil {
			t.Fatalf("invalid JSONL record %q: %v", line, err)
		}
		tools = append(tools, ev.Tool)
	}
	return tools
}

func TestJSONLPendingCapacityIncludesInFlightAndDropsNewest(t *testing.T) {
	sink := &controlledSink{writeStarted: make(chan struct{}), writeGate: make(chan struct{})}
	logger := testJSONL(sink, nil)
	defer func() {
		select {
		case <-sink.writeGate:
		default:
			close(sink.writeGate)
		}
	}()

	firstDone := logEventAsync(logger, context.Background(), "first")
	waitFor(t, sink.writeStarted, "first write")
	requireReturned(t, firstDone, "first Log")
	logEvent(logger, context.Background(), "second")
	logEvent(logger, context.Background(), "newest")

	stats := logger.Stats()
	if stats.Accepted != 2 || stats.QueueTimeoutDrops != 1 || stats.Pending != 2 {
		t.Fatalf("stats while full = %+v, want accepted=2 timeout_drops=1 pending=2", stats)
	}
	close(sink.writeGate)
	if err := logger.Close(); err == nil || !stringsContain(err.Error(), "queue") {
		t.Fatalf("Close error = %v, want queue loss summary", err)
	}
	writes, _, _ := sink.snapshot()
	if got := decodeTools(t, writes); !equalStrings(got, []string{"first", "second"}) {
		t.Fatalf("written tools = %v, want FIFO retained records", got)
	}
}

func TestJSONLPreCanceledContextAdmitsWhenCapacityExists(t *testing.T) {
	sink := &controlledSink{}
	logger := testJSONL(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	logEvent(logger, ctx, "accepted")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	if got := decodeTools(t, writes); !equalStrings(got, []string{"accepted"}) {
		t.Fatalf("written tools = %v", got)
	}
}

func TestJSONLCancellationWinsWhileQueueIsFull(t *testing.T) {
	sink := &controlledSink{writeStarted: make(chan struct{}), writeGate: make(chan struct{})}
	logger := testJSONL(sink, nil)
	defer func() {
		select {
		case <-sink.writeGate:
		default:
			close(sink.writeGate)
		}
	}()
	firstDone := logEventAsync(logger, context.Background(), "first")
	waitFor(t, sink.writeStarted, "first write")
	requireReturned(t, firstDone, "first Log")
	logEvent(logger, context.Background(), "second")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logEvent(logger, ctx, "canceled")
	if got := logger.Stats().CanceledDrops; got != 1 {
		t.Fatalf("CanceledDrops = %d, want 1", got)
	}
	close(sink.writeGate)
	_ = logger.Close()
}

func TestJSONLAdmissionSnapshotsNestedExtra(t *testing.T) {
	sink := &controlledSink{writeStarted: make(chan struct{}), writeGate: make(chan struct{})}
	logger := testJSONL(sink, nil)
	nested := map[string]any{"items": []any{"before"}}
	done := make(chan struct{})
	go func() {
		logger.Log(context.Background(), core.Event{Tool: "snapshot", Extra: nested})
		close(done)
	}()
	waitFor(t, sink.writeStarted, "snapshot write")
	requireReturned(t, done, "snapshot Log")
	nested["items"].([]any)[0] = "after"
	close(sink.writeGate)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	var ev core.Event
	if err := json.Unmarshal(bytes.TrimSpace(writes[0]), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items := ev.Extra["items"].([]any)
	if items[0] != "before" {
		t.Fatalf("snapshotted nested value = %v, want before", items[0])
	}
}

func TestJSONLOversizeRecordIsCappedAndCorrelated(t *testing.T) {
	var diagnosticsMu sync.Mutex
	var diagnostics []error
	sink := &controlledSink{}
	logger := testJSONL(sink, func(opts *jsonlOptions) {
		opts.maxRecordBytes = 1024
		opts.diagnostic = func(err error) {
			diagnosticsMu.Lock()
			defer diagnosticsMu.Unlock()
			diagnostics = append(diagnostics, err)
		}
	})
	ev := core.Event{
		Timestamp:  "2020-01-01T00:00:00Z",
		SessionID:  "correlation-session",
		GraphMode:  "graph",
		Tool:       "find_references",
		Generation: 42,
		Err:        string(bytes.Repeat([]byte("e"), 4096)),
		Extra:      map[string]any{"payload": string(bytes.Repeat([]byte("x"), 4096))},
	}
	logger.Log(context.Background(), ev)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	if len(writes) != 1 || len(writes[0]) > 1024 {
		t.Fatalf("record lengths = %v, want one <=1024-byte line", recordLengths(writes))
	}
	var got core.Event
	if err := json.Unmarshal(bytes.TrimSpace(writes[0]), &got); err != nil {
		t.Fatalf("decode capped record: %v", err)
	}
	if got.Timestamp != ev.Timestamp || got.SessionID != ev.SessionID || got.GraphMode != ev.GraphMode || got.Tool != ev.Tool || got.Generation != ev.Generation {
		t.Fatalf("capped record lost correlation: %+v", got)
	}
	if got.Err == ev.Err || !stringsContain(got.Err, "sha256:") {
		t.Fatalf("oversized err was not deterministically capped: %q", got.Err)
	}
	stats := logger.Stats()
	if stats.OversizeRecords != 1 {
		t.Fatalf("OversizeRecords = %d, want 1", stats.OversizeRecords)
	}
	diagnosticsMu.Lock()
	defer diagnosticsMu.Unlock()
	if len(diagnostics) == 0 {
		t.Fatal("oversize handling emitted no diagnostic")
	}
}

func TestJSONLRecordRespectsMinimumConfiguredCap(t *testing.T) {
	const recordCap = 512

	sink := &controlledSink{}
	logger := testJSONL(sink, func(opts *jsonlOptions) { opts.maxRecordBytes = recordCap })
	pathological := string(bytes.Repeat([]byte("<"), 1000))
	logger.Log(context.Background(), core.Event{
		Timestamp:  pathological,
		SessionID:  pathological,
		GraphMode:  pathological,
		Tool:       pathological,
		DurationMs: math.MinInt64,
		ResultSize: math.MinInt,
		Truncated:  true,
		Stale:      true,
		Generation: math.MaxUint64,
		Err:        pathological,
		Extra:      map[string]any{"payload": pathological},
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writes, _, _ := sink.snapshot()
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(writes))
	}
	line := writes[0]
	if len(line) > recordCap {
		t.Fatalf("record length = %d, want <= %d", len(line), recordCap)
	}
	if len(line) == 0 || line[len(line)-1] != '\n' || bytes.Count(line, []byte{'\n'}) != 1 {
		t.Fatalf("record is not exactly one newline-terminated JSON object: %q", line)
	}
	var got map[string]any
	if err := json.Unmarshal(line[:len(line)-1], &got); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if stats := logger.Stats(); stats.OversizeRecords != 1 {
		t.Fatalf("OversizeRecords = %d, want 1", stats.OversizeRecords)
	}
}

func TestJSONLPathologicalCorrelationUsesOriginalDigest(t *testing.T) {
	sink := &controlledSink{}
	logger := testJSONL(sink, func(opts *jsonlOptions) { opts.maxRecordBytes = 1024 })
	original := string(bytes.Repeat([]byte("session-value-"), 2000))
	logger.Log(context.Background(), core.Event{SessionID: original, GraphMode: "graph", Tool: "correlated", Generation: 7})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	var got core.Event
	if err := json.Unmarshal(bytes.TrimSpace(writes[0]), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	digest := sha256.Sum256([]byte(original))
	if !stringsContain(got.SessionID, hex.EncodeToString(digest[:])) {
		t.Fatalf("capped session_id %q does not retain the original digest", got.SessionID)
	}
}

func TestNewJSONLAcceptsProductionDiagnosticCallback(t *testing.T) {
	called := make(chan struct{})
	logger, err := NewJSONL(t.TempDir()+"/events.jsonl", func(error) {
		select {
		case <-called:
		default:
			close(called)
		}
	})
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	logger.Log(context.Background(), core.Event{Tool: "oversize", Extra: map[string]any{"x": string(bytes.Repeat([]byte("x"), defaultMaxRecordBytes*2))}})
	waitFor(t, called, "production diagnostic callback")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJSONLMarshalFailureWritesCorrelatedFallback(t *testing.T) {
	sink := &controlledSink{}
	logger := testJSONL(sink, nil)
	logger.Log(context.Background(), core.Event{SessionID: "s", Tool: "bad-extra", Generation: 9, Extra: map[string]any{"bad": func() {}}})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	if got := decodeTools(t, writes); !equalStrings(got, []string{"bad-extra"}) {
		t.Fatalf("fallback tools = %v", got)
	}
	if stats := logger.Stats(); stats.MarshalFallbacks != 1 {
		t.Fatalf("MarshalFallbacks = %d, want 1", stats.MarshalFallbacks)
	}
}

func TestJSONLWriteBehaviorAndPermanentFailure(t *testing.T) {
	root := errors.New("disk failed")
	for _, tc := range []struct {
		name string
		fn   func([]byte) (int, error)
	}{
		{name: "immediate", fn: func([]byte) (int, error) { return 0, root }},
		{name: "partial_error", fn: func(p []byte) (int, error) { return min(5, len(p)), root }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			sink := &controlledSink{
				writeStarted: make(chan struct{}),
				writeGate:    make(chan struct{}),
				writeFn: func(p []byte) (int, error) {
					calls++
					return tc.fn(p)
				},
			}
			logger := testJSONL(sink, nil)
			firstDone := logEventAsync(logger, context.Background(), "failed")
			waitFor(t, sink.writeStarted, "failing write")
			requireReturned(t, firstDone, "first Log")
			logEvent(logger, context.Background(), "accepted-before-failure")
			close(sink.writeGate)
			waitForCondition(t, logger.failed.Load, "sink failure latch")
			logEvent(logger, context.Background(), "dropped-after-failure")
			if err := logger.Close(); !errors.Is(err, root) {
				t.Fatalf("Close error = %v, want root error", err)
			}
			stats := logger.Stats()
			if stats.Accepted != 2 || stats.UndeliveredAccepted != 2 || stats.SinkFailureDrops != 1 {
				t.Fatalf("failure accounting = %+v, want accepted=2 undelivered=2 sink_drops=1", stats)
			}
			if calls != 1 {
				t.Fatalf("physical write calls = %d, want 1 after permanent failure", calls)
			}
		})
	}
}

func TestJSONLWaitingAdmissionIsDroppedAfterSinkFailure(t *testing.T) {
	root := errors.New("sink failed")
	writeGate := make(chan struct{})
	waitStarted := make(chan struct{})
	var waitOnce sync.Once
	sink := &controlledSink{
		writeStarted: make(chan struct{}),
		writeGate:    writeGate,
		writeFn:      func([]byte) (int, error) { return 0, root },
	}
	logger := testJSONL(sink, func(opts *jsonlOptions) {
		opts.capacity = 1
		opts.admissionWait = func() { waitOnce.Do(func() { close(waitStarted) }) }
	})
	firstDone := logEventAsync(logger, context.Background(), "accepted")
	waitFor(t, sink.writeStarted, "failing write")
	requireReturned(t, firstDone, "accepted Log")
	waitingDone := logEventAsync(logger, context.Background(), "waiting")
	waitFor(t, waitStarted, "waiting admission")
	close(writeGate)
	requireReturned(t, waitingDone, "post-failure waiter")
	if err := logger.Close(); !errors.Is(err, root) {
		t.Fatalf("Close error = %v, want root failure", err)
	}
	stats := logger.Stats()
	if stats.Accepted != 1 || stats.UndeliveredAccepted != 1 || stats.SinkFailureDrops != 1 {
		t.Fatalf("failure waiter accounting = %+v", stats)
	}
}

func TestJSONLWriteAllContinuesAfterShortWrite(t *testing.T) {
	var out bytes.Buffer
	var calls int
	sink := &controlledSink{writeFn: func(p []byte) (int, error) {
		calls++
		n := len(p)
		if calls == 1 {
			n = len(p) / 2
		}
		return out.Write(p[:n])
	}}
	logger := testJSONL(sink, nil)
	logEvent(logger, context.Background(), "short")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls != 2 {
		t.Fatalf("write calls = %d, want 2", calls)
	}
	if got := decodeTools(t, [][]byte{out.Bytes()}); !equalStrings(got, []string{"short"}) {
		t.Fatalf("decoded tools = %v", got)
	}
}

func TestJSONLSyncAndCloseErrorsAreJoinedAndStable(t *testing.T) {
	syncErr := errors.New("sync failed")
	closeErr := errors.New("close failed")
	sink := &controlledSink{syncErr: syncErr, closeErr: closeErr}
	logger := testJSONL(sink, nil)
	logEvent(logger, context.Background(), "one")

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- logger.Close()
		}()
	}
	wg.Wait()
	close(errs)
	var text string
	for err := range errs {
		if !errors.Is(err, syncErr) || !errors.Is(err, closeErr) {
			t.Fatalf("Close error = %v, want joined sync+close errors", err)
		}
		if text == "" {
			text = err.Error()
		} else if err.Error() != text {
			t.Fatalf("Close result changed: %q != %q", err, text)
		}
	}
	_, syncs, closes := sink.snapshot()
	if syncs != 1 || closes != 1 {
		t.Fatalf("syncs=%d closes=%d, want one each", syncs, closes)
	}
}

func TestJSONLCloseTimeoutIsBoundedStableAndInterruptsSink(t *testing.T) {
	sink := &controlledSink{writeStarted: make(chan struct{}), writeGate: make(chan struct{})}
	logger := testJSONL(sink, func(opts *jsonlOptions) { opts.shutdownTimeout = 30 * time.Millisecond })
	defer func() {
		select {
		case <-sink.writeGate:
		default:
			close(sink.writeGate)
		}
	}()
	done := logEventAsync(logger, context.Background(), "blocked")
	waitFor(t, sink.writeStarted, "blocked write")
	requireReturned(t, done, "blocked-sink Log")

	started := time.Now()
	first := logger.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close blocked for %s", elapsed)
	}
	if first == nil || !stringsContain(first.Error(), "timeout") {
		t.Fatalf("Close error = %v, want timeout", first)
	}
	second := logger.Close()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("repeated Close = %v, want stable %v", second, first)
	}
	started = time.Now()
	logEvent(logger, context.Background(), "post-close")
	if time.Since(started) > time.Second {
		t.Fatal("post-timeout Log blocked")
	}
	if stats := logger.Stats(); stats.ShutdownTimeouts != 1 || stats.PostCloseDrops != 1 || stats.UndeliveredAccepted != 1 {
		t.Fatalf("timeout stats = %+v", stats)
	}
	close(sink.writeGate)
}

func TestJSONLCloseDeadlineIncludesStalledSinkClose(t *testing.T) {
	sink := &controlledSink{closeStarted: make(chan struct{}), closeGate: make(chan struct{})}
	logger := testJSONL(sink, func(opts *jsonlOptions) { opts.shutdownTimeout = 30 * time.Millisecond })
	defer func() {
		close(sink.closeGate)
		waitFor(t, logger.sinkCloseDone, "released sink Close")
		waitFor(t, logger.workerDone, "released JSONL worker")
	}()

	done := make(chan error, 1)
	go func() { done <- logger.Close() }()
	waitFor(t, sink.closeStarted, "sink Close")
	select {
	case err := <-done:
		if err == nil || !stringsContain(err.Error(), "timeout") {
			t.Fatalf("Close error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("logger Close hung on stalled sink Close")
	}
}

func TestJSONLCloseInterruptsAdmissionWait(t *testing.T) {
	writeGate := make(chan struct{})
	waitStarted := make(chan struct{})
	var waitOnce sync.Once
	sink := &controlledSink{writeStarted: make(chan struct{}), writeGate: writeGate}
	logger := testJSONL(sink, func(opts *jsonlOptions) {
		opts.admissionTimeout = time.Second
		opts.shutdownTimeout = 50 * time.Millisecond
		opts.admissionWait = func() { waitOnce.Do(func() { close(waitStarted) }) }
	})
	defer func() {
		close(writeGate)
		waitFor(t, logger.sinkCloseDone, "admission-timeout sink close")
		waitFor(t, logger.workerDone, "admission-timeout JSONL worker")
	}()
	firstDone := logEventAsync(logger, context.Background(), "first")
	waitFor(t, sink.writeStarted, "first write")
	requireReturned(t, firstDone, "first Log")
	logEvent(logger, context.Background(), "second")
	thirdDone := logEventAsync(logger, context.Background(), "waiting")
	waitFor(t, waitStarted, "full-queue admission wait")

	started := time.Now()
	err := logger.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close exceeded bound while admission waited: %s", elapsed)
	}
	if err == nil || !stringsContain(err.Error(), "timeout") {
		t.Fatalf("Close error = %v, want bounded shutdown timeout", err)
	}
	requireReturned(t, thirdDone, "waiting Log")
}

func TestJSONLDiagnosticDispatcherIsBoundedAndObservable(t *testing.T) {
	callbackStarted := make(chan struct{})
	callbackGate := make(chan struct{})
	var once sync.Once
	sink := &controlledSink{}
	logger := testJSONL(sink, func(opts *jsonlOptions) {
		opts.diagnosticCapacity = 1
		opts.shutdownTimeout = 30 * time.Millisecond
		opts.maxRecordBytes = 1024
		opts.diagnostic = func(error) {
			once.Do(func() { close(callbackStarted) })
			<-callbackGate
		}
	})
	for i := 0; i < 4; i++ {
		logger.Log(context.Background(), core.Event{Tool: "oversize", Extra: map[string]any{"x": string(bytes.Repeat([]byte("x"), 2048))}})
	}
	waitFor(t, callbackStarted, "diagnostic callback")
	started := time.Now()
	err := logger.Close()
	if time.Since(started) > time.Second {
		t.Fatal("Close blocked on diagnostic callback")
	}
	if err == nil || !stringsContain(err.Error(), "diagnostic") {
		t.Fatalf("Close error = %v, want diagnostic loss/timeout", err)
	}
	stats := logger.Stats()
	if stats.DiagnosticDrops == 0 || stats.DiagnosticTimeouts != 1 {
		t.Fatalf("diagnostic stats = %+v", stats)
	}
	close(callbackGate)
}

func TestJSONLDiagnosticCallbackCanReenterLog(t *testing.T) {
	sink := &controlledSink{}
	called := make(chan struct{})
	var logger *JSONLLogger
	logger = testJSONL(sink, func(opts *jsonlOptions) {
		opts.maxRecordBytes = 1024
		opts.diagnostic = func(error) {
			logger.Log(context.Background(), core.Event{Tool: "reentrant"})
			select {
			case <-called:
			default:
				close(called)
			}
		}
	})
	logger.Log(context.Background(), core.Event{Tool: "oversize", Extra: map[string]any{"x": string(bytes.Repeat([]byte("x"), 2048))}})
	waitFor(t, called, "reentrant diagnostic")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestJSONLDiagnosticPanicIsRecoveredAndReported(t *testing.T) {
	sink := &controlledSink{}
	logger := testJSONL(sink, func(opts *jsonlOptions) {
		opts.maxRecordBytes = 1024
		opts.diagnostic = func(error) { panic("diagnostic panic") }
	})
	logger.Log(context.Background(), core.Event{Tool: "oversize", Extra: map[string]any{"x": string(bytes.Repeat([]byte("x"), 2048))}})
	if err := logger.Close(); err == nil || !stringsContain(err.Error(), "diagnostic") {
		t.Fatalf("Close error = %v, want recovered diagnostic panic", err)
	}
	if stats := logger.Stats(); stats.DiagnosticPanics != 1 {
		t.Fatalf("DiagnosticPanics = %d, want 1", stats.DiagnosticPanics)
	}
}

func TestJSONLConcurrentLogAndCloseIsRaceSafe(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		sink := &controlledSink{}
		logger := testJSONL(sink, nil)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				logEvent(logger, context.Background(), string(rune('a'+i)))
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = logger.Close()
		}()
		close(start)
		wg.Wait()
		stats := logger.Stats()
		if stats.Accepted+stats.QueueTimeoutDrops+stats.PostCloseDrops != 20 {
			t.Fatalf("iteration %d unclassified calls: %+v", iteration, stats)
		}
	}
}

func stringsContain(s, part string) bool { return bytes.Contains([]byte(s), []byte(part)) }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func recordLengths(records [][]byte) []int {
	out := make([]int, len(records))
	for i := range records {
		out[i] = len(records[i])
	}
	return out
}

var _ io.Writer = (*controlledSink)(nil)

package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/skflowne/portolan/internal/core"
)

type capturingExporter struct {
	mu    sync.Mutex
	spans tracetest.SpanStubs
}

func (e *capturingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracetest.SpanStubsFromReadOnlySpans(spans)...)
	return nil
}

func (e *capturingExporter) Shutdown(context.Context) error { return nil }

func (e *capturingExporter) snapshot() tracetest.SpanStubs {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(tracetest.SpanStubs, len(e.spans))
	copy(out, e.spans)
	return out
}

type controlledExporter struct {
	mu sync.Mutex

	exportStarted chan struct{}
	exportGate    chan struct{}
	shutdownDone  chan struct{}
	startOnce     sync.Once
	shutdownOnce  sync.Once

	exportErr     error
	ignoreContext bool
	exported      int
	shutdowns     int
}

func (e *controlledExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.exportStarted != nil {
		e.startOnce.Do(func() { close(e.exportStarted) })
	}
	if e.exportGate != nil {
		if e.ignoreContext {
			<-e.exportGate
		} else {
			select {
			case <-e.exportGate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	e.mu.Lock()
	e.exported += len(spans)
	e.mu.Unlock()
	return e.exportErr
}

func (e *controlledExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.shutdowns++
	e.mu.Unlock()
	if e.shutdownDone != nil {
		e.shutdownOnce.Do(func() { close(e.shutdownDone) })
	}
	return nil
}

func (e *controlledExporter) snapshot() (exported, shutdowns int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exported, e.shutdowns
}

func testOTEL(t *testing.T, exporter sdktrace.SpanExporter, mutate func(*otelOptions)) *OTELLogger {
	t.Helper()
	opts := defaultOTELOptions()
	opts.queueCapacity = 2
	opts.batchSize = 1
	opts.batchTimeout = time.Hour
	opts.exportTimeout = time.Second
	opts.shutdownTimeout = 250 * time.Millisecond
	if mutate != nil {
		mutate(&opts)
	}
	logger, err := newOTELLogger(exporter, opts)
	if err != nil {
		t.Fatalf("newOTELLogger: %v", err)
	}
	return logger
}

func attributesByName(attributes []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value)
	for _, attr := range attributes {
		out[string(attr.Key)] = attr.Value
	}
	return out
}

func requireAttribute(t *testing.T, attributes map[string]attribute.Value, key string) attribute.Value {
	t.Helper()
	value, ok := attributes[key]
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return value
}

func TestOTELLoggerExportsOneCompleteSpan(t *testing.T) {
	exporter := &capturingExporter{}
	logger := testOTEL(t, exporter, nil)
	ev := core.Event{
		Timestamp:  "2020-01-01T00:00:00Z",
		SessionID:  "session",
		GraphMode:  "graph",
		Tool:       "find_references",
		DurationMs: 42,
		ResultSize: 3,
		Truncated:  true,
		Generation: 7,
		Err:        "boom",
		Extra:      map[string]any{"key": "value"},
	}
	logger.Log(context.Background(), ev)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	spans := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != ev.Tool || span.Status.Code != codes.Error || span.Status.Description != ev.Err {
		t.Fatalf("span identity/status = name %q status %+v", span.Name, span.Status)
	}
	attrs := attributesByName(span.Attributes)
	for key, want := range map[string]string{
		"session_id": ev.SessionID,
		"graph_mode": ev.GraphMode,
		"tool":       ev.Tool,
		"ts":         ev.Timestamp,
		"err":        ev.Err,
		"extra.key":  "value",
	} {
		if got := requireAttribute(t, attrs, key).AsString(); got != want {
			t.Errorf("attribute %s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]int64{
		"duration_ms": int64(ev.DurationMs),
		"result_size": int64(ev.ResultSize),
		"generation":  int64(ev.Generation),
	} {
		if got := requireAttribute(t, attrs, key).AsInt64(); got != want {
			t.Errorf("attribute %s = %d, want %d", key, got, want)
		}
	}
	for key, want := range map[string]bool{"truncated": ev.Truncated, "stale": ev.Stale} {
		if got := requireAttribute(t, attrs, key).AsBool(); got != want {
			t.Errorf("attribute %s = %t, want %t", key, got, want)
		}
	}
	if stats := logger.Stats(); stats.Accepted != 1 || stats.Exported != 1 || stats.Pending != 0 {
		t.Fatalf("OTEL stats = %+v", stats)
	}
}

func TestOTELLoggerDefaultsEmptyToolIdentity(t *testing.T) {
	exporter := &capturingExporter{}
	logger := testOTEL(t, exporter, nil)
	logger.Log(context.Background(), core.Event{})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	if got := spans[0].Name; got != "unknown_tool" {
		t.Fatalf("span name = %q, want unknown_tool", got)
	}
	if got := requireAttribute(t, attributesByName(spans[0].Attributes), "tool").AsString(); got != "unknown_tool" {
		t.Fatalf("tool attribute = %q, want unknown_tool", got)
	}
}

func TestOTELLoggerObservableSaturationDoesNotBlock(t *testing.T) {
	exporter := &controlledExporter{exportStarted: make(chan struct{}), exportGate: make(chan struct{})}
	gate := exporter.exportGate
	logger := testOTEL(t, exporter, nil)
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()

	logger.Log(context.Background(), core.Event{Tool: "first"})
	waitFor(t, exporter.exportStarted, "first OTEL export")
	logger.Log(context.Background(), core.Event{Tool: "second"})
	started := time.Now()
	logger.Log(context.Background(), core.Event{Tool: "dropped"})
	if time.Since(started) > time.Second {
		t.Fatal("saturated OTEL Log blocked")
	}
	stats := logger.Stats()
	if stats.Accepted != 2 || stats.SaturationDrops != 1 || stats.Pending != 2 {
		t.Fatalf("saturation stats = %+v", stats)
	}
	close(gate)
	if err := logger.Close(); err == nil || !stringsContain(err.Error(), "saturation") {
		t.Fatalf("Close error = %v, want observable saturation loss", err)
	}
}

func TestOTELExporterFailureIsDiagnosedAndReturned(t *testing.T) {
	root := errors.New("collector failed")
	diagnostic := make(chan struct{})
	exporter := &controlledExporter{exportStarted: make(chan struct{}), exportErr: root}
	logger := testOTEL(t, exporter, func(opts *otelOptions) {
		opts.diagnostic = func(error) {
			select {
			case <-diagnostic:
			default:
				close(diagnostic)
			}
		}
	})
	logger.Log(context.Background(), core.Event{Tool: "failed"})
	waitFor(t, exporter.exportStarted, "failing export")
	waitFor(t, diagnostic, "export diagnostic")
	if err := logger.Close(); !errors.Is(err, root) {
		t.Fatalf("Close error = %v, want exporter root", err)
	}
	stats := logger.Stats()
	if stats.ExportFailures != 1 || stats.Undelivered != 1 || stats.Pending != 0 {
		t.Fatalf("failure stats = %+v", stats)
	}
}

func TestOTELCloseIsBoundedStableAndAdjudicatesPending(t *testing.T) {
	exporter := &controlledExporter{
		exportStarted: make(chan struct{}),
		exportGate:    make(chan struct{}),
		shutdownDone:  make(chan struct{}),
		ignoreContext: true,
	}
	logger := testOTEL(t, exporter, func(opts *otelOptions) { opts.shutdownTimeout = 30 * time.Millisecond })
	logger.Log(context.Background(), core.Event{Tool: "blocked"})
	waitFor(t, exporter.exportStarted, "blocked exporter")

	started := time.Now()
	first := logger.Close()
	if time.Since(started) > time.Second || first == nil || !stringsContain(first.Error(), "timeout") {
		t.Fatalf("Close = %v after %s, want bounded timeout", first, time.Since(started))
	}
	second := logger.Close()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("repeated Close = %v, want stable %v", second, first)
	}
	if stats := logger.Stats(); stats.ShutdownTimeouts != 1 || stats.Undelivered != 1 || stats.Pending != 0 {
		t.Fatalf("timeout stats = %+v", stats)
	}
	close(exporter.exportGate)
	waitFor(t, exporter.shutdownDone, "exporter shutdown after release")
}

func TestOTELSnapshotFailureUsesStableSinkOwnedRepresentation(t *testing.T) {
	exporter := &capturingExporter{}
	otel := testOTEL(t, exporter, nil)
	logger := &defaultingLogger{inner: Tee(otel)}

	logger.Log(context.Background(), core.Event{Tool: "unsupported", Extra: map[string]any{"bad": func() {}}})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	spans := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	attrs := attributesByName(spans[0].Attributes)
	const want = "snapshot_error: json: unsupported type: func()"
	if got := requireAttribute(t, attrs, "extra.bad").AsString(); got != want {
		t.Fatalf("OTEL unsupported value = %q, want %q", got, want)
	}
	if stats := otel.Stats(); stats.Accepted != 1 || stats.Exported != 1 {
		t.Fatalf("OTEL stats = %+v", stats)
	}
}

func TestDefaultingLoggerSnapshotsAndTimestampsBeforeFanout(t *testing.T) {
	a := &fakeLogger{}
	b := &fakeLogger{}
	logger := &defaultingLogger{inner: Tee(a, b), sessionID: "default-session", graphMode: "graph"}
	numbers := []int{1}
	labels := map[string]string{"state": "before"}
	extra := map[string]any{"nested": []any{"before"}, "numbers": numbers, "labels": labels}
	logger.Log(context.Background(), core.Event{Tool: "snapshot", Extra: extra})
	extra["nested"].([]any)[0] = "after"
	numbers[0] = 2
	labels["state"] = "after"

	first := a.snapshot()[0]
	second := b.snapshot()[0]
	if first.Timestamp == "" || first.Timestamp != second.Timestamp {
		t.Fatalf("fanout timestamps = %q and %q", first.Timestamp, second.Timestamp)
	}
	if first.SessionID != "default-session" || first.GraphMode != "graph" {
		t.Fatalf("defaults not applied: %+v", first)
	}
	if got := first.Extra["nested"].([]any)[0]; got != "before" {
		t.Fatalf("fanout nested snapshot mutated to %v", got)
	}
	if got := marshalJSON(t, first.Extra["numbers"].([]any)[0]); got != "1" {
		t.Fatalf("fanout typed-slice snapshot mutated to %s", got)
	}
	if got := first.Extra["labels"].(map[string]any)["state"]; got != "before" {
		t.Fatalf("fanout typed-map snapshot mutated to %v", got)
	}
}

type gatedCloseLogger struct {
	started  chan struct{}
	deadline chan time.Time
	gate     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	calls    int
}

func (l *gatedCloseLogger) Log(context.Context, core.Event) {}
func (l *gatedCloseLogger) Close() error {
	return errors.New("direct Close called instead of shared-deadline close")
}
func (l *gatedCloseLogger) closeBy(deadline time.Time) error {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	l.deadline <- deadline
	l.once.Do(func() { close(l.started) })
	<-l.gate
	return nil
}
func (l *gatedCloseLogger) closeCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func TestTeeClosesSinksConcurrently(t *testing.T) {
	gate := make(chan struct{})
	a := &gatedCloseLogger{started: make(chan struct{}), deadline: make(chan time.Time, 1), gate: gate}
	b := &gatedCloseLogger{started: make(chan struct{}), deadline: make(chan time.Time, 1), gate: gate}
	tee := Tee(a, b)
	done := make(chan error, 1)
	go func() { done <- tee.Close() }()
	waitFor(t, a.started, "first sink close")
	waitFor(t, b.started, "second sink close")
	firstDeadline, secondDeadline := <-a.deadline, <-b.deadline
	if !firstDeadline.Equal(secondDeadline) {
		t.Fatalf("sink deadlines differ: %s != %s", firstDeadline, secondDeadline)
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tee.Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Tee.Close did not finish")
	}
	if err := tee.Close(); err != nil {
		t.Fatalf("repeated Tee.Close: %v", err)
	}
	if a.closeCalls() != 1 || b.closeCalls() != 1 {
		t.Fatalf("repeated composite close invoked sinks again: a=%d b=%d", a.closeCalls(), b.closeCalls())
	}
}

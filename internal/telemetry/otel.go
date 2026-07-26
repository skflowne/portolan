package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/skflowne/portolan/internal/core"
)

const (
	tracerName               = "github.com/skflowne/portolan/internal/telemetry"
	defaultOTELQueueCapacity = 2048
	defaultOTELBatchSize     = 512
	defaultOTELBatchTimeout  = 5 * time.Second
	defaultOTELExportTimeout = time.Second
)

type otelOptions struct {
	queueCapacity      int
	batchSize          int
	batchTimeout       time.Duration
	exportTimeout      time.Duration
	shutdownTimeout    time.Duration
	diagnosticCapacity int
	diagnostic         func(error)
}

func defaultOTELOptions() otelOptions {
	return otelOptions{
		queueCapacity:      defaultOTELQueueCapacity,
		batchSize:          defaultOTELBatchSize,
		batchTimeout:       defaultOTELBatchTimeout,
		exportTimeout:      defaultOTELExportTimeout,
		shutdownTimeout:    defaultTelemetryCloseTimeout,
		diagnosticCapacity: defaultDiagnosticCapacity,
	}
}

// OTELStats is a snapshot of mirror admission, export, and lifecycle accounting.
type OTELStats struct {
	Accepted           uint64
	Exported           uint64
	Pending            uint64
	SaturationDrops    uint64
	PostCloseDrops     uint64
	ExportFailures     uint64
	Undelivered        uint64
	ShutdownTimeouts   uint64
	DiagnosticDrops    uint64
	DiagnosticPanics   uint64
	DiagnosticTimeouts uint64
}

type otelCounters struct {
	accepted         atomic.Uint64
	exported         atomic.Uint64
	pending          atomic.Uint64
	saturationDrops  atomic.Uint64
	postCloseDrops   atomic.Uint64
	exportFailures   atomic.Uint64
	undelivered      atomic.Uint64
	shutdownTimeouts atomic.Uint64
}

// OTELLogger mirrors Events as spans through an explicit exporter. Its token
// layer bounds all not-yet-adjudicated spans, including a batch currently held
// by the exporter, while the official BatchSpanProcessor owns export cadence.
type OTELLogger struct {
	provider  *sdktrace.TracerProvider
	processor sdktrace.SpanProcessor
	tracer    oteltrace.Tracer
	tokens    chan struct{}
	diag      *diagnosticDispatcher
	opts      otelOptions

	admitMu     sync.RWMutex
	closed      bool
	closedState atomic.Bool

	accountMu sync.Mutex
	abandoned bool

	errMu       sync.Mutex
	exportErr   error
	shutdownErr error

	closeOnce   sync.Once
	closeResult error

	counters otelCounters
}

type observedExporter struct {
	inner sdktrace.SpanExporter
	owner *OTELLogger
}

// NewOTEL builds a bounded OTEL mirror around an explicit exporter. There is
// deliberately no nil or stdout fallback.
func NewOTEL(exporter sdktrace.SpanExporter, diagnostic ...func(error)) (*OTELLogger, error) {
	opts := defaultOTELOptions()
	if len(diagnostic) > 0 {
		opts.diagnostic = diagnostic[0]
	}
	return newOTELLogger(exporter, opts)
}

func newOTELLogger(exporter sdktrace.SpanExporter, opts otelOptions) (*OTELLogger, error) {
	if exporter == nil {
		return nil, errors.New("telemetry: OTEL exporter is required")
	}
	defaults := defaultOTELOptions()
	if opts.queueCapacity <= 0 {
		opts.queueCapacity = defaults.queueCapacity
	}
	if opts.batchSize <= 0 || opts.batchSize > opts.queueCapacity {
		opts.batchSize = min(defaults.batchSize, opts.queueCapacity)
	}
	if opts.batchTimeout <= 0 {
		opts.batchTimeout = defaults.batchTimeout
	}
	if opts.exportTimeout <= 0 {
		opts.exportTimeout = defaults.exportTimeout
	}
	if opts.shutdownTimeout <= 0 {
		opts.shutdownTimeout = defaults.shutdownTimeout
	}
	if opts.diagnosticCapacity <= 0 {
		opts.diagnosticCapacity = defaults.diagnosticCapacity
	}

	logger := &OTELLogger{
		tokens: make(chan struct{}, opts.queueCapacity),
		diag:   newDiagnosticDispatcher(opts.diagnosticCapacity, opts.diagnostic),
		opts:   opts,
	}
	processor := sdktrace.NewBatchSpanProcessor(
		&observedExporter{inner: exporter, owner: logger},
		sdktrace.WithMaxQueueSize(opts.queueCapacity),
		sdktrace.WithMaxExportBatchSize(opts.batchSize),
		sdktrace.WithBatchTimeout(opts.batchTimeout),
		sdktrace.WithExportTimeout(opts.exportTimeout),
	)
	logger.processor = processor
	logger.provider = sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(processor),
	)
	logger.tracer = logger.provider.Tracer(tracerName)
	return logger, nil
}

// Log creates and ends one synthetic span. OnEnd only enqueues into the SDK
// processor; exporter latency never runs on this caller.
func (o *OTELLogger) Log(ctx context.Context, ev core.Event) {
	if o.closedState.Load() {
		o.counters.postCloseDrops.Add(1)
		o.diag.report(fmt.Errorf("telemetry: OTEL log after close tool=%q", ev.Tool))
		return
	}

	o.admitMu.RLock()
	defer o.admitMu.RUnlock()
	if o.closed {
		o.counters.postCloseDrops.Add(1)
		o.diag.report(fmt.Errorf("telemetry: OTEL log after close tool=%q", ev.Tool))
		return
	}
	select {
	case o.tokens <- struct{}{}:
		o.counters.accepted.Add(1)
		o.counters.pending.Add(1)
	default:
		o.counters.saturationDrops.Add(1)
		o.diag.report(fmt.Errorf("telemetry: OTEL queue saturated tool=%q", ev.Tool))
		return
	}

	name := ev.Tool
	if name == "" {
		name = "unknown_tool"
	}
	_, span := o.tracer.Start(ctx, name)
	attrs := []attribute.KeyValue{
		attribute.String("session_id", ev.SessionID),
		attribute.String("graph_mode", ev.GraphMode),
		attribute.String("tool", ev.Tool),
		attribute.Int64("duration_ms", ev.DurationMs),
		attribute.Int("result_size", ev.ResultSize),
		attribute.Bool("truncated", ev.Truncated),
		attribute.Bool("stale", ev.Stale),
		attribute.Int64("generation", int64(ev.Generation)),
	}
	if ev.Timestamp != "" {
		attrs = append(attrs, attribute.String("ts", ev.Timestamp))
	}
	if ev.Err != "" {
		attrs = append(attrs, attribute.String("err", ev.Err))
		span.SetStatus(codes.Error, ev.Err)
	}
	for key, value := range ev.Extra {
		attrs = append(attrs, attribute.String("extra."+key, toString(value)))
	}
	span.SetAttributes(attrs...)
	span.End()
}

func (e *observedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.inner.ExportSpans(ctx, spans)
	e.owner.finishExport(len(spans), err)
	// The owner has already latched, counted, and dispatched the final exporter
	// failure. Returning it would make BatchSpanProcessor call the global OTEL
	// error handler, adding an unbounded duplicate stderr path.
	return nil
}

func (e *observedExporter) Shutdown(ctx context.Context) error {
	err := e.inner.Shutdown(ctx)
	if err != nil {
		e.owner.errMu.Lock()
		if e.owner.shutdownErr == nil {
			e.owner.shutdownErr = fmt.Errorf("telemetry: OTEL exporter shutdown: %w", err)
		}
		e.owner.errMu.Unlock()
		e.owner.diag.report(fmt.Errorf("telemetry: OTEL exporter shutdown failed: %w", err))
	}
	return err
}

func (o *OTELLogger) finishExport(count int, exportErr error) {
	var diagnostic error
	o.accountMu.Lock()
	if o.abandoned {
		o.releaseTokens(count)
		o.accountMu.Unlock()
		return
	}
	if exportErr != nil {
		o.counters.exportFailures.Add(1)
		o.counters.undelivered.Add(uint64(count))
		o.errMu.Lock()
		if o.exportErr == nil {
			o.exportErr = fmt.Errorf("telemetry: OTEL export: %w", exportErr)
		}
		o.errMu.Unlock()
		diagnostic = fmt.Errorf("telemetry: OTEL export failed: %w", exportErr)
	} else {
		o.counters.exported.Add(uint64(count))
	}
	o.counters.pending.Add(^uint64(count - 1))
	o.releaseTokens(count)
	o.accountMu.Unlock()
	o.diag.report(diagnostic)
}

func (o *OTELLogger) releaseTokens(count int) {
	for range count {
		<-o.tokens
	}
}

func (o *OTELLogger) Close() error {
	return o.closeBy(time.Now().Add(o.opts.shutdownTimeout))
}

func (o *OTELLogger) closeBy(deadline time.Time) error {
	o.closeOnce.Do(func() {
		o.closedState.Store(true)
		o.admitMu.Lock()
		o.closed = true
		o.admitMu.Unlock()

		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		flushErr := o.processor.ForceFlush(ctx)
		// Call the processor directly: unlike TracerProvider.Shutdown, it still
		// initiates exporter shutdown when the shared context is already expired.
		shutdownErr := o.processor.Shutdown(ctx)
		cancel()

		o.accountMu.Lock()
		if pending := o.counters.pending.Load(); pending > 0 {
			o.abandoned = true
			o.counters.pending.Store(0)
			o.counters.undelivered.Add(pending)
		}
		if errors.Is(flushErr, context.DeadlineExceeded) || errors.Is(shutdownErr, context.DeadlineExceeded) || time.Now().After(deadline) {
			o.counters.shutdownTimeouts.Add(1)
		}
		o.accountMu.Unlock()

		diagnosticErr := o.diag.closeBy(deadline)
		o.errMu.Lock()
		exportErr, exporterShutdownErr := o.exportErr, o.shutdownErr
		o.errMu.Unlock()
		stats := o.Stats()
		var errs []error
		errs = append(errs, exportErr, exporterShutdownErr, flushErr, shutdownErr)
		if lost := stats.SaturationDrops + stats.PostCloseDrops + stats.Undelivered; lost > 0 {
			errs = append(errs, fmt.Errorf("telemetry: OTEL mirror lost %d spans (saturation=%d post_close=%d undelivered=%d)",
				lost, stats.SaturationDrops, stats.PostCloseDrops, stats.Undelivered))
		}
		if stats.ShutdownTimeouts > 0 {
			errs = append(errs, fmt.Errorf("telemetry: OTEL shutdown timeout"))
		}
		errs = append(errs, diagnosticErr)
		o.closeResult = errors.Join(errs...)
	})
	return o.closeResult
}

// Stats returns a race-safe accounting snapshot.
func (o *OTELLogger) Stats() OTELStats {
	diagnostic := o.diag.stats()
	return OTELStats{
		Accepted:           o.counters.accepted.Load(),
		Exported:           o.counters.exported.Load(),
		Pending:            o.counters.pending.Load(),
		SaturationDrops:    o.counters.saturationDrops.Load(),
		PostCloseDrops:     o.counters.postCloseDrops.Load(),
		ExportFailures:     o.counters.exportFailures.Load(),
		Undelivered:        o.counters.undelivered.Load(),
		ShutdownTimeouts:   o.counters.shutdownTimeouts.Load(),
		DiagnosticDrops:    diagnostic.drops,
		DiagnosticPanics:   diagnostic.panics,
		DiagnosticTimeouts: diagnostic.timeouts,
	}
}

var _ core.Logger = (*OTELLogger)(nil)
var _ sdktrace.SpanExporter = (*observedExporter)(nil)

func toString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

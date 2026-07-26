// Package telemetry is the daemon's telemetry spine: a bounded JSONL event
// logger and an optional OTEL mirror, both implementing core.Logger.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

const (
	defaultJSONLCapacity         = 512
	defaultAdmissionTimeout      = 50 * time.Millisecond
	defaultTelemetryCloseTimeout = 2 * time.Second
	defaultMaxRecordBytes        = 16 * 1024
	defaultDiagnosticCapacity    = 64
	minimumRecordBytes           = 512
)

type jsonlSink interface {
	io.Writer
	Sync() error
	Close() error
}

type jsonlOptions struct {
	capacity           int
	admissionTimeout   time.Duration
	shutdownTimeout    time.Duration
	maxRecordBytes     int
	diagnosticCapacity int
	diagnostic         func(error)
	admissionWait      func()
}

func defaultJSONLOptions() jsonlOptions {
	return jsonlOptions{
		capacity:           defaultJSONLCapacity,
		admissionTimeout:   defaultAdmissionTimeout,
		shutdownTimeout:    defaultTelemetryCloseTimeout,
		maxRecordBytes:     defaultMaxRecordBytes,
		diagnosticCapacity: defaultDiagnosticCapacity,
	}
}

type jsonlRecord struct {
	line []byte
}

// JSONLLogger admits immutable records to a bounded FIFO. One worker owns all
// writes, so JSONL lines never interleave. A slot remains occupied while its
// record is in the queue or held by the writer.
type JSONLLogger struct {
	sink jsonlSink
	opts jsonlOptions
	diag *diagnosticDispatcher

	queue         chan jsonlRecord
	slots         chan struct{}
	admissionStop chan struct{}
	failureStop   chan struct{}
	failureOnce   sync.Once

	admitMu     sync.RWMutex
	closed      bool
	closedState atomic.Bool
	failed      atomic.Bool

	workerDone chan struct{}
	accountMu  sync.Mutex
	abandoned  atomic.Bool

	errMu    sync.Mutex
	writeErr error
	syncErr  error
	closeErr error

	sinkCloseOnce sync.Once
	sinkCloseDone chan struct{}

	closeOnce   sync.Once
	closeResult error

	counters jsonlCounters
}

// NewJSONL opens path for append. Parent directories are created as needed.
func NewJSONL(path string, diagnostic ...func(error)) (*JSONLLogger, error) {
	opts := defaultJSONLOptions()
	if len(diagnostic) > 0 {
		opts.diagnostic = diagnostic[0]
	}
	return newJSONL(path, opts)
}

func newJSONL(path string, opts jsonlOptions) (*JSONLLogger, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("telemetry: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open %s: %w", path, err)
	}
	return newJSONLLogger(f, opts), nil
}

func newJSONLLogger(sink jsonlSink, opts jsonlOptions) *JSONLLogger {
	defaults := defaultJSONLOptions()
	if opts.capacity <= 0 {
		opts.capacity = defaults.capacity
	}
	if opts.admissionTimeout <= 0 {
		opts.admissionTimeout = defaults.admissionTimeout
	}
	if opts.shutdownTimeout <= 0 {
		opts.shutdownTimeout = defaults.shutdownTimeout
	}
	if opts.maxRecordBytes < minimumRecordBytes {
		opts.maxRecordBytes = defaults.maxRecordBytes
	}
	if opts.diagnosticCapacity <= 0 {
		opts.diagnosticCapacity = defaults.diagnosticCapacity
	}
	l := &JSONLLogger{
		sink:          sink,
		opts:          opts,
		diag:          newDiagnosticDispatcher(opts.diagnosticCapacity, opts.diagnostic),
		queue:         make(chan jsonlRecord, opts.capacity),
		slots:         make(chan struct{}, opts.capacity),
		admissionStop: make(chan struct{}),
		failureStop:   make(chan struct{}),
		workerDone:    make(chan struct{}),
		sinkCloseDone: make(chan struct{}),
	}
	go l.runWriter()
	return l
}

// Log snapshots and admits ev. Capacity is tried before ctx so a pre-canceled
// completed call is still recorded when a slot is immediately available.
func (l *JSONLLogger) Log(ctx context.Context, ev core.Event) {
	l.logSnapshot(ctx, snapshotEvent(ev))
}

func (l *JSONLLogger) logSnapshot(ctx context.Context, snapshot eventSnapshot) {
	ev := snapshot.event
	if l.closedState.Load() {
		l.counters.postCloseDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: log after close tool=%q", ev.Tool))
		return
	}
	if l.failed.Load() {
		l.counters.sinkFailureDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: event dropped after sink failure tool=%q", ev.Tool))
		return
	}

	line, oversize, fallback := encodeJSONLRecord(snapshot, l.opts.maxRecordBytes)
	if oversize {
		l.counters.oversizeRecords.Add(1)
		l.diag.report(fmt.Errorf("telemetry: capped oversize JSONL event tool=%q", ev.Tool))
	}
	if fallback {
		l.counters.marshalFallbacks.Add(1)
		l.diag.report(fmt.Errorf("telemetry: replaced unencodable JSONL event tool=%q", ev.Tool))
	}

	l.admitMu.RLock()
	defer l.admitMu.RUnlock()
	if l.closed {
		l.counters.postCloseDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: log after close tool=%q", ev.Tool))
		return
	}
	if l.failed.Load() {
		l.counters.sinkFailureDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: event dropped after sink failure tool=%q", ev.Tool))
		return
	}

	if !l.trySlot(ctx) {
		return
	}
	if l.closedState.Load() {
		<-l.slots
		l.counters.postCloseDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission stopped by close tool=%q", ev.Tool))
		return
	}
	if l.failed.Load() {
		<-l.slots
		l.counters.sinkFailureDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission stopped by sink failure tool=%q", ev.Tool))
		return
	}
	l.counters.accepted.Add(1)
	l.counters.pending.Add(1)
	l.queue <- jsonlRecord{line: line}
}

func (l *JSONLLogger) trySlot(ctx context.Context) bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
	}

	if err := ctx.Err(); err != nil {
		l.counters.canceledDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission canceled: %w", err))
		return false
	}
	if l.opts.admissionWait != nil {
		l.opts.admissionWait()
	}

	timer := time.NewTimer(l.opts.admissionTimeout)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return true
	case <-l.admissionStop:
		l.counters.postCloseDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission stopped by close"))
		return false
	case <-l.failureStop:
		l.counters.sinkFailureDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission stopped by sink failure"))
		return false
	case <-ctx.Done():
		l.counters.canceledDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: admission canceled: %w", ctx.Err()))
		return false
	case <-timer.C:
		l.counters.queueTimeoutDrops.Add(1)
		l.diag.report(fmt.Errorf("telemetry: JSONL queue full after %s", l.opts.admissionTimeout))
		return false
	}
}

func (l *JSONLLogger) runWriter() {
	defer close(l.workerDone)
	for record := range l.queue {
		if l.abandoned.Load() {
			l.releaseAbandoned()
			continue
		}
		if l.failed.Load() {
			l.finishRecord(false)
			continue
		}

		err := writeAll(l.sink, record.line)
		l.finishWrite(err)
	}

	if l.abandoned.Load() {
		return
	}
	if err := l.sink.Sync(); err != nil {
		l.errMu.Lock()
		l.syncErr = fmt.Errorf("telemetry: JSONL sync: %w", err)
		l.errMu.Unlock()
		l.diag.report(fmt.Errorf("telemetry: JSONL sync failed: %w", err))
	}
	if l.abandoned.Load() {
		return
	}
	if err := l.closeSink(); err != nil {
		l.diag.report(fmt.Errorf("telemetry: JSONL close failed: %w", err))
	}
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return fmt.Errorf("invalid write count %d for %d bytes", n, len(p))
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

var _ core.Logger = (*JSONLLogger)(nil)

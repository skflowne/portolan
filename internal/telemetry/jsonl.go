// Package telemetry is the daemon's telemetry spine: a bounded JSONL event
// logger and an optional OTEL mirror, both implementing core.Logger.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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

// JSONLStats is a snapshot of JSONL admission, delivery, and lifecycle
// accounting. Accepted records are classified as Written or
// UndeliveredAccepted once no longer pending.
type JSONLStats struct {
	Accepted            uint64
	Written             uint64
	Pending             uint64
	QueueTimeoutDrops   uint64
	CanceledDrops       uint64
	OversizeRecords     uint64
	MarshalFallbacks    uint64
	PostCloseDrops      uint64
	SinkFailureDrops    uint64
	UndeliveredAccepted uint64
	ShutdownTimeouts    uint64
	DiagnosticDrops     uint64
	DiagnosticPanics    uint64
	DiagnosticTimeouts  uint64
}

type jsonlCounters struct {
	accepted            atomic.Uint64
	written             atomic.Uint64
	pending             atomic.Uint64
	queueTimeoutDrops   atomic.Uint64
	canceledDrops       atomic.Uint64
	oversizeRecords     atomic.Uint64
	marshalFallbacks    atomic.Uint64
	postCloseDrops      atomic.Uint64
	sinkFailureDrops    atomic.Uint64
	undeliveredAccepted atomic.Uint64
	shutdownTimeouts    atomic.Uint64
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

	queue chan jsonlRecord
	slots chan struct{}

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
func NewJSONL(path string) (*JSONLLogger, error) {
	return newJSONL(path, defaultJSONLOptions())
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
		workerDone:    make(chan struct{}),
		sinkCloseDone: make(chan struct{}),
	}
	go l.runWriter()
	return l
}

// Log snapshots and admits ev. Capacity is tried before ctx so a pre-canceled
// completed call is still recorded when a slot is immediately available.
func (l *JSONLLogger) Log(ctx context.Context, ev core.Event) {
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

	line, oversize, fallback := encodeJSONLRecord(ev, l.opts.maxRecordBytes)
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

	timer := time.NewTimer(l.opts.admissionTimeout)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return true
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

func (l *JSONLLogger) finishWrite(writeErr error) {
	var diagnostic error
	l.accountMu.Lock()
	if l.abandoned.Load() {
		<-l.slots
		l.accountMu.Unlock()
		return
	}
	if writeErr != nil {
		l.errMu.Lock()
		if l.writeErr == nil {
			l.writeErr = fmt.Errorf("telemetry: JSONL write: %w", writeErr)
		}
		l.errMu.Unlock()
		l.failed.Store(true)
		l.counters.undeliveredAccepted.Add(1)
		diagnostic = fmt.Errorf("telemetry: JSONL sink failed: %w", writeErr)
	} else {
		l.counters.written.Add(1)
	}
	l.counters.pending.Add(^uint64(0))
	<-l.slots
	l.accountMu.Unlock()
	l.diag.report(diagnostic)
}

func (l *JSONLLogger) finishRecord(written bool) {
	l.accountMu.Lock()
	if !l.abandoned.Load() {
		if written {
			l.counters.written.Add(1)
		} else {
			l.counters.undeliveredAccepted.Add(1)
		}
		l.counters.pending.Add(^uint64(0))
	}
	<-l.slots
	l.accountMu.Unlock()
}

func (l *JSONLLogger) releaseAbandoned() {
	l.finishRecord(false)
}

// Close stops admission, drains accepted events, fsyncs, and closes the sink.
// The complete lifecycle is bounded and every caller receives one stable result.
func (l *JSONLLogger) Close() error {
	return l.closeBy(time.Now().Add(l.opts.shutdownTimeout))
}

func (l *JSONLLogger) closeBy(deadline time.Time) error {
	l.closeOnce.Do(func() {
		l.admitMu.Lock()
		l.closed = true
		l.closedState.Store(true)
		close(l.queue)
		l.admitMu.Unlock()

		if !waitUntil(l.workerDone, deadline) {
			// Give a completion racing the timer one final deterministic chance.
			select {
			case <-l.workerDone:
			default:
				l.accountMu.Lock()
				l.abandoned.Store(true)
				remaining := l.counters.pending.Swap(0)
				l.counters.undeliveredAccepted.Add(remaining)
				l.accountMu.Unlock()
				l.counters.shutdownTimeouts.Add(1)
				l.diag.report(fmt.Errorf("telemetry: JSONL shutdown timeout with %d accepted events undelivered", remaining))
				go func() { _ = l.closeSink() }()
			}
		}

		diagnosticErr := l.diag.closeBy(deadline)
		l.closeResult = l.buildCloseError(diagnosticErr)
	})
	return l.closeResult
}

func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	select {
	case <-done:
		return true
	default:
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (l *JSONLLogger) closeSink() error {
	l.sinkCloseOnce.Do(func() {
		l.errMu.Lock()
		err := l.sink.Close()
		if l.closeErr == nil && err != nil {
			l.closeErr = fmt.Errorf("telemetry: JSONL close: %w", err)
		}
		l.errMu.Unlock()
		close(l.sinkCloseDone)
	})
	<-l.sinkCloseDone
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.closeErr
}

func (l *JSONLLogger) buildCloseError(diagnosticErr error) error {
	l.errMu.Lock()
	writeErr, syncErr, closeErr := l.writeErr, l.syncErr, l.closeErr
	l.errMu.Unlock()
	stats := l.Stats()
	var errs []error
	errs = append(errs, writeErr, syncErr, closeErr)
	if lost := stats.QueueTimeoutDrops + stats.CanceledDrops + stats.PostCloseDrops + stats.SinkFailureDrops + stats.UndeliveredAccepted; lost > 0 {
		errs = append(errs, fmt.Errorf("telemetry: %d events lost (queue=%d canceled=%d post_close=%d sink_failure=%d undelivered_accepted=%d)",
			lost, stats.QueueTimeoutDrops, stats.CanceledDrops, stats.PostCloseDrops, stats.SinkFailureDrops, stats.UndeliveredAccepted))
	}
	if stats.ShutdownTimeouts > 0 {
		errs = append(errs, fmt.Errorf("telemetry: JSONL shutdown timeout"))
	}
	errs = append(errs, diagnosticErr)
	return errors.Join(errs...)
}

// Stats returns a race-safe snapshot of logger accounting.
func (l *JSONLLogger) Stats() JSONLStats {
	diagnostic := l.diag.stats()
	return JSONLStats{
		Accepted:            l.counters.accepted.Load(),
		Written:             l.counters.written.Load(),
		Pending:             l.counters.pending.Load(),
		QueueTimeoutDrops:   l.counters.queueTimeoutDrops.Load(),
		CanceledDrops:       l.counters.canceledDrops.Load(),
		OversizeRecords:     l.counters.oversizeRecords.Load(),
		MarshalFallbacks:    l.counters.marshalFallbacks.Load(),
		PostCloseDrops:      l.counters.postCloseDrops.Load(),
		SinkFailureDrops:    l.counters.sinkFailureDrops.Load(),
		UndeliveredAccepted: l.counters.undeliveredAccepted.Load(),
		ShutdownTimeouts:    l.counters.shutdownTimeouts.Load(),
		DiagnosticDrops:     diagnostic.drops,
		DiagnosticPanics:    diagnostic.panics,
		DiagnosticTimeouts:  diagnostic.timeouts,
	}
}

func encodeJSONLRecord(ev core.Event, maxBytes int) (line []byte, oversize, fallback bool) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(ev)
	if err != nil {
		fallback = true
		ev.Extra = map[string]any{
			"telemetry_encode_error": boundedString(err.Error(), 256),
		}
		encoded, _ = json.Marshal(ev)
	}
	encoded = append(encoded, '\n')
	if len(encoded) <= maxBytes {
		return encoded, false, fallback
	}

	oversize = true
	originalSize := len(encoded)
	ev.Extra = map[string]any{
		"telemetry_oversize":      true,
		"telemetry_original_size": originalSize,
	}
	encoded = marshalLine(ev)
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	// Err is not a correlation field. Keep a recognizable prefix and a stable
	// digest so repeated failures remain joinable without retaining unbounded text.
	ev.Err = boundedString(ev.Err, maxBytes/4)
	encoded = marshalLine(ev)
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	// Pathological correlation strings cannot all remain byte-for-byte exact
	// under a finite record cap. Retain prefixes plus hashes and diagnose the
	// oversize transformation through the same counter/callback as above.
	for len(encoded) > maxBytes {
		changed := false
		for _, field := range []*string{&ev.SessionID, &ev.Tool, &ev.GraphMode, &ev.Timestamp, &ev.Err} {
			if len(*field) > 96 {
				*field = boundedString(*field, max(96, len(*field)/2))
				changed = true
			}
		}
		encoded = marshalLine(ev)
		if !changed {
			break
		}
	}
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	// minimumRecordBytes guarantees this final valid Event fits.
	ev.Extra = nil
	ev.Timestamp = boundedString(ev.Timestamp, 80)
	ev.SessionID = boundedString(ev.SessionID, 80)
	ev.GraphMode = boundedString(ev.GraphMode, 80)
	ev.Tool = boundedString(ev.Tool, 80)
	ev.Err = boundedString(ev.Err, 80)
	return marshalLine(ev), true, fallback
}

func marshalLine(ev core.Event) []byte {
	encoded, _ := json.Marshal(ev)
	return append(encoded, '\n')
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := "…sha256:" + hex.EncodeToString(sum[:])
	if limit <= len(suffix) {
		digest := hex.EncodeToString(sum[:])
		return digest[:min(len(digest), max(1, limit))]
	}
	prefix := value[:limit-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

var _ core.Logger = (*JSONLLogger)(nil)

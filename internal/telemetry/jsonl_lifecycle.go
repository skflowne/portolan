package telemetry

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

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
		l.failureOnce.Do(func() { close(l.failureStop) })
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
		l.closedState.Store(true)
		close(l.admissionStop)
		l.admitMu.Lock()
		l.closed = true
		close(l.queue)
		l.admitMu.Unlock()

		if _, completed := receiveByDeadline(l.workerDone, deadline); !completed {
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

func (l *JSONLLogger) closeSink() error {
	l.sinkCloseOnce.Do(func() {
		err := l.sink.Close()
		l.errMu.Lock()
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

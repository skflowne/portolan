package telemetry

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type diagnosticStats struct {
	drops    uint64
	panics   uint64
	timeouts uint64
}

// diagnosticDispatcher keeps stderr or other diagnostic I/O off telemetry
// callers. A stalled callback can consume one worker and a bounded queue, but
// it cannot block event admission.
type diagnosticDispatcher struct {
	callback func(error)
	queue    chan error
	done     chan struct{}

	mu     sync.RWMutex
	closed bool

	drops    atomic.Uint64
	panics   atomic.Uint64
	timeouts atomic.Uint64

	closeOnce sync.Once
	closeErr  error
}

func newDiagnosticDispatcher(capacity int, callback func(error)) *diagnosticDispatcher {
	if callback == nil {
		return nil
	}
	if capacity <= 0 {
		capacity = defaultDiagnosticCapacity
	}
	d := &diagnosticDispatcher{
		callback: callback,
		queue:    make(chan error, capacity),
		done:     make(chan struct{}),
	}
	go d.run()
	return d
}

func (d *diagnosticDispatcher) run() {
	defer close(d.done)
	for err := range d.queue {
		if callDiagnostic(d.callback, err) {
			d.panics.Add(1)
		}
	}
}

func callDiagnostic(callback func(error), err error) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	callback(err)
	return false
}

func (d *diagnosticDispatcher) report(err error) {
	if d == nil || err == nil {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.drops.Add(1)
		return
	}
	select {
	case d.queue <- err:
	default:
		d.drops.Add(1)
	}
}

func (d *diagnosticDispatcher) closeBy(deadline time.Time) error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.queue)
		d.mu.Unlock()

		select {
		case <-d.done:
		default:
			remaining := time.Until(deadline)
			if remaining <= 0 {
				d.timeouts.Add(1)
			} else {
				timer := time.NewTimer(remaining)
				select {
				case <-d.done:
					if !timer.Stop() {
						<-timer.C
					}
				case <-timer.C:
					d.timeouts.Add(1)
				}
			}
		}

		stats := d.stats()
		var errs []error
		if stats.drops > 0 {
			errs = append(errs, fmt.Errorf("telemetry: %d diagnostics dropped", stats.drops))
		}
		if stats.panics > 0 {
			errs = append(errs, fmt.Errorf("telemetry: diagnostic callback panicked %d times", stats.panics))
		}
		if stats.timeouts > 0 {
			errs = append(errs, fmt.Errorf("telemetry: diagnostic shutdown timeout"))
		}
		d.closeErr = errors.Join(errs...)
	})
	return d.closeErr
}

func (d *diagnosticDispatcher) stats() diagnosticStats {
	if d == nil {
		return diagnosticStats{}
	}
	return diagnosticStats{
		drops:    d.drops.Load(),
		panics:   d.panics.Load(),
		timeouts: d.timeouts.Load(),
	}
}

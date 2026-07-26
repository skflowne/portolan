package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

type deadlineCloser interface {
	closeBy(time.Time) error
}

// teeLogger fans one Event out to every wrapped Logger.
type teeLogger struct {
	loggers []core.Logger

	closeOnce   sync.Once
	closeResult error
}

// Tee returns a logger that forwards to every non-nil sink.
func Tee(loggers ...core.Logger) core.Logger {
	filtered := make([]core.Logger, 0, len(loggers))
	for _, logger := range loggers {
		if logger != nil {
			filtered = append(filtered, logger)
		}
	}
	return &teeLogger{loggers: filtered}
}

func (t *teeLogger) Log(ctx context.Context, ev core.Event) {
	for _, logger := range t.loggers {
		logger.Log(ctx, ev)
	}
}

// Close starts all sink shutdowns together. Owned telemetry sinks receive one
// shared absolute deadline; an unknown sink is still prevented from hanging
// the composite indefinitely.
func (t *teeLogger) Close() error {
	t.closeOnce.Do(func() {
		deadline := time.Now().Add(defaultTelemetryCloseTimeout)
		results := make(chan error, len(t.loggers))
		for _, logger := range t.loggers {
			go func() {
				if closer, ok := logger.(deadlineCloser); ok {
					results <- closer.closeBy(deadline)
					return
				}
				results <- logger.Close()
			}()
		}

		var errs []error
		for range t.loggers {
			if !time.Now().Before(deadline) {
				errs = append(errs, fmt.Errorf("telemetry: composite shutdown timeout"))
				break
			}
			err, completed := receiveByDeadline(results, deadline)
			if !completed {
				errs = append(errs, fmt.Errorf("telemetry: composite shutdown timeout"))
				break
			}
			errs = append(errs, err)
			if time.Now().After(deadline) {
				break
			}
		}
		t.closeResult = errors.Join(errs...)
	})
	return t.closeResult
}

var _ core.Logger = (*teeLogger)(nil)

package telemetry

import (
	"context"
	"errors"

	"github.com/skflowne/portolan/internal/core"
)

// teeLogger fans one Event out to every wrapped Logger.
type teeLogger struct {
	loggers []core.Logger
}

// Tee returns a core.Logger that forwards every Log call to all of loggers,
// and whose Close closes all of them (collecting any errors). Nil loggers
// are skipped.
func Tee(loggers ...core.Logger) core.Logger {
	filtered := make([]core.Logger, 0, len(loggers))
	for _, l := range loggers {
		if l != nil {
			filtered = append(filtered, l)
		}
	}
	return &teeLogger{loggers: filtered}
}

// Log forwards ev to every wrapped logger.
func (t *teeLogger) Log(ctx context.Context, ev core.Event) {
	for _, l := range t.loggers {
		l.Log(ctx, ev)
	}
}

// Close closes every wrapped logger, returning a joined error if any fail.
func (t *teeLogger) Close() error {
	var errs []error
	for _, l := range t.loggers {
		if err := l.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ core.Logger = (*teeLogger)(nil)

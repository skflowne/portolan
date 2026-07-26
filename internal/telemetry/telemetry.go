package telemetry

import (
	"context"

	"github.com/skflowne/portolan/internal/core"
)

// FromConfig builds the daemon's default telemetry spine: a JSONLLogger
// writing to cfg.JSONLPath, teed with an OTEL mirror (stdout exporter for
// Phase 0). The returned Logger also stamps SessionID/GraphMode from cfg onto
// any Event that leaves those fields empty, so call sites don't all need to
// thread cfg through.
func FromConfig(cfg core.Config) (core.Logger, error) {
	jsonl, err := NewJSONL(cfg.JSONLPath)
	if err != nil {
		return nil, err
	}

	otelLog, err := NewOTEL(nil)
	if err != nil {
		_ = jsonl.Close()
		return nil, err
	}

	return &defaultingLogger{
		inner:     Tee(jsonl, otelLog),
		sessionID: cfg.SessionID,
		graphMode: cfg.GraphMode,
	}, nil
}

// defaultingLogger fills in SessionID/GraphMode from the Config default
// before forwarding to the wrapped Logger, whenever the Event leaves them
// empty (call sites are still free to set them explicitly per-event).
type defaultingLogger struct {
	inner     core.Logger
	sessionID string
	graphMode string
}

func (d *defaultingLogger) Log(ctx context.Context, ev core.Event) {
	if ev.SessionID == "" {
		ev.SessionID = d.sessionID
	}
	if ev.GraphMode == "" {
		ev.GraphMode = d.graphMode
	}
	d.inner.Log(ctx, ev)
}

func (d *defaultingLogger) Close() error {
	return d.inner.Close()
}

var _ core.Logger = (*defaultingLogger)(nil)

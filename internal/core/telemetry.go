package core

import "context"

// Event records one tool invocation. SessionID and GraphMode correlate events
// with the caller and its configured graph mode.
//
// Timestamp is filled in by the Logger implementation (do not set it at the
// call site) so all records share one clock.
type Event struct {
	Timestamp  string         `json:"ts"`
	SessionID  string         `json:"session_id"`
	GraphMode  string         `json:"graph_mode"` // "graph" | "no-graph"
	Tool       string         `json:"tool"`
	DurationMs int64          `json:"duration_ms"`
	ResultSize int            `json:"result_size"` // count of items returned
	Truncated  bool           `json:"truncated"`
	Stale      bool           `json:"stale"`
	Generation uint64         `json:"generation"`
	Err        string         `json:"err,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// Logger is the telemetry sink. Log must be non-blocking enough to sit on the
// hot path of every tool call; implementations buffer/async as needed. Log must
// be safe for concurrent use.
type Logger interface {
	Log(ctx context.Context, ev Event)
	Close() error
}

// NopLogger satisfies Logger while discarding every event.
type NopLogger struct{}

func (NopLogger) Log(context.Context, Event) {}
func (NopLogger) Close() error               { return nil }

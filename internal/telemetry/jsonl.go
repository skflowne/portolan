// Package telemetry is the daemon's telemetry spine: a JSONL event logger and
// an OTEL mirror, both implementing core.Logger, plus a fan-out Tee so both
// sinks can run side by side.
package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

// JSONLLogger appends one JSON object per line to a file. Safe for concurrent
// use: writes are serialized behind a mutex so lines are never interleaved
// and no event is lost.
type JSONLLogger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// NewJSONL opens (creating if necessary) path for append and returns a
// core.Logger that writes one JSON object per line to it. Parent directories
// are created as needed.
func NewJSONL(path string) (*JSONLLogger, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("telemetry: mkdir %s: %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open %s: %w", path, err)
	}

	return &JSONLLogger{
		file: f,
		enc:  json.NewEncoder(f),
	}, nil
}

// Log writes ev as one JSON line. If ev.Timestamp is empty it is stamped with
// the current time (RFC3339Nano, UTC).
func (l *JSONLLogger) Log(_ context.Context, ev core.Event) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Best-effort: Log has no error return on the core.Logger interface, so a
	// write failure here has nowhere to go. Encoding errors on a well-formed
	// Event are not expected; disk-full/IO errors are silently dropped, which
	// matches the "must not block/panic the hot path" contract.
	_ = l.enc.Encode(ev)
}

// Close flushes (there is no in-memory buffer beyond the OS/file layer, so
// this is effectively a sync+close) and closes the underlying file.
func (l *JSONLLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	syncErr := l.file.Sync()
	closeErr := l.file.Close()
	l.file = nil
	if closeErr != nil {
		return closeErr
	}
	return syncErr
}

var _ core.Logger = (*JSONLLogger)(nil)

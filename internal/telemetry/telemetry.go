package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"

	"github.com/skflowne/portolan/internal/core"
)

const otlpStartupTimeout = time.Second

// FromConfig builds the authoritative JSONL logger and, only when a standard
// OTLP endpoint is explicitly configured, an independent OTLP/HTTP mirror.
// There is no localhost or stdout fallback.
func FromConfig(cfg core.Config, diagnostic ...func(error)) (core.Logger, error) {
	var report func(error)
	if len(diagnostic) > 0 {
		report = diagnostic[0]
	}
	jsonlOpts := defaultJSONLOptions()
	jsonlOpts.diagnostic = report
	jsonl, err := newJSONL(cfg.JSONLPath, jsonlOpts)
	if err != nil {
		return nil, err
	}

	loggers := []core.Logger{jsonl}
	if otlpEndpointConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), otlpStartupTimeout)
		exporter, exporterErr := otlptracehttp.New(
			ctx,
			otlptracehttp.WithTimeout(defaultOTELExportTimeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
				Enabled:         true,
				InitialInterval: 100 * time.Millisecond,
				MaxInterval:     250 * time.Millisecond,
				MaxElapsedTime:  defaultOTELExportTimeout,
			}),
		)
		cancel()
		if exporterErr != nil {
			return nil, errors.Join(
				fmt.Errorf("telemetry: create OTLP/HTTP exporter: %w", exporterErr),
				jsonl.Close(),
			)
		}
		otelLogger, otelErr := NewOTEL(exporter, report)
		if otelErr != nil {
			return nil, errors.Join(
				otelErr,
				exporter.Shutdown(context.Background()),
				jsonl.Close(),
			)
		}
		loggers = append(loggers, otelLogger)
	}

	return &defaultingLogger{
		inner:     Tee(loggers...),
		sessionID: cfg.SessionID,
		graphMode: cfg.GraphMode,
	}, nil
}

func otlpEndpointConfigured() bool {
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// defaultingLogger owns the immutable pre-fanout Event snapshot. Both JSONL
// and OTEL therefore observe one timestamp and caller mutation after Log
// returns cannot alter either sink's record.
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
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	ev.Extra = snapshotExtra(ev.Extra)
	d.inner.Log(ctx, ev)
}

func snapshotExtra(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	encoded, err := json.Marshal(extra)
	if err == nil {
		var snapshot map[string]any
		if json.Unmarshal(encoded, &snapshot) == nil {
			return snapshot
		}
	}
	// Unsupported values still get detached from the caller's map and remain
	// observable as strings rather than making asynchronous ownership unclear.
	snapshot := make(map[string]any, len(extra))
	for key, value := range extra {
		snapshot[key] = toString(value)
	}
	return snapshot
}

func (d *defaultingLogger) Close() error {
	return d.inner.Close()
}

var _ core.Logger = (*defaultingLogger)(nil)

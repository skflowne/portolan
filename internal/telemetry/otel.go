package telemetry

import (
	"context"
	"fmt"
	"io"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/skflowne/portolan/internal/core"
)

// tracerName is the instrumentation-scope name registered with the
// TracerProvider for every span the OTEL mirror emits.
const tracerName = "github.com/skflowne/portolan/internal/telemetry"

// otelLogger mirrors Events as OTEL spans: one span per Log call, named after
// the tool, with the Event fields attached as attributes.
type otelLogger struct {
	provider *sdktrace.TracerProvider
	tracer   oteltrace.Tracer
}

// NewOTEL builds a TracerProvider backed by the stdout span exporter and
// returns a core.Logger that emits one span per Log call. Passing w == nil
// defaults the exporter to os.Stdout (via stdouttrace's own default).
func NewOTEL(w io.Writer) (core.Logger, error) {
	opts := []stdouttrace.Option{}
	if w != nil {
		opts = append(opts, stdouttrace.WithWriter(w))
	}

	exp, err := stdouttrace.New(opts...)
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
	)

	return &otelLogger{
		provider: provider,
		tracer:   provider.Tracer(tracerName),
	}, nil
}

// Log emits a single span named after ev.Tool, tagged with the Event fields
// as attributes, and immediately ends it (Events describe already-completed
// tool calls, so the span's lifetime is synthetic: it exists to carry
// attributes through the OTEL pipeline).
func (o *otelLogger) Log(ctx context.Context, ev core.Event) {
	name := ev.Tool
	if name == "" {
		name = "unknown_tool"
	}

	_, span := o.tracer.Start(ctx, name)
	defer span.End()

	attrs := []attribute.KeyValue{
		attribute.String("session_id", ev.SessionID),
		attribute.String("graph_mode", ev.GraphMode),
		attribute.String("tool", ev.Tool),
		attribute.Int64("duration_ms", ev.DurationMs),
		attribute.Int("result_size", ev.ResultSize),
		attribute.Bool("truncated", ev.Truncated),
		attribute.Bool("stale", ev.Stale),
		attribute.Int64("generation", int64(ev.Generation)),
	}
	if ev.Timestamp != "" {
		attrs = append(attrs, attribute.String("ts", ev.Timestamp))
	}
	if ev.Err != "" {
		attrs = append(attrs, attribute.String("err", ev.Err))
		span.SetStatus(codes.Error, ev.Err)
	}
	for k, v := range ev.Extra {
		attrs = append(attrs, attribute.String("extra."+k, toString(v)))
	}

	span.SetAttributes(attrs...)
}

// Close shuts down the TracerProvider, flushing any buffered spans.
func (o *otelLogger) Close() error {
	return o.provider.Shutdown(context.Background())
}

var _ core.Logger = (*otelLogger)(nil)

// toString renders an arbitrary Extra value as a string attribute. OTEL
// attributes are typed, but Extra is `map[string]any` from arbitrary call
// sites, so we fall back to fmt-style stringification via a type switch to
// avoid pulling in reflection-heavy formatting on the hot path for the common
// cases.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

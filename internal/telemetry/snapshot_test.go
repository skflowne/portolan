package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestExtraSnapshotDetachesSupportedNestedValues(t *testing.T) {
	numbers := []int{1}
	labels := map[string]string{"state": "before"}
	extra := map[string]any{
		"nested":  []any{"before"},
		"numbers": numbers,
		"labels":  labels,
	}

	snapshot := snapshotExtra(extra)
	extra["nested"].([]any)[0] = "after"
	numbers[0] = 2
	labels["state"] = "after"

	values := snapshot.values()
	if got := values["nested"].([]any)[0]; got != "before" {
		t.Fatalf("nested snapshot = %v, want before", got)
	}
	if got := marshalJSON(t, values["numbers"].([]any)[0]); got != "1" {
		t.Fatalf("typed slice snapshot = %s, want 1", got)
	}
	if got := values["labels"].(map[string]any)["state"]; got != "before" {
		t.Fatalf("typed map snapshot = %v, want before", got)
	}
}

func TestExtraSnapshotPreservesIntegerJSONRepresentation(t *testing.T) {
	signed := map[string]int64{"min": -9223372036854775808}
	unsigned := []uint64{18446744073709551615}
	extra := map[string]any{"signed": signed, "unsigned": unsigned}
	const want = `{"signed":{"min":-9223372036854775808},"unsigned":[18446744073709551615]}`

	snapshot := snapshotExtra(extra)
	signed["min"] = 0
	unsigned[0] = 0
	if got := marshalJSON(t, snapshot.values()); got != want {
		t.Fatalf("snapshot JSON = %s, want %s", got, want)
	}

	captured := &fakeLogger{}
	dispatchSnapshot(context.Background(), captured, eventSnapshot{event: core.Event{Tool: "boundaries"}, extra: snapshot})
	if got := marshalJSON(t, captured.snapshot()[0].Extra); got != want {
		t.Fatalf("generic sink JSON = %s, want %s", got, want)
	}

	sink := &controlledSink{}
	logger := testJSONL(sink, nil)
	logger.logSnapshot(context.Background(), eventSnapshot{event: core.Event{Tool: "boundaries"}, extra: snapshot})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	writes, _, _ := sink.snapshot()
	if len(writes) != 1 {
		t.Fatalf("JSONL writes = %d, want 1", len(writes))
	}
	var record struct {
		Extra json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(writes[0]), &record); err != nil {
		t.Fatalf("decode JSONL record: %v", err)
	}
	if got := string(record.Extra); got != want {
		t.Fatalf("JSONL Extra = %s, want %s", got, want)
	}
}

func TestExtraSnapshotPreservesNilAndEmpty(t *testing.T) {
	if values := snapshotExtra(nil).values(); values != nil {
		t.Fatalf("nil Extra snapshot = %#v, want nil", values)
	}
	if values := snapshotExtra(map[string]any{}).values(); values == nil || len(values) != 0 {
		t.Fatalf("empty Extra snapshot = %#v, want non-nil empty map", values)
	}
}

func TestExtraSnapshotReportsUnsupportedValueWithoutRetainingIt(t *testing.T) {
	snapshot := snapshotExtra(map[string]any{"bad": func() {}})
	reason, failed := snapshot.failure("bad")
	if !failed || reason != "json: unsupported type: func()" {
		t.Fatalf("unsupported snapshot failure = %q, %t", reason, failed)
	}
	if containsReflectKind(reflect.ValueOf(snapshot), reflect.Func) {
		t.Fatal("snapshot retained a function value")
	}
}

func TestExtraSnapshotReportsCycleWithoutRetainingIt(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	snapshot := snapshotExtra(map[string]any{"cycle": cycle})
	reason, failed := snapshot.failure("cycle")
	if !failed || !stringsContain(reason, "encountered a cycle") {
		t.Fatalf("cyclic snapshot failure = %q, %t", reason, failed)
	}
	cycle["state"] = "after"
	if _, retained := snapshot.values()["cycle"]; retained {
		t.Fatal("cyclic snapshot retained caller data")
	}
}

func TestComposedSinksApplyIndependentSnapshotFailurePolicies(t *testing.T) {
	jsonSink := &controlledSink{}
	jsonl := testJSONL(jsonSink, nil)
	exporter := &capturingExporter{}
	otel := testOTEL(t, exporter, nil)
	logger := Tee(jsonl, otel)

	logger.Log(context.Background(), core.Event{Tool: "unsupported", Extra: map[string]any{"bad": func() {}}})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writes, _, _ := jsonSink.snapshot()
	if len(writes) != 1 {
		t.Fatalf("JSONL writes = %d, want 1", len(writes))
	}
	var event core.Event
	if err := json.Unmarshal(bytes.TrimSpace(writes[0]), &event); err != nil {
		t.Fatalf("decode JSONL fallback: %v", err)
	}
	if _, ok := event.Extra["telemetry_encode_error"]; !ok {
		t.Fatalf("JSONL fallback Extra = %#v", event.Extra)
	}
	if stats := jsonl.Stats(); stats.MarshalFallbacks != 1 {
		t.Fatalf("JSONL stats = %+v", stats)
	}

	spans := exporter.snapshot()
	if len(spans) != 1 {
		t.Fatalf("OTEL spans = %d, want 1", len(spans))
	}
	attrs := attributesByName(spans[0].Attributes)
	if got := attrs["extra.bad"].AsString(); got != "snapshot_error: json: unsupported type: func()" {
		t.Fatalf("OTEL unsupported value = %q", got)
	}
}

func marshalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(encoded)
}

func containsReflectKind(value reflect.Value, target reflect.Kind) bool {
	if !value.IsValid() {
		return false
	}
	if value.Kind() == target {
		return true
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		return !value.IsNil() && containsReflectKind(value.Elem(), target)
	case reflect.Map:
		for _, key := range value.MapKeys() {
			if containsReflectKind(key, target) || containsReflectKind(value.MapIndex(key), target) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if containsReflectKind(value.Index(i), target) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if containsReflectKind(value.Field(i), target) {
				return true
			}
		}
	}
	return false
}

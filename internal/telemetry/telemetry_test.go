package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

// fakeLogger is an in-memory core.Logger for testing Tee fan-out.
type fakeLogger struct {
	mu     sync.Mutex
	events []core.Event
	closed bool
}

func (f *fakeLogger) Log(_ context.Context, ev core.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeLogger) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeLogger) snapshot() []core.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Event, len(f.events))
	copy(out, f.events)
	return out
}

func TestJSONLLogger_ConcurrentWritesAllLand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "events.jsonl")

	logger, err := NewJSONL(path)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			logger.Log(context.Background(), core.Event{
				SessionID:  "sess-1",
				GraphMode:  "graph",
				Tool:       "find_refs",
				DurationMs: int64(i),
				ResultSize: i,
			})
		}(i)
	}
	wg.Wait()

	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open written file: %v", err)
	}
	defer f.Close()

	var lines []core.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev core.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		lines = append(lines, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}

	for _, ev := range lines {
		if ev.Timestamp == "" {
			t.Fatalf("event has empty timestamp: %+v", ev)
		}
		if _, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err != nil {
			t.Fatalf("timestamp %q not RFC3339Nano: %v", ev.Timestamp, err)
		}
		if ev.SessionID != "sess-1" {
			t.Fatalf("wrong session_id: %+v", ev)
		}
		if ev.GraphMode != "graph" {
			t.Fatalf("wrong graph_mode: %+v", ev)
		}
		if ev.Tool != "find_refs" {
			t.Fatalf("wrong tool: %+v", ev)
		}
	}
}

func TestJSONLLogger_FillsTimestampOnlyWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	logger, err := NewJSONL(path)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}

	const explicitTs = "2020-01-01T00:00:00Z"
	logger.Log(context.Background(), core.Event{Tool: "a", Timestamp: explicitTs})
	logger.Log(context.Background(), core.Event{Tool: "b"})

	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	var first, second core.Event
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if err := json.Unmarshal(lines[1], &second); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}

	if first.Timestamp != explicitTs {
		t.Fatalf("explicit timestamp got overwritten: %q", first.Timestamp)
	}
	if second.Timestamp == "" {
		t.Fatalf("empty timestamp was not filled in")
	}
}

func TestTee_FansOutToAllLoggers(t *testing.T) {
	a := &fakeLogger{}
	b := &fakeLogger{}

	tee := Tee(a, b)

	ev := core.Event{Tool: "goto_def", SessionID: "s1", GraphMode: "no-graph"}
	tee.Log(context.Background(), ev)

	for name, l := range map[string]*fakeLogger{"a": a, "b": b} {
		got := l.snapshot()
		if len(got) != 1 {
			t.Fatalf("logger %s: got %d events, want 1", name, len(got))
		}
		if got[0].Tool != "goto_def" || got[0].SessionID != "s1" {
			t.Fatalf("logger %s: wrong event: %+v", name, got[0])
		}
	}

	if err := tee.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !a.closed || !b.closed {
		t.Fatalf("Tee.Close did not close all wrapped loggers: a=%v b=%v", a.closed, b.closed)
	}
}

func TestTee_SkipsNilLoggers(t *testing.T) {
	a := &fakeLogger{}
	tee := Tee(a, nil)

	tee.Log(context.Background(), core.Event{Tool: "x"})
	if len(a.snapshot()) != 1 {
		t.Fatalf("expected event to reach the non-nil logger")
	}
	if err := tee.Close(); err != nil {
		t.Fatalf("Close with nil logger present: %v", err)
	}
}

func TestOTELLogger_LogAndClose(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewOTEL(&buf)
	if err != nil {
		t.Fatalf("NewOTEL: %v", err)
	}

	logger.Log(context.Background(), core.Event{
		SessionID:  "sess-otel",
		GraphMode:  "graph",
		Tool:       "find_impls",
		DurationMs: 42,
		ResultSize: 3,
		Truncated:  true,
		Stale:      false,
		Generation: 7,
		Err:        "boom",
		Extra:      map[string]any{"k": "v"},
	})

	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatalf("expected the stdout exporter to have written span output")
	}
	if !bytes.Contains(buf.Bytes(), []byte("find_impls")) {
		t.Fatalf("exported span output missing tool name, got: %s", buf.String())
	}
}

func TestOTELLogger_DefaultsToolNameWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewOTEL(&buf)
	if err != nil {
		t.Fatalf("NewOTEL: %v", err)
	}
	defer logger.Close()

	// Must not panic on an empty Tool.
	logger.Log(context.Background(), core.Event{})
}

func TestFromConfig_WritesJSONLAndTeesOTEL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	logger, err := FromConfig(core.Config{
		JSONLPath: path,
		SessionID: "default-session",
		GraphMode: "graph",
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	// Leave SessionID/GraphMode empty to exercise the defaulting behavior.
	logger.Log(context.Background(), core.Event{Tool: "find_refs"})

	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	var ev core.Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.SessionID != "default-session" {
		t.Fatalf("expected default session_id to be stamped, got %+v", ev)
	}
	if ev.GraphMode != "graph" {
		t.Fatalf("expected default graph_mode to be stamped, got %+v", ev)
	}
}

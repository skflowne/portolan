package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

type snapshotLogger interface {
	logSnapshot(context.Context, eventSnapshot)
}

type eventSnapshot struct {
	event core.Event
	extra extraSnapshot
}

type extraSnapshot struct {
	present bool
	entries map[string]extraSnapshotEntry
}

type extraSnapshotEntry struct {
	value   any
	failure string
}

type snapshotFailure struct {
	Reason string
}

func snapshotEvent(ev core.Event) eventSnapshot {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	snapshot := eventSnapshot{
		event: ev,
		extra: snapshotExtra(ev.Extra),
	}
	snapshot.event.Extra = nil
	return snapshot
}

func snapshotExtra(extra map[string]any) extraSnapshot {
	snapshot := extraSnapshot{present: extra != nil}
	if extra == nil {
		return snapshot
	}
	snapshot.entries = make(map[string]extraSnapshotEntry, len(extra))
	for key, value := range extra {
		encoded, err := json.Marshal(value)
		if err != nil {
			snapshot.entries[key] = extraSnapshotEntry{failure: err.Error()}
			continue
		}
		var detached any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&detached); err != nil {
			snapshot.entries[key] = extraSnapshotEntry{failure: err.Error()}
			continue
		}
		snapshot.entries[key] = extraSnapshotEntry{value: detached}
	}
	return snapshot
}

func (s extraSnapshot) values() map[string]any {
	if !s.present {
		return nil
	}
	values := make(map[string]any, len(s.entries))
	for key, entry := range s.entries {
		if entry.failure == "" {
			values[key] = cloneSnapshotValue(entry.value)
		}
	}
	return values
}

func (s extraSnapshot) failure(key string) (string, bool) {
	entry, ok := s.entries[key]
	return entry.failure, ok && entry.failure != ""
}

func (s extraSnapshot) firstFailure() (string, bool) {
	for _, key := range s.keys() {
		if reason, failed := s.failure(key); failed {
			return reason, true
		}
	}
	return "", false
}

func (s extraSnapshot) keys() []string {
	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s extraSnapshot) value(key string) (any, string) {
	entry := s.entries[key]
	return cloneSnapshotValue(entry.value), entry.failure
}

func (s eventSnapshot) eventValue() core.Event {
	ev := s.event
	if !s.extra.present {
		return ev
	}
	ev.Extra = make(map[string]any, len(s.extra.entries))
	for key, entry := range s.extra.entries {
		if entry.failure != "" {
			ev.Extra[key] = snapshotFailure{Reason: entry.failure}
			continue
		}
		ev.Extra[key] = cloneSnapshotValue(entry.value)
	}
	return ev
}

func dispatchSnapshot(ctx context.Context, logger core.Logger, snapshot eventSnapshot) {
	if logger, ok := logger.(snapshotLogger); ok {
		logger.logSnapshot(ctx, snapshot)
		return
	}
	logger.Log(ctx, snapshot.eventValue())
}

func cloneSnapshotValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(value))
		for key, item := range value {
			clone[key] = cloneSnapshotValue(item)
		}
		return clone
	case []any:
		clone := make([]any, len(value))
		for i, item := range value {
			clone[i] = cloneSnapshotValue(item)
		}
		return clone
	default:
		return value
	}
}

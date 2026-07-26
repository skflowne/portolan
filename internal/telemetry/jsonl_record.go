package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/skflowne/portolan/internal/core"
)

func encodeJSONLRecord(ev core.Event, maxBytes int) (line []byte, oversize, fallback bool) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(ev)
	if err != nil {
		fallback = true
		ev.Extra = map[string]any{
			"telemetry_encode_error": boundedString(err.Error(), 256),
		}
		encoded, _ = json.Marshal(ev)
	}
	encoded = append(encoded, '\n')
	if len(encoded) <= maxBytes {
		return encoded, false, fallback
	}

	oversize = true
	originalSize := len(encoded)
	ev.Extra = map[string]any{
		"telemetry_oversize":      true,
		"telemetry_original_size": originalSize,
	}
	encoded = marshalLine(ev)
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	originals := []string{ev.SessionID, ev.Tool, ev.GraphMode, ev.Timestamp, ev.Err}
	limits := []int{len(ev.SessionID), len(ev.Tool), len(ev.GraphMode), len(ev.Timestamp), len(ev.Err)}

	// Err is not a correlation field. Keep a recognizable prefix and a stable
	// digest so repeated failures remain joinable without retaining unbounded text.
	limits[4] = maxBytes / 4
	ev.Err = boundedString(originals[4], limits[4])
	encoded = marshalLine(ev)
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	// Pathological correlation strings cannot all remain byte-for-byte exact
	// under a finite record cap. Retain prefixes plus hashes and diagnose the
	// oversize transformation through the same counter/callback as above.
	fields := []*string{&ev.SessionID, &ev.Tool, &ev.GraphMode, &ev.Timestamp, &ev.Err}
	for len(encoded) > maxBytes {
		changed := false
		for i, field := range fields {
			if limits[i] > 96 {
				limits[i] = max(96, limits[i]/2)
				*field = boundedString(originals[i], limits[i])
				changed = true
			}
		}
		encoded = marshalLine(ev)
		if !changed {
			break
		}
	}
	if len(encoded) <= maxBytes {
		return encoded, true, fallback
	}

	// minimumRecordBytes guarantees this final valid Event fits.
	ev.Extra = nil
	ev.SessionID = boundedString(originals[0], 80)
	ev.Tool = boundedString(originals[1], 80)
	ev.GraphMode = boundedString(originals[2], 80)
	ev.Timestamp = boundedString(originals[3], 80)
	ev.Err = boundedString(originals[4], 80)
	return marshalLine(ev), true, fallback
}

func marshalLine(ev core.Event) []byte {
	encoded, _ := json.Marshal(ev)
	return append(encoded, '\n')
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := "…sha256:" + hex.EncodeToString(sum[:])
	if limit <= len(suffix) {
		digest := hex.EncodeToString(sum[:])
		return digest[:min(len(digest), max(1, limit))]
	}
	prefix := value[:limit-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

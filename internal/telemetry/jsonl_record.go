package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/skflowne/portolan/internal/core"
)

func encodeJSONLRecord(snapshot eventSnapshot, maxBytes int) (line []byte, oversize, fallback bool) {
	ev := snapshot.event
	failure, snapshotFailed := snapshot.extra.firstFailure()
	if snapshotFailed {
		fallback = true
		ev.Extra = map[string]any{
			"telemetry_encode_error": boundedString(failure, 256),
		}
	} else {
		ev.Extra = snapshot.extra.values()
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

	// Drop optional metadata, then size correlation fields against their encoded
	// representation because JSON escaping can expand retained source bytes.
	// minimumRecordBytes guarantees convergence before every field reaches one byte.
	ev.Extra = nil
	for {
		encoded = marshalLine(ev)
		if len(encoded) <= maxBytes {
			return encoded, true, fallback
		}
		for i, field := range fields {
			if limits[i] > 1 {
				limits[i] = max(1, limits[i]/2)
				*field = boundedString(originals[i], limits[i])
			}
		}
	}
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

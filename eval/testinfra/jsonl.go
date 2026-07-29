package testinfra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// WaitForQuiescentJSONL waits for exactly expected newline-terminated records
// and requires the valid byte snapshot to remain unchanged for quiet.
func WaitForQuiescentJSONL(path string, expected int, timeout, quiet time.Duration) ([]map[string]any, error) {
	deadline := time.Now().Add(timeout)
	var stable []byte
	var stableSince time.Time
	var candidateObserved bool
	lastState := "file not created"
	for {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading JSONL %s: %w", path, err)
		}
		if err == nil {
			records, complete, parseErr := parseJSONLSnapshot(data)
			if parseErr != nil {
				return nil, fmt.Errorf("parsing JSONL %s: %w", path, parseErr)
			}
			switch {
			case !complete:
				lastState = "unterminated final record"
				candidateObserved = false
				stable = nil
			case len(records) > expected:
				return nil, fmt.Errorf("JSONL %s has %d records, want exactly %d", path, len(records), expected)
			case len(records) < expected:
				lastState = fmt.Sprintf("%d records, want exactly %d", len(records), expected)
				candidateObserved = false
				stable = nil
			default:
				if !candidateObserved || !bytes.Equal(stable, data) {
					stable = append(stable[:0], data...)
					stableSince = time.Now()
					candidateObserved = true
				} else if time.Since(stableSince) >= quiet {
					return records, nil
				}
				lastState = "valid records not yet quiescent"
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("waiting for quiescent JSONL %s: %s", path, lastState)
		}
		wait := min(PollInterval, time.Until(deadline))
		timer := time.NewTimer(wait)
		<-timer.C
	}
}

func parseJSONLSnapshot(data []byte) ([]map[string]any, bool, error) {
	complete := len(data) == 0 || data[len(data)-1] == '\n'
	parts := bytes.Split(data, []byte{'\n'})
	if complete {
		parts = parts[:len(parts)-1]
	} else {
		parts = parts[:len(parts)-1]
	}
	records := make([]map[string]any, 0, len(parts))
	for i, line := range parts {
		if len(line) == 0 {
			return nil, complete, fmt.Errorf("record %d is empty", i+1)
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, complete, fmt.Errorf("record %d: %w", i+1, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, complete, fmt.Errorf("record %d has trailing JSON value", i+1)
			}
			return nil, complete, fmt.Errorf("record %d has trailing data: %w", i+1, err)
		}
		record, ok := value.(map[string]any)
		if !ok {
			return nil, complete, fmt.Errorf("record %d must be an object", i+1)
		}
		records = append(records, record)
	}
	return records, complete, nil
}

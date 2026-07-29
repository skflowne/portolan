package testinfra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJSONLSnapshot(t *testing.T) {
	tests := []struct {
		name         string
		data         string
		wantRecords  int
		wantComplete bool
		wantErr      string
	}{
		{name: "complete", data: "{\"tool\":\"one\"}\n{\"tool\":\"two\"}\n", wantRecords: 2, wantComplete: true},
		{name: "partial final", data: "{\"tool\":\"one\"}\n{\"tool\":", wantRecords: 1},
		{name: "valid unterminated final", data: "{\"tool\":\"one\"}", wantComplete: false},
		{name: "malformed terminated final", data: "{\"tool\":\n", wantErr: "record 1"},
		{name: "malformed middle", data: "{\"tool\":\n{\"tool\":\"two\"}\n", wantErr: "record 1"},
		{name: "concatenated values", data: "{\"tool\":\"one\"}{\"tool\":\"two\"}\n", wantErr: "trailing JSON value"},
		{name: "blank record", data: "{\"tool\":\"one\"}\n\n", wantErr: "record 2 is empty"},
		{name: "non-object", data: "[]\n", wantErr: "must be an object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, complete, err := parseJSONLSnapshot([]byte(tt.data))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJSONLSnapshot: %v", err)
			}
			if len(got) != tt.wantRecords || complete != tt.wantComplete {
				t.Fatalf("records, complete = %d, %t, want %d, %t", len(got), complete, tt.wantRecords, tt.wantComplete)
			}
		})
	}
}

func TestWaitForQuiescentJSONL(t *testing.T) {
	const quiet = 40 * time.Millisecond
	t.Run("empty snapshot waits and exposes late record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(path, []byte("{\"tool\":\"late\"}\n"), 0o600)
		}()
		_, err := WaitForQuiescentJSONL(path, 0, time.Second, quiet)
		if err == nil || !strings.Contains(err.Error(), "1 records, want exactly 0") {
			t.Fatalf("error = %v, want late record rejection", err)
		}
	})

	t.Run("partial final becomes complete", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte("{\"tool\":\"one\"}\n{\"tool\":"), 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(path, []byte("{\"tool\":\"one\"}\n{\"tool\":\"two\"}\n"), 0o600)
		}()
		records, err := WaitForQuiescentJSONL(path, 2, time.Second, quiet)
		if err != nil || len(records) != 2 {
			t.Fatalf("records = %v, error = %v", records, err)
		}
	})

	t.Run("append resets quiescence and exposes duplicate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte("{\"tool\":\"one\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				_, _ = f.WriteString("{\"tool\":\"duplicate\"}\n")
				_ = f.Close()
			}
		}()
		_, err := WaitForQuiescentJSONL(path, 1, time.Second, quiet)
		if err == nil || !strings.Contains(err.Error(), "2 records, want exactly 1") {
			t.Fatalf("error = %v, want late duplicate rejection", err)
		}
	})

	t.Run("permanent partial final times out", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte("{\"tool\":\"one\"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := WaitForQuiescentJSONL(path, 1, 60*time.Millisecond, 10*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "unterminated final record") {
			t.Fatalf("error = %v, want unterminated timeout", err)
		}
	})

	t.Run("malformed committed record fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte("{\"tool\":\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := WaitForQuiescentJSONL(path, 1, time.Second, quiet)
		if err == nil || !strings.Contains(err.Error(), "record 1") {
			t.Fatalf("error = %v, want malformed record rejection", err)
		}
	})
}

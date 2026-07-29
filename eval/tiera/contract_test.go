package tiera

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/tools"
)

type pinnedContract struct {
	DefinitionTotalArea tools.FindDefinitionOutput `json:"definition_total_area"`
	ReferencesCircle    tools.FindReferencesOutput `json:"references_circle"`
	OutlineGeometry     tools.GetOutlineOutput     `json:"outline_geometry"`
	OutlineMain         tools.GetOutlineOutput     `json:"outline_main"`
	OutlineInvalidFile  tools.GetOutlineOutput     `json:"outline_invalid_file"`
	Telemetry           []expectedEvent            `json:"telemetry"`
}

type expectedEvent struct {
	Tool       string `json:"tool"`
	SessionID  string `json:"session_id"`
	GraphMode  string `json:"graph_mode"`
	ResultSize int    `json:"result_size"`
	Truncated  bool   `json:"truncated"`
	Generation uint64 `json:"generation"`
	Stale      bool   `json:"stale"`
	Err        string `json:"err,omitempty"`
}

func loadPinnedContract(t *testing.T) pinnedContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot(t), "expected.json"))
	if err != nil {
		t.Fatalf("reading pinned contract: %v", err)
	}
	var contract pinnedContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("decoding pinned contract: %v", err)
	}
	return contract
}

func normalizeContractPaths(t *testing.T, contract *pinnedContract) {
	t.Helper()
	normalizeLocations := func(locations []core.Location) {
		for i := range locations {
			locations[i].File = fixtureRelativePath(t, locations[i].File)
		}
	}
	normalizeSymbols := func(symbols []tools.OutlineSymbol) {
		for i := range symbols {
			symbols[i].File = fixtureRelativePath(t, symbols[i].File)
		}
	}
	normalizeLocations(contract.DefinitionTotalArea.Locations)
	normalizeLocations(contract.ReferencesCircle.Locations)
	normalizeSymbols(contract.OutlineGeometry.Symbols)
	normalizeSymbols(contract.OutlineMain.Symbols)
}

func fixtureRelativePath(t *testing.T, path string) string {
	t.Helper()
	rel, err := filepath.Rel(fixtureRoot(t), path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("result path %q is outside fixture root: %v", path, err)
	}
	return filepath.ToSlash(rel)
}

func validatePinnedContract(got, want pinnedContract) error {
	checks := []struct {
		path string
		got  any
		want any
	}{
		{"definition_total_area", got.DefinitionTotalArea, want.DefinitionTotalArea},
		{"references_circle", got.ReferencesCircle, want.ReferencesCircle},
		{"outline_geometry", got.OutlineGeometry, want.OutlineGeometry},
		{"outline_main", got.OutlineMain, want.OutlineMain},
		{"outline_invalid_file", got.OutlineInvalidFile, want.OutlineInvalidFile},
	}
	for _, check := range checks {
		if err := contractMismatch(check.path, check.got, check.want); err != nil {
			return err
		}
	}
	return nil
}

func validateTelemetry(events []map[string]any, want []expectedEvent) error {
	if len(events) != len(want) {
		return fmt.Errorf("telemetry: records = %d, want %d", len(events), len(want))
	}
	baseRequired := []string{"duration_ms", "generation", "graph_mode", "result_size", "session_id", "stale", "tool", "truncated", "ts"}
	for i, event := range events {
		required := append([]string(nil), baseRequired...)
		if want[i].Err != "" {
			required = append(required, "err")
			sort.Strings(required)
		}
		keys := make([]string, 0, len(event))
		for key := range event {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, required) {
			return fmt.Errorf("telemetry[%d].keys = %v, want %v", i, keys, required)
		}
		timestamp, ok := event["ts"].(string)
		if !ok {
			return fmt.Errorf("telemetry[%d].ts has type %T, want string", i, event["ts"])
		}
		if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
			return fmt.Errorf("telemetry[%d].ts = %q: %w", i, timestamp, err)
		}
		duration, ok := event["duration_ms"].(json.Number)
		if !ok {
			return fmt.Errorf("telemetry[%d].duration_ms has type %T, want number", i, event["duration_ms"])
		}
		milliseconds, err := duration.Int64()
		if err != nil || milliseconds < 0 {
			return fmt.Errorf("telemetry[%d].duration_ms = %q, want non-negative integer", i, duration)
		}
		static := make(map[string]any, len(event)-2)
		for key, value := range event {
			if key != "ts" && key != "duration_ms" {
				static[key] = value
			}
		}
		if err := contractMismatch(fmt.Sprintf("telemetry[%d]", i), static, want[i]); err != nil {
			return err
		}
	}
	return nil
}

func contractMismatch(path string, got, want any) error {
	gotValue, err := jsonValue(got)
	if err != nil {
		return fmt.Errorf("%s: encode actual: %w", path, err)
	}
	wantValue, err := jsonValue(want)
	if err != nil {
		return fmt.Errorf("%s: encode expected: %w", path, err)
	}
	return compareJSON(path, gotValue, wantValue)
}

func jsonValue(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func compareJSON(path string, got, want any) error {
	if reflect.DeepEqual(got, want) {
		return nil
	}
	switch want := want.(type) {
	case map[string]any:
		got, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s has type %T, want object", path, got)
		}
		keys := make([]string, 0, len(want))
		for key := range want {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			gotValue, ok := got[key]
			if !ok {
				return fmt.Errorf("%s.%s is missing", path, key)
			}
			if err := compareJSON(path+"."+key, gotValue, want[key]); err != nil {
				return err
			}
		}
		for key := range got {
			if _, ok := want[key]; !ok {
				return fmt.Errorf("%s.%s is unexpected", path, key)
			}
		}
	case []any:
		got, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s has type %T, want array", path, got)
		}
		if len(got) != len(want) {
			return fmt.Errorf("%s length = %d, want %d", path, len(got), len(want))
		}
		for i := range want {
			if err := compareJSON(path+"["+strconv.Itoa(i)+"]", got[i], want[i]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s = %v, want %v", path, got, want)
	}
	return nil
}

func TestPinnedContractRejectsMutations(t *testing.T) {
	want := loadPinnedContract(t)
	goodEvents := make([]map[string]any, len(want.Telemetry))
	for i, event := range want.Telemetry {
		value, err := jsonValue(event)
		if err != nil {
			t.Fatal(err)
		}
		goodEvents[i] = value.(map[string]any)
		goodEvents[i]["ts"] = "2026-07-29T00:00:00Z"
		goodEvents[i]["duration_ms"] = json.Number("0")
	}
	tests := []struct {
		name   string
		mutate func(*pinnedContract, []map[string]any)
	}{
		{name: "wrong same-file definition location", mutate: func(c *pinnedContract, _ []map[string]any) {
			c.DefinitionTotalArea.Locations[0].Range.Start.Character++
		}},
		{name: "swapped reference locations", mutate: func(c *pinnedContract, _ []map[string]any) {
			c.ReferencesCircle.Locations[2], c.ReferencesCircle.Locations[3] = c.ReferencesCircle.Locations[3], c.ReferencesCircle.Locations[2]
		}},
		{name: "swapped outline responses", mutate: func(c *pinnedContract, _ []map[string]any) {
			c.OutlineGeometry, c.OutlineMain = c.OutlineMain, c.OutlineGeometry
		}},
		{name: "missing geometry signature", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineGeometry.Symbols[4].Signature = "" }},
		{name: "missing main signature", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineMain.Symbols[5].Signature = "" }},
		{name: "wrong complete range", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineGeometry.Symbols[0].Range.End.Character++ }},
		{name: "wrong selection range", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineGeometry.Symbols[0].SelRange.End.Character++ }},
		{name: "wrong kind", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineGeometry.Symbols[0].Kind = "class" }},
		{name: "wrong nesting", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineGeometry.Symbols[1].Depth = 0 }},
		{name: "wrong freshness", mutate: func(c *pinnedContract, _ []map[string]any) { c.ReferencesCircle.Freshness.Generation = 1 }},
		{name: "wrong truncation", mutate: func(c *pinnedContract, _ []map[string]any) { c.OutlineMain.Truncated = true }},
		{name: "duplicate replaces missing event", mutate: func(_ *pinnedContract, events []map[string]any) { events[2] = events[1] }},
		{name: "swapped outline events", mutate: func(_ *pinnedContract, events []map[string]any) { events[0], events[3] = events[3], events[0] }},
		{name: "wrong session tag", mutate: func(_ *pinnedContract, events []map[string]any) { events[0]["session_id"] = "other" }},
		{name: "wrong graph tag", mutate: func(_ *pinnedContract, events []map[string]any) { events[0]["graph_mode"] = "no-graph" }},
		{name: "wrong result size", mutate: func(_ *pinnedContract, events []map[string]any) { events[0]["result_size"] = json.Number("12") }},
		{name: "unexpected error", mutate: func(_ *pinnedContract, events []map[string]any) { events[0]["err"] = "wrong call" }},
		{name: "missing expected error", mutate: func(_ *pinnedContract, events []map[string]any) { delete(events[4], "err") }},
		{name: "misattributed error", mutate: func(_ *pinnedContract, events []map[string]any) { events[4]["err"] = "other failure" }},
		{name: "missing timestamp", mutate: func(_ *pinnedContract, events []map[string]any) { delete(events[0], "ts") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contract := cloneContract(t, want)
			events := cloneEvents(t, goodEvents)
			tt.mutate(&contract, events)
			contractErr := validatePinnedContract(contract, want)
			telemetryErr := validateTelemetry(events, want.Telemetry)
			if contractErr == nil && telemetryErr == nil {
				t.Fatal("mutation passed the pinned contract")
			}
		})
	}
	t.Run("extra duplicate event", func(t *testing.T) {
		events := append(cloneEvents(t, goodEvents), cloneEvents(t, goodEvents)[0])
		if err := validateTelemetry(events, want.Telemetry); err == nil {
			t.Fatal("extra duplicate event passed the pinned contract")
		}
	})
}

func cloneContract(t *testing.T, source pinnedContract) pinnedContract {
	t.Helper()
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone pinnedContract
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func cloneEvents(t *testing.T, source []map[string]any) []map[string]any {
	t.Helper()
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var clone []map[string]any
	if err := decoder.Decode(&clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

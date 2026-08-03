package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

type definitionEnrichmentProvider struct {
	*core.StubProvider
	gotLocations []core.Location
	definitions  []core.Definition
	err          error
}

func (p *definitionEnrichmentProvider) DefinitionSources(_ context.Context, locations []core.Location) ([]core.Definition, error) {
	p.gotLocations = append([]core.Location(nil), locations...)
	return append([]core.Definition(nil), p.definitions...), p.err
}

func TestFindDefinitionCapsBeforeEnrichmentAndPublishesTypedDefinitions(t *testing.T) {
	file := "/repo/main.go"
	locations := []core.Location{
		{File: "/repo/c.ts", Range: rng(8, 4, 8, 7)},
		{File: "/repo/a.ts", Range: rng(2, 4, 2, 7)},
		{File: "/repo/b.ts", Range: rng(5, 4, 5, 7)},
	}
	want := []core.Definition{
		renderedDefinition(locations[0].File, locations[0].Range, rng(8, 0, 10, 1), "function run() {\n  return 3;\n}"),
		renderedDefinition(locations[1].File, locations[1].Range, rng(2, 0, 4, 1), "function run() {\n  return 1;\n}"),
	}
	provider := &definitionEnrichmentProvider{
		StubProvider: &core.StubProvider{
			Symbols:     map[string][]core.SymbolNode{file: {symbolNode(core.Symbol{Name: "run", SelRange: rng(12, 4, 12, 7)})}},
			Definitions: map[string][]core.Location{file: locations},
		},
		definitions: want,
	}
	logger := &capturingLogger{}
	out, err := newTestTools(provider, logger, core.Config{MaxResults: 2}).FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "run"})
	if err != nil {
		t.Fatalf("FindDefinition: %v", err)
	}
	if !out.Found || !out.Truncated || !reflect.DeepEqual(out.Definitions, want) {
		t.Fatalf("output = %#v, want found typed definitions %#v with truncation", out, want)
	}
	if !reflect.DeepEqual(provider.gotLocations, locations[:2]) {
		t.Fatalf("enrichment locations = %#v, want capped provider prefix %#v", provider.gotLocations, locations[:2])
	}
	event, _ := logger.last()
	if event.ResultSize != 2 || !event.Truncated || event.Err != "" {
		t.Fatalf("telemetry event = %#v, want two truncated definitions", event)
	}
}

func TestFindDefinitionEnrichmentFailuresAreAtomicSoftErrors(t *testing.T) {
	file := "/repo/main.go"
	locations := []core.Location{
		{File: "/repo/a.ts", Range: rng(2, 4, 2, 7)},
		{File: "/repo/b.ts", Range: rng(5, 4, 5, 7)},
	}
	tests := []struct {
		name        string
		definitions []core.Definition
		err         error
		wantError   string
		maxResults  int
	}{
		{name: "provider mapping error after cap", err: errors.New("target does not match a declaration"), wantError: "target does not match a declaration", maxResults: 1},
		{name: "cardinality mismatch", definitions: []core.Definition{{Target: locations[0]}}, wantError: "provider returned 1 definitions for 2 locations", maxResults: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &definitionEnrichmentProvider{
				StubProvider: &core.StubProvider{
					Symbols:     map[string][]core.SymbolNode{file: {symbolNode(core.Symbol{Name: "run", SelRange: rng(12, 4, 12, 7)})}},
					Definitions: map[string][]core.Location{file: locations},
				},
				definitions: tc.definitions,
				err:         tc.err,
			}
			logger := &capturingLogger{}
			out, err := newTestTools(provider, logger, core.Config{MaxResults: tc.maxResults}).FindDefinition(context.Background(), FindDefinitionInput{File: file, Symbol: "run"})
			if err != nil {
				t.Fatalf("FindDefinition returned Go error: %v", err)
			}
			if out.Found || out.Truncated || out.Definitions != nil || out.Error != tc.wantError || out.Message == "" {
				t.Fatalf("atomic soft output = %#v, want error %q with no definitions/truncation", out, tc.wantError)
			}
			event, _ := logger.last()
			if event.ResultSize != 0 || event.Truncated || event.Err != tc.wantError {
				t.Fatalf("telemetry event = %#v, want atomic soft failure", event)
			}
		})
	}
}

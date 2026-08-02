package core

import "context"

// StubProvider supplies canned LanguageProvider results for tests and wiring
// checks.
type StubProvider struct {
	Definitions             map[string][]Location // keyed by file
	DefinitionSourcesResult []Definition
	Refs                    map[string][]Location
	Symbols                 map[string][]SymbolNode
}

func (s *StubProvider) Definition(_ context.Context, file string, _ Position) ([]Location, error) {
	return s.Definitions[file], nil
}

func (s *StubProvider) DefinitionSources(_ context.Context, locations []Location) ([]Definition, error) {
	if s.DefinitionSourcesResult != nil {
		return append([]Definition(nil), s.DefinitionSourcesResult...), nil
	}
	definitions := make([]Definition, len(locations))
	for i, location := range locations {
		definitions[i] = Definition{Target: location, DeclarationRange: location.Range}
	}
	return definitions, nil
}

func (s *StubProvider) References(_ context.Context, file string, _ Position, _ bool) ([]Location, error) {
	return s.Refs[file], nil
}

func (s *StubProvider) DocumentSymbols(_ context.Context, file string) ([]SymbolNode, error) {
	return s.Symbols[file], nil
}

func (s *StubProvider) SymbolSignatures(_ context.Context, _ string, symbols []Symbol) ([]string, error) {
	signatures := make([]string, len(symbols))
	for i := range symbols {
		signatures[i] = symbols[i].Signature
	}
	return signatures, nil
}

func (s *StubProvider) Close() error { return nil }

var _ LanguageProvider = (*StubProvider)(nil)

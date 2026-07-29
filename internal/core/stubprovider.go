package core

import "context"

// StubProvider supplies canned LanguageProvider results for tests and wiring
// checks.
type StubProvider struct {
	Definitions map[string][]Location // keyed by file
	Refs        map[string][]Location
	Symbols     map[string][]Symbol
}

func (s *StubProvider) Definition(_ context.Context, file string, _ Position) ([]Location, error) {
	return s.Definitions[file], nil
}

func (s *StubProvider) References(_ context.Context, file string, _ Position, _ bool) ([]Location, error) {
	return s.Refs[file], nil
}

func (s *StubProvider) DocumentSymbols(_ context.Context, file string) ([]Symbol, error) {
	return s.Symbols[file], nil
}

func (s *StubProvider) Close() error { return nil }

var _ LanguageProvider = (*StubProvider)(nil)

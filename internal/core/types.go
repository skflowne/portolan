// Package core defines the shared contracts for the portolan daemon:
// the LanguageProvider interface, the LSP-style position/location types, the
// tool result envelopes (which always carry freshness), telemetry, and config.
//
// Everything else in the daemon is built against this package. It has no
// dependencies on the other internal packages, so it can be the stable center
// that the LSP provider, telemetry spine, MCP server, and tools all depend on.
package core

import "context"

// Position is a zero-based LSP-style position. Character is a UTF-16 code-unit
// offset (tsgo reports positionEncoding utf-16).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open [Start, End) span, LSP-style.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a resolved position in a file. File is an absolute path,
// normalized for the host OS (see internal/pathnorm).
type Location struct {
	File  string `json:"file"`
	Range Range  `json:"range"`
}

// SymbolKind mirrors the LSP SymbolKind names we care about (lower-cased,
// human-readable) so tool output is legible without a numeric lookup table.
type SymbolKind string

// Symbol is the canonical non-recursive description of a source symbol.
// File is an absolute host-normalized path. Range is the complete declaration
// span, while SelRange identifies the name used for navigation. Signature is a
// compact provider-authoritative declaration or type summary and Detail is the
// provider's independent document-symbol detail; either may be empty.
type Symbol struct {
	Name      string     `json:"name"`
	Kind      SymbolKind `json:"kind"`
	File      string     `json:"file"`
	Range     Range      `json:"range"`
	SelRange  Range      `json:"selRange"`
	Signature string     `json:"signature,omitempty" jsonschema:"compact provider-authoritative declaration or type summary; omitted when unavailable"`
	Detail    string     `json:"detail,omitempty" jsonschema:"provider document-symbol detail, independent of signature; omitted when unavailable"`
}

// SymbolNode owns outline hierarchy separately from canonical Symbol identity.
// A symbol's nesting may change without changing the symbol atom itself.
type SymbolNode struct {
	Symbol
	Children []SymbolNode `json:"children,omitempty"`
}

// Freshness is stamped on every tool result to identify the source generation
// it observed and whether it may lag the current source state.
type Freshness struct {
	Generation uint64 `json:"generation"`
	Stale      bool   `json:"stale"`
}

// LanguageProvider is the seam between the daemon and a language's LSP server.
// It is position-based so the tsgo provider can map calls directly to LSP
// requests.
//
// Symbol-name-path addressing (resolving a human-supplied symbol name to a
// position, because offsets shift under unobserved edits) is deliberately NOT
// here — it lives one layer up in the tool handlers, which call DocumentSymbols
// to resolve a name to a Position before calling Definition/References.
//
// All methods take file as an absolute, host-normalized path. Implementations
// must be safe for concurrent use by multiple goroutines.
type LanguageProvider interface {
	// Definition returns the definition site(s) of the symbol at pos in file.
	// A found-nothing result is (nil, nil) — an honest null, not an error.
	Definition(ctx context.Context, file string, pos Position) ([]Location, error)

	// References returns references to the symbol at pos in file. When
	// includeDeclaration is true the declaration itself is included.
	References(ctx context.Context, file string, pos Position, includeDeclaration bool) ([]Location, error)

	// DocumentSymbols returns the outline of file as a (possibly nested) symbol
	// tree. Used both for the get_outline tool and for name→position resolution.
	DocumentSymbols(ctx context.Context, file string) ([]SymbolNode, error)

	// SymbolSignatures returns one signature for each symbol, in input order.
	// An empty signature means the provider has no authoritative summary. The
	// caller bounds symbols before invoking this method.
	SymbolSignatures(ctx context.Context, file string, symbols []Symbol) ([]string, error)

	// Close shuts the provider (and its LSP subprocess) down.
	Close() error
}

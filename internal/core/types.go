// Package core defines the shared contracts for the portolan daemon:
// the LanguageProvider interface, normalized navigation atoms and hierarchy,
// freshness, telemetry, and config.
//
// Everything else in the daemon is built against this package. It has no
// dependencies on the other internal packages, so it can be the stable center
// that the LSP provider, telemetry spine, MCP server, and tools all depend on.
package core

import (
	"context"
	"fmt"
)

// Position is a zero-based LSP-style position. Character is a UTF-16 code-unit
// offset (tsgo reports positionEncoding utf-16).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Validate rejects coordinates outside the normalized position domain.
func (p Position) Validate() error {
	if p.Line < 0 || p.Character < 0 {
		return fmt.Errorf("negative position %+v", p)
	}
	return nil
}

// Range is a half-open [Start, End) span, LSP-style. Zero-width and multiline
// ranges are valid.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Validate rejects invalid positions and ranges whose end precedes their start.
func (r Range) Validate() error {
	if err := r.Start.Validate(); err != nil {
		return fmt.Errorf("range start: %w", err)
	}
	if err := r.End.Validate(); err != nil {
		return fmt.Errorf("range end: %w", err)
	}
	if r.End.Line < r.Start.Line || r.End.Line == r.Start.Line && r.End.Character < r.Start.Character {
		return fmt.Errorf("range end %+v precedes start %+v", r.End, r.Start)
	}
	return nil
}

// Contains reports whether inner lies entirely within r.
func (r Range) Contains(inner Range) bool {
	return !positionBefore(inner.Start, r.Start) && !positionBefore(r.End, inner.End)
}

func positionBefore(a, b Position) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
}

// Location is a resolved position in a file. File is an absolute path,
// normalized for the host OS (see internal/pathnorm).
type Location struct {
	File  string `json:"file"`
	Range Range  `json:"range"`
}

// Definition is an enriched definition site. Target is the provider-returned
// navigation fact, DeclarationRange is the matching symbol's complete range,
// and Source is the exact text inside that range from the provider's analyzed
// snapshot.
type Definition struct {
	Target           Location `json:"target"`
	DeclarationRange Range    `json:"declarationRange"`
	Source           string   `json:"source"`
}

// SymbolKind is a provider-neutral, human-readable symbol classification.
type SymbolKind string

const (
	SymbolKindUnknown       SymbolKind = "unknown"
	SymbolKindFile          SymbolKind = "file"
	SymbolKindModule        SymbolKind = "module"
	SymbolKindNamespace     SymbolKind = "namespace"
	SymbolKindPackage       SymbolKind = "package"
	SymbolKindClass         SymbolKind = "class"
	SymbolKindMethod        SymbolKind = "method"
	SymbolKindProperty      SymbolKind = "property"
	SymbolKindField         SymbolKind = "field"
	SymbolKindConstructor   SymbolKind = "constructor"
	SymbolKindEnum          SymbolKind = "enum"
	SymbolKindInterface     SymbolKind = "interface"
	SymbolKindFunction      SymbolKind = "function"
	SymbolKindVariable      SymbolKind = "variable"
	SymbolKindConstant      SymbolKind = "constant"
	SymbolKindString        SymbolKind = "string"
	SymbolKindNumber        SymbolKind = "number"
	SymbolKindBoolean       SymbolKind = "boolean"
	SymbolKindArray         SymbolKind = "array"
	SymbolKindObject        SymbolKind = "object"
	SymbolKindKey           SymbolKind = "key"
	SymbolKindNull          SymbolKind = "null"
	SymbolKindEnumMember    SymbolKind = "enummember"
	SymbolKindStruct        SymbolKind = "struct"
	SymbolKindEvent         SymbolKind = "event"
	SymbolKindOperator      SymbolKind = "operator"
	SymbolKindTypeParameter SymbolKind = "typeparameter"
)

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
	Signature string     `json:"signature,omitempty"`
	Detail    string     `json:"detail,omitempty"`
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

	// DefinitionSources enriches definition locations with their complete
	// declaration ranges and exact source from provider-retained analyzed
	// snapshots. Results correspond one-for-one with locations in input order;
	// any mapping or extraction failure returns no partial result.
	DefinitionSources(ctx context.Context, locations []Location) ([]Definition, error)

	// References returns references to the symbol at pos in file. When
	// includeDeclaration is true the declaration itself is included.
	References(ctx context.Context, file string, pos Position, includeDeclaration bool) ([]Location, error)

	// DocumentSymbols returns the outline of file as a SymbolNode tree. Used both
	// for the get_outline tool and for name→position resolution.
	DocumentSymbols(ctx context.Context, file string) ([]SymbolNode, error)

	// SymbolSignatures returns one signature for each symbol, in input order.
	// An empty signature means the provider has no authoritative summary. The
	// caller bounds symbols before invoking this method.
	SymbolSignatures(ctx context.Context, file string, symbols []Symbol) ([]string, error)

	// Close shuts the provider (and its LSP subprocess) down.
	Close() error
}

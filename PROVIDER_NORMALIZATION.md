# Provider normalization contract

This document is the canonical description of Portolan's current provider-normalization boundary.
It records existing behavior; it does not define a new adapter, capability-negotiation layer, or
server-specific policy.

## Contract and ownership

`core.LanguageProvider` is the only outward provider-neutral query seam used by tools. Its methods
accept absolute host-normalized file paths and canonical `core.Position` values and return only
canonical atoms:

- `core.Position` is a zero-based UTF-16 code-unit position.
- `core.Range` is a validated half-open range.
- `core.Location` contains a canonical absolute host path and range.
- `core.Symbol` contains a name, normalized `core.SymbolKind`, canonical file and ranges, and
  independent optional `Signature` and `Detail` strings.
- `core.SymbolNode` adds hierarchy without making hierarchy part of symbol identity.

The boundary has one owner at each stage:

| Responsibility | Owner | Contract |
| --- | --- | --- |
| Canonical atoms and normalized query interface | `internal/core` | Defines `LanguageProvider`, `Position`, `Range`, `Location`, `Symbol`, and `SymbolNode`. |
| Caller path normalization and file-URI identity | `internal/pathnorm` | Canonicalizes tool input paths and exclusively encodes/decodes strict file URIs. |
| JSON-RPC transport | `internal/lsp.transport` | Owns Content-Length framing, request IDs, pending responses, writes, cancellation dispatch, server errors, and connection/process lifecycle. |
| Language-operation orchestration | `internal/lsp.Provider` | Opens canonical files, builds method-specific parameters, calls the transport, and sends successful raw results to the matching decoder. Transport, open, and request errors return before decoding. |
| Raw-result normalization | Concrete functions in `internal/lsp` | `isJSONNull`, `decodeDocumentSymbols`, `decodeLocations`, `rawLocation.toLocation`, position/range converters, `lspDocumentSymbol.toCoreSymbol`, `symbolKindName`, `decodeHoverSignature`, and `decodeHoverContents` validate and convert successful method-specific results. |
| Tool behavior | `internal/tools` | Uses only `core.LanguageProvider`, resolves names, applies caps, stamps freshness, and turns provider failures into structured soft tool errors. |
| Process composition | `cmd/portoland` | Constructs `lsp.Provider` and supplies it to the tools layer as a `core.LanguageProvider`. |

Consequently, tools receive no `json.RawMessage`, JSON-RPC envelopes, LSP numeric enums, file URIs,
or Markdown wrappers. Numeric symbol kinds become `core.SymbolKind` values, file URIs become
canonical paths, and hover presentation wrappers become compact plain strings before values cross
`LanguageProvider`.

## Inventory and call sites

### Implementations and wire shapes

- `internal/core/types.go` owns `LanguageProvider` and the canonical atoms.
- `internal/core/stubprovider.go` supplies map-backed definition, reference, and symbol results for
  upper-layer tests. Its signature method returns the existing canonical signatures in input order.
- `internal/lsp/provider.go` contains the production `Provider` implementation and the definition,
  references, and document-symbol request orchestration.
- `internal/lsp/signatures.go` implements signature planning and bounded hover requests;
  `internal/lsp/declaration_headers.go` owns retained-range and name validation plus lexical-span
  classification, trivia removal, and whitespace normalization; `internal/lsp/declaration_structure.go`
  owns every request-local validity transition, including operators, pending function arrows,
  generic purposes, conditional stages, class/interface heritage phases, operands, separators, and
  closure; `internal/lsp/declaration_structure_scan.go` classifies and dispatches source tokens to
  that owner, locates the outer-body boundary, and rejects slash-ambiguous headers; and
  `internal/lsp/hover_conversion.go` owns hover decoding
  and tsgo display normalization.
- `internal/lsp/protocol.go` contains private method-specific wire shapes: `rawLocation`,
  `lspDocumentSymbol`, `hoverResult`, `markupContent`, and the numeric symbol-kind map.
- `internal/lsp/conversion.go` contains document-symbol, location, position, and range conversion;
  `internal/lsp/uri.go` contains the LSP-facing `pathFromURI` wrapper that delegates canonical
  identity conversion to `internal/pathnorm` and qualifies conversion errors.
- `internal/lsp/rpc.go`, `internal/lsp/transport.go`, and `internal/lsp/framing.go` own JSON-RPC
  request/response delivery and framing. They do not construct canonical navigation atoms.

The complete production query call graph through `LanguageProvider` is:

| Tool call site | First provider stage | Second provider stage |
| --- | --- | --- |
| `internal/tools/find_definition.go` | `DocumentSymbols` for name-to-position resolution | `Definition` |
| `internal/tools/find_references.go` | `DocumentSymbols` for name-to-position resolution | `References` with `includeDeclaration=true` |
| `internal/tools/get_outline.go` | `DocumentSymbols` for structure | `SymbolSignatures` for retained symbols |

No MCP handler calls `internal/lsp.Provider` directly. `internal/mcp` registers the typed tool methods,
and the MCP SDK derives schemas from their canonical input and output types.

Tests use the same seam through `core.StubProvider` and focused test providers in
`internal/tools/tools_test.go` and `internal/tools/operation_budget_test.go`. Direct production
provider calls are confined to `internal/lsp` integration and concurrency tests: `provider_test.go`,
`provider_concurrency_test.go`, and `signatures_test.go`.

### Decoder and boundary coverage

Raw conversion is tested independently of the subprocess transport:

- `navigation_conversion_test.go` covers hierarchy, kind normalization, detail preservation, and
  invalid position/range geometry.
- `uri_test.go` covers `Location` and `LocationLink` URI normalization, target-selection fallback,
  unsupported or unrepresentable URIs, and conversion cancellation.
- `signatures_test.go` and pinned hover fixtures cover Markdown extraction, symbol-aware tsgo
  display normalization, separation of signature from document-symbol detail, missing and malformed
  hover, retained-source class/interface headers, source-backed synthetic signatures, input ordering,
  bounded concurrency, cancellation, and first-error behavior.
- `framing_conversion_test.go` covers cancellation precedence and the absence of partial converted
  output.

Pinned raw document-symbol fixtures additionally cover interface, class, constructor, property,
method, function, anonymous-callback hierarchy, unknown kinds, and complete canonical ranges.
`provider_test.go` exercises document symbols, definitions, references, honest-null definitions,
canonical escaped paths, and concurrent queries against the real pinned language server.
`internal/tools` tests cover name resolution, caps, retained-symbol enrichment, honest-empty
results, soft errors, and the shared operation budget. `internal/mcp/server_test.go` checks typed MCP
round trips, while `eval/tiera` checks the final real-daemon structured contract. Transport framing,
pending-response, cancellation, and shutdown behavior has separate owner-level tests under
`internal/lsp`.

## Current response semantics

These are the current concrete decoder semantics, not requirements imposed on future servers.
Transport or JSON-RPC errors do not enter these decoders: `transport.call` returns the error to
`Provider`, which propagates it to the tools layer.

### Document symbols

`decodeDocumentSymbols` checks cancellation before considering the payload. An absent raw result or
JSON `null` returns `(nil, nil)`, and an empty JSON array also normalizes to `(nil, nil)`. Otherwise
it decodes the payload into the private `DocumentSymbol[]` shape. Malformed JSON or a top-level value
that cannot unmarshal as that array returns a decoding error. Decoding is not strict schema
validation: omitted JSON fields take Go zero values, so a structurally different object whose
remaining fields happen to validate can produce a zero-valued atom rather than an error.

Every node is converted atomically:

- positions must be non-negative; ranges must be ordered; and each selection range must be contained
  by its full range;
- known numeric `SymbolKind` values become named `core.SymbolKind` values, while unknown numbers
  become `core.SymbolKindUnknown`;
- the already canonical file argument is attached to every node;
- `detail` remains independent, hierarchy is preserved in `Children`, and no signature is inferred
  during document-symbol decoding.

Cancellation before or during recursive conversion returns `context.Canceled` or
`context.DeadlineExceeded` with no partial tree. Any invalid node likewise fails the whole
conversion rather than returning a partial tree.

### Definition and reference locations

`decodeLocations` checks cancellation first and accepts the LSP result forms `null`, one `Location`,
`Location[]`, or `LocationLink[]`. Null and empty arrays return `(nil, nil)`. A single object is
retried as one location after array decoding fails.

A plain location uses `uri` and `range`. A location link uses `targetUri` and prefers
`targetSelectionRange`, falling back to `targetRange`. An entry missing its URI or applicable range
is ignored; if no entries remain, the result is `(nil, nil)`. Each retained URI is decoded through
`internal/pathnorm`, and every range is validated before a `core.Location` is built.

Malformed JSON, a value that cannot decode as either the array or single-object form, an invalid or
unrepresentable URI, or invalid range geometry returns an error and no partial location list.
Cancellation before or during conversion also returns the context error and no partial list. A
JSON-RPC provider error bypasses conversion and is returned as a method-qualified transport error
from the provider stage.

### Hover and signature content

`decodeHoverSignature` treats an absent raw result or JSON `null` as an unavailable signature:
`("", nil)`. Otherwise it decodes the hover envelope and normalizes `contents` as follows:

- absent or null contents become an empty signature;
- an object with a non-empty `value` is trimmed; for `kind: "markdown"`, the first complete fenced
  code block is extracted, or the trimmed full value is used when no fence exists; an incomplete
  fence is malformed and returns an error rather than leaking Markdown syntax;
- a string is trimmed;
- an array is examined in order and returns its first non-empty normalized item;
- malformed JSON or content outside those accepted shapes returns an error.

The extracted display is then normalized against the exact input symbol. Anchored tsgo method,
property, and accessor displays are accepted only when their kind and member name match, then lose
the provider adornment and any enclosing-type qualification. Qualified optional methods and
unqualified object members retain their semantic declaration tail. Matching constructor displays
become `constructor(<parameters>)`; matching anonymous callback and default-function displays become their
parameter-and-return summary. A recognized tsgo wrapper that does not match the symbol is omitted
rather than crossing the boundary with misleading provider presentation. Other unrecognized text is
left unchanged; this is deliberately not a general TypeScript parser or alias normalizer.

The output is a plain signature string rather than an LSP `MarkupContent` or `MarkedString` wrapper.
A complete fenced quick-info block loses its fence and surrounding documentation, and no Markdown
fence crosses the normalization boundary. An honest null/empty hover leaves the corresponding
canonical signature empty; it does not remove the symbol.

`SymbolSignatures` returns one string per input symbol in input order. Empty input returns a
non-nil empty slice. Named classes and interfaces prefer a complete declaration header extracted
from their canonical range in the exact source snapshot retained by `didOpen`.
`declaration_headers.go` validates the retained range, symbol kind, and name, removes preceding
modifiers and lexical trivia, preserves literals and non-trivia source such as valid generic and
`extends`/`implements` text, and collapses syntactic whitespace. One request-local
`declarationStructure` owns every token's validity effect, including operator and pending-arrow
transitions, conditional completeness, generic purpose and relational ambiguity, legal
class/interface heritage phases, operands, separators, and closure. The scanner only classifies and
dispatches tokens, accepts the outer-body boundary when that owner reports a complete state, and
conservatively rejects ambiguous slash syntax.
Anonymous, mismatched, lexically incomplete, structurally malformed, slash-ambiguous, or otherwise
unrecognized headers remain on the existing hover path; extraction never returns a partial source
header. Some synthetic bodyless declarations are also summarized directly from the
retained snapshot, while other entries use hover. Hover work is capped at eight concurrent requests.
Planning or retained-source errors return before hover dispatch. After the hover batch starts, the
first request or decoding error cancels its sibling work; caller cancellation also stops the batch.
Every error path returns no signature slice. The tools layer also rejects a successful result whose
length differs from the retained symbol count.

## Two-stage bounded structural retrieval

`get_outline` deliberately separates structure from semantic enrichment:

1. `DocumentSymbols` obtains the complete provider hierarchy without hover work.
2. `internal/tools.GetOutline` flattens depth-first and stops at `Config.Cap()`, recording whether
   more symbols existed.
3. Only the retained `core.Symbol` values are passed to `SymbolSignatures`.
4. Signatures are associated by input index, then the bounded flat `OutlineSymbol` list is returned.

This order prevents a large file from causing hover requests for symbols that the tool will discard.
Definition and reference tools also use `DocumentSymbols` first, but only to resolve a requested
name to the canonical selection-range position before making their second provider request.

## Errors at the tool boundary

All provider stages share the tools-owned operation context. Transport errors, provider errors,
decoder errors, signature-batch errors, and cancellation are returned through `LanguageProvider`.
The tools layer exposes them as a populated output `error` and explanatory `message`, emits its one
telemetry event, and returns no Go error to the MCP handler. An honest null or empty required
structure/location stage produces `found: false` with no output error. Null or empty hover content
only omits that symbol's optional signature, so an outline that retained symbols remains
`found: true`. A successful provider result that arrives after operation cancellation is rejected
before the tool accepts it.

## Deferred concerns

Unsupported-capability handling is not special-cased today. If a server rejects a method, the
resulting JSON-RPC error follows the ordinary provider-error and soft-tool-error path. Capability
negotiation is deferred until requirements establish its policy.

Server-specific adapter extraction is also deferred. The concrete decoder functions are the
current raw-to-canonical boundary. A second server or demonstrated protocol variation must reveal
the necessary abstraction before an adapter interface is introduced. In particular, this contract
does not add a second normalized interface parallel to `core.LanguageProvider`, tsgo-specific tool
behavior, fallback parsing, or speculative compatibility logic.

## Recorded follow-up gaps

This documentation inventory exposes two gaps without changing behavior:

- `decodeDocumentSymbols` requests hierarchical `DocumentSymbol[]` but does not strictly reject the
  alternative flat `SymbolInformation[]` shape or other objects with omitted fields; Go zero values
  can pass current geometry validation. Unsupported-capability and alternate-shape policy remains
  deferred rather than adding negotiation or an adapter here.
- Independent decoder tests now pin uncanceled null/empty/malformed document-symbol and location
  results plus accepted hover object/string forms. The legacy accepted hover-array form remains
  covered only through its decoder implementation rather than a dedicated fixture; expanding that
  legacy-shape matrix should follow demonstrated provider output rather than broadening this
  tsgo-focused change.

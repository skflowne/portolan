## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,349,376 bytes; finally 5,718,016 bytes)
- Invocations: 9/9 succeeded
- Most useful for:
  - locating the existing canonical atoms in `internal/core/types.go` and the LSP conversion boundary in `internal/lsp/conversion.go`;
  - tracing the three tools through `DocumentSymbols`, location results, outline flattening, MCP registration, and Tier A;
  - identifying the duplicate `OutlineSymbol` field bag and the LSP-owned kind vocabulary;
  - confirming the final `SymbolAtom`/`Symbol`/`OutlineSymbol` ownership split and blast radius.
- Limitations:
  - generated call paths sometimes led with unrelated lifecycle symbols rather than the requested navigation flow;
  - multi-symbol results were truncated and omitted exact test tails, Markdown documentation, fixtures, and some elided composite-literal consumers;
  - the final graph reported no covering tests for `SymbolKind` despite the explicit adapter matrix in `internal/lsp/framing_conversion_test.go`;
  - CodeGraph could not predict the MCP SDK schema recursion caused by anonymously embedding recursive `core.Symbol`; focused MCP execution exposed it.
- Fallbacks:
  - targeted `go doc`, `go list`, and `rg` completed the required producer/consumer/serializer/default inventory after CodeGraph established the relevant packages;
  - narrow reads of existing test sections located the exact behavior homes for conversion, URI, traversal, schema, and live-provider assertions;
  - `git log`/`git show` established why capped outline shaping preserves exact provider symbols for signature planning;
  - focused tests verified the MCP schema and live tsgo behavior that static graph exploration could not prove.

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `Issue 75 normalized navigation atoms: locate every producer and consumer of get_outline, find_references, and find_definition; existing internal/core navigation, symbol, location/range, kind, freshness/status types; provider adapters; tests and docs governing these paths` | Success | Found `Position`, `Range`, `Location`, `Symbol`, `Freshness`, `LanguageProvider`, `StubProvider`, and provider methods. | Only three files returned; later focused calls and `rg` enumerated tools, MCP, evals, and tests. |
| 2 | `get_outline OutlineSymbol flattenSymbols GetOutline output cap truncated freshness producers consumers tests MCP registration and Tier A serialization` | Success | Exposed the seven duplicated symbol fields in `OutlineSymbol`, depth-first cap logic, preserved originals, and Tier A consumer. | Output truncated before all schema/tests; narrow reads covered `server_test.go`, traversal tests, and contract helpers. |
| 3 | `find_definition FindDefinitionInput FindDefinitionOutput FindDefinition resolveSymbolPath Location truncated freshness producers consumers tests MCP and eval` | Success | Confirmed direct `[]core.Location`, name-to-selection-position resolution, cap application, and envelope facts. | Irrelevant lifecycle path preceded useful symbols; targeted inventory found serializers and tests. |
| 4 | `find_references FindReferencesInput FindReferencesOutput FindReferences Location truncated freshness includeDeclaration producers consumers tests MCP and eval` | Success | Confirmed direct `[]core.Location`, declaration inclusion, shared resolution, cap, and freshness path. | Same unrelated call-path issue; `rg` completed consumers. |
| 5 | `decodeDocumentSymbols decodeLocations lspSymbolKind lspPosition lspRange provider JSON numeric SymbolKind file URI adapters tests unknown kinds nested children detail signature ranges` | Success | Located wire DTOs, URI conversion, numeric kind table, recursive symbol adapter, unknown fallback, and range copying. | Needed narrow reads for complete URI assertions and current test gaps. |
| 6 | `positionEncoding utf-16 initialize capabilities encoding negotiation core Position character encoding tests tsgo` | Success | Confirmed the documented UTF-16 contract and live-provider seam; revealed existing tests used ASCII positions. | Added a focused astral-character tsgo fixture because static copying cannot prove analyzer encoding semantics. |
| 7 | `core SymbolKind all producers string literals constants symbolKindName consumers signatures synthetic symbols tests` | Success | Showed normalized vocabulary/fallback was incorrectly owned as strings by `internal/lsp`, while `core.SymbolKind` was open. | Targeted tests enumerated every LSP kind and unknown values. |
| 8 | `After anonymous embedding core.Symbol in tools.OutlineSymbol caused MCP JSON schema recursive cycle, evaluate all core.Symbol composite literals and consumers needed to introduce shared non-recursive SymbolAtom embedded by hierarchical core.Symbol and flat OutlineSymbol` | Success | Confirmed the recursive `Symbol` blast radius and supported the non-recursive shared atom migration. | CodeGraph missed some elided test composite literals; compiler errors and a narrow AST migration completed them. |
| 9 | `Final issue 75 ownership inventory after SymbolAtom migration: every producer consumer serializer validator fallback and default for Position Range Location SymbolAtom Symbol SymbolKind OutlineSymbol get_outline find_definition find_references freshness truncation` | Success | Confirmed `core.SymbolAtom` as shared field owner, `core.Symbol` as hierarchy owner, flat `OutlineSymbol`, direct location outputs, and envelope separation. | Final targeted `rg` verified omitted producers, tests, MCP/Tier A consumers, cap/freshness owners, and no parallel production field bag. |

## Ownership and migration inventory

`internal/core` owns normalized navigation values and vocabulary. `internal/pathnorm` owns canonical host paths and file-URI conversion rules. `internal/lsp` exclusively owns LSP JSON DTOs and explicit inbound/outbound adaptation. `internal/tools` owns name resolution and bounded outline projection; `GenerationCounter.Current()` owns freshness and `Config.Cap()` owns the effective result bound.

| Concern | Producers / adapters | Consumers / serializers | Validation, fallback, migration |
| --- | --- | --- | --- |
| Position and range | LSP conversion; callers provide positions through `LanguageProvider` | definition/reference requests, symbol resolution, signature planning, tool JSON through existing typed outputs | Zero-based UTF-16 and half-open invariants are core-owned; multiline and zero-width conversion plus live astral-character behavior are covered. |
| Location | `decodeLocations` converts Location/LocationLink URIs and ranges; `StubProvider` supplies tests | definition/reference outputs, MCP generic typed boundary, Tier A | Unsupported URIs error at `internal/pathnorm`; exact selection/target fallback ranges are covered; no migration required. |
| Symbol kind | Core constants own 26 normalized values plus `SymbolKindUnknown`; LSP numeric adapter selects them | all symbol consumers in the three tools | Previous LSP-owned string vocabulary is superseded; 1–26 and unknown numeric values are covered. |
| Symbol fields and hierarchy | `decodeDocumentSymbols`, `StubProvider`, and provider signature enrichment produce `SymbolAtom`/`Symbol` | outline shaping, name resolution, signature planning, MCP schema, Tier A | Duplicate tool field bag is removed; `Symbol.Children` remains canonical; extra provider JSON cannot set file/signature; optional fields and nested ranges are covered. |
| Outline projection | `flattenSymbols` projects `SymbolAtom` and derived `Depth`, preserving exact original symbols separately | `get_outline`, existing MCP typed schema/structured output, Tier A | Flat runtime JSON and schema exclude `children`; cap/truncation and input hierarchy immutability are covered. |
| Envelope facts | `GenerationCounter.Current()`, `Config.Cap()`, and tools' result shaping | all tool outputs and telemetry | `Freshness` and `Truncated` remain envelope facts; `Depth` remains projection-only. No atom contains them. |

The only production supersessions are the former `tools.OutlineSymbol` field copies and the LSP-local string kind vocabulary. No renderer, Markdown wrapper, custom MCP serialization, universal reference/call/diagnostic/edit union, cache identity, or new dependency was introduced.

## Decision log

### Projection representation

- Context/problem: `get_outline` must reuse canonical fields while remaining flat and bounded, retaining hierarchy upstream, preserving existing JSON/schema keys, and avoiding custom MCP serialization.
- Selected: introduce non-recursive `core.SymbolAtom`; compose it into hierarchical `core.Symbol` and flat `tools.OutlineSymbol`, with `Depth` only on the latter.
- Why: one owner defines symbol fields; the recursive hierarchy remains canonical; MCP schema generation sees a non-recursive flat projection; existing runtime field names and optionality remain unchanged.
- Considered:
  - anonymously embed `core.Symbol` and clear `Children`: smallest code change and correct runtime JSON, but the MCP SDK recursively expanded `Symbol.Children` and panicked while generating the output schema;
  - retain copied outline fields: preserves compatibility but leaves the exact drift issue unresolved;
  - add custom JSON/schema handling around embedded `Symbol`: avoids type migration but introduces the rendering/serialization mechanism explicitly excluded by the issue.

### Kind ownership

- Selected: core constants own normalized spellings and the unknown sentinel; the LSP package owns only numeric-to-core mapping.
- Trade-off: the constant list is longer, but future providers now converge on one vocabulary instead of independently spelling domain values.

### Character encoding evidence

- Selected: retain the fixed UTF-16 core invariant used by pinned tsgo and expose `PositionCharacterEncoding`; prove non-BMP offsets with the real provider.
- Alternative rejected: a JSON round trip proves only integer preservation, not whether the analyzer counts bytes, runes, or UTF-16 code units.

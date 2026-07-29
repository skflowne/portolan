## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,259,264 bytes; finally 5,357,568 bytes)
- Invocations: 4/4 succeeded
- Most useful for:
  - locating `runContextWork`, its four production call sites, and the independent `readFileContext` mechanism;
  - identifying `writeFrameLocked` as a distinct partial-I/O state machine rather than discardable finite work;
  - tracing response conversion callers and existing cancellation coverage;
  - confirming the final `runFiniteWork` caller inventory after migration.
- Limitations:
  - broad results included irrelevant symbols from `internal/mcp`, `internal/telemetry`, and `internal/core`;
  - large test and provider files were trimmed, so exact fixtures and lifecycle assertions were not always visible;
  - the final query reported seven `runFiniteWork` callers while the source inventory needed a targeted search to distinguish five production calls from test references.
- Fallbacks:
  - complete Markdown reads were required because CodeGraph is source-symbol oriented and repository policy required `PLAN.md`, `ARCHITECTURE.md`, `ENGINEERING.md`, and `TESTING.md`;
  - narrow reads of `provider.go`, `provider_concurrency_test.go`, `framing_conversion_test.go`, `transport_fixture_test.go`, and `transport_write_cancellation_test.go` filled trimmed source/test gaps;
  - targeted `rg` inventoried all buffered result-channel patterns after CodeGraph identified the owning symbols, to classify the frame writer and test-only channels;
  - `git log`/`git show` established why cancellation and first-open retry behavior had evolved.

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `Issue 43 finite work cancellation: inventory internal/lsp file-read serialization frame-preparation response-conversion completion versus cancellation mechanisms, owners, call paths, and tests` | Success | Located `runContextWork`, `readFileContext`, frame-write state, pending completion, and principal cancellation tests. | Output was truncated and included unrelated MCP symbols; followed with focused CodeGraph queries and narrow reads. |
| 2 | `internal/lsp all uses of runContextWork contextWorkResult os.ReadFile file reads json marshal unmarshal serialization frameBytes response conversion protocol conversion context cancellation` | Success | Found four `runContextWork` callers and the separate buffered `readFileContext`, including its transport-unavailable branch. | Provider and tests were trimmed; used narrow reads and targeted `rg` for exact inventory. |
| 3 | `runContextWork callers decodeLocations decodeDocumentSymbols conversion.go full functions and tests; Provider readFile field constructors default os.ReadFile readFileContext tests cancellation unavailable transport` | Success | Returned complete conversion bodies, provider migration seam, call counts, and relevant existing tests. | Constructor/history and full lifecycle tests still required narrow reads and `git show`. |
| 4 | `runFiniteWork finiteWorkInterruption finiteWorkCancellation current callers tests and remaining duplicated buffered completion cancellation mechanisms in internal/lsp after migration` | Success | Confirmed the new owner, package placement, migrated production files, and owner tests from current on-disk source. | Included irrelevant telemetry/core results and an ambiguous caller count; targeted `rg` classified remaining result channels. |

## Decision log

### Named owner and API

- **Context/problem:** File reads needed transport-unavailability as a second stop signal, while serialization, frame preparation, and conversion needed only caller cancellation. A rename in `framing.go` would leave the shared invariant owned by the wrong responsibility.
- **Chosen option and why:** Add package-local `runFiniteWork` in `internal/lsp/finite_work.go`, with one explicit optional `finiteWorkInterruption`. This gives the invariant a narrow responsibility home and lets file reads preserve the transport's first unavailable cause without a second arbitration wrapper.
- **Options considered and material trade-offs:** Keeping the owner in `framing.go` minimized movement but coupled filesystem and conversion policy to framing; composing a derived context for transport loss would add per-read lifecycle machinery and obscure the exact cause; variadic interruption options generalized beyond the single evidenced need.

### Arbitration precedence

- **Context/problem:** Existing `runContextWork` favored cancellation observable after completion, while `readFileContext` favored a ready result after cancellation or transport loss. Simultaneously observable stop signals could otherwise remain scheduler-dependent.
- **Chosen option and why:** Caller context cancellation wins over transport interruption, and either wins over completion when visible at the owner's final arbitration. This preserves the caller's operation budget as authoritative, returns the transport's first cause when the caller remains live, and prevents stale finite-work results from progressing to output.
- **Options considered and material trade-offs:** Completion precedence preserves the old file-read race but can admit stale work after timeout; raw `select` precedence is simpler but nondeterministic; transport-first precedence weakens the caller's explicit deadline/cancellation contract during shutdown races.

### Frame writes remain distinct

- **Context/problem:** The frame writer also uses a buffered result channel, but its underlying `io.Writer` may partially emit bytes and cannot be treated as discardable pre-output work.
- **Chosen option and why:** Keep `writeFrameLocked`'s atomic dispatch/abort owner and document the semantic distinction. Cancellation that wins makes the transport terminal; completed dispatch remains observable exactly once.
- **Options considered and material trade-offs:** Migrating the writer into `runFiniteWork` would reduce surface duplication but lose partial-write ownership and risk later frames on a corrupted transport.

### Documentation scope

- **Context/problem:** Architecture prose described operation cancellation and frame writes but did not name finite-work ownership or its exclusion boundary.
- **Chosen option and why:** Update the transport narrative in `ARCHITECTURE.md`; no package edge, request path, or diagram topology changed.
- **Options considered and material trade-offs:** Leaving prose unchanged would make future accidental consolidation likely; changing Mermaid diagrams would imply a structural component change that did not occur.

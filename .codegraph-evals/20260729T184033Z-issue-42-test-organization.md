## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,185,536 bytes; finally 5,369,856 bytes)
- Invocations: 3/3 succeeded
- Most useful for:
  - locating the oversized `internal/lsp/transport_test.go` and `internal/tools/tools_test.go` owners;
  - identifying `newTestTools`' broad caller set and the cross-file `recordingWriteCloser` fixture;
  - checking production call paths exercised by the relocated operation-budget and transport tests.
- Limitations:
  - large test files were heavily truncated, so CodeGraph did not provide a complete declaration-to-home inventory;
  - the post-split query surfaced representative symbols and blast radius but could not prove absence of every duplicate or moved assertion;
  - issue text, Markdown policy, git history, exact test manifests, and coverage equivalence are outside the graph's proof surface.
- Fallbacks:
  - `gh issue view 42` supplied the acceptance contract after CodeGraph located the relevant code;
  - complete reads of `AGENTS.md`, `ENGINEERING.md`, and `TESTING.md` supplied repository policy;
  - targeted `rg` declaration/reference inventories filled the truncated fixture-consumer and test-home gaps;
  - `go doc`, `go list`, and targeted `git log --follow` established package ownership and history;
  - Go AST fingerprints, `go test -json`, and coverage profiles provided exact relocation-equivalence evidence.

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `Issue #42 test organization: internal/lsp and internal/tools existing operation budget test files, test helpers, behavior groupings, inventory documentation, and production symbols exercised by those tests. Show ownership and call paths relevant to reorganizing tests without behavior changes.` | Success | Located both oversized test owners; reported 18 callers of `newTestTools`; connected tools tests to `Tools.runTool`/`operationContext` and LSP tests to `transport.call`. | Output included only fragments of the large test files. Used issue text, targeted declaration inventory, and complete repository-policy reads. |
| 2 | `internal/lsp transport_test.go and transport_lifecycle_test.go: map every Test function and shared fixture declaration to pending lifecycle, framing/conversion, write/cancellation, shutdown/process cleanup, or transport lifecycle state ownership. Show cross-file fixture consumers, especially recordingWriteCloser. Also map internal/tools/tools_test.go fixtures and tests to ordinary contracts, operation-budget/cancellation, and traversal.` | Success | Confirmed `transport_lifecycle_test.go` already exercised shutdown admission/state/process behavior and that `recordingWriteCloser` crossed files. | Still returned only selected declarations. Used targeted `rg`, a Go AST relocation script, and consumer inventories to classify every declaration. |
| 3 | `After issue #42 split, verify behavior-specific test homes and shared fixture consumers in internal/lsp and internal/tools. Identify tests or fixtures still in an unrelated home, duplicate helper implementations, or missing/deleted test symbols compared with the transport and tools production call paths.` | Success | Reconfirmed 18 `newTestTools` callers across ordinary and budget homes and connected split tests to the same production owners. | Could not prove exhaustive equality. Used move-independent AST hashes, runtime manifests, normalized coverage blocks, and exact path checks. |

## Direction decisions

### One shutdown/process-cleanup home

- Context/problem: `internal/lsp/transport_lifecycle_test.go` already owned transport state transitions, shutdown admission, abort, and process reaping, while related close/cleanup cases remained in `transport_test.go`.
- Chosen option: merge both sets into `transport_shutdown_test.go`, making it the single shutdown, lifecycle-transition, and process-cleanup home.
- Why: the tests reason about one lifecycle invariant and share fixtures; retaining both files would preserve overlapping behavior homes and fail issue #42's organization goal.
- Alternatives/trade-offs: retaining `transport_lifecycle_test.go` plus a second shutdown file would minimize rename churn but split one invariant; renaming the old file without merging would leave the original mixed file partly intact.
- Assumption: no external tooling depends on Go test filenames; runtime identities and declarations are the stable test contract.

### Shared fixture placement

- Context/problem: `recordingWriteCloser`, `newUnitProvider`, and wait helpers are consumed by several transport behavior files and existing provider/URI tests; `capturingLogger`, `existingSignatures`, and `newTestTools` span ordinary and operation-budget tool tests.
- Chosen option: centralize only those cross-home declarations in narrowly named `transport_fixture_test.go` and `tools_fixture_test.go`; keep behavior-local providers, contexts, and writers beside their tests.
- Why: this avoids duplication without creating a package-wide generic helper bucket.
- Alternatives/trade-offs: duplicating fixtures improves local readability but creates parallel mutable test contracts; placing every fake in fixture files centralizes too much and obscures behavior ownership.
- Assumption: reference inventories accurately identify all package-local consumers; compilation and AST inventory check that no declaration was stranded.

### Coverage-profile normalization

- Context/problem: a combined two-package Go coverprofile can contain duplicate production blocks, with one package's record uncovered and the other covered; raw line-by-line profiles therefore differed despite identical aggregate `go tool cover -func` output.
- Chosen option: canonicalize each production block by source range and statement count, OR its duplicate hit states to a covered boolean, then sort and compare.
- Why: this preserves the requested covered/uncovered invariant while removing package execution-order and duplicate-record noise.
- Alternatives/trade-offs: raw counter comparison is nondeterministic; `cover -func` alone is stable but too coarse to prove block equality.
- Assumption: Go's block coordinates and statement counts uniquely identify production blocks within the unchanged source tree.

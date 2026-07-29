## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,259,264 bytes; finally 5,423,104 bytes)
- Invocations: 4/4 succeeded
- Most useful for:
  - locating the tools-owned operation context and telemetry lifecycle
  - inventorying the six `LanguageProvider` stages across the three tools
  - distinguishing provider-stage arbitration from local traversal cancellation and `internal/lsp` transport cancellation
  - confirming the final `runProviderStage` owner and its test blast radius
- Limitations:
  - broad results were truncated and sometimes returned symbol skeletons instead of the exact test bodies needed for editing
  - the final query reported four callers of `runProviderStage` because it grouped by calling function, not the six invocation sites
  - the final query included an irrelevant telemetry diagnostic implementation
  - Markdown policy and architecture discovery required direct reads
- Fallbacks:
  - direct reads of `AGENTS.md`, `ENGINEERING.md`, `TESTING.md`, `PLAN.md`, and `ARCHITECTURE.md` because repository policy and Markdown are outside CodeGraph's useful symbol flow
  - a bounded read of `internal/tools/operation_budget_test.go` because CodeGraph returned a skeleton/truncated body for the exact test fixture being extended
  - targeted `rg` after exploration to count every provider invocation and every post-return `ctx.Err()` arbitration site
  - `go doc`, `go list`, and narrow `git log` inspection to satisfy repository ownership and history discovery requirements

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `Issue 45 provider-stage invocation completion-versus-cancellation arbitration in internal/tools; inventory current call sites, provider results, telemetry, freshness, caps, soft errors, cancellation tests` | Success | Located `Tools.runTool`, `operationContext`, `LanguageProvider`, LSP cancellation, and behavioral tests; established the package boundary and preservation contracts. | Broad output was truncated; follow-up focused exploration was needed. |
| 2 | `internal/tools all calls to core.LanguageProvider methods DocumentSymbols Definition References Hover provider stage ctx.Err post-return arbitration find_definition find_references get_outline` | Success | Enumerated `DocumentSymbols` x3, `Definition`, `References`, and the provider interface, exposing the full provider seam. | `SymbolSignatures` and exact call-site bodies required a more focused follow-up; targeted `rg` later verified counts. |
| 3 | `full source FindDefinition FindReferences GetOutline provider calls SymbolSignatures ctx.Err arbitration and existing operation budget cancellation tests fixtures blocking providers` | Success | Returned all three tool call paths, all six post-return guards, `SymbolSignatures`, and the existing cancellation-test owners. | The operation-budget test was returned largely as a skeleton and the long output was truncated; a bounded direct read supplied the exact edit region. |
| 4 | `After issue 45 changes, inventory internal/tools provider-stage calls and runProviderStage ownership; verify direct LanguageProvider invocations, post-return cancellation arbitration, tests and documentation` | Success | Confirmed `runProviderStage` as the tools-owned boundary and showed its implementation, package contract, tests, and dynamic provider boundary. | Caller count grouped six invocations into four calling functions and included irrelevant telemetry code; targeted `rg` proved the six-site inventory and sole arbitration statement. |

## Decision log

### Provider-stage owner shape

- Context/problem: Go methods cannot introduce their own type parameters, while all provider stages return different result types and must share one acceptance decision.
- Choice and why: use a package-local generic function, `runProviderStage`, whose callback receives the authoritative operation context. It invokes the provider, preserves provider errors, checks cancellation after a successful return, and zeroes rejected values. This is the smallest reversible owner that mechanically prevents callers from accepting late non-zero results.
- Alternatives and trade-offs:
  - one non-generic helper per result type would duplicate the invariant and recreate multiple write paths;
  - an `any`-based method would lose compile-time result typing;
  - moving arbitration into `internal/lsp` would miss non-LSP providers and absorb issue #43's distinct transport/finite-work scope.

### Regression synchronization

- Context/problem: cancellation called serially inside a fake provider would demonstrate post-return checking but not cancellation concurrent with an in-flight provider stage.
- Choice and why: coordinate callback entry, cancellation from the test goroutine, and callback release with channels, then assert `context.Canceled` and a zero result. The ordering is deterministic without timing sleeps and exercises the real caller/provider concurrency shape.
- Alternatives and trade-offs:
  - sleeps would be flaky;
  - serial cancellation would not meet the concurrency criterion;
  - an LSP integration test would be slower and test the wrong owner.

### Documentation scope

- Context/problem: the tools request path and ownership claim changed, but package dependencies, public protocol shapes, and phase decisions did not.
- Choice and why: update the package contract and the existing operation-budget paragraph in `ARCHITECTURE.md`; leave `PLAN.md` and diagrams unchanged because no phase decision, package edge, client face, or diagrammed component changed.
- Alternatives and trade-offs: changing `PLAN.md` or Mermaid diagrams would imply an architecture/phase change that did not occur and create documentation churn.

## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,312,512 bytes; finally 5,472,256 bytes)
- Invocations: 4/4 succeeded
- Most useful for:
  - locating `requestSignatures` as the remaining synchronous hover-conversion path;
  - confirming `runFiniteWork` as the package owner for finite completion-versus-cancellation arbitration;
  - identifying the bounded fanout, first-error state, and existing cancellation/race coverage around the change;
  - verifying the final production path invokes the hover decoder only inside `runFiniteWork` and publishes its returned local value afterward.
- Limitations:
  - broad results were sometimes truncated and one query produced an irrelevant lifecycle call path or telemetry file;
  - the final query did not include the newly added tests despite naming them, and classified the injected decoder type as uncovered even though the cancellation test exercises it.
- Fallbacks:
  - read `AGENTS.md`, `ENGINEERING.md`, `TESTING.md`, `PLAN.md`, and `ARCHITECTURE.md` because Markdown policy and architecture documents are outside useful symbol exploration;
  - used `gh issue view` for the authoritative issue text and acceptance criteria;
  - read the bounded relevant portion of `internal/lsp/signatures_test.go` after CodeGraph truncated it, then used targeted `rg` to verify every changed `requestSignatures` call site;
  - used `go doc ./internal/lsp` and targeted `git log` to verify package ownership and recent finite-work history.

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `Issue 70 hover conversion finite-work arbitration: requestSignatures runFiniteWork hover conversion cancellation first-error bounded fanout. Show ownership, call paths, and relevant tests/docs.` | Success | Found the direct synchronous decoder assignment, worker fanout, wait, cancellation ordering, `runFiniteWork`, and owner tests. | `signatures.go` and tests were partially trimmed; policy/docs required direct Markdown reads. |
| 2 | `requestSignatures decodeHoverSignature signaturePlan maxConcurrentSignatureRequests complete implementation and all tests, including first-error behavior and cancellation helpers. How should hover conversion be passed through runFiniteWork without late mutation?` | Success | Confirmed the production caller and direct assignment seam; exposed that existing tests covered fanout and request cancellation but not blocked conversion. | Reported an irrelevant lifecycle call path and still trimmed the implementation/test file. |
| 3 | `How do internal/lsp tests deterministically block or inject finite response conversion? Show decodeLocations/decodeDocumentSymbols runFiniteWork wrappers, conversion test hooks or injected function patterns, and first-error tests for requestSignatures.` | Success | Showed established `runFiniteWork` wrapping in response conversion and confirmed no existing decoder-injection or first-error test. | Output truncated; a narrow read of the existing signature test area was needed to edit safely. |
| 4 | `After issue 70 changes, show requestSignatures hoverDecoder decodeHoverSignature runFiniteWork call path, local publication behavior, bounded fanout and tests ReturnsBeforeLateDecode PreservesFirstError BoundsConcurrency. Identify any remaining hover conversion bypass.` | Success | Verified `SymbolSignatures -> requestSignatures -> runFiniteWork -> decode`, with assignment only from the arbitrated local result; no production hover-conversion bypass appeared. | Returned an irrelevant telemetry file and omitted the named new test bodies, so focused tests and race execution supplied proof. |

## Decision log

- **Context/problem:** deterministic RED coverage must pause hover conversion after the request has completed, but production called `decodeHoverSignature` directly.
  - **Chosen:** pass an unexported per-invocation `hoverDecoder` into `requestSignatures` and supply `decodeHoverSignature` from production.
  - **Why:** enables channel-controlled tests without mutable package-global state, preserves one production path, and is race-safe.
  - **Alternative:** a package-global decoder override was rejected because concurrent tests could race and production would gain mutable test-only state.
- **Context/problem:** late conversion must not mutate state returned by `requestSignatures`.
  - **Chosen:** let the `runFiniteWork` closure return a local string, then assign `signatures[index]` only after arbitration succeeds.
  - **Why:** cancellation returns the zero value/error while the late goroutine can only publish to `runFiniteWork`'s buffered private result channel.
  - **Alternative:** assigning inside the closure was rejected because it would retain a write path after the caller returned.
- **Context/problem:** preserve first-error semantics while adding conversion cancellation.
  - **Chosen:** leave `sync.Once`, worker count, job dispatch, and wait ordering intact and add deterministic channel-ordered coverage.
  - **Why:** this is the smallest reversible owner-level change and directly proves the existing contract.

## CodeGraph evaluation

- Tool available: yes
- Index: `.codegraph/codegraph.db` (initially 5,492,736 bytes; finally 5,521,408 bytes)
- Invocations: 1/1 succeeded
- Most useful for:
  - Locating the signature-planning switch, the core-owned `SymbolKindFunction` and `SymbolKindClass` constants, the `signaturePlan` callers, and existing zero-width TypeScript signature coverage in one query.
- Limitations:
  - The result included unrelated `internal/pathnorm` material and trimmed parts of the test file.
  - It did not by itself prove that the raw case labels were the only bypasses at the owned path.
- Fallbacks:
  - `go doc ./internal/core` verified package ownership and the exported `SymbolKind` contract.
  - Targeted `rg` against `internal/lsp/signatures.go`, `internal/lsp/signatures_test.go`, and `internal/core/types.go` verified the exact raw case labels before editing and the canonical case labels with no raw case-label bypass afterward.
  - `git log` was used narrowly to confirm the recent ownership migration history.

### Per-call report

| # | Query | Result | Useful discovery | Gap / fallback |
|---|---|---|---|---|
| 1 | `internal/lsp/signatures.go signature planning normalized symbol kind checks; core.SymbolKindFunction core.SymbolKindClass ownership; callers and tests covering declaration-token relocation for zero-width TypeScript callbacks/declarations` | Success | Returned `core.SymbolKindClass`, `core.SymbolKindFunction`, the raw cases in `planSignature`, its call path through `SymbolSignatures`, and `TestSymbolSignaturesFromTsgo` coverage. | Included unrelated path-normalization results and trimmed test tails; followed with narrow `go doc`, `rg`, and `git log` checks. |

## Decision record

### Test evidence for a behavior-preserving ownership repair

- Context: the requested change replaces string case labels with typed constants whose current values are identical. Runtime signature-planning behavior must remain unchanged.
- Chosen option: rely on the existing focused `internal/lsp` signature tests, plus a negative search for raw `case "function"` / `case "class"` labels and a positive search for both canonical constants at this path.
- Why: this directly proves both behavior preservation and the architectural prohibition. A new behavioral test could not fail before the change, so it would not provide valid RED evidence. The existing real-tsgo test already exercises zero-width function and class declaration-token relocation.
- Considered options:
  - Add a new behavioral regression test: rejected because identical constant values make it pass before and after, adding duplicate coverage without proving canonical ownership.
  - Add a permanent source-shape test: rejected as disproportionate for two typed switch labels; it would couple tests to local source layout when the requested one-time targeted search supplies deterministic acceptance evidence.
  - Change the signature-planning seam or kind representation: rejected as outside C1 and unnecessary because `internal/lsp` already imports the canonical owner.
- Safest reversible assumption: no test file or documentation change is needed because public behavior, serialization, package boundaries, and architecture diagrams do not change.

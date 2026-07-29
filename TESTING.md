# TESTING.md

Automated-test policy for `portolan`. [`AGENTS.md`](./AGENTS.md) owns the conventions that apply to
every change, including the verification gate every change must pass. **This file is the canonical
copy of the rules below**; read it before writing or changing a test, an eval, or test
infrastructure, and do not restate its rules in `AGENTS.md`.

## Test layers

Four layers, each owning a different class of failure. Unit tests alone cannot cover this system:
its hardest invariants live across a subprocess boundary, a protocol wire, or a lifecycle.

| Layer | Where | Runs against | Owns |
| --- | --- | --- | --- |
| Unit | `internal/tools`, `internal/pathnorm`, `internal/telemetry`, most of `internal/mcp` | `core.StubProvider`, capturing loggers, in-process fakes | Behavior that is a function of its inputs: caps and `Truncated`, freshness stamping, one-event-per-path, path normalization, honest-empty and soft-error results |
| Integration | `internal/lsp` | A real `tsgo --lsp -stdio` subprocess | LSP semantics we do not define: symbol shapes, definition/reference resolution, transport framing, concurrent request handling |
| End-to-end | `eval/tiera`, `eval/lifecycle` via `eval/testinfra` | The real daemon: process, MCP transport, control socket, signals | Cross-process invariants: retrieval correctness over MCP, startup and shutdown ordering, socket ownership and staleness |
| Measurement | Tier B and Tier C (see [`PLAN.md`](./PLAN.md), [`EVAL.md`](./EVAL.md)) | Harness × model × graph-mode cells | Navigation efficiency and task capability. Statistical and quota-bound, not pass/fail |

Choose the layer by pushing the test down until it can no longer fail for the reason you care about,
then stop. Specifically:

- Behavior derivable from inputs belongs in a unit test with a stub. Stubbing is not a shortcut
  here — it is what makes the tool contract in `AGENTS.md` cheap enough to assert on every path.
- Anything crossing a **process boundary or protocol wire** belongs at the integration or
  end-to-end layer. Faking one asserts our assumptions about the dependency, not the dependency.
- Anything with **shared mutable state or a lifecycle** spans two layers at once, under the
  one-end-to-end-home rule below. Do not try to prove a race through an end-to-end test, and do not
  let an owner-level race test stand in for the user-visible invariant.
- When a value or invariant crosses packages, processes, or protocol boundaries, one integration or
  end-to-end test follows it from its authoritative producer through a final consumer, including
  the relevant live-state transition. Package-local mocks do not prove propagation completeness.

Measurement suites never join the `go test ./...` gate. Tier A is the exception and is already a
gate, because retrieval correctness is pass/fail on a pinned fixture.

The integration and end-to-end layers skip when the pinned `tsgo` or Unix sockets are unavailable.
**A skip is not a pass.** Do not complete a change whose only coverage sits in a layer that skipped
locally; report any skip in the gate output. Set `PORTOLAN_REQUIRE_TSGO=1` for required eval
execution: missing or incompatible `tsgo` then fails instead of skipping.

## Red-green procedure

Develop behavioral changes red-green at the lowest deterministic layer:

1. Write or extend the behavior test first.
2. Confirm it fails against pre-change code for the expected reason. Use an isolated worktree when
   the current tree has changes; never checkout over another agent's work.
3. Implement the complete fix, including required ownership changes.
4. Run focused tests, then the full gate in `AGENTS.md`, and report actual output.

Concurrency and lifecycle changes require focused race coverage in addition to running the
race-enabled packages.

## Test integrity

Tests must:

- use durable behavior names, never PR, issue, review-round, or author identifiers;
- extend the existing behavior area instead of adding a parallel regression file;
- keep one end-to-end home per user-visible or protocol invariant, with race interleavings and state
  transitions tested directly on their owner;
- never weaken or delete an assertion merely to make output pass;
- derive expected values independently of the production decision under test;
- use source-shape assertions only for exact architectural prohibitions that behavioral tests cannot
  express cheaply; target a specific forbidden construct, import, or bounded count rather than a
  broad substring-presence check; and
- reuse `eval/testinfra` for process startup, readiness, cleanup, and environment setup.

When changing an existing test, account for every removed case, assertion, platform path, and
normalization property. Adapt the old regression contract before adding coverage for a new one.
Organize tests by behavior and split files that have become collections of unrelated regressions.

## Eval gates

`eval/tiera` is the real-daemon MCP retrieval gate; `eval/lifecycle` covers startup and shutdown.
Keep both green. Eval suites use `eval/testinfra`, never a second daemon harness.

Tier A is authored against `tsgo` `Version 7.0.0-dev.20260707.2`. Install the matching
`@typescript/native-preview@7.0.0-dev.20260707.2` package and run the required gate with:

```bash
PORTOLAN_REQUIRE_TSGO=1 go test -count=1 ./eval/tiera
```

This mode fails on a missing or incompatible analyzer rather than accepting a skip.

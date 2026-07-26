# AGENTS.md

Conventions that apply to every change in `portolan`. For system intent and sequence, read
[`ARCHITECTURE.md`](./ARCHITECTURE.md) and [`PLAN.md`](./PLAN.md). For automated-test policy, read
[`TESTING.md`](./TESTING.md).

**This is the canonical copy.** [`CLAUDE.md`](./CLAUDE.md) imports it; never duplicate these
instructions there.

## Before changing code

- Check `git status`, `git worktree list`, and whether another agent is active. Preserve existing
  work; one writer per worktree, with a separate worktree for overlapping scopes.
- For issue work, create a dedicated branch before editing and open a PR after validation.
- Read the relevant parts of `PLAN.md`, `ARCHITECTURE.md`, and linked decision documents.
- Keep changes cohesive. Include structural work required for correctness and maintainability, but
  report unrelated defects instead of mixing them into the change.

Before adding or relocating a package, shared helper, type, constant, protocol shape, or stateful
mechanism:

1. Run `go doc ./internal/<pkg>` and inspect its imports and consumers one hop out. Read exported
   names and signatures before bodies.
2. Search for the concept and for code serving the same role under a different name.
3. Check recent history when the file is stateful or repeatedly fixed.
4. Name the owner in one sentence: “X owns Y.”

Apply this proportionally; a local implementation detail needs no architecture report. Mention the
search and ownership result at handoff when it affected the design.

For a cross-package migration of a shared type, protocol shape, identity, or state representation,
record a search-derived inventory of its producers, consumers, serializers, validators,
default/fallback sites, identity or cache-key builders, and superseded helpers. Migrate every
production occurrence or document why it is unaffected. Repeat the inventory against the final diff.
When work is delegated, one integrator owns the shared contract, merged inventory, and end-to-end
acceptance. Delegated slices do not independently redesign the seam or add local adapters to avoid
it.

## Ownership and boundaries

A domain decision or invariant has one owner. Duplication is a defect when two locations can
independently change the same behavior; similar local mechanics are not automatically one concept.

- Constants live with the package owning their meaning. `internal/core` contains only stable
  cross-package contracts and defaults, not general utilities.
- Callers use an owning accessor or formula instead of importing its raw values and rebuilding part
  of the decision.
- Shared helpers, fixtures, protocol adapters, and lifecycle mechanisms have one implementation.
  Extend that owner rather than creating a local fork.
- Values crossing a process, protocol, or persistence boundary are decoded, validated, and
  defaulted once by the owning package. Downstream code receives a valid domain value or an
  explicit error; it does not reconstruct the value from primitives, assign semantic meaning to an
  empty value, or maintain a local validator.
- Existing violations are debt, not precedent. When touched, bring the concept under one owner.
- Never bypass, inline, or delete a shared abstraction merely to close a bug or review finding.
  Repair or replace it and migrate all callers coherently.

Account for deletions by responsibility: state which behavior, invariant, API, test coverage, or
documentation claim disappeared and why. Never silently drop an assertion, error path, platform
case, or shared owner.

Allowed production dependencies and stable ownership:

| Package | Owns | May import |
| --- | --- | --- |
| `internal/core` | `LanguageProvider`, shared result/telemetry contracts, `Config` | standard library |
| `internal/pathnorm` | WSL/Windows path normalization | standard library |
| `internal/lsp` | LSP JSON-RPC, transport, URI conversion, provider | `internal/core` |
| `internal/telemetry` | JSONL, OTel, composed sinks | `internal/core` |
| `internal/tools` | Tool behavior and shared call policy | `internal/core`, `internal/pathnorm` |
| `internal/mcp` | MCP adaptation and control-socket lifecycle | `internal/core`, `internal/tools` |
| `cmd/portoland` | Concrete wiring | internal packages as composition requires |
| `eval/testinfra` | Real-daemon startup and teardown for evals | test infrastructure only |

Tests and evals may depend on the package exercised and `eval/testinfra`. Keep dependencies acyclic.
A new production edge means responsibility moved; update the package graph in `ARCHITECTURE.md` in
the same change.

Nothing outside `internal/lsp` parses LSP JSON. Telemetry users depend on `core.Logger`, not a sink.
Caller-supplied tool paths enter through the tools normalization boundary, not ad hoc `filepath.Abs`
calls.

Prefer the standard library and existing `go.mod` dependencies. A new dependency requires verified
maintenance/fit evidence and PR justification; consult the user only when the choice materially
changes architecture or long-term integration direction.

## Accessors and tool contracts

- Effective caps come from `Config.Cap()`; callers do not read `DefaultMaxResults` or repeat its
  fallback.
- Freshness comes from `GenerationCounter.Current()`; tools do not construct `core.Freshness`.
- A formula owns every bound of its decision; callers do not centralize one side and recreate the
  other.
- Each cache or deduplication mechanism has one canonical identity builder used for lookup,
  insertion, invalidation, and telemetry correlation. Its key includes every semantic discriminator;
  changing it preserves or deliberately revises each documented normalization property and its
  tests.

Every tool call:

1. snapshots freshness at the beginning;
2. emits exactly one `core.Event` on every success, empty, and failure path;
3. uses shared event initialization/emission in `internal/tools`; if the initializer is missing, add
   it instead of reproducing telemetry fields;
4. normalizes every caller-supplied path at the tools boundary;
5. returns “found nothing” as an honest structured result;
6. surfaces provider failures as soft output errors rather than panics; and
7. honors `ctx` and bounded per-request timeouts.

List-returning tools also cap through `Cfg.Cap()` and derive `Truncated` from that decision. New tools
satisfy these rules through shared mechanisms rather than by copying an existing method.

## State and structural ratchets

- Retries, in-flight tracking, ordering, shutdown sequencing, socket ownership, and rollback belong
  in a named, unit-testable owner, not flags scattered across handlers or server structs.
- A mutable fact has one write path. Derived views and caches are not independent sources of truth;
  changes flow through the owner's transition API. Enumerate the applicable creation, update,
  reconnect or restart, recovery, invalidation, and shutdown transitions before changing that state.
  Adding another synchronization writer is a signal to consolidate ownership instead.
- When a fix would add another guard, flag, counter, fallback, or rollback branch to already complex
  state, consolidate the state and fix its owner in the same change.
- When a stateful seam appears across more than two consecutive fix commits, refactor its ownership
  now instead of stacking another patch.
- Migrate an invariant atomically. Do not leave parallel old/new mechanisms unless an evidenced
  external compatibility requirement demands it.
- Do not add speculative compatibility shims, fallbacks, retries, or defensive branches without an
  evidenced caller or failure mode.
- Tool calls, control handlers, LSP reader paths, filesystem watchers, telemetry emission, and
  polling loops do not perform unbounded or serial per-item external work. A timeout bounds the
  whole operation, not merely each item; subprocess, network, and disk work is batched, cached, or
  concurrency-limited as appropriate and always honors cancellation.

No non-test `.go` file may exceed 400 lines. Pre-existing oversized files may not grow; split their
responsibilities in the same change when modifying behavior there. Do not evade the limit with
meaningless `helpers.go`, `utils.go`, or `misc.go` files—name files for the responsibility they own.

Comments and package docs describe current contracts and non-obvious invariants, not implementation
history, review narratives, phases, or temporary reasoning.

## Verification

Develop behavioral changes red-green at the lowest deterministic layer, then run the full gate:

```bash
go build ./...
go vet ./...
gofmt -l internal/ cmd/ eval/   # must print nothing
go test ./...
```

All four must pass before completion; report actual output. Concurrency and lifecycle changes also
require `go test -race` on affected packages. Go 1.26+ and `tsgo`
(`npm i -g @typescript/native-preview`) must be available; LSP and Tier A checks depend on
`tsgo --lsp -stdio`.

[`TESTING.md`](./TESTING.md) owns the test layers, the red-green procedure, test-integrity rules, and
the eval gates. Read it before writing or changing a test, an eval, or test infrastructure.

## Protected files and documentation

- `PLAN.md` is the human-authored source of truth for sequence and decisions. Change it only when
  those decisions change, deliberately and with a date.
- `AGENTS.md` is canonical; `CLAUDE.md` remains its import. `TESTING.md` is canonical for
  automated-test policy; link to it instead of restating its rules here.
- Phase status reflects verified exit criteria, never intention.

Ship documentation with the behavior it describes. Update the relevant `ARCHITECTURE.md` Mermaid
diagrams when packages, dependencies, tools, request paths, daemon components, client faces, the
staleness barrier, or phase status change. Keep README status and links accurate; any manual test
count must come from current output. If diagrams change, mention that the rendered Claude Code
artifact needs republishing. If a diagrams-only reviewer would be misled, the change is incomplete.

## Repository hygiene

- Never commit to `main`; use a branch. For issue work, open a PR after the full gate passes.
- Keep commits cohesive and describe the invariant or responsibility changed, not the review round.
  Rename the branch if its cohesive scope changes.
- Do not commit build artifacts, `/portoland`, telemetry JSONL, temporary database/socket state, or
  worktree-specific configuration.
- Before handoff, inspect the final diff and status for temporary verification edits and unrelated
  files.

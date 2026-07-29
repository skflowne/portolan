# AGENTS.md

Conventions for every change in `portolan`. This is canonical; [`CLAUDE.md`](./CLAUDE.md) imports it. Read the relevant parts of [`PLAN.md`](./PLAN.md) for decisions and sequence and [`ARCHITECTURE.md`](./ARCHITECTURE.md) for current system and package structure.

Read [`ENGINEERING.md`](./ENGINEERING.md) before adding or relocating a package, shared helper, type, constant, protocol shape, identity, cache, or stateful mechanism; adding a production dependency or import edge; changing package boundaries, tool behavior or call paths, accessors, mutable state or transitions, synchronization, lifecycle, concurrency, retries, handlers, watchers, polling, or other external-work paths; or performing a cross-package migration. Read [`TESTING.md`](./TESTING.md) before changing behavior, tests, evals, or test infrastructure.

## Workflow

- Before editing, check `git status`, `git worktree list`, and active agents. Preserve existing work and keep one writer per worktree; isolate overlapping writers.
- Issue work uses a dedicated branch and, after validation, a PR. Never commit to `main`.
- Trace the current path to its owner before editing. Make the smallest complete, cohesive change through that owner and its callers. Include migrations required for correctness, but report unrelated defects instead of mixing them in.
- Consult the user before unapproved product, dependency, integration, or architecture choices. Prefer the standard library and existing `go.mod` dependencies; new dependencies require verified maintenance/fit evidence and PR justification.
- Account for deletions by responsibility. State which behavior, invariant, API, test coverage, assertion, error path, platform case, shared owner, or documentation claim disappeared and why.

## Core rules

- A domain decision or mutable fact has one owner and one write path. Extend or repair shared owners; do not bypass them, recreate their decisions downstream, or add parallel mechanisms or speculative compatibility behavior.
- Fix uncovered user-visible bugs with regression coverage at the lowest deterministic layer. Develop behavioral changes red-green as specified in `TESTING.md`.
- No non-test `.go` file may exceed 400 lines. Pre-existing oversized files may not grow; split by responsibility, never into generic `helpers.go`, `utils.go`, or `misc.go` buckets.
- Comments are exceptional: explain only non-obvious invariants, safety constraints, or dependency behavior and why they matter. Never narrate obvious code, tasks, issues, reviews, phases, temporary reasoning, or implementation history. Package docs may state current contracts and ownership.

## Verification

Assume an independent reviewer will verify every change and completion claim against the request, these rules, the final diff, and actual command output. Rule violations, skipped validation, and unsupported claims will be rejected.

Run and report the full gate:

```bash
go build ./...
go vet ./...
gofmt -l internal/ cmd/ eval/   # must print nothing
go test ./...
```

All four must pass before completion. Concurrency and lifecycle changes also require `go test -race` on affected packages. A skip is not a pass. Go 1.26+ and the Tier A-pinned `tsgo` (`npm i -g @typescript/native-preview@7.0.0-dev.20260707.2`) must be available; LSP and Tier A checks depend on `tsgo --lsp -stdio`. Use `PORTOLAN_REQUIRE_TSGO=1` for required eval execution that fails on a missing or incompatible analyzer.

## Documentation and handoff

- `PLAN.md` is the human-authored source of truth for dated decisions and sequence; change it only deliberately when those decisions change. `TESTING.md` owns automated-test policy. Keep `CLAUDE.md` as an import only.
- Phase status reflects verified exit criteria, never intention. Ship documentation with behavior. Update `ARCHITECTURE.md` diagrams when packages, dependencies, tools, request paths, daemon components, client faces, lifecycle, staleness, or phase status change. Keep README status, links, and measured counts current. Mention that changed diagrams need republishing; if a diagrams-only reviewer would be misled, the change is incomplete.
- Keep commits cohesive and describe the changed invariant or responsibility, not a review round. Rename the branch if its cohesive scope changes.
- Do not commit build artifacts, `/portoland`, telemetry JSONL, temporary database/socket state, or worktree-specific configuration.
- Before handoff, inspect the final diff and status for unrelated or temporary verification edits. Report ownership searches when they affected the design and report actual validation output.

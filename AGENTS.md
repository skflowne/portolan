# AGENTS.md

Working conventions for agents and contributors on `portolan`. Read this before making
changes. For *what* the system is and *why*, see [`ARCHITECTURE.md`](./ARCHITECTURE.md) and
[`PLAN.md`](./PLAN.md).

## Toolchain

- **Go** (1.26+) — the daemon. `tsgo` (`npm i -g @typescript/native-preview`) must be on `PATH`;
  the daemon spawns `tsgo --lsp -stdio`, and the LSP + Tier A tests skip or fail without it.
- Dependencies are already in `go.mod` (MCP Go SDK, OpenTelemetry). Prefer not to add new ones;
  if you must, do it deliberately and explain why in the PR.

## Build, vet, test

```bash
go build ./...        # whole tree must build
go vet ./...          # must be clean
gofmt -l internal/ cmd/ eval/   # must print nothing
go test ./...         # all packages green (includes eval/tiera, which spawns the real daemon)
```

The Tier A gate (`eval/tiera`) is the retrieval-correctness regression net: it drives the actual
`portoland` binary over MCP against a pinned TS fixture. Keep it green — a red Tier A means navigation
correctness regressed.

## Code conventions

- **`internal/core` is the frozen contract center.** The `LanguageProvider` interface,
  result/telemetry types, and `Config` live there; every other package depends *inward* on it and
  not on each other. Change `core` only with intent — it ripples everywhere.
- Every list-returning tool **caps** results (`Cfg.Cap()`), sets `Truncated`, stamps `Freshness`,
  and emits **exactly one** telemetry `Event`. "Found nothing" is an honest empty result, not a Go
  error; a provider failure is a *soft* error surfaced in the output, never a panic.
- Bounded waits everywhere — never hang the model. Honor `ctx` and per-request timeouts.
- Match the surrounding style; keep `gofmt`/`go vet` clean; add tests with every behavioral change.

## Keeping documentation current (required)

Docs are part of the change, not an afterthought. When a change alters the system, update the docs
**in the same commit/PR**:

- **`ARCHITECTURE.md`** — update the relevant Mermaid diagram(s) whenever you:
  - add/remove/rename a package or change the dependency graph → *Package dependency graph*;
  - add or change a tool, or the request path → *A tool call, end to end*;
  - change the daemon's components or the two client faces → *System architecture*;
  - implement or change the staleness barrier → *The staleness barrier*;
  - complete or reshape a phase → *Phase roadmap* (flip the phase's status/legend color).

  The diagrams carry a built-vs-scaffold color legend — keep it truthful (green = implemented,
  amber = scaffold). If a diagram no longer matches the code, it is a bug.
- **`README.md`** — keep the **Status** line and doc list accurate (current phase, test count).
- **`PLAN.md`** — the source of truth for sequence/decisions; if the plan itself changes, edit it
  and note the date.
- The rendered artifact of `ARCHITECTURE.md` (Claude Code) can be refreshed by re-publishing the
  same file to its existing URL — mention it in the PR if the diagrams changed.

Rule of thumb: **if a reviewer reading only the diagrams would be misled by your change, the change
is incomplete.**

## Commits & branches

- Default branch is `main`.
- Small, focused commits with a clear subject line. Don't commit build artifacts (`/portoland` is
  gitignored) or the telemetry JSONL stream.

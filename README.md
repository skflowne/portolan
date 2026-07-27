# portolan

A **code graph** (AST + symbol/reference graph) served to coding agents over MCP, to reduce
token usage and improve the model's understanding and correctness of a codebase — letting the
model answer relational questions ("where is this type, what are its properties, where is it
used") by graph lookup instead of expensive text search.

portolan is **not a harness**. It is an augmentation layer for harnesses you already use:
a portable daemon that plugs into Claude Code, Pi, or any MCP client, plus thin per-harness
adapters. (A *portolan chart* is a navigational map drawn as lines connecting ports — a graph
of how places reach each other, rather than a picture of the terrain.)

## Status

**Phase 0 complete** — walking skeleton: a Go daemon (`portoland`) with an MCP stdio server, three
passthrough tools (`find_definition` / `find_references` / `get_outline`) over a `tsgo --lsp`
provider, deterministic daemon/control-socket/LSP cancellation lifecycle, JSONL telemetry,
WSL↔Windows path handling, and a Tier A retrieval-correctness gate that drives the real daemon over
MCP. The repository has **114 automated tests**. **Phase 1 (the staleness barrier) is next.**

Docs:
- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — **visual architecture** (Mermaid diagrams): components, request flow, the staleness barrier, package graph, phase roadmap.
- [`INITIAL_RESEARCH.md`](./INITIAL_RESEARCH.md) — evidence: tokens vs correctness, prior art.
- [`INTEGRATION_CONSTRAINTS.md`](./INTEGRATION_CONSTRAINTS.md) — building into Claude Code; decisions + problem map.
- [`EVAL.md`](./EVAL.md) — how we measure whether it actually helps (from day one).
- [`PLAN.md`](./PLAN.md) — architecture + phased build sequence.
- [`PHASE0_PARALLEL.md`](./PHASE0_PARALLEL.md) — how Phase 0 was decomposed for a parallel build.
- [`AGENTS.md`](./AGENTS.md) — working conventions for agents/contributors (build, test, keeping docs current).

## Direction (settled)

- **LSP-backed** deterministic typed symbol graph — correctness first (users install the LSP).
- **Daemon in Go**; ships as a **portable MCP daemon** (any MCP harness) + a thin **Claude
  Code adapter** (hooks + CLAUDE.md). Claude Code first.
- **First target: TypeScript** via **`tsgo --lsp`** (TS 7 native), out-of-process behind a
  `LanguageProvider` interface (polyglot via other LSP servers later).
- **Thin graph layer:** LSP passthrough for point queries; a lightweight materialized index
  (in-memory adjacency + SQLite) only for derived queries (repo-map/PageRank, blast-radius).
- **Staleness is the hard problem:** a blocking `PostToolUse` hook is a deterministic sync
  barrier; freshness metadata + model instruction back it up.
- **Never deny grep** — a search-strategy doc (always in context) teaches graph-vs-grep.
- **Two target harnesses:** Claude Code (rich, product-first) + Pi (minimal, bare-bones eval
  control). Thin adapters over the portable Go core.
- **Budget-shaped eval from day one:** free retrieval-correctness CI gate; a **local model
  (Qwen3-Coder-30B-A3B) carries free high-volume runs in both harnesses** while frontier arms
  (Claude in Claude Code, OpenAI in Pi) stay sparse. Measures the graph's effect, whether it
  helps weaker models more, and whether it generalizes across harnesses/model families.
- Windows ↔ WSL path handling as a first-class differentiator.

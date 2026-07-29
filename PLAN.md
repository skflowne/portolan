# PLAN — Build sequence

**Date:** 2026-07-26

**2026-07-26 telemetry note:** JSONL is the authoritative bounded local record (512 pending
records, 50 ms admission bound, 16 KiB record cap, two-second drain/fsync/close bound). OTEL is an
independent, opt-in OTLP/HTTP mirror with no stdout or implicit-localhost fallback. Loss and sink
failures are explicit through counters, stderr diagnostics, and shutdown errors.
**Reads with:** `INITIAL_RESEARCH.md` (evidence), `INTEGRATION_CONSTRAINTS.md` (decisions),
`EVAL.md` (measurement). This is the how and the order.

---

## Stack decisions
- **Daemon implementation: Go.**
- **First target language: TypeScript**, analyzed via **`tsgo --lsp`** (the TS 7 native Go
  compiler's LSP server) — out-of-process, behind a `LanguageProvider` interface. Other
  languages implement the same interface via their own LSP servers (pyright, gopls,
  rust-analyzer) later. **Everything is out-of-process LSP** — the old in-process TS
  Language Service is gone in TS 7 (Corsa dropped the Strada API; programmatic API is IPC,
  WIP until 7.1).
- **Target harnesses:** **Claude Code** (rich, product-first) + **Pi** (minimal, bare-bones
  control) — two thin adapters over the portable Go core. Pi's frontier arm is **OpenAI** —
  Anthropic's Max subscription is harness-locked (first-party only; subscription OAuth is barred
  from third-party tools), so Claude-in-Pi would need a pay-as-you-go API key, whereas OpenAI's
  API is harness-agnostic and runs cleanly in a minimal harness (which is what makes Pi usable as
  a frontier arm). See `EVAL.md`.
- **Eval:** no separate runner — Claude Code swaps backend via `ANTHROPIC_BASE_URL` (Claude /
  local via Ollama); Pi runs its own providers. Local model (Qwen3-Coder-30B-A3B) is the free
  common thread in both.

### Why Go (over TypeScript / Rust)
- The hardest, most correctness-critical component — the **staleness barrier + multi-LSP
  orchestration** — is a concurrency problem. Go's goroutines/channels map onto it directly,
  and `go test -race` catches the exact bug class (stale reads from races).
- **AI-authored Go is reliable** (simple, uniform, great stdlib) — matters because AI writes
  most of this. Async Rust is the least reliable for AI to generate; TS/Node is single-thread
  (worker_threads + uncatchable async races) — the worst runtime fit for the barrier.
- **MCP Go SDK is production-ready** (v1.4.x); Rust's is less prominent.
- **Same language as tsgo** — tooling/version affinity now, and first in line for an
  in-process option if MS opens the API in 7.1+ (blocked today: tsgo internals are under
  `internal/`, unimportable externally).
- The **graph layer is thin** (see below), so Rust's perf edge on a bespoke graph engine
  doesn't apply — we deliberately don't build that.

---

## Architecture

The diagram below is the **target state**, not the current Phase 0 implementation. One long-lived
**Go daemon** = the portable core, **two client faces on one process** (forced by staleness: the hook
and the model must share live LSP/graph state).

```
┌──────────────── Claude Code (harness adapter lives here) ───────────────┐
│  Model loop → built-in Grep/Read/Edit  +  MCP graph tools               │
│  CLAUDE.md search-strategy (always in context)                          │
│  Hooks:  SessionStart → inject PageRank repo-map + graph status         │
│          PostToolUse(Edit|Write) → BLOCKING sync barrier                │
└───────────┬───────────────────────────────────────┬────────────────────┘
   (1) MCP over stdio                    (2) control socket (project-keyed)
      graph tools                           "file X changed: sync + wait"
            │                                          │
            ▼                                          ▼
┌───────────────────── Graph Daemon — Go (portable core) ─────────────────┐
│  LSP client pool ....... tsgo --lsp (TS), pyright, gopls, rust-analyzer  │
│  Query router .......... point queries → LSP passthrough (no storage)   │
│  Materialized index .... in-memory adjacency + SQLite (derived queries) │
│  Freshness tracker ..... per-file dirty + monotonic generation          │
│  FS watcher ............ out-of-band edits (git, external editor)        │
│  Path normalizer ....... WSL ↔ Windows                                  │
│  Telemetry ............. JSONL + OTEL, session + graph_mode tagged       │
└──────────────────────────────────────────────────────────────────────────┘
```

**Current Phase 0 status:** the project-keyed control socket accepts `sync <file>` and bumps the
shared generation counter only. The harness hook, LSP `didChange`/`didSave`, and settle detection
shown above are Phase 1 target behavior and are not implemented yet. The materialized index and
`SessionStart` injection are Phase 2 targets.

**Cross-cutting principles:** signatures-not-bodies default · symbol-name-path addressing
(offsets shift under edits we didn't observe) · cap/paginate every tool · never deny grep ·
bounded waits everywhere · accept honest null results.

---

## The graph layer

The LSP **already holds the semantic graph.** So the layer is thin:

- **Point queries → LSP passthrough, no storage.** `find_definition`, `find_references`,
  `get_type`, `get_members`, implementations. Phase 1 targets always-fresh results by keeping the
  LSP current through the barrier; results remain shaped + freshness-tagged. Phase 0 is entirely
  passthrough plus the generation-bump scaffold.
- **Derived / aggregate queries → a lightweight materialized index** (an index *over* the
  LSP's knowledge), for what LSP does poorly:
  - repo-map ranking (PageRank over the symbol reference graph — the SessionStart injection),
  - impact / blast-radius (transitive closure),
  - interface → consumer expansion (multi-hop architectural tracing),
  - import/dependency overview.

**Node/edge schema (modest, structural — not every ref edge):**
```
Nodes:  symbol { id, name, kind, file, range, signature? } · file · module
Edges:  contains · imports · extends/implements · calls · references(sampled/lazy)
```
Precise reference edges (millions) are fetched on demand from the LSP, not stored. The
approximate ref graph for PageRank comes from a fast syntactic pass.

**Storage: in-memory adjacency (hot traversal / PageRank) + SQLite index
(`modernc.org/sqlite`, pure-Go, FTS5 for name search). No graph DB** — our patterns are
lookups + shallow traversals + PageRank; Kùzu/Neo4j would be overkill + heavy native deps.

**Target freshness design — two tracks off the same `PostToolUse` hook:** (1) the Phase 1 LSP
barrier for precise queries; (2) Phase 2 incremental re-parse of the edited file to patch the
materialized index.

Enters the build at **Phase 2** (repo-map) and **Phase 3** (impact). Phases 0–1 don't need it.

---

## The staleness barrier (the hard core — Phase 1)

In TS 7 all languages (incl. TS) are analyzed **out-of-process via LSP**, so there is no
in-process freshness freebie for anyone — but tsgo is ~10× faster, so re-analysis after an
edit is cheap, which keeps the barrier's wait short. Three-layer defense (deepest first):
1. **Deterministic barrier (primary):** blocking `PostToolUse` → tiny hook CLI → daemon
   control socket → LSP `didChange`/`didSave` → **wait for settle** → return. The model's
   turn cannot continue until the graph is current.
2. **Freshness metadata (safety net):** every result carries `generation` + `stale`.
3. **Model instruction (last resort):** search-strategy doc — how to react to `stale: true`.

**"Settle" detection research spike** (no universal LSP signal): in-order-request probe +
`$/progress` + diagnostics quiescence + **bounded wait (≤~1–2s) with generation tag**.
Never hang the model. Prototype on tsgo first (TS is target #1), other servers later.

---

## Phases

### Phase 0 — Walking skeleton + telemetry spine + Tier A scaffold
- Go daemon: MCP (stdio) + control socket, project-keyed path.
- **`tsgo --lsp` client** as the first `LanguageProvider` (out-of-process LSP).
- Tools v0: `find_definition`, `find_references`, `get_outline` — LSP-backed retrieval
  (capped compact semantic signatures when authoritative provider data is available, never bodies;
  carry `generation` + `stale` even if trivially fresh).
- **Path normalizer (WSL ↔ Windows) from the start.**
- **Telemetry spine (full stack):** bounded asynchronous JSONL event stream + explicit opt-in
  OTLP/HTTP exporter + session/`graph_mode` tagging. MCP stdout is protocol-only; telemetry
  diagnostics use stderr.
- **Tier A eval scaffold:** retrieval-correctness harness on a pinned TS repo.
- *Exit:* MCP round-trip works; every call logged; Tier A green on a pinned repo.

### Phase 1 — Staleness barrier + freshness + Tier A live
- Freshness tracker (per-file dirty, monotonic generation).
- Blocking `PostToolUse` hook → control socket → LSP sync → settle detection → return.
- FS watcher for out-of-band edits; debounce/coalesce rapid edits.
- `graph_status` tool; `stale`/`generation` on every result.
- **Tier A stale-correctness tests:** scripted edit sequences assert post-edit correctness
  (the barrier's regression gate).
- *Exit:* no stale reads under scripted edit races; barrier latency within budget.

### Phase 2 — Materialized graph + adoption layer + Tier B (thesis signal)
- Materialized structural index (in-memory + SQLite); incremental re-parse on edit.
- PageRank repo-map; `SessionStart` hook injects it (≤10k chars) + graph status.
- CLAUDE.md **search-strategy** (graph vs grep, stale-flag protocol) + strong tool descriptions.
- **Model-agnostic eval runner** + **Tier B (navigation efficiency):** `{local, Claude} ×
  {graph, no-graph}`, stratified by spread. Local carries volume; Claude quota-boxed. See
  `EVAL.md`.
- **Full observability:** OTEL token join by session_id + live dashboard.
- *Exit:* reproducible tokens-to-answer delta, sliced by spread and model.

### Phase 3 — Breadth (tools + languages), Tier C
- Tools: `get_type`, `get_members`, `get_callers`/`get_callees`, `who_imports`/`imports_of`,
  `impact` (blast-radius), `get_source`. Prioritize typed edges (`extends`/`implements`/
  `type-of`) + interface→consumer expansion; bidirectional traversal (`INITIAL_RESEARCH.md`
  §4c).
- Languages via LSP registry: pyright, gopls, rust-analyzer (install hints, graceful
  degradation on missing servers).
- **Tier C (task capability):** tiny curated TS set (~10–20) from Multi-SWE-bench /
  SWE-bench Multilingual TS subset + hand-curated multi-file tasks; `{local, Claude} ×
  {graph, no-graph}`; milestone-only.

### Phase 4 — Hardening & second harness adapter (Pi)
- Portable-core audit: zero Claude-Code assumptions in the daemon.
- **Pi adapter** — Pi is *minimal core + deeply extensible* (MCP/hooks are primitives you
  supply, not baked in). So we build our own integration as TS extensions: graph tools via a
  native Pi extension over the daemon; the **staleness barrier as an Edit/Write-wrapping
  extension** (Pi's own "hook" — expected fully doable, not a fallback; confirm the pre/post
  wrap API when building); repo-map via a Prompt Template/Skill.
- **Pi reproducibility:** Pi self-extends at runtime, so **freeze + pin the harness + extension
  set per eval run**, and keep graph-on/off identical except our stack — else the harness
  becomes an uncontrolled variable.
- Perf: LSP warmup, big-repo indexing, coalescing, caching.
- Dashboard polish; continuous eval in CI.

---

## Immediate next step

**As of 2026-07-26:** Phase 1 — implement the blocking edit-sync barrier, freshness tracking,
settle detection, filesystem watcher, `graph_status`, and stale-correctness regression gates.

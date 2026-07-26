# Architecture

Visual companion to `PLAN.md` (the *how* and the *order*), `INTEGRATION_CONSTRAINTS.md`
(decisions), and `EVAL.md` (measurement). Diagrams are [Mermaid](https://mermaid.js.org/) and
render natively on GitHub.

**One sentence:** a long-lived **Go daemon** (`cgraphd`) exposes an always-fresh, LSP-derived
code graph to a coding agent through **two faces on one process** — MCP tools for the model, and a
control socket for the harness's edit-sync barrier — so the agent navigates code by typed graph
lookup instead of grep.

---

## 1. System architecture

Two client faces share one process because *staleness forces it*: the edit-sync hook and the model
must see the same live LSP/graph state.

```mermaid
flowchart TB
    subgraph Harness["Harness adapter — Claude Code first (Pi later)"]
        direction TB
        Model["Model loop<br/>built-in Grep · Read · Edit"]
        Hooks["Hooks<br/>SessionStart · PostToolUse"]
    end

    subgraph Daemon["cgraphd — Go daemon (portable core)"]
        direction TB
        MCP["MCP server<br/>(stdio)"]
        Ctl["Control socket<br/>(project-keyed)"]
        Tools["Tools layer<br/>find_definition · find_references · get_outline"]
        Gen["GenerationCounter<br/>freshness source"]
        Prov{{"LanguageProvider<br/>(interface)"}}
        LSP["lsp.Provider<br/>tsgo --lsp client"]
        Path["pathnorm<br/>WSL ↔ Windows"]
        Tel["telemetry<br/>JSONL now · +OTEL later"]
    end

    Tsgo["tsgo --lsp -stdio<br/>(subprocess)"]
    Files[("source files")]
    JSONL[["telemetry.jsonl"]]

    Model -->|"(1) MCP tool calls"| MCP
    Hooks -->|"(2) 'file X changed: sync + wait'"| Ctl
    MCP --> Tools
    Tools --> Path
    Tools --> Gen
    Tools --> Prov
    Tools --> Tel
    Prov -. implemented by .-> LSP
    LSP <-->|"LSP JSON-RPC<br/>Content-Length framing"| Tsgo
    Tsgo -.reads.-> Files
    Ctl --> Gen
    Tel --> JSONL

    classDef built fill:#1f6f43,stroke:#0f3,color:#fff;
    classDef scaffold fill:#7a5c00,stroke:#fc0,color:#fff;
    class Model,Hooks,MCP,Tools,Gen,Prov,LSP,Path,Tel,Tsgo built;
    class Ctl scaffold;
```

**Legend:** green = implemented in Phase 0 · amber = Phase 0 *scaffold* (real socket + protocol, but
the blocking barrier logic lands in Phase 1). The materialized graph index (PageRank repo-map,
blast-radius) is deliberately **not** here yet — it enters at Phase 2.

**Control-socket lifecycle:** each daemon holds an advisory lock in a private per-user runtime
directory for the listener lifetime. Socket directories must be user-owned and non-writable by
other users; listeners publish with mode `0600`, authorize peer credentials, and replace only the
exact inode confirmed stale without overwriting a concurrent replacement. Shutdown closes the
listener first, marks shutdown under the connection mutex, closes every accepted connection
(including idle clients), and waits for handlers before removing only the socket inode this daemon
bound. The lock file remains in place so ownership release cannot race with path cleanup.

**Cross-cutting principles** (from `PLAN.md`): signatures-not-bodies · symbol-name-path addressing ·
cap/paginate every tool · never deny grep · bounded waits everywhere · accept honest null results.

---

## 2. A tool call, end to end

How `find_definition` resolves — note the name→position step (the "symbol-name-path addressing"
principle: the model names a symbol, the tool resolves it to an LSP position via the file outline,
because raw offsets shift under unobserved edits).

```mermaid
sequenceDiagram
    autonumber
    participant M as Model
    participant S as MCP server
    participant T as Tools layer
    participant P as lsp.Provider
    participant G as tsgo LSP
    participant L as JSONL log

    M->>S: tools/call find_definition {file, symbol}
    S->>T: FindDefinition(in)
    T->>T: normFile(file) — pathnorm (C:\… → /mnt/c/…)
    T->>P: DocumentSymbols(file)
    P->>G: textDocument/documentSymbol
    G-->>P: symbol tree
    P-->>T: []Symbol
    T->>T: resolve name → Position (SelRange.Start)
    T->>P: Definition(file, pos)
    P->>G: textDocument/definition
    G-->>P: Location[]
    P-->>T: []core.Location
    T->>T: cap at Cfg.Cap() · stamp Freshness{gen, stale:false}
    T->>L: emit exactly one Event (tool, duration, size, …)
    T-->>S: FindDefinitionOutput{found, locations, freshness}
    S-->>M: structured result
```

The `lsp.Provider` is concurrency-safe: one background reader goroutine demuxes responses by
JSON-RPC id into per-request channels, writes are mutex-serialized, and every call is bounded by a
timeout so the model is never left hanging. It also answers tsgo's server-initiated
`client/registerCapability` request (which carries a *string* id) with `MethodNotFound` — otherwise
tsgo stalls its whole request queue waiting for a reply.

---

## 3. The staleness barrier (Phase 1 — the hard core)

In TS 7 *all* languages (including TS) are analyzed out-of-process via LSP, so there is no
in-process freshness freebie. The barrier makes edits deterministically visible before the model's
next turn. Phase 0 ships the socket + generation plumbing; Phase 1 makes `PostToolUse` **blocking**
and adds settle detection.

```mermaid
sequenceDiagram
    autonumber
    participant M as Model
    participant H as PostToolUse hook<br/>(blocking)
    participant C as Control socket
    participant D as Daemon
    participant G as tsgo LSP

    M->>M: Edit / Write a file
    Note over M,H: model's turn CANNOT continue
    H->>C: sync <file>
    C->>D: bump generation
    D->>G: didChange / didSave
    D->>D: wait for settle<br/>(in-order probe · $/progress ·<br/>diagnostics quiescence · bounded ≤~1–2s)
    D-->>C: ok generation=n
    C-->>H: ok
    H-->>M: unblock → next query sees the fresh graph
```

Three-layer defense, deepest first: **(1)** the deterministic barrier above; **(2)** freshness
metadata — every result carries `generation` + `stale`; **(3)** a model-facing search-strategy doc
on how to react to `stale: true`. Never hang the model — bounded waits, then return with a tag.

---

## 4. Package dependency graph (Phase 0)

`internal/core` is the frozen center; everything depends inward on it and nothing on each other
(except the daemon and eval, which wire the pieces together). This is exactly what let the four
implementation packages be built in parallel.

```mermaid
flowchart LR
    core["internal/core<br/>contracts: LanguageProvider,<br/>Event/Logger, Config, StubProvider"]
    lsp["internal/lsp<br/>tsgo client"]
    path["internal/pathnorm<br/>(stdlib only)"]
    tel["internal/telemetry"]
    tools["internal/tools"]
    mcp["internal/mcp"]
    cmd["cmd/cgraphd<br/>(daemon main)"]
    eval["eval/tiera<br/>(Tier A gate)"]
    lifecycle["eval/lifecycle<br/>(daemon lifecycle gate)"]
    testinfra["eval/testinfra<br/>(shared real-daemon harness)"]

    lsp --> core
    tel --> core
    tools --> core
    tools --> path
    mcp --> core
    mcp --> tools
    cmd --> core
    cmd --> lsp
    cmd --> tel
    cmd --> mcp
    cmd --> tools
    eval --> tools
    eval --> testinfra
    lifecycle --> testinfra
    testinfra --> cmd

    classDef center fill:#243b53,stroke:#8bd,color:#fff;
    class core center;
```

The daemon wires the seam: `cmd/cgraphd` swaps the `StubProvider` for `lsp.New(cfg)` and the
`NopLogger` for a JSONL logger — the only two lines that know the concrete implementations.

---

## 5. Phase roadmap

```mermaid
flowchart LR
    P0["Phase 0 ✅<br/>walking skeleton<br/>+ telemetry spine<br/>+ Tier A scaffold"]
    P1["Phase 1<br/>staleness barrier<br/>+ freshness<br/>+ Tier A live"]
    P2["Phase 2<br/>materialized graph<br/>+ PageRank repo-map<br/>+ Tier B signal"]
    P3["Phase 3<br/>more tools + languages<br/>(pyright, gopls, rust-analyzer)<br/>+ Tier C"]
    P4["Phase 4<br/>hardening<br/>+ Pi adapter"]

    P0 ==> P1 ==> P2 ==> P3 ==> P4

    classDef done fill:#1f6f43,stroke:#0f3,color:#fff;
    class P0 done;
```

**Phase 0 exit criteria — all green:** MCP round-trip works · every call logged (JSONL) · Tier A
retrieval-correctness green on a pinned TS repo (`eval/tiera`, which drives the *real* daemon over
MCP).

---

## Eval axis (context)

The whole thing exists to be measured. The eval design is `{harness} × {frontier | local} ×
{graph | no-graph}`, stratified by "navigation spread" — see `EVAL.md`. The `graph_mode` tag on
every telemetry Event is what makes the graph-on vs graph-off comparison sliceable.

```mermaid
flowchart LR
    subgraph Axes
        H["harness<br/>Claude Code · Pi"]
        Mdl["model<br/>frontier · local (Qwen3-Coder)"]
        Grph["mode<br/>graph · no-graph"]
    end
    H --> Cell
    Mdl --> Cell
    Grph --> Cell["measured cell<br/>tokens-to-answer · correctness"]
    Cell --> Tiers["Tier A retrieval (CI gate)<br/>Tier B navigation efficiency<br/>Tier C task capability"]
```

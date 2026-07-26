# Architecture

Visual companion to `PLAN.md` (the *how* and the *order*), `INTEGRATION_CONSTRAINTS.md`
(decisions), and `EVAL.md` (measurement). Diagrams are [Mermaid](https://mermaid.js.org/) and
render natively on GitHub.

**One sentence:** a long-lived **Go daemon** (`portoland`) exposes an always-fresh, LSP-derived
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

    subgraph Daemon["portoland — Go daemon (portable core)"]
        direction TB
        MCP["MCP server<br/>(stdio)"]
        Ctl["Control socket<br/>(project-keyed)"]
        Tools["Tools layer<br/>find_definition · find_references · get_outline"]
        Gen["GenerationCounter<br/>freshness source"]
        Prov{{"LanguageProvider<br/>(interface)"}}
        LSP["lsp.Provider<br/>tsgo --lsp client"]
        Path["pathnorm<br/>WSL ↔ Windows"]
        Tel["telemetry<br/>bounded JSONL · opt-in OTLP/HTTP"]
    end

    Tsgo["tsgo --lsp -stdio<br/>(subprocess)"]
    Files[("source files")]
    JSONL[["telemetry.jsonl"]]
    OTLP["OTLP/HTTP collector<br/>(explicit endpoint only)"]
    Stderr[["stderr diagnostics"]]

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
    Tel -. independent bounded mirror .-> OTLP
    Tel -. failures and loss .-> Stderr

    classDef built fill:#1f6f43,stroke:#0f3,color:#fff;
    classDef scaffold fill:#7a5c00,stroke:#fc0,color:#fff;
    class Model,Hooks,MCP,Tools,Gen,Prov,LSP,Path,Tel,Tsgo,JSONL,OTLP,Stderr built;
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
    participant R as lsp.transport<br/>(write gate · lifecycle · reader)
    participant G as tsgo LSP
    participant L as Telemetry owner

    M->>S: tools/call find_definition {file, symbol}
    S->>T: FindDefinition(in)
    T->>T: establish one 5s operation context
    T->>T: normFile(file) — pathnorm (C:\… → /mnt/c/…)
    T->>P: DocumentSymbols(file, operation context)
    opt first query for this file
        P->>P: read file under per-file open transition
        P->>R: dispatch textDocument/didOpen
        R->>G: textDocument/didOpen
    end
    P->>R: register + dispatch textDocument/documentSymbol
    R->>G: textDocument/documentSymbol
    G-->>R: symbol tree
    R-->>P: demultiplexed response
    P-->>T: []Symbol
    T->>T: resolve name → Position (SelRange.Start)
    T->>P: Definition(file, pos, same operation context)
    P->>R: register + dispatch textDocument/definition
    R->>G: textDocument/definition
    G-->>R: Location[]
    R-->>P: demultiplexed response
    P-->>T: []core.Location
    T->>T: cap at Cfg.Cap() · stamp Freshness{gen, stale:false}
    T->>L: admit exactly one Event (tool, duration, size, …)
    L-->>L: bounded FIFO → JSONL writer<br/>+ independent OTLP batch mirror
    T-->>S: FindDefinitionOutput{found, locations, freshness}
    S-->>M: structured result
```

Telemetry admission snapshots each Event once before fan-out. JSONL is authoritative: one writer
owns a 512-record FIFO, waits at most 50 ms for capacity, preserves accepted-record order, and caps
serialized records at 16 KiB. Shutdown stops admission, drains, fsyncs, and closes within one shared
two-second lifecycle bound. OTLP/HTTP is enabled only by an explicit standard endpoint environment
variable and uses an independent bounded batch processor. Loss and sink failures are counted, joined
into shutdown errors, and diagnosed on stderr; MCP stdout remains protocol-only.

The **tools layer owns one fixed 5-second operation budget** for the complete invocation. The same
context covers path preparation, first-open disk reads and `didOpen`, name resolution, both provider
requests, serialization, pipe writes, and response waits; the provider does not reset the deadline
between stages. Provider initialization keeps its separate 20-second budget for project loading.

The `lsp.Provider` is concurrency-safe and delegates JSON-RPC connection ownership to one
`transport`. That owner arbitrates open, closing, closed, and aborted states; pending-request
completion; write admission; stdin closure; process kill and reap; and repeated close or abort.
External requests, notifications, and `$/cancelRequest` writes are admitted only while open. The
shutdown request owns the transition to closing, server-request responses remain permitted while
open or closing until stdin closure begins, `exit` is permitted only while closing, and no frame is
admitted after stdin closure or in a terminal state.
One background reader goroutine demuxes responses into pending entries that accept exactly one
terminal response or error, so late responses are ignored. Per-file open transitions retain one
canonical `didOpen` while allowing unrelated files to read concurrently. A context-aware write gate
serializes complete frames; cancellation before dispatch writes nothing, cancellation that wins
after dispatch removes the pending request and schedules a bounded best-effort cancellation
notification, and cancellation during a blocked or partial frame aborts the transport. Server
requests such as `client/registerCapability` are answered asynchronously within a bounded
server-response budget, so they cannot stall response demultiplexing.

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
    cmd["cmd/portoland<br/>(daemon main)"]
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

The daemon wires the seam: `cmd/portoland` swaps the `StubProvider` for `lsp.New(cfg)` and the
`NopLogger` for `telemetry.FromConfig`. The telemetry owner always creates bounded JSONL and adds
OTLP/HTTP only when a standard OTLP endpoint is explicitly configured.

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

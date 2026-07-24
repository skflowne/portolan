# PHASE 0 — Parallel execution plan

**Derived from:** `PLAN.md` → Phase 0 (walking skeleton + telemetry spine + Tier A scaffold).
**Orchestration:** foundation built serially (contracts), then a 4-agent parallel wave against
those contracts, then a serial integration wave.

## Why this decomposition
Phases in `PLAN.md` are strictly sequential. **Within Phase 0**, the components parallelize once
the shared contracts in `internal/core` are frozen. The four wave-1 packages touch **disjoint
directories** and depend only on `internal/core` (interfaces + types) — never on each other — so
they build and unit-test in isolation. Integration (wave 2) wires the real provider into the daemon
and runs the end-to-end + Tier A gates.

## Foundation (DONE — serial, built by orchestrator)
- `go.mod` (module `github.com/skflowne/portolan`, Go 1.26) with **deps pre-added**:
  `github.com/modelcontextprotocol/go-sdk` v1.6.1, `go.opentelemetry.io/otel` + sdk + stdouttrace v1.44.0.
  **Agents must NOT run `go get` / `go mod tidy`** — deps are present; report any missing dep instead.
- `internal/core`: `types.go` (Position/Range/Location/Symbol/Freshness + `LanguageProvider`),
  `telemetry.go` (`Event` + `Logger` + `NopLogger`), `config.go` (`Config` + `GenerationCounter`),
  `stubprovider.go` (`StubProvider` for building/testing upper layers).

## Wave 1 — 4 parallel agents (sonnet-5), each owns one directory

| Agent | Package | Owns | Depends on | Blocks |
|------|---------|------|-----------|--------|
| **A — LSP provider** | `internal/lsp` | `tsgo --lsp -stdio` client implementing `core.LanguageProvider` (Definition/References/DocumentSymbols); Content-Length framing; initialize handshake; didOpen; concurrent-safe request routing | `internal/core` | integration |
| **B — Path normalizer** | `internal/pathnorm` | WSL↔Windows absolute-path conversion both directions; `file://` URI ↔ path helpers used by A and MCP | `internal/core` (types only) | — |
| **C — Telemetry spine** | `internal/telemetry` | JSONL `Logger` (append, concurrent-safe, one Event per line) + OTEL span/metric mirror; constructors returning `core.Logger` | `internal/core` | integration |
| **D — MCP + tools + daemon** | `internal/mcp`, `internal/tools`, `cmd/portoland` | MCP stdio server (SDK), 3 tools (`find_definition`, `find_references`, `get_outline`) with name→position resolution via `DocumentSymbols`, result capping + `Freshness` stamping + telemetry; project-keyed control socket (accept + echo scaffold for Phase 1); `cmd/portoland` main wiring config→provider→logger→server | `internal/core` (builds against `StubProvider` + `NopLogger`) | integration |

**Contract rules for all agents:** every list tool caps at `cfg.Cap()`, sets `Truncated`, stamps
`Freshness` (from `core.GenerationCounter`, always fresh in Phase 0), emits exactly one `core.Event`,
returns honest nulls (nil,nil) not errors on found-nothing, never panics on a dead LSP.

## Wave 2 — integration (serial, orchestrator)
1. Swap `StubProvider` → `internal/lsp.Provider` in `cmd/portoland`; wire real telemetry.
2. `go build ./... && go vet ./... && go test ./...` green; `go mod tidy`.
3. End-to-end: launch daemon, drive an MCP `initialize` + `tools/call` round-trip on a pinned TS repo.
4. **Tier A eval scaffold** (`eval/tiera`): retrieval-correctness harness — fixture queries with
   expected definition/reference locations on a pinned TS repo; asserts green.
5. Verify Phase 0 **exit criteria**: MCP round-trip works; every call logged; Tier A green.

## Exit criteria (from PLAN.md Phase 0)
MCP round-trip works · every call logged (JSONL + OTEL) · Tier A green on a pinned repo.

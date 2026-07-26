// Package tools implements the three MCP-facing code-graph tools
// (find_definition, find_references, get_outline) against the
// core.LanguageProvider seam. It is deliberately provider-agnostic: every
// exported method here is exercised in tests against core.StubProvider, and
// the internal/mcp package is a thin adapter from these methods to the MCP
// SDK's tool-handler shape.
//
// Every tool method in this package follows the same contract (see PLAN.md /
// PHASE0_PARALLEL.md):
//   - result lists are capped at cfg.Cap(), with Truncated set when capped
//   - every output carries a Freshness stamp taken from the GenerationCounter
//     at the start of the call
//   - exactly one core.Event is emitted per call, success or failure
//   - "found nothing" (unresolved symbol name, or a provider returning no
//     results) is an honest, non-error result: Found=false with a clear
//     Message, never a Go error
//   - a provider error is a soft failure: it is surfaced in the Event (Err)
//     and in the output's Error field, and the method still returns (out, nil)
//     — callers (notably the MCP layer) never need to translate a Go error
//     into a tool-level failure for this case, and the process never panics
package tools

import (
	"context"
	"time"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/pathnorm"
)

// Tools holds everything the three tool methods need: the language provider
// (StubProvider in Phase 0, internal/lsp.Provider from Wave 2), the shared
// freshness counter, the telemetry sink, and the config (for Cap/SessionID/
// GraphMode).
const defaultOperationTimeout = 5 * time.Second

type Tools struct {
	Provider core.LanguageProvider
	Gen      *core.GenerationCounter
	Logger   core.Logger
	Cfg      core.Config

	operationTimeout time.Duration
}

// New builds a Tools. gen and logger must not be nil; use core.NopLogger{}
// for a discarding logger and a fresh core.GenerationCounter{} in tests.
func New(provider core.LanguageProvider, gen *core.GenerationCounter, logger core.Logger, cfg core.Config) *Tools {
	return &Tools{
		Provider:         provider,
		Gen:              gen,
		Logger:           logger,
		Cfg:              cfg,
		operationTimeout: defaultOperationTimeout,
	}
}

// operationContext establishes the one budget for a complete tool invocation.
// Provider implementations must consume this context without resetting it.
func (t *Tools) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := t.operationTimeout
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	return context.WithTimeout(parent, timeout)
}

// callTimer starts the bookkeeping shared by every tool method: it captures
// the freshness snapshot up front and returns a finish func that fills in
// duration/result-size/truncated/err on ev and logs it exactly once. Callers
// build ev's static fields (Tool, SessionID, GraphMode, Generation, Stale)
// before deferring finish.
func (t *Tools) emit(ctx context.Context, ev *core.Event, start time.Time, resultSize int, truncated bool, errMsg string) {
	ev.DurationMs = time.Since(start).Milliseconds()
	ev.ResultSize = resultSize
	ev.Truncated = truncated
	ev.Err = errMsg
	t.Logger.Log(ctx, *ev)
}

// normFile canonicalizes a caller-supplied file path to the daemon's host
// (WSL/Linux) form before it reaches the provider. This is where a Windows
// harness's "C:\...\a.ts" becomes "/mnt/c/.../a.ts" — the WSL↔Windows seam the
// plan wants wired "from the start". It is a no-op for already-host paths.
func (t *Tools) normFile(p string) string {
	return pathnorm.Normalize(p)
}

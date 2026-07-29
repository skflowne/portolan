// Package tools implements the three MCP-facing code-graph tools
// (find_definition, find_references, get_outline) against the
// core.LanguageProvider seam.
//
// Every tool method in this package follows the same contract:
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

const defaultOperationTimeout = 5 * time.Second

// Tools owns the language provider, freshness counter, telemetry sink, and
// shared call policy used by every tool method.
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

func (t *Tools) emit(ctx context.Context, ev *core.Event, start time.Time, resultSize int, truncated bool, errMsg string) {
	ev.DurationMs = time.Since(start).Milliseconds()
	ev.ResultSize = resultSize
	ev.Truncated = truncated
	ev.Err = errMsg
	t.Logger.Log(ctx, *ev)
}

// normFile canonicalizes a caller-supplied file path to the daemon's host
// (WSL/Linux) form so every provider sees the same path identity.
func (t *Tools) normFile(p string) string {
	return pathnorm.Normalize(p)
}

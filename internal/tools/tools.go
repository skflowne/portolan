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
//   - input-validation and provider errors are soft failures: they are surfaced
//     in the Event (Err) and output Error field, and the method still returns
//     (out, nil), preserving the tool-level contract without panicking
//   - provider stages share one invocation boundary that rejects successful
//     results completed after operation cancellation
package tools

import (
	"context"
	"time"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/pathnorm"
)

const (
	defaultOperationTimeout = 5 * time.Second
	invalidFileError        = "invalid file path"
	invalidFileMessage      = "file must be an absolute Linux/WSL path, Windows drive path, or same-distro WSL UNC path"
)

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

// runProviderStage accepts a successful provider result only while the shared
// tool-operation context remains active.
func runProviderStage[T any](ctx context.Context, invoke func(context.Context) (T, error)) (T, error) {
	result, err := invoke(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

// toolResult exposes completion-owned telemetry without letting tool bodies
// construct or emit events.
type toolResult interface {
	setFreshness(core.Freshness)
	telemetryFields() (resultSize int, truncated bool, errMsg string)
}

type toolCall struct {
	logger core.Logger
	start  time.Time
	event  core.Event
}

// runTool owns the complete event lifecycle. Configuration supplies session
// and graph correlation, the call identity supplies the tool name, freshness
// is snapped before work, the runner clock supplies duration, and the completed
// result supplies size, truncation, and error. The sink supplies Timestamp;
// current tools do not populate Extra.
func (t *Tools) runTool(parent context.Context, tool string, result toolResult, execute func(context.Context)) {
	start := time.Now()
	fresh := t.Gen.Current()
	call := toolCall{
		logger: t.Logger,
		start:  start,
		event: core.Event{
			SessionID:  t.Cfg.SessionID,
			GraphMode:  t.Cfg.GraphMode,
			Tool:       tool,
			Generation: fresh.Generation,
			Stale:      fresh.Stale,
		},
	}
	result.setFreshness(fresh)

	ctx, cancel := t.operationContext(parent)
	defer cancel()
	defer call.finish(ctx, result)
	execute(ctx)
}

func (c *toolCall) finish(ctx context.Context, result toolResult) {
	c.event.ResultSize, c.event.Truncated, c.event.Err = result.telemetryFields()
	c.event.DurationMs = time.Since(c.start).Milliseconds()
	c.logger.Log(ctx, c.event)
}

type fileValidationFailure struct {
	err     string
	message string
}

func validateFile(input string) (string, *fileValidationFailure) {
	file, err := pathnorm.Canonicalize(input)
	if err == nil {
		return file, nil
	}
	return "", &fileValidationFailure{err: invalidFileError, message: invalidFileMessage}
}

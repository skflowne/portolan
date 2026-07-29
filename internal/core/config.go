package core

import "sync/atomic"

// Config is the daemon's runtime configuration, assembled in cmd/portoland from
// flags and environment. It is passed by value to constructors.
type Config struct {
	// ProjectRoot is the absolute, host-normalized root of the analyzed repo.
	// The control socket path is keyed off this so one daemon serves one project.
	ProjectRoot string

	// SessionID tags every telemetry Event; supplied by the harness (Claude Code
	// hook / Pi) so token accounting can be joined to graph activity.
	SessionID string

	// GraphMode is "graph" or "no-graph"; the eval axis. Stamped on telemetry.
	GraphMode string

	// TsgoPath is the tsgo executable (default "tsgo", resolved on PATH).
	TsgoPath string

	// JSONLPath is where the telemetry JSONL stream is written.
	JSONLPath string

	// ControlSocket is the control-socket path (unix socket / named pipe),
	// project-keyed. Empty means derive from ProjectRoot.
	ControlSocket string

	// MaxResults caps every list-returning tool. 0 means DefaultMaxResults.
	MaxResults int
}

// DefaultMaxResults is the cap applied when Config.MaxResults is 0. Every
// list-returning tool paginates/caps — never dump an unbounded result.
const DefaultMaxResults = 100

// Cap returns the effective result cap.
func (c Config) Cap() int {
	if c.MaxResults <= 0 {
		return DefaultMaxResults
	}
	return c.MaxResults
}

// GenerationCounter orders control-socket sync commands so tool results can
// identify the shared source generation they observed.
type GenerationCounter struct {
	n atomic.Uint64
}

// Current returns the current generation with Stale=false because this counter
// tracks sync ordering, not per-file dirty state.
func (g *GenerationCounter) Current() Freshness {
	return Freshness{Generation: g.n.Load(), Stale: false}
}

// Bump atomically advances the generation and returns the new value.
func (g *GenerationCounter) Bump() uint64 {
	return g.n.Add(1)
}

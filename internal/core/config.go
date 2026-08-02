package core

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// Config is the daemon's runtime configuration, assembled in cmd/portoland from
// flags and environment. It is passed by value to constructors.
type Config struct {
	// ProjectRoot is the absolute, host-normalized root of the analyzed repo.
	// The control socket path is keyed off this so one daemon serves one project.
	ProjectRoot string

	// SessionID identifies the caller session associated with telemetry events.
	SessionID string

	// GraphMode identifies the configured graph mode associated with telemetry events.
	GraphMode string

	// TsgoPath is the tsgo executable (default "tsgo", resolved on PATH).
	TsgoPath string

	// JSONLPath is where the telemetry JSONL stream is written.
	JSONLPath string

	// ControlSocket is the control-socket path (unix socket / named pipe),
	// project-keyed. Empty means derive from ProjectRoot.
	ControlSocket string

	// MaxResults caps returned items, except find_references where it caps
	// retained first-seen file groups. 0 means DefaultMaxResults.
	MaxResults int
}

const (
	// GraphModeGraph enables graph-backed navigation during an evaluation.
	GraphModeGraph = "graph"
	// GraphModeNoGraph identifies the graph-disabled evaluation control.
	GraphModeNoGraph = "no-graph"
	// DefaultGraphMode is used when no graph mode flag or environment value is supplied.
	DefaultGraphMode = GraphModeGraph

	// DefaultMaxResults is the effective cap when Config.MaxResults is 0.
	DefaultMaxResults = 100
)

var (
	ErrInvalidSessionID = errors.New("invalid telemetry session ID")
	ErrInvalidGraphMode = errors.New("invalid telemetry graph mode")
)

// ValidateTelemetryDimensions rejects configuration that cannot produce
// authoritative telemetry correlation fields.
func (c Config) ValidateTelemetryDimensions() error {
	if strings.TrimSpace(c.SessionID) == "" {
		return fmt.Errorf("%w: value must not be empty or whitespace", ErrInvalidSessionID)
	}
	if c.GraphMode != GraphModeGraph && c.GraphMode != GraphModeNoGraph {
		return fmt.Errorf("%w %q: accepted values are %q and %q", ErrInvalidGraphMode, c.GraphMode, GraphModeGraph, GraphModeNoGraph)
	}
	return nil
}

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

package tools

import (
	"context"
	"sync"

	"github.com/skflowne/portolan/internal/core"
)

// capturingLogger records every Event logged so tests can assert on it.
type capturingLogger struct {
	mu     sync.Mutex
	events []core.Event
}

func (c *capturingLogger) Log(_ context.Context, ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *capturingLogger) Close() error { return nil }

func (c *capturingLogger) last() (core.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return core.Event{}, false
	}
	return c.events[len(c.events)-1], true
}

func (c *capturingLogger) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func existingSignatures(symbols []core.Symbol) []string {
	signatures := make([]string, len(symbols))
	for i := range symbols {
		signatures[i] = symbols[i].Signature
	}
	return signatures
}

func newTestTools(provider core.LanguageProvider, logger *capturingLogger, cfg core.Config) *Tools {
	return New(provider, &core.GenerationCounter{}, logger, cfg)
}

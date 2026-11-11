// Package adapters reads native Codex and Claude conversation history.
//
// Both adapters are deliberately read-only. They never import synthetic
// transcripts, start a model turn, or advance a conversation's delivery state.
package adapters

import (
	"context"
	"errors"
	"fmt"

	"terminus/internal/model"
)

// Adapter discovers and reads native sessions without modifying them.
type Adapter interface {
	Discover(ctx context.Context, cwd string) ([]model.Session, error)
	Read(ctx context.Context, s model.Session) ([]model.Event, error)
}

// ErrUnsupportedAgent is returned when no adapter exists for an agent.
var ErrUnsupportedAgent = errors.New("unsupported agent")

// New returns the read-only adapter for agent.
func New(agent model.Agent) (Adapter, error) {
	switch agent {
	case model.Codex:
		return newCodexAdapter(), nil
	case model.Claude:
		return newClaudeAdapter(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAgent, agent)
	}
}

package adapters

import (
	"errors"
	"testing"

	"baton/internal/model"
)

func TestNew(t *testing.T) {
	for _, agent := range []model.Agent{model.Codex, model.Claude} {
		adapter, err := New(agent)
		if err != nil {
			t.Fatalf("New(%q): %v", agent, err)
		}
		if adapter == nil {
			t.Fatalf("New(%q) returned nil", agent)
		}
	}
	if _, err := New(model.Agent("other")); !errors.Is(err, ErrUnsupportedAgent) {
		t.Fatalf("New(other) error = %v, want ErrUnsupportedAgent", err)
	}
}

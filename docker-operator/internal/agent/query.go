package agent

import (
	"context"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// Get returns one agent record by ID, or an error satisfying
// store.IsNotFound.
//
// This is a thin pass-through to the store: reading a record touches no
// Docker resource, so Manager has nothing of its own to add. It exists on
// Manager (rather than making internal/api depend on *store.Store directly
// too) so internal/api can depend on exactly one interface -- the one its
// own tests fake.
func (m *Manager) Get(ctx context.Context, id string) (store.Agent, error) {
	return m.store.Get(ctx, id)
}

// List returns every agent record, in the same order store.List does.
func (m *Manager) List(ctx context.Context) ([]store.Agent, error) {
	return m.store.List(ctx)
}

// MaxAgents returns the MAX_AGENTS cap this Manager's store enforces, so a
// caller (internal/api's list response) can report capacity without
// duplicating configuration.
func (m *Manager) MaxAgents() int {
	return m.store.MaxAgents()
}

// Rename updates an agent's Name and/or Description. A nil pointer leaves
// the corresponding field unchanged; a non-nil pointer sets it, including to
// an empty string. At least one of name or description must be non-nil.
//
// This never touches Docker: the name/description are purely cosmetic, UI-
// facing fields, so this is a direct store.Update rather than a step in the
// Create/Delete lifecycle.
func (m *Manager) Rename(ctx context.Context, id string, name, description *string) (store.Agent, error) {
	if name == nil && description == nil {
		return store.Agent{}, fmt.Errorf("renaming agent %q: at least one of name or description must be provided", id)
	}
	return m.store.Update(ctx, id, func(a *store.Agent) error {
		if name != nil {
			a.Name = *name
		}
		if description != nil {
			a.Description = *description
		}
		return nil
	})
}

package agent

import (
	"context"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ListCronAgents returns every saved recurring-job definition, in the same
// order store.ListCronAgents does. A thin pass-through, the same way
// ListTemplates is: a CronAgent touches no Docker resource of its own.
func (m *Manager) ListCronAgents(ctx context.Context) ([]store.CronAgent, error) {
	return m.store.ListCronAgents(ctx)
}

// GetCronAgent returns one CronAgent record by ID, or an error satisfying
// store.IsCronAgentNotFound.
func (m *Manager) GetCronAgent(ctx context.Context, id string) (store.CronAgent, error) {
	return m.store.GetCronAgent(ctx, id)
}

// CreateCronAgent generates a fresh CronAgent ID and inserts a new record.
// No Docker resource to provision and nothing to roll back on failure, so
// this is a direct store call plus ID generation, mirroring CreateTemplate.
func (m *Manager) CreateCronAgent(ctx context.Context, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	id, err := store.NewCronAgentID()
	if err != nil {
		return store.CronAgent{}, err
	}
	spec.ID = id
	return m.store.CreateCronAgent(ctx, spec)
}

// UpdateCronAgent replaces every field of the CronAgent with the given ID
// from spec (a full-record replace, matching the edit form's submit-every-
// field-every-time contract, mirroring UpdateTemplate). spec.ID is
// ignored; the ID in the URL path is authoritative and immutable.
func (m *Manager) UpdateCronAgent(ctx context.Context, id string, spec store.CronAgentCreateSpec) (store.CronAgent, error) {
	return m.store.UpdateCronAgent(ctx, id, func(c *store.CronAgent) error {
		c.Name = spec.Name
		c.TemplateID = spec.TemplateID
		c.Schedule = spec.Schedule
		c.Enabled = spec.Enabled
		c.Task = spec.Task
		c.OnComplete = spec.OnComplete
		return nil
	})
}

// DeleteCronAgent removes the CronAgent record (idempotent).
func (m *Manager) DeleteCronAgent(ctx context.Context, id string) error {
	return m.store.DeleteCronAgent(ctx, id)
}

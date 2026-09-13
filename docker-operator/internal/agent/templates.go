package agent

import (
	"context"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ListTemplates returns every saved agent-creation template, in the same
// order store.ListTemplates does.
//
// This is a thin pass-through to the store, the same way Get/List are for
// agents: a template touches no Docker resource, so Manager has nothing of
// its own to add. It exists on Manager (rather than making internal/api
// depend on *store.Store directly too) so internal/api can depend on exactly
// one interface -- the one its own tests fake.
func (m *Manager) ListTemplates(ctx context.Context) ([]store.Template, error) {
	return m.store.ListTemplates(ctx)
}

// GetTemplate returns one template record by ID, or an error satisfying
// store.IsTemplateNotFound.
func (m *Manager) GetTemplate(ctx context.Context, id string) (store.Template, error) {
	return m.store.GetTemplate(ctx, id)
}

// CreateTemplate generates a fresh template ID and inserts a new template
// record. Unlike agent creation there is no Docker resource to provision and
// nothing to roll back on failure, so this is a direct store call plus ID
// generation -- no translation layer like Create's is needed.
func (m *Manager) CreateTemplate(ctx context.Context, spec store.TemplateCreateSpec) (store.Template, error) {
	id, err := store.NewTemplateID()
	if err != nil {
		return store.Template{}, err
	}
	spec.ID = id
	return m.store.CreateTemplate(ctx, spec)
}

// UpdateTemplate replaces every field of the template with the given ID from
// spec (a full-record replace, not a partial patch -- the create-form
// submits every field every time). spec.ID is ignored; the ID in the URL
// path is authoritative and immutable.
func (m *Manager) UpdateTemplate(ctx context.Context, id string, spec store.TemplateCreateSpec) (store.Template, error) {
	return m.store.UpdateTemplate(ctx, id, func(t *store.Template) error {
		t.Name = spec.Name
		t.Description = spec.Description
		t.Backend = spec.Backend
		t.Model = spec.Model
		t.FastModel = spec.FastModel
		t.OllamaURL = spec.OllamaURL
		t.Repo = spec.Repo
		t.AutoCompactThreshold = spec.AutoCompactThreshold
		t.MaxContextTokens = spec.MaxContextTokens
		t.ImageTag = spec.ImageTag
		t.AutoMode = spec.AutoMode
		return nil
	})
}

// DeleteTemplate removes the template record (idempotent).
func (m *Manager) DeleteTemplate(ctx context.Context, id string) error {
	return m.store.DeleteTemplate(ctx, id)
}

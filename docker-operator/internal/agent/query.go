package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
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

// DefaultBackend / DefaultModel / DefaultFastModel / DefaultOllamaURL /
// DefaultRepo expose the operator's configured create-form defaults, so
// internal/api's list response can carry them and the UI can pre-fill without
// a second request. DefaultRepo is "" when the operator set no GITHUB_REPO;
// DefaultOllamaURL is "" when the operator cleared OLLAMA_URL.
func (m *Manager) DefaultBackend() string   { return m.cfg.DefaultBackend }
func (m *Manager) DefaultModel() string     { return m.cfg.AgentModel }
func (m *Manager) DefaultFastModel() string { return m.cfg.AgentFastModel }
func (m *Manager) DefaultOllamaURL() string { return m.cfg.OllamaURL }
func (m *Manager) DefaultRepo() string      { return m.cfg.GithubRepo }

// DefaultAutoCompactThreshold exposes the operator's default Claude Code
// auto-compact threshold, "" when the operator set no
// AGENT_AUTO_COMPACT_THRESHOLD. The UI shows it as the create-form default;
// empty means the variable is omitted from the agent so it uses Claude Code's
// built-in default.
func (m *Manager) DefaultAutoCompactThreshold() string { return m.cfg.AutoCompactThreshold }

// DefaultMaxContextTokens exposes the operator's default Claude Code
// max-context token budget, "" when the operator set no
// AGENT_MAX_CONTEXT_TOKENS. The UI shows it as the create-form default;
// empty means the variable is omitted from the agent so it uses Claude Code's
// built-in default.
func (m *Manager) DefaultMaxContextTokens() string { return m.cfg.MaxContextTokens }

// DefaultAutoMode exposes the operator's DefaultAutoMode (AGENT_AUTO_MODE)
// as config.AutoModeOn/AutoModeOff, so the create form can pre-fill and show
// what "operator default" resolves to.
func (m *Manager) DefaultAutoMode() string { return config.AutoModeString(m.cfg.DefaultAutoMode) }

// AgentImage and DockerRuntime expose the operator-level parameters that
// apply to every agent regardless of its create-time request: the agent
// container image and the Docker-in-Docker sidecar's container runtime.
// internal/api's GET /api/agents/{id}/info serves them next to the agent
// record so the UI's "Agent info" overlay can show what the operator set.
func (m *Manager) AgentImage() string    { return m.cfg.AgentImage }
func (m *Manager) DockerRuntime() string { return m.cfg.DockerRuntime }

// AnthropicAuthStatus reports whether the shared Anthropic credential is
// configured -- its kind and last-set time, never its value. The value
// stays inside internal/agent (resolveBackend) and internal/store; nothing
// that could serialise it to a client ever holds it.
func (m *Manager) AnthropicAuthStatus(ctx context.Context) (kind string, updatedAt time.Time, configured bool, err error) {
	auth, ok, err := m.store.GetAnthropicAuth(ctx)
	if err != nil || !ok {
		return "", time.Time{}, false, err
	}
	return auth.Kind, auth.UpdatedAt, true, nil
}

// SetAnthropicAuth stores (replacing) the shared Anthropic credential.
// ClearAnthropicAuth removes it and is idempotent. Both are thin
// pass-throughs -- the store validates the kind and rejects an empty value.
func (m *Manager) SetAnthropicAuth(ctx context.Context, kind, value string) error {
	return m.store.SetAnthropicAuth(ctx, kind, value)
}

// ClearAnthropicAuth removes the shared Anthropic credential (idempotent).
func (m *Manager) ClearAnthropicAuth(ctx context.Context) error {
	return m.store.ClearAnthropicAuth(ctx)
}

// MarkUnexpectedExit records that an agent's own container stopped or died
// without going through Delete -- the reactive correction
// cmd/docker-operator's Docker-events goroutine applies when it observes
// that container leave the running state on its own.
//
// A no-op if the record is not currently StatusRunning: an agent already
// StatusDeleting is mid-teardown (Delete marks that BEFORE removing any
// container, so the "stop"/"die" event this same removal generates arrives
// against an already-non-running record and correctly changes nothing), and
// one already StatusStopped/StatusError has nothing left to correct. This is
// what lets the caller subscribe to every managed container's events without
// distinguishing an expected shutdown from an unexpected one itself.
//
// The record is read once to short-circuit the common case (nothing to do)
// without writing a spurious UpdatedAt bump; the actual status change still
// happens inside store.Update's own transaction, so a status change racing
// this check is not lost, only possibly redone.
//
// containerID is the ID of the container the event fired on (the Docker
// event's actor ID). When it and the record's own ContainerID are both set
// and differ, the event belongs to a container this record no longer owns --
// the classic case is the OLD container's "die"/"stop" that an in-place
// Update's recreate generates, arriving after Update has already stamped the
// record with the NEW container's ID. That is not an unexpected exit, so it is
// a no-op. Checked both before store.Update and inside its mutator, because
// the record's ContainerID can change between the two. An empty containerID
// (an event source that names none) disables the guard, preserving the old
// behaviour.
func (m *Manager) MarkUnexpectedExit(ctx context.Context, id, containerID string, newStatus store.Status, message string) error {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if a.Status != store.StatusRunning {
		return nil
	}
	if isStaleContainerEvent(containerID, a.ContainerID) {
		return nil
	}
	_, err = m.store.Update(ctx, id, func(ag *store.Agent) error {
		if ag.Status != store.StatusRunning {
			return nil
		}
		if isStaleContainerEvent(containerID, ag.ContainerID) {
			return nil
		}
		ag.Status = newStatus
		ag.ErrorMessage = message
		return nil
	})
	return err
}

// isStaleContainerEvent reports whether a container event fired on eventID
// concerns a container the record (now on recordID) no longer owns. Both must
// be non-empty to conclude anything: an empty eventID disables the guard.
func isStaleContainerEvent(eventID, recordID string) bool {
	return eventID != "" && recordID != "" && eventID != recordID
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

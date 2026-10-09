package agent

import (
	"context"
	"errors"
	"fmt"

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
func (m *Manager) AgentImage() string    { return m.cfg.AgentImageClaudeCode }
func (m *Manager) DockerRuntime() string { return m.cfg.DockerRuntime }

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

// ErrAgentNotAnthropic is returned by Rename for a non-nil accountID against
// an agent whose Backend is not config.BackendAnthropic -- there is nothing
// to pin on an ollama agent. internal/api maps it to a 400.
var ErrAgentNotAnthropic = errors.New("agent is not on the anthropic backend")

// IsAgentNotAnthropic reports whether err was caused by a PATCH account_id
// against a non-anthropic agent.
func IsAgentNotAnthropic(err error) bool { return errors.Is(err, ErrAgentNotAnthropic) }

// Rename updates an agent's Name, Description and/or its pinned Anthropic
// account. A nil pointer leaves the corresponding field unchanged; a
// non-nil pointer sets it (including to an empty string, for name/
// description). At least one of the three must be non-nil.
//
// This never touches Docker: all three are either cosmetic UI-facing
// fields or a plain store pin change, so this is a direct store.Update
// rather than a step in the Create/Update/Delete lifecycle -- changing
// which account an agent uses does NOT recreate its container; the new
// credential applies starting at the agent's next restart/wake.
//
// accountID is validated BEFORE the store.Update call, not inside its
// mutator: a deleted-between-check-and-write race just means the
// fail-closed dangling-pin path in resolveBackend catches it at the
// agent's next restart/wake -- an already-accepted outcome, not a new hole
// this adds.
func (m *Manager) Rename(ctx context.Context, id string, name, description, accountID *string) (store.Agent, error) {
	if name == nil && description == nil && accountID == nil {
		return store.Agent{}, fmt.Errorf("renaming agent %q: at least one of name, description or account_id must be provided", id)
	}
	if accountID != nil {
		a, err := m.store.Get(ctx, id)
		if err != nil {
			return store.Agent{}, err
		}
		if a.Backend != config.BackendAnthropic {
			return store.Agent{}, fmt.Errorf("renaming agent %q: %w", id, ErrAgentNotAnthropic)
		}
		if _, err := m.store.GetAnthropicAccount(ctx, *accountID); err != nil {
			if store.IsAnthropicAccountNotFound(err) {
				return store.Agent{}, fmt.Errorf("renaming agent %q: %w: %q", id, ErrUnknownAnthropicAccount, *accountID)
			}
			return store.Agent{}, fmt.Errorf("renaming agent %q: checking the anthropic account: %w", id, err)
		}
	}
	return m.store.Update(ctx, id, func(a *store.Agent) error {
		if name != nil {
			a.Name = *name
		}
		if description != nil {
			a.Description = *description
		}
		if accountID != nil {
			a.AnthropicAccountID = *accountID
		}
		return nil
	})
}

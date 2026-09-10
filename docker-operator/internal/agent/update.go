package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ErrNotUpdatable is returned by Update when the agent is in a state an
// in-place update cannot start from. Only running, stopped and error agents
// are updatable: a creating/updating/deleting agent is mid-operation and its
// container must not be pulled out from under that. internal/api maps it to a
// 409.
var ErrNotUpdatable = errors.New("agent is not in an updatable state")

// IsNotUpdatable reports whether err was caused by an update attempt against
// an agent in a non-updatable state.
func IsNotUpdatable(err error) bool { return errors.Is(err, ErrNotUpdatable) }

// updatableStatuses is the set of states Update will start from.
func updatableStatus(s store.Status) bool {
	switch s {
	case store.StatusRunning, store.StatusStopped, store.StatusError:
		return true
	default:
		return false
	}
}

// UpdateRequest is everything an in-place update can change: every
// create-form field (via the embedded CreateRequest) plus the image tag. The
// embedded CreateRequest.ImageTag is ignored -- ImageTag here is the one that
// applies.
type UpdateRequest struct {
	CreateRequest
	// ImageTag pins the recreated agent to a specific tag of the operator's
	// agent-image repository, exactly like CreateRequest.ImageTag. Empty means
	// the operator's configured AgentImage verbatim.
	ImageTag string
}

// Update recreates an existing agent's container in place -- a new image tag,
// a changed backend, or any other create-form field -- under the SAME agent
// ID.
//
// What it recreates and what it keeps:
//   - Only the agent container is recreated. The DinD sidecar, the private
//     dinernet and the dependaproxy attachment are left running and untouched.
//   - The three named volumes (workspace, Claude config, DinD cache) are kept
//     by name, so every byte of the agent's work -- and its Claude Code
//     session history -- survives.
//   - The agent ID is kept. Every Docker resource name is derived from it, so
//     the recreated container reuses them.
//
// What it ends: the running tmux/claude session. The recreate stops and
// removes the old container, which kills tmux and the `claude` process in it.
// The new container boots a fresh tmux session running `claude --continue`,
// which resumes the previous conversation from the preserved
// CLAUDE_CONFIG_DIR volume. Edge case: if that volume holds no prior
// conversation, `claude --continue` exits non-zero; `remain-on-exit` keeps the
// dead pane visible and the user can run `claude` fresh. In practice every
// agent runs `claude` on first boot, so a session exists by update time.
//
// No auto-rollback. A failure after the old container is gone leaves the
// record StatusError with volumes intact; the user fixes the cause and
// retries Update (idempotent -- the old container is already gone). A crash
// mid-update leaves the record StatusUpdating, which the next startup
// Reconcile pass turns into StatusError (never tearing the volumes down).
//
// The flow, in order:
//  1. store.Get (404 via store.IsNotFound); reject unless running/stopped/error.
//  2. resolveSpec + resolveAgentImageRef -- both validate, nothing mutated yet.
//  3. store.Update -> StatusUpdating, BEFORE any Docker call, so the old
//     container's die/stop event is a no-op (its status is not running, and
//     the ActorID guard in MarkUnexpectedExit catches it too).
//  4. docker.ImagePull(newRef). Failure here -> failUpdate (container intact,
//     but the record is already StatusUpdating).
//  5. removeAgentContainer -- stop+remove the agent container ONLY.
//  6. store.Update -> new config fields + Image + ContainerID="" (still updating).
//  7. startAgentContainer("--continue") -> waitTmuxSession -> markRunning.
//     Any failure from step 5 on -> failUpdate.
func (m *Manager) Update(ctx context.Context, id string, req UpdateRequest) (store.Agent, error) {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	if !updatableStatus(a.Status) {
		return store.Agent{}, fmt.Errorf("updating agent %q: %w (status %q)", id, ErrNotUpdatable, a.Status)
	}

	rs, err := m.resolveSpec(ctx, req.CreateRequest)
	if err != nil {
		return store.Agent{}, fmt.Errorf("updating agent %q: %w", id, err)
	}
	newRef, err := m.resolveAgentImageRef(req.ImageTag)
	if err != nil {
		return store.Agent{}, fmt.Errorf("updating agent %q: %w", id, err)
	}

	// StatusUpdating BEFORE any Docker call.
	a, err = m.store.Update(ctx, id, func(ag *store.Agent) error {
		ag.Status = store.StatusUpdating
		ag.ErrorMessage = ""
		return nil
	})
	if err != nil {
		return store.Agent{}, fmt.Errorf("updating agent %q: marking it updating: %w", id, err)
	}

	// Pull the new image. The container is still intact here, but the record is
	// already StatusUpdating -- failUpdate settles it to StatusError.
	if err := m.docker.ImagePull(ctx, newRef); err != nil {
		return m.failUpdate(ctx, &a, fmt.Errorf("pulling the new agent image %q: %w", newRef, err))
	}

	// Remove the agent container ONLY. From here on, any failure is
	// unrecoverable without a retry: failUpdate.
	if err := m.removeAgentContainer(ctx, a); err != nil {
		return m.failUpdate(ctx, &a, fmt.Errorf("removing the old agent container: %w", err))
	}

	// Apply the resolved fields. Status stays StatusUpdating.
	a, err = m.store.Update(ctx, id, func(ag *store.Agent) error {
		ag.Name = req.Name
		ag.Description = req.Description
		ag.Backend = rs.rb.kind
		ag.Model = rs.rb.model
		ag.FastModel = rs.rb.fastModel
		ag.OllamaURL = rs.rb.ollamaURL
		ag.Repo = rs.repo
		ag.AutoCompactThreshold = rs.autoCompact
		ag.MaxContextTokens = rs.maxContextTokens
		ag.Image = newRef
		ag.ContainerID = ""
		return nil
	})
	if err != nil {
		return m.failUpdate(ctx, &a, fmt.Errorf("recording the updated agent config: %w", err))
	}

	if err := m.startAgentContainer(ctx, &a, rs.rb, "--continue"); err != nil {
		return m.failUpdate(ctx, &a, err)
	}
	if err := m.waitTmuxSession(ctx, a); err != nil {
		return m.failUpdate(ctx, &a, err)
	}
	if err := m.markRunning(ctx, &a); err != nil {
		return m.failUpdate(ctx, &a, err)
	}
	m.log.InfoContext(ctx, "agent updated", "agent_id", id, "image", newRef)
	return a, nil
}

// removeAgentContainer stops and removes an agent's OWN container and nothing
// else -- not the sidecar, not the network, not a volume, not the record. It
// is modeled on teardown's container block but deliberately separate: teardown
// is shared by Delete, create-failure rollback and Reconcile, and none of them
// should gain an "agent container only" mode.
//
// The container is referenced by ID, then name, then the ID-derived name --
// the daemon resolves any of them, and dockerclient's stop/remove both treat
// "already gone" as success, so a retried Update is a clean no-op here.
func (m *Manager) removeAgentContainer(ctx context.Context, a store.Agent) error {
	ref := firstNonEmpty(a.ContainerID, a.ContainerName, agentContainerName(a.ID))
	var errs []error
	if err := m.docker.ContainerStop(ctx, ref, m.opts.StopTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stopping the agent container %q: %w", ref, err))
	}
	if err := m.docker.ContainerRemove(ctx, ref); err != nil {
		errs = append(errs, fmt.Errorf("removing the agent container %q: %w", ref, err))
	}
	return errors.Join(errs...)
}

// failUpdate settles a failed in-place update: the record goes StatusError
// with an explanatory ErrorMessage and no container ID, the volumes are left
// exactly as they are (no auto-rollback), and the wrapped cause is returned.
//
// The final write uses context.WithoutCancel(ctx): the usual reason the tail
// of Update fails is a cancelled context, and the record must still be moved
// off StatusUpdating so it does not look like a crash to the next Reconcile.
func (m *Manager) failUpdate(ctx context.Context, a *store.Agent, cause error) (store.Agent, error) {
	m.log.ErrorContext(ctx, "in-place agent update failed; leaving the record in error with its volumes intact for a retry",
		"agent_id", a.ID, "error", cause)

	if _, err := m.store.Update(context.WithoutCancel(ctx), a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "update failed: " + cause.Error()
		ag.ContainerID = ""
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record the failed update; the record stays in updating and the next reconcile pass will mark it error",
			"agent_id", a.ID, "error", err)
	}
	return store.Agent{}, fmt.Errorf("updating agent %q: %w", a.ID, cause)
}

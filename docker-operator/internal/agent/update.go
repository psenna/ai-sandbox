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
//     the ActorID guard in MarkUnexpectedExit catches it too). The mutator
//     RE-CHECKS the status inside store.Update's transaction, so a Delete (or
//     a second Update) that won the race between step 1 and here is rejected
//     rather than silently un-marked.
//  4. ensureImage(newRef) -- inspect, and pull only if the daemon does not
//     already have it, exactly as Create does. Failure here -> failUpdate
//     (container intact, but the record is already StatusUpdating).
//  5. removeAgentContainer -- stop+remove the agent container ONLY.
//  6. store.Update -> new config fields + Image + ContainerID="" (still updating).
//  7. ensureAgentFiles, then startAgentContainer("--continue") ->
//     waitTmuxSession -> markRunning. Any failure from step 5 on -> failUpdate.
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
	//
	// The status is re-checked INSIDE the mutator, which runs in store.Update's
	// single bbolt read-write transaction: the check above raced anything else
	// holding this ID. Without the re-check, a Delete that marked the record
	// StatusDeleting in between would be silently reverted to StatusUpdating
	// here, and this flow would then recreate a container for an agent whose
	// volumes, network and record the delete is concurrently tearing down. The
	// same re-check rejects a second concurrent Update (updating is not an
	// updatable status), so only one recreate is ever in flight.
	a, err = m.store.Update(ctx, id, func(ag *store.Agent) error {
		if !updatableStatus(ag.Status) {
			return fmt.Errorf("%w (status %q)", ErrNotUpdatable, ag.Status)
		}
		ag.Status = store.StatusUpdating
		ag.ErrorMessage = ""
		return nil
	})
	if err != nil {
		if IsNotUpdatable(err) || store.IsNotFound(err) {
			// Lost the race to a concurrent delete/update: nothing was
			// mutated, so this is the caller's 409/404, not a failed update.
			return store.Agent{}, err
		}
		return store.Agent{}, fmt.Errorf("updating agent %q: marking it updating: %w", id, err)
	}

	// Make sure the new image is on the daemon. ensureImage inspects first and
	// pulls only on a miss -- the same call Create uses, and the reason a
	// locally built agent image (`make agent-image`, never pushed anywhere)
	// stays updatable: an unconditional pull would fail for it every time.
	if err := m.ensureImage(ctx, newRef); err != nil {
		return m.failUpdate(ctx, &a, err)
	}

	// The recreated agent container needs its DinD sidecar answering on
	// DOCKER_HOST from the moment it boots. Update normally leaves the
	// sidecar alone entirely (see the doc comment above), but an agent that
	// went through a host/daemon restart without the operator's startup
	// reconcile pass ever running against it (or whose sidecar died for some
	// other reason) can reach here with a stopped sidecar; recreating the
	// agent container on top of a dead DOCKER_HOST would just trade one
	// broken state for another. ensureDindRunning is a no-op when the sidecar
	// is already running.
	if _, err := m.ensureDindRunning(ctx, a); err != nil {
		return m.failUpdate(ctx, &a, fmt.Errorf("checking the dind sidecar before recreating the agent container: %w", err))
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
		ag.AutoMode = rs.autoMode
		ag.Image = newRef
		ag.ContainerID = ""
		// Re-assert the ID-derived resource names agentSpec reads, in case the
		// record predates stampNames or lost one. They are pure functions of
		// the immutable agent ID, so filling a blank in can only ever restore
		// the right name -- and an EMPTY Mount.Source would make Docker attach
		// a fresh ANONYMOUS volume, silently detaching the agent from its work.
		ag.ContainerName = firstNonEmpty(ag.ContainerName, agentContainerName(id))
		ag.DinernetName = firstNonEmpty(ag.DinernetName, dinernetName(id))
		ag.WorkspaceVolume = firstNonEmpty(ag.WorkspaceVolume, workspaceVolumeName(id))
		ag.ClaudeConfigVolume = firstNonEmpty(ag.ClaudeConfigVolume, claudeConfigVolumeName(id))
		return nil
	})
	if err != nil {
		return m.failUpdate(ctx, &a, fmt.Errorf("recording the updated agent config: %w", err))
	}

	// The file store's per-agent and shared subpaths must exist before the
	// daemon resolves agentSpec's subpath mounts. Create asserts this too; the
	// re-assertion matters when the file store was enabled (or its directory
	// re-created) after this agent was built.
	if err := m.ensureAgentFiles(ctx, a); err != nil {
		return m.failUpdate(ctx, &a, err)
	}
	if err := m.startAgentContainer(ctx, &a, rs.rb, append(autoModeArgs(a), "--continue")...); err != nil {
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
// The ErrorMessage says outright that nothing was lost, because that is the
// question the state raises: whichever step failed, an update NEVER removes a
// volume, the DinD sidecar or the private network, so the agent's workspace
// and Claude session history are always still there and Update can simply be
// re-run from StatusError.
//
// The only Docker work here is none at all: the sole call is the store write,
// deliberately, so nothing in this path can fail on the dead context that
// probably caused the failure being recorded. That write uses
// context.WithoutCancel(ctx) for the same reason -- the usual reason the tail
// of Update fails is a cancelled context, and the record must still be moved
// off StatusUpdating so it does not look like a crash to the next Reconcile.
func (m *Manager) failUpdate(ctx context.Context, a *store.Agent, cause error) (store.Agent, error) {
	m.log.ErrorContext(ctx, "in-place agent update failed; leaving the record in error with its volumes intact for a retry",
		"agent_id", a.ID, "error", cause)

	if _, err := m.store.Update(context.WithoutCancel(ctx), a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "update failed (nothing was lost: the workspace, Claude-config and DinD-cache volumes are intact -- fix the cause and retry the update): " + cause.Error()
		ag.ContainerID = ""
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record the failed update; the record stays in updating and the next reconcile pass will mark it error",
			"agent_id", a.ID, "error", err)
	}
	return store.Agent{}, fmt.Errorf("updating agent %q: %w", a.ID, cause)
}

package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// Delete tears down every Docker resource belonging to the agent and removes
// its store record.
//
// The order is forced by Docker itself: containers before the network they are
// attached to, and before the volumes they reference (Docker refuses to remove
// a volume any container still references, and that refusal is a real signal,
// not an inconvenience to force past).
//
//	mark deleting  -- frees the MAX_AGENTS slot immediately, before any slow
//	                  Docker call runs
//	stop + remove the agent container
//	stop + remove the DinD sidecar
//	disconnect the shared dependaproxy from the dinernet
//	remove the dinernet
//	remove the three volumes
//	delete the store record
//
// Delete is idempotent. Calling it on an agent that is already gone is a clean
// no-op that touches Docker not at all, and every individual step tolerates its
// object already being gone -- store.Delete, and dockerclient's ContainerStop /
// ContainerRemove / NetworkRemove / NetworkDisconnect / VolumeRemove, all
// document "already gone" as success. That is what lets the same routine serve
// as create-failure rollback and as the reconcile pass's cleanup, and it is why
// nothing here re-implements idempotency checks of its own: an extra inspect
// before each remove would add a race, not remove one.
func (m *Manager) Delete(ctx context.Context, id string) error {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		if store.IsNotFound(err) {
			// Second call. Deliberately NOT a label sweep for leftovers:
			// deciding what to do with Docker resources that no record claims
			// is Reconcile's job, under Reconcile's conservative rules.
			m.log.InfoContext(ctx, "delete: no such agent record; nothing to do", "agent_id", id)
			return nil
		}
		return fmt.Errorf("deleting agent %q: %w", id, err)
	}

	// Marking deleting first is what frees the slot immediately:
	// store.Status.CountsTowardCapacity excludes deleting on purpose, so the
	// cap is not held hostage by a slow teardown. Skipped when the record is
	// already deleting, so a resumed or retried delete does not churn the
	// record's UpdatedAt for nothing.
	if a.Status != store.StatusDeleting {
		a, err = m.store.Update(ctx, id, func(ag *store.Agent) error {
			ag.Status = store.StatusDeleting
			return nil
		})
		if err != nil {
			return fmt.Errorf("marking agent %q deleting: %w", id, err)
		}
	}

	if err := m.teardown(ctx, a); err != nil {
		return fmt.Errorf("deleting agent %q: %w", id, err)
	}
	if err := m.store.Delete(ctx, id); err != nil {
		return fmt.Errorf("deleting agent %q: %w", id, err)
	}
	m.log.InfoContext(ctx, "agent deleted", "agent_id", id)
	return nil
}

// teardown removes an agent's Docker resources, in dependency order, without
// touching its store record. It is the single routine Delete, create-failure
// rollback and the reconcile pass all share.
//
// It does NOT stop at the first failure. One volume the daemon refuses to
// remove must not strand the other five resources; every error is collected and
// returned together, and the record survives so the next reconcile pass sees
// exactly what is left.
func (m *Manager) teardown(ctx context.Context, a store.Agent) error {
	a = withDerivedNames(a)
	var errs []error

	// Containers first. Referenced by ID when create got far enough to record
	// one, otherwise by name -- the daemon resolves either, and a record that
	// crashed before any ID was stamped still names everything.
	for _, c := range []struct {
		ref  string
		what string
	}{
		{firstNonEmpty(a.ContainerID, a.ContainerName), "agent container"},
		{firstNonEmpty(a.DindContainerID, a.DindContainerName), "dind sidecar"},
	} {
		if c.ref == "" {
			continue
		}
		if err := m.docker.ContainerStop(ctx, c.ref, m.opts.StopTimeout); err != nil {
			errs = append(errs, fmt.Errorf("stopping the %s %q: %w", c.what, c.ref, err))
		}
		if err := m.docker.ContainerRemove(ctx, c.ref); err != nil {
			errs = append(errs, fmt.Errorf("removing the %s %q: %w", c.what, c.ref, err))
		}
	}

	// The shared dependaproxy container is joined to this network but must
	// survive it, so detach before removing. Docker refuses to remove a
	// network that still has an endpoint on it.
	if a.DinernetName != "" {
		if err := m.docker.NetworkDisconnect(ctx, a.DinernetName, m.cfg.DependaproxyContainer); err != nil {
			errs = append(errs, fmt.Errorf("disconnecting the shared dependaproxy container %q from network %q: %w",
				m.cfg.DependaproxyContainer, a.DinernetName, err))
		}
		if err := m.docker.NetworkRemove(ctx, a.DinernetName); err != nil {
			errs = append(errs, fmt.Errorf("removing network %q: %w", a.DinernetName, err))
		}
	}

	// Volumes last: this is the step that actually destroys the agent's data,
	// and Docker will refuse it outright if any container above survived.
	//
	// The shared centralized file-store volume is DELIBERATELY ABSENT here:
	// it is shared infrastructure, and an agent's agents/<id>/ subtree in it
	// is meant to outlive the agent (files survive Delete). teardown is
	// shared by Delete, create-failure rollback and Reconcile, so removing
	// the subtree here would silently destroy files a create retry, a
	// rollback or a reconcile pass had no business touching. PurgeAgentFiles
	// removes it explicitly, only when the caller asked for it.
	for _, v := range []string{a.WorkspaceVolume, a.ClaudeConfigVolume, a.DindCacheVolume} {
		if v == "" {
			continue
		}
		if err := m.docker.VolumeRemove(ctx, v); err != nil {
			errs = append(errs, fmt.Errorf("removing volume %q: %w", v, err))
		}
	}

	return errors.Join(errs...)
}

// PurgeAgentFiles removes the agent's agents/<id>/ subtree from the shared
// centralized file store. It is the one path that deletes those files --
// teardown deliberately does not (see its volume-removal block) -- so an
// agent's stored files survive Delete unless a caller asks for this
// explicitly (DELETE /api/agents/{id}?purge_files=true, or the web UI).
//
// A no-op returning nil when the file store is disabled. Idempotent:
// purging an agent whose directory is already gone is success.
func (m *Manager) PurgeAgentFiles(ctx context.Context, id string) error {
	if m.files == nil {
		return nil
	}
	if err := m.files.RemoveAgentDir(id); err != nil {
		return fmt.Errorf("purging the file-store directory for agent %q: %w", id, err)
	}
	m.log.InfoContext(ctx, "purged an agent's file-store directory", "agent_id", id)
	return nil
}

// withDerivedNames fills in any resource name the record is missing from the
// agent ID.
//
// Create stamps all six names in before it touches Docker, so in practice this
// changes nothing. It exists for the one case that matters: a record that
// crashed between store.Create and that first stamp. Names are a pure function
// of the ID, so teardown can still find everything that could possibly have
// been created rather than silently skipping it.
func withDerivedNames(a store.Agent) store.Agent {
	if a.ID == "" {
		return a
	}
	if a.ContainerName == "" {
		a.ContainerName = agentContainerName(a.ID)
	}
	if a.DindContainerName == "" {
		a.DindContainerName = dindContainerName(a.ID)
	}
	if a.DinernetName == "" {
		a.DinernetName = dinernetName(a.ID)
	}
	if a.WorkspaceVolume == "" {
		a.WorkspaceVolume = workspaceVolumeName(a.ID)
	}
	if a.ClaudeConfigVolume == "" {
		a.ClaudeConfigVolume = claudeConfigVolumeName(a.ID)
	}
	if a.DindCacheVolume == "" {
		a.DindCacheVolume = dindCacheVolumeName(a.ID)
	}
	return a
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

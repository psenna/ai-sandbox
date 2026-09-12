package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// Unmanaged is one Docker resource that carries the managed label but that no
// store record claims. Reconcile reports these and leaves them alone.
type Unmanaged struct {
	// Kind is "container", "network" or "volume".
	Kind string
	// Name is the resource's Docker name.
	Name string
	// AgentID is the agent-id label it carries, or "" if it carries none.
	AgentID string
}

// Report summarises one reconcile pass, so the caller can log a single line
// and the tests can assert on outcomes rather than on log output.
type Report struct {
	// Records is how many agent records the store held at the start of the pass.
	Records int
	// CleanedUp lists the IDs of records that were stuck mid-operation and
	// have now been torn down and removed.
	CleanedUp []string
	// Unmanaged lists the managed-labelled Docker resources no record claims.
	// They were reported, not touched.
	Unmanaged []Unmanaged
	// Woken lists the IDs of StatusRunning records whose DinD sidecar and/or
	// agent container were not actually running (typically because the host
	// or the Docker daemon restarted without the operator's own containers
	// carrying a restart policy) and have been started back up.
	Woken []string
}

// Reconcile is the startup pass that squares Docker's actual state with the
// store's record of it. It is deliberately conservative, and the asymmetry is
// the whole design:
//
//   - A resource carrying the managed label that NO store record claims is
//     logged as unmanaged and left completely alone. A lost, truncated or
//     rolled-back state file would otherwise cascade into silently destroying
//     live agent containers and every byte of work inside their volumes. The
//     store is not authoritative enough to justify that, and a human deleting
//     six leftover objects by hand is cheap next to the alternative.
//
//   - A record stuck in creating or deleting IS torn down automatically,
//     because that state is positive proof of a crash mid-operation: nothing
//     but this package writes those states, and nothing but a crash leaves one
//     behind across a restart. Its resources are half-built or half-removed by
//     definition, and its slot is either leaked or already released.
//
//   - A record stuck in updating (a crash mid in-place update) is marked
//     error, NOT torn down: the agent container may be gone but the three
//     volumes -- with every byte of the agent's work and its Claude session
//     history -- are always intact at that point, and teardown would destroy
//     them. The user retries Update, which is idempotent.
//
// Records in stopped or error are left untouched: keeping their status honest
// against the daemon is the event-stream goroutine's job (task 13), not a
// startup sweep's -- a StatusStopped or StatusError agent already reflects a
// decision the operator (or a human) made while it was watching, and this
// pass has no business overriding it.
//
// A StatusRunning record is different: it is the store's memory of "this
// agent is supposed to be running", and a host or Docker-daemon restart can
// silently invalidate that -- the daemon comes back up with every one of
// this agent's containers Exited, because none of them carry a restart
// policy (task 13's event goroutine, which would normally notice and flip
// the record to stopped/error, was not running to see it happen). So after
// the stuck-record sweep above, Reconcile also walks every StatusRunning
// record and starts back up whichever of its DinD sidecar and agent
// container is not actually running -- see wakeStoppedAgents. A container
// that IS already running is left completely untouched.
//
// Reconcile reports the first listing failure as an error but does not abort on
// a per-agent teardown or wake-up failure: one stuck or unrecoverable agent
// must not prevent the others from being cleaned up or woken. Whatever it
// could not remove is still labelled, so the next pass finds it again.
//
// The centralized file store is out of scope here: its volume carries no
// managed label, so findUnmanaged (which filters server-side by that label)
// never sees it, and an orphan agents/<id>/ directory left behind for a gone
// record is left alone by design -- the operator removes it via the web UI
// (or DELETE /api/agents/{id}?purge_files=true before the record is gone).
func (m *Manager) Reconcile(ctx context.Context) (Report, error) {
	agents, err := m.store.List(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("reconciling: %w", err)
	}

	rep := Report{Records: len(agents)}
	known := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		known[a.ID] = struct{}{}
	}

	// Computed BEFORE any teardown: a resource belonging to a stuck record is
	// claimed by that record and is not an orphan, and must not be reported as
	// one just because the pass is about to remove it.
	unmanaged, err := m.findUnmanaged(ctx, known)
	if err != nil {
		return Report{}, fmt.Errorf("reconciling: %w", err)
	}
	rep.Unmanaged = unmanaged
	for _, u := range unmanaged {
		m.log.WarnContext(ctx, "unmanaged docker resource left untouched: it carries the docker-operator managed label but no agent record claims it; remove it by hand if it really is an orphan",
			"kind", u.Kind, "name", u.Name, "agent_id", u.AgentID)
	}

	var errs []error
	for _, a := range agents {
		// A stuck in-place update: the container may be half-gone but the
		// volumes are intact. Mark it error and move on -- never teardown.
		if a.Status == store.StatusUpdating {
			m.log.InfoContext(ctx, "marking an agent whose record is stuck mid-update as error (its volumes are kept; retry the update)",
				"agent_id", a.ID)
			if _, err := m.store.Update(ctx, a.ID, func(ag *store.Agent) error {
				ag.Status = store.StatusError
				ag.ErrorMessage = "update interrupted; retry the update"
				return nil
			}); err != nil {
				errs = append(errs, fmt.Errorf("reconciling agent %q: marking a stuck update as error: %w", a.ID, err))
			}
			continue
		}
		if a.Status != store.StatusCreating && a.Status != store.StatusDeleting {
			continue
		}
		m.log.InfoContext(ctx, "tearing down an agent whose record is stuck mid-operation (proof of a crash during create or delete)",
			"agent_id", a.ID, "status", a.Status)
		if err := m.teardown(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("reconciling agent %q: %w", a.ID, err))
			continue
		}
		if err := m.store.Delete(ctx, a.ID); err != nil {
			errs = append(errs, fmt.Errorf("reconciling agent %q: deleting its record: %w", a.ID, err))
			continue
		}
		rep.CleanedUp = append(rep.CleanedUp, a.ID)
	}

	woken, wakeErrs := m.wakeStoppedAgents(ctx, agents)
	rep.Woken = woken
	errs = append(errs, wakeErrs...)

	m.log.InfoContext(ctx, "reconcile pass complete",
		"records", rep.Records, "cleaned_up", len(rep.CleanedUp), "unmanaged", len(rep.Unmanaged), "woken", len(rep.Woken))
	return rep, errors.Join(errs...)
}

// wakeStoppedAgents walks every StatusRunning record and starts back up
// whichever of its DinD sidecar and agent container is not actually running
// on the daemon -- see Reconcile's doc comment for why StatusRunning is the
// one status this pass acts on. It returns the IDs it successfully woke and
// collects (rather than aborting on) a per-agent failure, so one agent whose
// resources are unrecoverable does not stop the pass from waking the rest.
//
// A woken agent that failed is left StatusError (see wakeAgent), never
// silently re-marked StatusRunning against a reality that does not back it
// up.
func (m *Manager) wakeStoppedAgents(ctx context.Context, agents []store.Agent) ([]string, []error) {
	var woken []string
	var errs []error
	for _, a := range agents {
		if a.Status != store.StatusRunning {
			continue
		}
		awoke, err := m.wakeAgent(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("waking agent %q: %w", a.ID, err))
			continue
		}
		if awoke {
			woken = append(woken, a.ID)
		}
	}
	return woken, errs
}

// wakeAgent checks agent a's DinD sidecar and agent container against the
// daemon's actual state and starts back up whichever of the two is not
// running. It reports awoke=true only when it actually had to start
// something; a fully already-running agent is left completely untouched and
// reports awoke=false, nil.
//
// The agent container, when it needs restarting, is NOT simply `docker
// start`ed: a stopped container's ENTRYPOINT/Cmd -- including whatever
// claude session-resumption arg it was created or last updated with --
// replays unchanged on a plain start, and tmux itself does not survive the
// container stopping (its server dies with the container's PID 1), so a
// plain restart would boot a brand new tmux session anyway, just with a
// possibly-stale claude arg. Instead the agent container is recreated
// exactly like an in-place Update -- same volumes, same network, same
// DinD sidecar, same agent ID -- but with "--resume" so the fresh session
// picks the agent's previous Claude Code conversation back up from the
// preserved CLAUDE_CONFIG_DIR volume, the same way "--continue" does for an
// explicit Update.
//
// Any failure marks the record StatusError (never tearing anything down --
// exactly failUpdate's contract, which this mirrors): the workspace, Claude-
// config and DinD-cache volumes are always left intact, and the user can
// retry via Update once the underlying cause (a genuinely missing sidecar or
// container, most likely) is fixed.
func (m *Manager) wakeAgent(ctx context.Context, a store.Agent) (bool, error) {
	dindAwoke, err := m.ensureDindRunning(ctx, a)
	if err != nil {
		m.markWakeError(ctx, a.ID, fmt.Errorf("the dind sidecar: %w", err))
		return false, fmt.Errorf("the dind sidecar: %w", err)
	}

	ref := firstNonEmpty(a.ContainerID, a.ContainerName, agentContainerName(a.ID))
	c, err := m.docker.ContainerInspect(ctx, ref)
	switch {
	case dockerclient.IsNotFound(err):
		err = fmt.Errorf("the agent container %q no longer exists; recreate this agent", ref)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	case err != nil:
		err = fmt.Errorf("inspecting the agent container %q: %w", ref, err)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	case c.State == dockerclient.StateRunning:
		// Already running: don't touch it. The sidecar may still have needed
		// waking above -- that alone counts as having woken the agent.
		return dindAwoke, nil
	}

	m.log.InfoContext(ctx, "the agent container is not running (state %q); recreating it to resume the previous session",
		"agent_id", a.ID, "agent_container", ref, "state", c.State)

	rb, err := m.resolveBackendFromAgent(ctx, a)
	if err != nil {
		err = fmt.Errorf("resolving the backend to restart the agent container: %w", err)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	if err := m.removeAgentContainer(ctx, a); err != nil {
		err = fmt.Errorf("removing the old agent container: %w", err)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	if err := m.startAgentContainer(ctx, &a, rb, append(autoModeArgs(a), "--resume")...); err != nil {
		m.markWakeError(ctx, a.ID, fmt.Errorf("starting the agent container: %w", err))
		return false, err
	}
	if err := m.waitTmuxSession(ctx, a); err != nil {
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	return true, nil
}

// resolveBackendFromAgent re-derives a's resolvedBackend (the model routing
// and, for the anthropic backend, the CURRENT shared credential) from the
// agent record's already-resolved fields, for the one path that recreates an
// agent container with no caller-supplied CreateRequest: wakeAgent. It is
// exactly what resolveBackend computes for a create/update request that
// changed none of these fields, so re-resolving is idempotent.
func (m *Manager) resolveBackendFromAgent(ctx context.Context, a store.Agent) (resolvedBackend, error) {
	return m.resolveBackend(ctx, CreateRequest{
		Backend: a.Backend, Model: a.Model, FastModel: a.FastModel, OllamaURL: a.OllamaURL,
	})
}

// markWakeError settles a failed wake-up attempt the same way failUpdate
// settles a failed in-place update: StatusError with an explanatory message,
// no container ID (it may already be gone), and every volume left exactly as
// it is. Logged, not returned -- the caller already has the error that
// matters, and this write itself uses a context detached from ctx so it
// still lands even when ctx is what is failing.
func (m *Manager) markWakeError(ctx context.Context, id string, cause error) {
	if _, err := m.store.Update(context.WithoutCancel(ctx), id, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "could not bring this agent back up after a host/daemon restart " +
			"(its workspace, Claude-config and DinD-cache volumes are intact -- fix the cause and retry via Update): " + cause.Error()
		ag.ContainerID = ""
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record a failed wake-up attempt; the record stays running against a reality that does not back it up",
			"agent_id", id, "error", err)
	}
}

// ensureDindRunning makes sure agent a's DinD sidecar is actually running,
// starting it and waiting for it to report healthy when it is not. It
// reports awoke=true only when it actually had to start the container; an
// already-running sidecar is left completely untouched (awoke=false, nil).
//
// It does not attempt to create a missing sidecar: a DinD container that
// does not exist at all is not a "stopped" container recoverable by
// starting it -- that is a lost resource with no automatic recovery story,
// reported as an error rather than silently doing something more drastic.
//
// A sidecar that exits immediately (rather than timing out) is retried up to
// Options.DindWakeRetries times, DindWakeRetryDelay apart, before this gives
// up: right after a host reboot, the operator (and the sidecars it wakes
// back up here) can start before the host's sysbox-runc systemd units have
// finished initializing, so the very first start of a sysbox-runc container
// fails even though the runtime is correctly installed and a retry moments
// later succeeds. Any other waitHealthy failure (a timeout, or the sidecar
// having vanished) is not this kind of transient startup race and is
// returned on the first attempt.
//
// Shared by wakeAgent (the startup reconcile pass) and Update (an in-place
// update's recreated agent container needs the sidecar answering on
// DOCKER_HOST from the moment it boots, and Update otherwise never touches
// the sidecar at all).
func (m *Manager) ensureDindRunning(ctx context.Context, a store.Agent) (bool, error) {
	ref := firstNonEmpty(a.DindContainerID, a.DindContainerName, dindContainerName(a.ID))
	c, err := m.docker.ContainerInspect(ctx, ref)
	switch {
	case dockerclient.IsNotFound(err):
		return false, fmt.Errorf("the dind sidecar %q no longer exists; recreate this agent", ref)
	case err != nil:
		return false, fmt.Errorf("inspecting the dind sidecar %q: %w", ref, err)
	case c.State == dockerclient.StateRunning:
		return false, nil
	}

	m.log.InfoContext(ctx, "the dind sidecar is not running; starting it back up",
		"agent_id", a.ID, "dind_container", ref, "state", c.State)

	for attempt := 1; ; attempt++ {
		if err := m.docker.ContainerStart(ctx, ref); err != nil {
			return false, fmt.Errorf("starting the dind sidecar %q: %w", ref, err)
		}
		err := m.waitHealthy(ctx, ref, a.DindContainerName)
		if err == nil {
			return true, nil
		}
		var exited *dindExitedError
		if !errors.As(err, &exited) || attempt > m.opts.DindWakeRetries {
			return false, err
		}
		m.log.WarnContext(ctx, "the dind sidecar exited immediately; this looks like the sysbox-runc runtime not being fully initialized yet right after a host/daemon restart -- retrying",
			"agent_id", a.ID, "dind_container", ref, "attempt", attempt, "max_attempts", m.opts.DindWakeRetries+1, "error", err)
		if err := sleepCtx(ctx, m.opts.DindWakeRetryDelay); err != nil {
			return false, err
		}
	}
}

// findUnmanaged lists every managed-labelled container, network and volume and
// returns the ones whose agent-id label names no record in known.
//
// All three kinds are listed separately rather than inferred from container
// labels: a network or a volume routinely outlives the containers that used it
// -- that is precisely the leak this pass exists to surface -- so deriving them
// from containers would miss the most likely orphan there is.
func (m *Manager) findUnmanaged(ctx context.Context, known map[string]struct{}) ([]Unmanaged, error) {
	sel := managedSelector()
	var out []Unmanaged

	containers, err := m.docker.ContainerList(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("listing managed containers: %w", err)
	}
	for _, c := range containers {
		out = appendIfUnmanaged(out, known, "container", c.Name, c.Labels)
	}

	networks, err := m.docker.NetworkList(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("listing managed networks: %w", err)
	}
	for _, n := range networks {
		out = appendIfUnmanaged(out, known, "network", n.Name, n.Labels)
	}

	volumes, err := m.docker.VolumeList(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("listing managed volumes: %w", err)
	}
	for _, v := range volumes {
		out = appendIfUnmanaged(out, known, "volume", v.Name, v.Labels)
	}

	return out, nil
}

// appendIfUnmanaged records the resource unless a store record claims it. A
// resource with no agent-id label at all is unmanaged too: store IDs are never
// empty, so the empty key can never be in known.
func appendIfUnmanaged(out []Unmanaged, known map[string]struct{}, kind, name string, labels map[string]string) []Unmanaged {
	id := labels[LabelAgentID]
	if _, ok := known[id]; ok {
		return out
	}
	return append(out, Unmanaged{Kind: kind, Name: name, AgentID: id})
}

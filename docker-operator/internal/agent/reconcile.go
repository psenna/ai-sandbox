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
	// Skipped lists the IDs of records that looked stuck -- or, for a
	// StatusRunning record, needed waking -- but whose operation was in flight
	// in this process at the moment the pass reached them, and that were
	// therefore left completely alone. On a ticker these are healthy
	// create/update/delete/wake operations, not stuck records: the startup
	// pass never skips anything, because at startup nothing is in flight.
	Skipped []string
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
//     That first framing -- "nothing but a crash leaves one behind" -- is only
//     true at STARTUP. Once cmd/docker-operator runs this on a ticker, a record
//     in updating, creating or deleting is exactly what a HEALTHY in-flight
//     operation looks like too, and the two cannot be told apart from the store
//     alone. The missing half of the question is answered by Manager.inFlight, a
//     process-local registry of agent IDs with an operation in flight IN THIS
//     PROCESS: a record a crash left stuck has nothing in flight, precisely
//     because the process that would have been running its operation is gone.
//     So every branch below that would mark or tear a record down is taken only
//     when the record's ID is NOT in that registry; when it is, the record is
//     skipped and reported in Report.Skipped. At startup nothing is ever in
//     flight, so this changes startup's behaviour not at all.
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
// that IS already running is never stopped, restarted or recreated; the one
// thing wakeAgent still does to it is the best-effort dependaproxy address
// re-sync described on syncDependaproxyDinernetIP.
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
	rep.Unmanaged = m.reportUnmanaged(ctx, unmanaged)

	var errs []error
	for _, a := range agents {
		if !mayBeStuck(a.Status) {
			continue
		}
		// A record whose operation is in flight in THIS process is not stuck,
		// however it looks: it is a healthy create/update/delete that will
		// finish on its own. Skip it -- do not mark it, do not tear it down.
		if m.inFlight.busy(a.ID) {
			m.noteSkipped(ctx, &rep, a, "an operation is already in flight in this process")
			continue
		}
		// tryBegin is the authoritative gate: it closes the race the busy()
		// check above leaves open, so an operation that started in between
		// still wins and this pass skips rather than clobbering it. Holding
		// the reference for the teardown/mark below also stops a user-
		// initiated Update/Delete from interleaving with it.
		if !m.inFlight.tryBegin(a.ID) {
			m.noteSkipped(ctx, &rep, a, "an operation is already in flight in this process")
			continue
		}
		m.cleanupStuckRecord(ctx, a, &rep, &errs)
		m.inFlight.release(a.ID)
	}

	woken, wakeSkipped, wakeErrs := m.wakeStoppedAgents(ctx, agents)
	rep.Woken = woken
	rep.Skipped = append(rep.Skipped, wakeSkipped...)
	errs = append(errs, wakeErrs...)

	m.log.InfoContext(ctx, "reconcile pass complete",
		"records", rep.Records, "cleaned_up", len(rep.CleanedUp), "unmanaged", len(rep.Unmanaged),
		"woken", len(rep.Woken), "skipped", len(rep.Skipped))
	return rep, errors.Join(errs...)
}

// mayBeStuck reports whether a record in status s is one the pass would act
// on: the three states that, at startup, are proof of a crash, and that on a
// ticker must be checked against the in-flight registry first.
func mayBeStuck(s store.Status) bool {
	return s == store.StatusUpdating || s == store.StatusCreating || s == store.StatusDeleting
}

// noteSkipped records, in the report and the log, that a is mid-operation in
// this process and was therefore left alone.
func (m *Manager) noteSkipped(ctx context.Context, rep *Report, a store.Agent, reason string) {
	rep.Skipped = append(rep.Skipped, a.ID)
	m.log.InfoContext(ctx, "skipping an agent: it is not stuck, its operation is healthy and running now",
		"agent_id", a.ID, "status", a.Status, "reason", reason)
}

// cleanupStuckRecord handles one record the pass believes is stuck, having
// already reserved its ID in the in-flight registry (so no user-initiated
// operation can interleave): a stuck in-place update is marked error -- its
// volumes are kept, never torn down -- and a creating/deleting record is torn
// down and removed. Failures are collected rather than aborting the pass, so
// one unrecoverable agent cannot stop the others from being cleaned up.
func (m *Manager) cleanupStuckRecord(ctx context.Context, a store.Agent, rep *Report, errs *[]error) {
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
			*errs = append(*errs, fmt.Errorf("reconciling agent %q: marking a stuck update as error: %w", a.ID, err))
		}
		return
	}
	m.log.InfoContext(ctx, "tearing down an agent whose record is stuck mid-operation (proof of a crash during create or delete)",
		"agent_id", a.ID, "status", a.Status)
	if err := m.teardown(ctx, a); err != nil {
		*errs = append(*errs, fmt.Errorf("reconciling agent %q: %w", a.ID, err))
		return
	}
	if err := m.store.Delete(ctx, a.ID); err != nil {
		*errs = append(*errs, fmt.Errorf("reconciling agent %q: deleting its record: %w", a.ID, err))
		return
	}
	rep.CleanedUp = append(rep.CleanedUp, a.ID)
}

// reportUnmanaged filters findUnmanaged's result against the in-flight
// registry and logs whatever survives, warning only the FIRST time a resource
// name is seen in this process and dropping to Debug on every later sighting.
//
// The filter: a resource carrying an agent-id label is NOT an orphan while
// that agent has an operation in flight in this process. findUnmanaged keys on
// the store listing taken at the top of this pass, and Create registers in the
// in-flight registry BEFORE it inserts the record, so without this a mid-create
// agent whose record the pass's listing predates could have its very first
// resources reported as unmanaged.
//
// The warn-once: the Warn below is right once per boot, but on a 15-minute
// ticker a single persistent orphan would emit it ~96 times a day and bury
// every other warning. Report.Unmanaged stays COMPLETE either way -- this is a
// logging change only, never a filtering of the report from it.
func (m *Manager) reportUnmanaged(ctx context.Context, resources []Unmanaged) []Unmanaged {
	out := make([]Unmanaged, 0, len(resources))
	for _, u := range resources {
		if u.AgentID != "" && m.inFlight.busy(u.AgentID) {
			continue
		}
		out = append(out, u)
		key := u.Kind + "\x00" + u.Name
		if _, loaded := m.warnedUnmanaged.LoadOrStore(key, struct{}{}); loaded {
			m.log.DebugContext(ctx, "unmanaged docker resource still left untouched", "kind", u.Kind, "name", u.Name, "agent_id", u.AgentID)
			continue
		}
		m.log.WarnContext(ctx, "unmanaged docker resource left untouched: it carries the docker-operator managed label but no agent record claims it; remove it by hand if it really is an orphan",
			"kind", u.Kind, "name", u.Name, "agent_id", u.AgentID)
	}
	return out
}

// wakeStoppedAgents walks every StatusRunning record and starts back up
// whichever of its DinD sidecar and agent container is not actually running
// on the daemon -- see Reconcile's doc comment for why StatusRunning is the
// one status this pass acts on. It returns the IDs it successfully woke, the
// IDs it skipped because an operation for them was already in flight in this
// process (a concurrent Update/Delete, say), and collects (rather than
// aborting on) a per-agent failure, so one agent whose resources are
// unrecoverable does not stop the pass from waking the rest.
//
// Each wake reserves its agent ID in the in-flight registry first: it is a
// real operation on the record (it even moves it through StatusUpdating -- see
// wakeAgent's shield), so a concurrent reconcile pass must skip it rather than
// mistake its temporary StatusUpdating for a crashed update.
//
// A woken agent that failed is left StatusError (see wakeAgent), never
// silently re-marked StatusRunning against a reality that does not back it
// up.
func (m *Manager) wakeStoppedAgents(ctx context.Context, agents []store.Agent) (woken, skipped []string, errs []error) {
	for _, a := range agents {
		if a.Status != store.StatusRunning {
			continue
		}
		if !m.inFlight.tryBegin(a.ID) {
			skipped = append(skipped, a.ID)
			m.log.InfoContext(ctx, "skipping a running agent whose operation is already in flight in this process", "agent_id", a.ID)
			continue
		}
		awoke, err := m.wakeAgent(ctx, a)
		m.inFlight.release(a.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("waking agent %q: %w", a.ID, err))
			continue
		}
		if awoke {
			woken = append(woken, a.ID)
		}
	}
	return woken, skipped, errs
}

// wakeAgent checks agent a's DinD sidecar and agent container against the
// daemon's actual state and starts back up whichever of the two is not
// running. It reports awoke=true only when it actually had to start
// something; a fully already-running agent keeps both of its containers and
// reports awoke=false, nil.
//
// The one thing it does to an agent that needs no waking at all is the
// best-effort dependaproxy address re-sync it runs first
// (syncDependaproxyDinernetIP): that drift is invisible to every container
// state check below, and when it corrects an already-running agent container
// the corrected address is written into it in place
// (refreshDependaproxyIPFile) rather than by recreating it.
//
// The agent container, when it needs restarting, is NOT simply `docker
// start`ed: a stopped container's ENTRYPOINT/Cmd -- including whatever
// session-resumption arg it was created or last updated with -- replays
// unchanged on a plain start, and tmux itself does not survive the
// container stopping (its server dies with the container's PID 1), so a
// plain restart would boot a brand new tmux session anyway, just with a
// possibly-stale harness arg. Instead the agent container is recreated
// exactly like an in-place Update -- same volumes, same network, same
// DinD sidecar, same agent ID -- but with firstInvocationArgs(a,
// sessionResume) so the fresh session picks the agent's previous conversation
// back up from the preserved config volume. The resumption flag itself is
// harness-dependent: claude-code takes "--resume" here (this old tmux
// session did not survive), but opencode takes "--continue" -- opencode's
// "--resume" exits 1 immediately -- exactly like firstInvocationArgs decides
// for sessionContinue on an explicit Update.
//
// Any failure marks the record StatusError (never tearing anything down --
// exactly failUpdate's contract, which this mirrors): the workspace, Claude-
// config and DinD-cache volumes are always left intact, and the user can
// retry via Update once the underlying cause (a genuinely missing sidecar or
// container, most likely) is fixed.
//
// Because the recreate removes a container under a record that still says
// StatusRunning, the record is moved to StatusUpdating for the duration (the
// "shield" described at its call site below) so the status-sync events
// goroutine treats the old container's own die/stop as expected. The caller
// (wakeStoppedAgents) holds this agent's ID in the in-flight registry around
// this call, so Reconcile's StatusUpdating -> error branch never mistakes a
// healthy wake in progress for a crashed update.
func (m *Manager) wakeAgent(ctx context.Context, a store.Agent) (bool, error) {
	// Re-assert the dependaproxy dinernet attachment before anything else: if
	// the shared dependaproxy container was recreated while this agent's
	// containers were down, nothing else on this path would ever notice.
	dependaproxyChanged := m.syncDependaproxyDinernetIP(ctx, &a)

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
		// waking above -- that alone counts as having woken the agent. If the
		// dependaproxy address changed, this container is not about to be
		// recreated (which would pick it up via a fresh entrypoint.sh run on
		// its own), so refresh the file inside it directly.
		if dependaproxyChanged {
			m.refreshDependaproxyIPFile(ctx, a)
		}
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

	// SHIELD the record before removing the container.
	//
	// removeAgentContainer tears the container down while the record would
	// otherwise still say StatusRunning, and the status-sync events goroutine
	// (cmd/docker-operator's startStatusSync -> handleContainerEvent ->
	// MarkUnexpectedExit) watches for exactly that container leaving the
	// running state and would flip the record to stopped/error. At startup
	// this never bit, because startStatusSync is started AFTER the startup
	// reconcile pass; on a ticker it is already running, so a successfully
	// woken agent would end up in error. Update closes the same window the
	// same way: StatusUpdating is not StatusRunning, so MarkUnexpectedExit's
	// "not running" guard no-ops. markWakeUpdating re-checks the status inside
	// the store transaction and stands this wake down silently if an Update or
	// Delete won the race.
	//
	// Deliberate trade: a process crash mid-wake now leaves the record
	// StatusUpdating, which the next reconcile pass turns into StatusError
	// (volumes intact, manual retry), whereas before the next boot would have
	// re-woken it automatically. That is the price of making the wake safe to
	// run on a ticker at all.
	ok, err := m.markWakeUpdating(ctx, a.ID)
	if err != nil {
		err = fmt.Errorf("marking the agent updating before recreating its container: %w", err)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	if !ok {
		m.log.InfoContext(ctx, "the agent is no longer running; standing down from the wake-up and leaving the record to whichever operation won",
			"agent_id", a.ID)
		return false, nil
	}

	if err := m.removeAgentContainer(ctx, a); err != nil {
		err = fmt.Errorf("removing the old agent container: %w", err)
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	if err := m.startAgentContainer(ctx, &a, rb, firstInvocationArgs(a, sessionResume)...); err != nil {
		m.markWakeError(ctx, a.ID, fmt.Errorf("starting the agent container: %w", err))
		return false, err
	}
	if err := m.waitTmuxSession(ctx, a); err != nil {
		m.markWakeError(ctx, a.ID, err)
		return false, err
	}
	// The wake is complete: put the record back to StatusRunning, closing the
	// shield opened above. A failure here (the store is unhealthy) is recorded
	// like any other failed wake.
	if err := m.markRunning(ctx, &a); err != nil {
		m.markWakeError(ctx, a.ID, fmt.Errorf("marking the agent running after the wake-up: %w", err))
		return false, err
	}
	return true, nil
}

// errWakeStandDown is the internal sentinel markWakeUpdating's mutator returns
// to abort its store write (rolling the transaction back) when the record is
// no longer StatusRunning. It never escapes this package: markWakeUpdating
// maps it to (false, nil).
var errWakeStandDown = errors.New("the record is no longer running")

// markWakeUpdating transitions an agent record to StatusUpdating as the shield
// that makes wakeAgent's container removal safe on a ticker: while the record
// is updating, the old container's own die/stop event hits MarkUnexpectedExit's
// "not running" guard and no-ops (see wakeAgent).
//
// It reports ok=false, with a nil error, when the record was no longer
// StatusRunning inside the store transaction -- an Update or Delete won the
// race between the pass listing the record and this write -- meaning the wake
// must stand down without touching anything and without clobbering the winner.
//
// Extracted from wakeAgent so the transition and its stand-down are
// unit-testable on their own.
func (m *Manager) markWakeUpdating(ctx context.Context, id string) (bool, error) {
	_, err := m.store.Update(ctx, id, func(ag *store.Agent) error {
		if ag.Status != store.StatusRunning {
			return errWakeStandDown
		}
		ag.Status = store.StatusUpdating
		ag.ErrorMessage = ""
		return nil
	})
	switch {
	case errors.Is(err, errWakeStandDown):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// resolveBackendFromAgent re-derives a's resolvedBackend (the model routing
// and, for the anthropic backend, the record's existing Anthropic account
// pin) from the agent record's already-resolved fields, for the one path
// that recreates an agent container with no caller-supplied CreateRequest:
// wakeAgent. It is exactly what resolveBackend computes for a create/update
// request that changed none of these fields, so re-resolving is idempotent
// -- and, critically, it preserves a.AnthropicAccountID rather than
// re-resolving against whatever is currently default, exactly like Update.
func (m *Manager) resolveBackendFromAgent(ctx context.Context, a store.Agent) (resolvedBackend, error) {
	return m.resolveBackend(ctx, CreateRequest{
		Backend: a.Backend, Model: a.Model, FastModel: a.FastModel, OllamaURL: a.OllamaURL,
	}, a.AnthropicAccountID)
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

// ensureDindRunning makes sure agent a's DinD sidecar is actually running AND
// actually working, starting or recreating it as needed. It reports
// awoke=true only when it actually had to do one of those; a sidecar that
// is already running and healthy is left completely untouched (awoke=false,
// nil).
//
// It does not attempt to create a missing sidecar: a DinD container that
// does not exist at all is not a "stopped" container recoverable by
// starting it -- that is a lost resource with no automatic recovery story,
// reported as an error rather than silently doing something more drastic.
//
// A sidecar reported "running" by Docker but "unhealthy" by its own
// healthcheck (see dindSpec's Healthcheck.Test / dindSmokeTestImage) is
// recreated in place via recreateDind -- confirmed for real: a sysbox-backed
// sidecar can survive a host reboot while sysbox-mgr/sysbox-fs restart
// underneath it, keep answering ordinary API calls, yet be permanently
// unable to create a container until it is recreated. Restarting an
// already-running container would not fix this.
//
// A sidecar that exits immediately when (re)started (rather than timing out)
// is retried up to Options.DindWakeRetries times, DindWakeRetryDelay apart,
// before falling back to recreateDind: right after a host reboot, the
// operator (and the sidecars it wakes back up here) can start before the
// host's sysbox-runc systemd units have finished initializing, so the very
// first start of a sysbox-runc container fails even though the runtime is
// correctly installed and a retry moments later succeeds. Any other
// waitHealthy failure (a timeout) also falls back to recreateDind once,
// rather than being returned immediately -- the same last-resort recovery
// the running-but-unhealthy case above uses.
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
	case c.State == dockerclient.StateRunning && c.Health == dockerclient.HealthUnhealthy:
		// Running, but its own healthcheck (a real container-creation smoke
		// test -- see dindSmokeTestImage) has already proven it can no
		// longer create containers. This is exactly the failure mode a host
		// reboot leaves behind when sysbox-mgr/sysbox-fs restart underneath
		// an already-running sysbox container: Docker still shows it
		// "running" (its init process never died), but it lost whatever
		// runtime-level registration made it work. Restarting would not fix
		// this -- only a genuine recreate does.
		m.log.WarnContext(ctx, "the dind sidecar is running but unhealthy; recreating it in place",
			"agent_id", a.ID, "dind_container", ref)
		if err := m.recreateDind(ctx, &a); err != nil {
			return false, fmt.Errorf("recreating the unhealthy dind sidecar %q: %w", ref, err)
		}
		m.verifyDependaproxyReachable(ctx, a)
		return true, nil
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
			m.verifyDependaproxyReachable(ctx, a)
			return true, nil
		}
		var exited *dindExitedError
		if errors.As(err, &exited) && attempt <= m.opts.DindWakeRetries {
			m.log.WarnContext(ctx, "the dind sidecar exited immediately; this looks like the sysbox-runc runtime not being fully initialized yet right after a host/daemon restart -- retrying",
				"agent_id", a.ID, "dind_container", ref, "attempt", attempt, "max_attempts", m.opts.DindWakeRetries+1, "error", err)
			if err := sleepCtx(ctx, m.opts.DindWakeRetryDelay); err != nil {
				return false, err
			}
			continue
		}
		// Starting the existing container did not work, even after
		// retrying an immediate-exit up to the configured budget. One last
		// resort before giving up: recreate it from scratch, the same
		// recovery the running-but-unhealthy case above uses.
		m.log.WarnContext(ctx, "starting the existing dind sidecar did not succeed; attempting to recreate it",
			"agent_id", a.ID, "dind_container", ref, "error", err)
		if recreateErr := m.recreateDind(ctx, &a); recreateErr != nil {
			return false, fmt.Errorf("%w (recreate also failed: %s)", err, recreateErr)
		}
		m.verifyDependaproxyReachable(ctx, a)
		return true, nil
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

// SyncDependaproxyAddresses re-checks every RUNNING agent's recorded
// dependaproxy dinernet address against the daemon, reconnecting and
// re-stamping whatever drifted, and refreshing the file the running agent
// container's workload containers read the address from.
//
// Running-only on purpose: a stopped/error agent's container is not there to
// exec into, and its dinernet attachment is re-asserted by the Update or
// wake-up that brings it back. Cheap in the steady state -- one
// ContainerInspect of the shared dependaproxy container per agent and nothing
// else when nothing drifted.
func (m *Manager) SyncDependaproxyAddresses(ctx context.Context) error {
	agents, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("syncing dependaproxy addresses: %w", err)
	}
	for _, a := range agents {
		if a.Status != store.StatusRunning {
			continue
		}
		if m.syncDependaproxyDinernetIP(ctx, &a) {
			m.refreshDependaproxyIPFile(ctx, a)
		}
	}
	return nil
}

package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// ErrNotStoppable is returned by StopAgent when the agent is not running.
var ErrNotStoppable = errors.New("agent is not in a stoppable state")

// IsNotStoppable reports whether err was caused by a Stop attempt against an
// agent that is not running.
func IsNotStoppable(err error) bool { return errors.Is(err, ErrNotStoppable) }

// ErrNotResumable is returned by ResumeAgent when the agent is not stopped.
var ErrNotResumable = errors.New("agent is not in a resumable state")

// IsNotResumable reports whether err was caused by a Resume attempt against
// an agent that is not stopped.
func IsNotResumable(err error) bool { return errors.Is(err, ErrNotResumable) }

// StopAgent stops a running agent's containers -- the agent container and
// its DinD sidecar -- without removing anything. The record, its volumes,
// and its container IDs are left exactly as they are; only their
// daemon-side running state changes, so ResumeAgent can bring the same
// agent back up later.
//
// It refuses to start if another operation already holds this agent in this
// process (ErrOperationInFlight, a 409), exactly like Update and Delete.
func (m *Manager) StopAgent(ctx context.Context, id string) (store.Agent, error) {
	if !m.inFlight.tryBegin(id) {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w", id, ErrOperationInFlight)
	}
	defer m.inFlight.release(id)

	a, err := m.store.Get(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	if a.Status != store.StatusRunning {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w (status %q)", id, ErrNotStoppable, a.Status)
	}

	ok, err := m.markShielded(ctx, id, store.StatusRunning)
	if err != nil {
		return store.Agent{}, fmt.Errorf("stopping agent %q: marking it updating: %w", id, err)
	}
	if !ok {
		return store.Agent{}, fmt.Errorf("stopping agent %q: %w (status changed concurrently)", id, ErrNotStoppable)
	}

	agentRef := firstNonEmpty(a.ContainerID, a.ContainerName, agentContainerName(a.ID))
	dindRef := firstNonEmpty(a.DindContainerID, a.DindContainerName, dindContainerName(a.ID))
	var errs []error
	if err := m.docker.ContainerStop(ctx, agentRef, m.opts.StopTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stopping the agent container %q: %w", agentRef, err))
	}
	if err := m.docker.ContainerStop(ctx, dindRef, m.opts.StopTimeout); err != nil {
		errs = append(errs, fmt.Errorf("stopping the dind sidecar %q: %w", dindRef, err))
	}
	if err := errors.Join(errs...); err != nil {
		m.markStopError(ctx, id, err)
		return store.Agent{}, err
	}

	if err := m.markStopped(ctx, &a); err != nil {
		err = fmt.Errorf("stopping agent %q: marking it stopped: %w", id, err)
		m.markStopError(ctx, id, err)
		return store.Agent{}, err
	}
	return a, nil
}

// markStopped is StopAgent's success step: both containers are confirmed
// stopped, so the record becomes StatusStopped, closing the shield opened by
// markShielded.
func (m *Manager) markStopped(ctx context.Context, a *store.Agent) error {
	return m.stamp(ctx, a, "the stopped status", func(ag *store.Agent) {
		ag.Status = store.StatusStopped
		ag.ErrorMessage = ""
	})
}

// markStopError settles a failed Stop attempt: StatusError with an
// explanatory message. Unlike markRebuildError, nothing was removed here --
// Stop only ever calls ContainerStop, never ContainerRemove -- so container
// IDs and volumes are left completely untouched, and the message says so.
func (m *Manager) markStopError(ctx context.Context, id string, cause error) {
	if _, err := m.store.Update(context.WithoutCancel(ctx), id, func(ag *store.Agent) error {
		ag.Status = store.StatusError
		ag.ErrorMessage = "could not stop this agent (its containers, workspace, Claude-config and DinD-cache volumes are all intact -- fix the cause and retry): " + cause.Error()
		return nil
	}); err != nil {
		m.log.ErrorContext(ctx, "could not record a failed stop attempt; the record may no longer reflect reality",
			"agent_id", id, "error", err)
	}
}

// ResumeAgent brings a stopped agent back to StatusRunning. It is not a
// plain `docker start`: the agent container is recreated with
// firstInvocationArgs(a, sessionResume), exactly like wakeAgent's own
// recovery, because a stopped container's session does not survive a plain
// restart any better than it survives a recreate.
//
// It refuses to start if another operation already holds this agent in this
// process (ErrOperationInFlight, a 409), exactly like Update and Delete.
func (m *Manager) ResumeAgent(ctx context.Context, id string) (store.Agent, error) {
	if !m.inFlight.tryBegin(id) {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w", id, ErrOperationInFlight)
	}
	defer m.inFlight.release(id)

	a, err := m.store.Get(ctx, id)
	if err != nil {
		return store.Agent{}, err
	}
	if a.Status != store.StatusStopped {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w (status %q)", id, ErrNotResumable, a.Status)
	}

	if _, err := m.ensureDindRunning(ctx, a); err != nil {
		err = fmt.Errorf("the dind sidecar: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return store.Agent{}, err
	}

	rb, err := m.resolveBackendFromAgent(ctx, a)
	if err != nil {
		err = fmt.Errorf("resolving the backend to restart the agent container: %w", err)
		m.markRebuildError(ctx, a.ID, err)
		return store.Agent{}, err
	}

	ok, err := m.markShielded(ctx, id, store.StatusStopped)
	if err != nil {
		err = fmt.Errorf("marking the agent updating before resuming its container: %w", err)
		m.markRebuildError(ctx, id, err)
		return store.Agent{}, err
	}
	if !ok {
		return store.Agent{}, fmt.Errorf("resuming agent %q: %w (status changed concurrently)", id, ErrNotResumable)
	}

	if err := m.rebuildAgentContainer(ctx, &a, rb); err != nil {
		return store.Agent{}, err
	}
	return a, nil
}

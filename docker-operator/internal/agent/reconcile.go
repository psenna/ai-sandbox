package agent

import (
	"context"
	"errors"
	"fmt"

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
// Records in running, stopped or error are left untouched: keeping their status
// honest against the daemon is the event-stream goroutine's job (task 13), not
// a startup sweep's.
//
// Reconcile reports the first listing failure as an error but does not abort on
// a per-agent teardown failure: one stuck agent must not prevent the others
// from being cleaned up. Whatever it could not remove is still labelled, so the
// next pass finds it again.
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

	m.log.InfoContext(ctx, "reconcile pass complete",
		"records", rep.Records, "cleaned_up", len(rep.CleanedUp), "unmanaged", len(rep.Unmanaged))
	return rep, errors.Join(errs...)
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

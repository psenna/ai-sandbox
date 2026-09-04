package agent

import (
	"context"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// seedStuckAgent creates a store record in the given status with every
// resource name derived and the matching Docker resources actually present
// in f, simulating a process that crashed mid-create or mid-delete.
func seedStuckAgent(t *testing.T, m *Manager, f *dockerclienttest.Fake, status store.Status) store.Agent {
	t.Helper()
	ctx := context.Background()

	a, err := m.store.Create(ctx, store.CreateSpec{ID: "agt_" + string(status)})
	if err != nil {
		t.Fatalf("seeding a %s record: %v", status, err)
	}
	a, err = m.store.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.ContainerName = agentContainerName(ag.ID)
		ag.DindContainerName = dindContainerName(ag.ID)
		ag.DinernetName = dinernetName(ag.ID)
		ag.WorkspaceVolume = workspaceVolumeName(ag.ID)
		ag.ClaudeConfigVolume = claudeConfigVolumeName(ag.ID)
		ag.DindCacheVolume = dindCacheVolumeName(ag.ID)
		ag.Status = status
		return nil
	})
	if err != nil {
		t.Fatalf("stamping names on the %s record: %v", status, err)
	}

	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: a.WorkspaceVolume, Labels: labelsFor(a.ID, RoleWorkspaceVolume)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: a.DinernetName, Labels: labelsFor(a.ID, RoleDinernet)}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	if _, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: a.DindContainerName, Labels: labelsFor(a.ID, RoleDind)}); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	return a
}

// TestReconcile_CreatingStuckRecord_TornDownAndRemoved is #66's explicit
// acceptance criterion: a record stuck in StatusCreating (proof of a crash
// mid-create) is torn down and removed automatically.
func TestReconcile_CreatingStuckRecord_TornDownAndRemoved(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	a := seedStuckAgent(t, m, f, store.StatusCreating)

	rep, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 1 || rep.CleanedUp[0] != a.ID {
		t.Errorf("Reconcile Report.CleanedUp = %v, want [%q]", rep.CleanedUp, a.ID)
	}
	if len(rep.Unmanaged) != 0 {
		t.Errorf("Reconcile Report.Unmanaged = %v, want none (the stuck record claims its own resources)", rep.Unmanaged)
	}
	if _, err := st.Get(context.Background(), a.ID); !store.IsNotFound(err) {
		t.Errorf("Get after Reconcile: err = %v, want store.IsNotFound", err)
	}
	if got := snapshotCounts(f); got.volumes != 0 || got.networks != 0 || got.containers != 1 {
		t.Errorf("docker resources after Reconcile = %+v, want the stuck agent's resources torn down (containers=1 is just the untouched shared dependaproxy)", got)
	}
}

// TestReconcile_DeletingStuckRecord_TornDownAndRemoved is #66's other
// explicit acceptance criterion, for the mirror case: a record stuck in
// StatusDeleting is torn down and removed too.
func TestReconcile_DeletingStuckRecord_TornDownAndRemoved(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	a := seedStuckAgent(t, m, f, store.StatusDeleting)

	rep, err := m.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 1 || rep.CleanedUp[0] != a.ID {
		t.Errorf("Reconcile Report.CleanedUp = %v, want [%q]", rep.CleanedUp, a.ID)
	}
	if _, err := st.Get(context.Background(), a.ID); !store.IsNotFound(err) {
		t.Errorf("Get after Reconcile: err = %v, want store.IsNotFound", err)
	}
	if got := snapshotCounts(f); got.volumes != 0 || got.networks != 0 || got.containers != 1 {
		t.Errorf("docker resources after Reconcile = %+v, want the stuck agent's resources torn down (containers=1 is just the untouched shared dependaproxy)", got)
	}
}

// TestReconcile_UnmanagedResource_ReportedNotTouched is #66's explicit
// acceptance criterion for the conservative side: a managed-labelled
// resource no store record claims is reported, and left alone -- still
// there afterward.
func TestReconcile_UnmanagedResource_ReportedNotTouched(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	const orphanID = "agt_orphan1"
	const volName = "docker-operator-agent-agt_orphan1-workspace"
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: volName, Labels: labelsFor(orphanID, RoleWorkspaceVolume)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 0 {
		t.Errorf("Reconcile Report.CleanedUp = %v, want none", rep.CleanedUp)
	}
	found := false
	for _, u := range rep.Unmanaged {
		if u.Kind == "volume" && u.Name == volName {
			found = true
			if u.AgentID != orphanID {
				t.Errorf("Unmanaged{%q}.AgentID = %q, want %q", volName, u.AgentID, orphanID)
			}
		}
	}
	if !found {
		t.Errorf("Reconcile Report.Unmanaged = %v, want it to include volume %q", rep.Unmanaged, volName)
	}

	// The whole point: it must still exist afterward.
	if _, err := f.VolumeInspect(ctx, volName); err != nil {
		t.Errorf("VolumeInspect(%q) after Reconcile: %v, want it left untouched", volName, err)
	}
}

// TestReconcile_ManagedLabelNoAgentIDLabel_ReportedAsUnmanaged proves a
// resource that carries the managed label but no agent-id label at all is
// also reported unmanaged, not skipped or mistaken for a match.
func TestReconcile_ManagedLabelNoAgentIDLabel_ReportedAsUnmanaged(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	const netName = "some-network-with-only-the-managed-label"
	if _, err := f.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: netName, Labels: map[string]string{LabelManaged: LabelManagedValue}}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	found := false
	for _, u := range rep.Unmanaged {
		if u.Kind == "network" && u.Name == netName {
			found = true
			if u.AgentID != "" {
				t.Errorf("Unmanaged{%q}.AgentID = %q, want empty", netName, u.AgentID)
			}
		}
	}
	if !found {
		t.Errorf("Reconcile Report.Unmanaged = %v, want it to include network %q", rep.Unmanaged, netName)
	}
	if _, err := f.NetworkInspect(ctx, netName); err != nil {
		t.Errorf("NetworkInspect(%q) after Reconcile: %v, want it left untouched", netName, err)
	}
}

// TestReconcile_HealthyRunningAgent_SurvivesUntouched proves a normal,
// healthy StatusRunning agent is left completely alone by a reconcile pass:
// not cleaned up, and not reported as unmanaged (its own record claims it).
func TestReconcile_HealthyRunningAgent_SurvivesUntouched(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := snapshotCounts(f)

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 0 {
		t.Errorf("Reconcile Report.CleanedUp = %v, want none", rep.CleanedUp)
	}
	if len(rep.Unmanaged) != 0 {
		t.Errorf("Reconcile Report.Unmanaged = %v, want none", rep.Unmanaged)
	}
	if rep.Records != 1 {
		t.Errorf("Reconcile Report.Records = %d, want 1", rep.Records)
	}

	got, gerr := st.Get(ctx, a.ID)
	if gerr != nil {
		t.Fatalf("Get after Reconcile: %v", gerr)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status after Reconcile = %q, want %q (untouched)", got.Status, store.StatusRunning)
	}
	if after := snapshotCounts(f); after != before {
		t.Errorf("docker resources after Reconcile = %+v, want unchanged from %+v", after, before)
	}
}

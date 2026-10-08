package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/psenna/ai-sandbox/docker-operator/internal/config"
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

// TestReconcile_StuckUpdatingMarkedError proves a record stuck in
// StatusUpdating (the operator crashed mid in-place update) is marked
// StatusError -- with the "retry the update" message -- and, crucially, is
// NOT torn down: its volumes (holding the agent's work and Claude session
// history) must survive, and its record must not be deleted.
func TestReconcile_StuckUpdatingMarkedError(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := seedStuckAgent(t, m, f, store.StatusUpdating)
	before := snapshotCounts(f)

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 0 {
		t.Errorf("Report.CleanedUp = %v, want none (a stuck update is not torn down)", rep.CleanedUp)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get after Reconcile: %v (the record must survive)", err)
	}
	if got.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusError)
	}
	if got.ErrorMessage != "update interrupted; retry the update" {
		t.Errorf("ErrorMessage = %q, want the retry message", got.ErrorMessage)
	}
	if after := snapshotCounts(f); after != before {
		t.Errorf("docker resources after Reconcile = %+v, want unchanged from %+v (volumes must survive)", after, before)
	}
}

// --- in-flight registry: the periodic-reconcile hazard --------------------

// TestReconcile_InFlightRecord_SkippedThenCleanedUp is the acceptance
// criterion for making Reconcile safe on a ticker: while an operation for a
// record is in flight in THIS process, the pass must leave the record
// completely alone (not mark it, not tear it down, not wake it); once that
// operation is gone, a later pass must clean it up exactly as the startup pass
// always did.
func TestReconcile_InFlightRecord_SkippedThenCleanedUp(t *testing.T) {
	cases := []struct {
		name   string
		status store.Status
	}{
		{"creating", store.StatusCreating},
		{"deleting", store.StatusDeleting},
		{"updating", store.StatusUpdating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f, st := newTestManager(t, 5)
			ctx := context.Background()
			a := seedStuckAgent(t, m, f, tc.status)
			before := snapshotCounts(f)

			// Simulate a healthy operation in flight for this record.
			m.inFlight.begin(a.ID)

			rep, err := m.Reconcile(ctx)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(rep.CleanedUp) != 0 {
				t.Errorf("Report.CleanedUp = %v, want none (the record is mid-operation, not stuck)", rep.CleanedUp)
			}
			if !containsID(rep.Skipped, a.ID) {
				t.Errorf("Report.Skipped = %v, want it to include %q", rep.Skipped, a.ID)
			}
			got, gerr := st.Get(ctx, a.ID)
			if gerr != nil {
				t.Fatalf("Get after the skipped pass: %v (the record must survive)", gerr)
			}
			if got.Status != tc.status {
				t.Errorf("Status after the skipped pass = %q, want it left at %q (untouched)", got.Status, tc.status)
			}
			if after := snapshotCounts(f); after != before {
				t.Errorf("docker resources after the skipped pass = %+v, want unchanged from %+v", after, before)
			}

			// The operation finishes and releases the record: a later pass must
			// do exactly what the startup pass does, and nothing must be left
			// skipped.
			m.inFlight.release(a.ID)
			rep2, err := m.Reconcile(ctx)
			if err != nil {
				t.Fatalf("Reconcile after release: %v", err)
			}
			if len(rep2.Skipped) != 0 {
				t.Errorf("Report.Skipped after release = %v, want none", rep2.Skipped)
			}
			switch tc.status {
			case store.StatusUpdating:
				got2, gerr := st.Get(ctx, a.ID)
				if gerr != nil {
					t.Fatalf("Get after release: %v (a stuck update is kept)", gerr)
				}
				if got2.Status != store.StatusError {
					t.Errorf("Status after release = %q, want %q", got2.Status, store.StatusError)
				}
			default:
				if len(rep2.CleanedUp) != 1 || rep2.CleanedUp[0] != a.ID {
					t.Errorf("Report.CleanedUp after release = %v, want [%q]", rep2.CleanedUp, a.ID)
				}
				if _, gerr := st.Get(ctx, a.ID); !store.IsNotFound(gerr) {
					t.Errorf("Get after release: err = %v, want store.IsNotFound", gerr)
				}
			}
		})
	}
}

// TestReconcile_UnmanagedInFlight_Filtered proves a managed resource belonging
// to an agent with an operation in flight in this process is NOT reported as
// an orphan mid-operation, and is reported once that operation is gone.
func TestReconcile_UnmanagedInFlight_Filtered(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	const ghostID = "agt_ghost_inflight"
	const volName = "docker-operator-agent-agt_ghost_inflight-workspace"
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: volName, Labels: labelsFor(ghostID, RoleWorkspaceVolume)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}

	m.inFlight.begin(ghostID)
	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if hasUnmanaged(rep.Unmanaged, volName) {
		t.Errorf("Report.Unmanaged = %v, want the in-flight agent's resource excluded", rep.Unmanaged)
	}

	m.inFlight.release(ghostID)
	rep2, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after release: %v", err)
	}
	if !hasUnmanaged(rep2.Unmanaged, volName) {
		t.Errorf("Report.Unmanaged after release = %v, want it to include %q", rep2.Unmanaged, volName)
	}
}

// TestReconcile_UnmanagedWarnedOnce_ThenDebug pins the logging dedup: a
// persistent orphan is Warned about the FIRST time this process sees it and
// logged at Debug on every later pass, while Report.Unmanaged stays complete
// on every pass.
func TestReconcile_UnmanagedWarnedOnce_ThenDebug(t *testing.T) {
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImageClaudeCode)
	st := newTestStore(t, 5)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(f, newTestRegistries(), st, cfg, log, testOptions())
	ctx := context.Background()

	const orphanID = "agt_orphan_warn_once"
	const volName = "docker-operator-agent-agt_orphan_warn_once-workspace"
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: volName, Labels: labelsFor(orphanID, RoleWorkspaceVolume)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}

	for i := 1; i <= 2; i++ {
		rep, err := m.Reconcile(ctx)
		if err != nil {
			t.Fatalf("Reconcile pass %d: %v", i, err)
		}
		if !hasUnmanaged(rep.Unmanaged, volName) {
			t.Errorf("pass %d: Report.Unmanaged = %v, want it to include %q (the report is never filtered by the dedup)", i, rep.Unmanaged, volName)
		}
	}

	if n := strings.Count(buf.String(), "unmanaged docker resource left untouched"); n != 1 {
		t.Errorf("Warn appeared %d times, want exactly 1 across two passes; log:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "unmanaged docker resource still left untouched"); n != 1 {
		t.Errorf("Debug appeared %d times, want exactly 1 (the second pass); log:\n%s", n, buf.String())
	}
}

// blockingExecCreate wraps a Fake so its ExecCreate call blocks until the test
// releases it, ONCE. It lets a lifecycle operation be held open deterministically
// -- no timing guesswork -- so a concurrently-running Reconcile can be shown to
// skip that record. Armed after construction so the same manager can build its
// fixture agent first.
type blockingExecCreate struct {
	*dockerclienttest.Fake
	armed   atomic.Bool
	enter   chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingExecCreate(f *dockerclienttest.Fake) *blockingExecCreate {
	return &blockingExecCreate{Fake: f, enter: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingExecCreate) arm() { b.armed.Store(true) }

func (b *blockingExecCreate) ExecCreate(ctx context.Context, containerID string, spec dockerclient.ExecSpec) (string, error) {
	if b.armed.Load() {
		b.once.Do(func() { close(b.enter) })
		<-b.release
	}
	return b.Fake.ExecCreate(ctx, containerID, spec)
}

var _ dockerclient.Client = (*blockingExecCreate)(nil)

// blockedTestManager wires a Manager whose ExecCreate blocks once armed, so a
// lifecycle operation can be parked mid-flight.
func blockedTestManager(t *testing.T) (*Manager, *dockerclienttest.Fake, *store.Store, *blockingExecCreate) {
	t.Helper()
	f := dockerclienttest.New()
	f.AutoHealthy = true
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	f.AddImage(dindImage)
	f.AddImage(cfg.AgentImageClaudeCode)
	st := newTestStore(t, 5)
	blocked := newBlockingExecCreate(f)
	m := NewManager(blocked, newTestRegistries(), st, cfg, testLogger(), testOptions())
	return m, f, st, blocked
}

// singleRecord returns the one agent record the store holds, waiting briefly
// for it to appear.
func singleRecord(t *testing.T, st *store.Store) store.Agent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		agents, err := st.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(agents) == 1 {
			return agents[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("store held %d records, want exactly 1", len(agents))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestReconcile_CreateInFlight_Skipped is the sharpest form of the hazard:
// with a Create parked mid-operation (its record already StatusCreating and
// its ID already registered), a Reconcile pass must NOT tear that record down
// the way a startup pass would, and must report it as Skipped.
func TestReconcile_CreateInFlight_Skipped(t *testing.T) {
	m, _, st, blocked := blockedTestManager(t)
	ctx := context.Background()

	blocked.arm()
	createDone := make(chan error, 1)
	go func() {
		_, err := m.Create(ctx, CreateRequest{})
		createDone <- err
	}()

	<-blocked.enter // Create is now parked, its record already inserted and in flight
	a := singleRecord(t, st)
	if a.Status != store.StatusCreating {
		t.Fatalf("parked record Status = %q, want %q", a.Status, store.StatusCreating)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 0 {
		t.Errorf("Report.CleanedUp = %v, want none (the create is healthy, not a crash)", rep.CleanedUp)
	}
	if !containsID(rep.Skipped, a.ID) {
		t.Errorf("Report.Skipped = %v, want it to include %q", rep.Skipped, a.ID)
	}
	if _, gerr := st.Get(ctx, a.ID); gerr != nil {
		t.Fatalf("the record was not left alone: Get after Reconcile = %v", gerr)
	}

	close(blocked.release) // let the create finish
	<-createDone
}

// TestReconcile_UpdateInFlight_Skipped proves the same for the record status
// the startup pass turns into an error: a record StatusUpdating because a real
// in-place Update is mid-flight must be skipped, never marked error.
func TestReconcile_UpdateInFlight_Skipped(t *testing.T) {
	m, _, st, blocked := blockedTestManager(t)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	blocked.arm()
	updateDone := make(chan error, 1)
	go func() {
		_, err := m.Update(ctx, a.ID, UpdateRequest{})
		updateDone <- err
	}()

	<-blocked.enter // Update is parked (its waitTmuxSession exec), the record already StatusUpdating
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusUpdating {
		t.Fatalf("parked record Status = %q, want %q", got.Status, store.StatusUpdating)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 0 {
		t.Errorf("Report.CleanedUp = %v, want none", rep.CleanedUp)
	}
	if !containsID(rep.Skipped, a.ID) {
		t.Errorf("Report.Skipped = %v, want it to include %q", rep.Skipped, a.ID)
	}
	still, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if still.Status != store.StatusUpdating {
		t.Errorf("Status after Reconcile = %q, want it left at %q (not clobbered)", still.Status, store.StatusUpdating)
	}

	close(blocked.release)
	if err := <-updateDone; err != nil {
		t.Fatalf("Update after release: %v", err)
	}
	done, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if done.Status != store.StatusRunning {
		t.Errorf("Status after Update = %q, want %q", done.Status, store.StatusRunning)
	}
}

// TestReconcile_FailedDeleteThenReconcile_CleansUp is the motivating case for
// the whole change: a Delete that failed partway left a record StatusDeleting
// with resources behind, and -- with nothing in flight any more -- a later
// reconcile pass finishes the teardown, without a restart.
func TestReconcile_FailedDeleteThenReconcile_CleansUp(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	// Fail the network removal: teardown collects the error and Delete returns
	// without deleting the record, leaving it StatusDeleting with the network
	// (and nothing else) behind.
	f.FailOnce(dockerclienttest.OpNetworkRemove, errors.New("boom: network remove"))
	if err := m.Delete(ctx, a.ID); err == nil {
		t.Fatal("Delete = nil error, want the injected network-remove failure")
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get after the failed delete: %v", err)
	}
	if got.Status != store.StatusDeleting {
		t.Fatalf("Status after the failed delete = %q, want %q", got.Status, store.StatusDeleting)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.CleanedUp) != 1 || rep.CleanedUp[0] != a.ID {
		t.Errorf("Report.CleanedUp = %v, want [%q]", rep.CleanedUp, a.ID)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("Report.Skipped = %v, want none (nothing is in flight)", rep.Skipped)
	}
	if _, gerr := st.Get(ctx, a.ID); !store.IsNotFound(gerr) {
		t.Errorf("Get after Reconcile: err = %v, want store.IsNotFound", gerr)
	}
	if n := f.Networks(); len(n) != 0 {
		t.Errorf("networks after Reconcile = %v, want the leftover dinernet removed", n)
	}
}

// --- wake shield ----------------------------------------------------------

// TestMarkWakeUpdating pins the shield's transition and its stand-down.
func TestMarkWakeUpdating(t *testing.T) {
	t.Run("a running record is moved to updating", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)

		ok, err := m.markWakeUpdating(ctx, a.ID)
		if err != nil {
			t.Fatalf("markWakeUpdating: %v", err)
		}
		if !ok {
			t.Fatal("markWakeUpdating = false, want true for a running record")
		}
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != store.StatusUpdating {
			t.Errorf("Status = %q, want %q", got.Status, store.StatusUpdating)
		}
	})

	t.Run("a record that is no longer running stands the wake down untouched", func(t *testing.T) {
		m, _, st := newTestManager(t, 5)
		ctx := context.Background()
		a := createRunningAgent(t, m)
		if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
			ag.Status = store.StatusError
			ag.ErrorMessage = "set by another operation"
			return nil
		}); err != nil {
			t.Fatalf("racing the wake: %v", err)
		}

		ok, err := m.markWakeUpdating(ctx, a.ID)
		if err != nil {
			t.Fatalf("markWakeUpdating: %v", err)
		}
		if ok {
			t.Fatal("markWakeUpdating = true, want false (the record is no longer running)")
		}
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != store.StatusError || got.ErrorMessage != "set by another operation" {
			t.Errorf("record = %+v, want it untouched by the losing wake", got)
		}
	})

	t.Run("a missing record is an error, not a silent stand-down", func(t *testing.T) {
		m, _, _ := newTestManager(t, 5)
		if _, err := m.markWakeUpdating(context.Background(), "agt_missing"); !store.IsNotFound(err) {
			t.Errorf("markWakeUpdating(missing) err = %v, want store.IsNotFound", err)
		}
	})
}

// TestWakeAgent_LosesRace_StandsDown proves wakeAgent does not clobber a
// record an Update/Delete won: with the record no longer running, it stands
// down without removing or recreating anything and leaves the record as it is.
func TestWakeAgent_LosesRace_StandsDown(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	if err := f.ContainerStop(ctx, a.ContainerID, 0); err != nil {
		t.Fatalf("stopping the agent container: %v", err)
	}
	// An Update/Delete won the race in the window between the pass listing the
	// record and this wake's write.
	if _, err := st.Update(ctx, a.ID, func(ag *store.Agent) error {
		ag.Status = store.StatusStopped
		return nil
	}); err != nil {
		t.Fatalf("racing the wake: %v", err)
	}

	before := len(f.Calls())
	awoke, err := m.wakeAgent(ctx, a) // a still carries the stale StatusRunning copy
	if err != nil {
		t.Fatalf("wakeAgent: %v", err)
	}
	if awoke {
		t.Error("wakeAgent = true, want false (it stood down)")
	}
	for _, c := range callsAfter(f, before) {
		if c.Op == dockerclienttest.OpContainerRemove || c.Op == dockerclienttest.OpContainerCreate {
			t.Errorf("the losing wake touched the agent container: %+v", c)
		}
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q (the winner's value, untouched)", got.Status, store.StatusStopped)
	}
}

// TestReconcile_WakeLeavesRecordRunningAndReleasesRegistry proves a successful
// wake ends StatusRunning (the shield closed) and leaves the in-flight registry
// empty, so the next pass treats the record normally.
func TestReconcile_WakeLeavesRecordRunningAndReleasesRegistry(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()
	a := createRunningAgent(t, m)

	if err := f.ContainerStop(ctx, a.DindContainerID, 0); err != nil {
		t.Fatalf("stopping the dind sidecar: %v", err)
	}
	if err := f.ContainerStop(ctx, a.ContainerID, 0); err != nil {
		t.Fatalf("stopping the agent container: %v", err)
	}

	rep, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Woken) != 1 || rep.Woken[0] != a.ID {
		t.Fatalf("Report.Woken = %v, want [%q]", rep.Woken, a.ID)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, store.StatusRunning)
	}
	if m.inFlight.busy(a.ID) {
		t.Error("the in-flight registry still holds the agent after the pass; a wake must release its reference")
	}
}

// containsID reports whether ids includes id.
func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// hasUnmanaged reports whether resources includes a resource named name.
func hasUnmanaged(resources []Unmanaged, name string) bool {
	for _, u := range resources {
		if u.Name == name {
			return true
		}
	}
	return false
}

// TestResolveBackendFromAgent_PreservesRecordsOwnPin is a regression test for
// Task 3's review finding: resolveBackendFromAgent (the wake-agent path's
// direct resolveBackend caller) must pass the AGENT RECORD's own
// AnthropicAccountID as existingAccountID, not "" -- otherwise a routine
// wake-up would silently re-resolve to whatever is currently the operator
// default, moving the agent off the account it was created/updated against.
// Passing "" here would still compile and still pass every other test in
// this package; only this assertion catches it.
func TestResolveBackendFromAgent_PreservesRecordsOwnPin(t *testing.T) {
	m, _, st := newTestManager(t, 5)
	ctx := context.Background()

	first, err := st.CreateAnthropicAccount(ctx, "Work", store.AnthropicKindAPIKey, "sk-ant-work")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount(first): %v", err)
	}
	second, err := st.CreateAnthropicAccount(ctx, "Personal", store.AnthropicKindAPIKey, "sk-ant-personal")
	if err != nil {
		t.Fatalf("CreateAnthropicAccount(second): %v", err)
	}
	if err := st.SetDefaultAnthropicAccount(ctx, second.ID); err != nil {
		t.Fatalf("SetDefaultAnthropicAccount: %v", err)
	}

	// A record already pinned to the account that is NOT the current
	// default -- the realistic "existing agent whose pin differs from the
	// current default" case a wake-up must not disturb.
	a := store.Agent{Backend: config.BackendAnthropic, AnthropicAccountID: first.ID}

	rb, err := m.resolveBackendFromAgent(ctx, a)
	if err != nil {
		t.Fatalf("resolveBackendFromAgent: %v", err)
	}
	if rb.accountID != first.ID {
		t.Fatalf("resolveBackendFromAgent accountID = %q; want the agent's own pin %q (the current default %q must NOT win)", rb.accountID, first.ID, second.ID)
	}
	if rb.apiKey != "sk-ant-work" {
		t.Fatalf("resolveBackendFromAgent apiKey = %q; want the first account's key", rb.apiKey)
	}
}

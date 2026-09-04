package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient"
	"github.com/psenna/ai-sandbox/docker-operator/internal/dockerclient/dockerclienttest"
	"github.com/psenna/ai-sandbox/docker-operator/internal/store"
)

// TestDelete_OrderingContainerBeforeNetworkBeforeVolume proves teardown
// removes resources in the order Docker itself requires: both containers,
// then the network's dependaproxy endpoint and the network, then the three
// volumes.
func TestDelete_OrderingContainerBeforeNetworkBeforeVolume(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := len(f.Calls())

	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []dockerclienttest.Call{
		{Op: dockerclienttest.OpContainerStop, Target: a.ContainerID},
		{Op: dockerclienttest.OpContainerRemove, Target: a.ContainerID},
		{Op: dockerclienttest.OpContainerStop, Target: a.DindContainerID},
		{Op: dockerclienttest.OpContainerRemove, Target: a.DindContainerID},
		{Op: dockerclienttest.OpNetworkDisconnect, Target: a.DinernetName + "/" + m.cfg.DependaproxyContainer},
		{Op: dockerclienttest.OpNetworkRemove, Target: a.DinernetName},
		{Op: dockerclienttest.OpVolumeRemove, Target: a.WorkspaceVolume},
		{Op: dockerclienttest.OpVolumeRemove, Target: a.ClaudeConfigVolume},
		{Op: dockerclienttest.OpVolumeRemove, Target: a.DindCacheVolume},
	}
	got := f.Calls()[before:]
	if len(got) != len(want) {
		t.Fatalf("Delete calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Delete calls[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if _, err := st.Get(ctx, a.ID); !store.IsNotFound(err) {
		t.Errorf("Get after Delete: err = %v, want store.IsNotFound", err)
	}
	if got := snapshotCounts(f); got.containers != 1 || got.networks != 0 || got.volumes != 0 {
		t.Errorf("resources after Delete = %+v, want only the shared dependaproxy container left", got)
	}
}

// TestDelete_Idempotent is #66's explicit acceptance criterion: calling
// Delete a second time on an already-deleted agent makes zero new Docker
// calls.
func TestDelete_Idempotent(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete (first): %v", err)
	}

	before := len(f.Calls())
	if err := m.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete (second): %v", err)
	}
	if after := len(f.Calls()); after != before {
		t.Errorf("Delete (second call) made %d new docker calls, want 0: %v", after-before, f.Calls()[before:])
	}
}

// TestDelete_UnknownID_CleanNoOp proves deleting an ID that was never created
// is a clean no-op, not an error.
func TestDelete_UnknownID_CleanNoOp(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	before := len(f.Calls())

	if err := m.Delete(context.Background(), "agt_does_not_exist"); err != nil {
		t.Fatalf("Delete(unknown id) = %v, want nil", err)
	}
	if after := len(f.Calls()); after != before {
		t.Errorf("Delete(unknown id) made %d docker calls, want 0: %v", after-before, f.Calls()[before:])
	}
}

// TestDelete_SlotFreedEvenIfTeardownPartiallyFails is #66's other explicit
// acceptance criterion: even when Docker refuses to remove something, the
// record is already marked deleting (which does not count toward
// MAX_AGENTS) before any Docker call runs, so the slot is freed regardless
// of whether teardown fully succeeds.
func TestDelete_SlotFreedEvenIfTeardownPartiallyFails(t *testing.T) {
	m, f, st := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.FailOnce(dockerclienttest.OpVolumeRemove, errors.New("boom: the daemon refuses to remove this volume"))

	if err := m.Delete(ctx, a.ID); err == nil {
		t.Fatal("Delete = nil error, want the injected volume-remove failure to propagate")
	}

	got, gerr := st.Get(ctx, a.ID)
	if gerr != nil {
		t.Fatalf("Get after a partially-failed Delete: %v", gerr)
	}
	if got.Status != store.StatusDeleting {
		t.Errorf("Status after a partially-failed Delete = %q, want %q", got.Status, store.StatusDeleting)
	}
	if got.Status.CountsTowardCapacity() {
		t.Error("StatusDeleting counts toward MAX_AGENTS capacity, want it excluded so the slot is freed immediately")
	}
}

// TestDelete_ErrorAggregationWhenMultipleRemovesFail proves teardown does not
// stop at the first failure: every failing removal is reported, so an
// operator sees the whole picture rather than one symptom at a time across
// repeated retries.
func TestDelete_ErrorAggregationWhenMultipleRemovesFail(t *testing.T) {
	m, f, _ := newTestManager(t, 5)
	ctx := context.Background()

	a, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Fail(dockerclienttest.OpContainerRemove, errors.New("boom: container remove refused"))
	f.Fail(dockerclienttest.OpVolumeRemove, errors.New("boom: volume remove refused"))

	err = m.Delete(ctx, a.ID)
	if err == nil {
		t.Fatal("Delete = nil error, want the aggregated failures")
	}
	if !strings.Contains(err.Error(), "container remove refused") {
		t.Errorf("Delete error = %v, want it to mention the container remove failure", err)
	}
	if !strings.Contains(err.Error(), "volume remove refused") {
		t.Errorf("Delete error = %v, want it to mention the volume remove failure", err)
	}
}

// TestDelete_DerivedNamesFromIDOnlyRecord proves teardown still finds every
// resource for a record that crashed before Create stamped any name in --
// withDerivedNames must reconstruct all six names from the ID alone.
func TestDelete_DerivedNamesFromIDOnlyRecord(t *testing.T) {
	f := dockerclienttest.New()
	cfg := testConfig(5)
	newDependaproxy(t, f, cfg.DependaproxyContainer)
	st := newTestStore(t, 5)
	m := NewManager(f, st, cfg, testLogger(), testOptions())
	ctx := context.Background()

	const id = "agt_bareid1"
	if _, err := st.Create(ctx, store.CreateSpec{ID: id}); err != nil {
		t.Fatalf("seeding a bare (ID-only) record: %v", err)
	}

	// Pre-create every resource under the names Create WOULD have used, as if
	// the process had crashed after touching Docker but before the first
	// stamp landed.
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: workspaceVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: claudeConfigVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.VolumeCreate(ctx, dockerclient.VolumeSpec{Name: dindCacheVolumeName(id)}); err != nil {
		t.Fatalf("VolumeCreate: %v", err)
	}
	if _, err := f.NetworkCreate(ctx, dockerclient.NetworkSpec{Name: dinernetName(id)}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	ctrID, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: agentContainerName(id)})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	dindID, err := f.ContainerCreate(ctx, dockerclient.ContainerSpec{Name: dindContainerName(id)})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}

	if err := m.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, gone := range []string{workspaceVolumeName(id), claudeConfigVolumeName(id), dindCacheVolumeName(id)} {
		if _, err := f.VolumeInspect(ctx, gone); !dockerclient.IsNotFound(err) {
			t.Errorf("VolumeInspect(%q) after Delete: err = %v, want IsNotFound", gone, err)
		}
	}
	if _, err := f.NetworkInspect(ctx, dinernetName(id)); !dockerclient.IsNotFound(err) {
		t.Errorf("NetworkInspect after Delete: err = %v, want IsNotFound", err)
	}
	for _, gone := range []string{ctrID, dindID} {
		if _, err := f.ContainerInspect(ctx, gone); !dockerclient.IsNotFound(err) {
			t.Errorf("ContainerInspect(%q) after Delete: err = %v, want IsNotFound", gone, err)
		}
	}
	if _, err := st.Get(ctx, id); !store.IsNotFound(err) {
		t.Errorf("Get after Delete: err = %v, want store.IsNotFound", err)
	}
}

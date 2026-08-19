package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// mustCreateClassWithTTL creates a SandboxClass named name with the given
// storage backend type and warmCacheTTL, mirroring mustCreateClass's shape
// (S3 creds secret included so resolveCredentials would succeed). Used by
// the eligibility matrix to vary exactly the two class-level conditions
// WarmCacheGC branches on: backend type and TTL.
func mustCreateClassWithTTL(t *testing.T, name string, backendType sandboxv1alpha1.StorageBackendType, ttl string) *sandboxv1alpha1.SandboxClass {
	t.Helper()
	mustCreateS3CredsSecret(t, "default", "s3-creds")
	class := &sandboxv1alpha1.SandboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sandboxv1alpha1.SandboxClassSpec{
			Agent: sandboxv1alpha1.AgentSpec{Image: "ghcr.io/psenna/ai-sandbox-agent:v1"},
			Storage: sandboxv1alpha1.StorageSpec{
				Backend:      sandboxv1alpha1.BackendSpec{Type: backendType},
				WarmCacheTTL: ttl,
			},
		},
	}
	if backendType == sandboxv1alpha1.StorageBackendTypeS3 {
		class.Spec.Storage.Backend.S3 = &sandboxv1alpha1.S3Backend{
			Endpoint: "https://s3.example.com",
			Bucket:   "sandbox-snapshots",
			CredentialsSecretRef: sandboxv1alpha1.SecretKeyRef{
				Name: "s3-creds",
			},
		}
	} else {
		class.Spec.Storage.Backend.PVC = &sandboxv1alpha1.PVCBackend{ClaimName: "snapshots"}
	}
	if err := k8s.Create(ctx, class); err != nil {
		t.Fatalf("creating SandboxClass %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx, class)
	})
	return class
}

// mustCreateEnvInWithClass is mustCreateEnvIn with an explicit class name,
// for matrix cases whose class is not "default".
func mustCreateEnvInWithClass(t *testing.T, ns, name, className string, priority int32) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	mustCreateNamespace(t, ns)
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SandboxEnvironmentSpec{
			ClassRef: sandboxv1alpha1.ClassRef{Name: className},
			Repo:     "org/repo",
			Task:     sandboxv1alpha1.TaskSpec{Prompt: "do stuff"},
			Priority: priority,
		},
	}
	if err := k8s.Create(ctx, env); err != nil {
		t.Fatalf("creating SandboxEnvironment %s/%s: %v", ns, name, err)
	}
	return env
}

// mustSetFrozenEnv forces key's status into a frozen shape via a direct
// Status().Update, bypassing the reconciler: the given phase, freezeCount,
// snapshot, and snapshotAttempt. This is the fixture shape WarmCacheGC's
// eligibility branches on.
func mustSetFrozenEnv(t *testing.T, key types.NamespacedName, phase sandboxv1alpha1.Phase, snapshot *sandboxv1alpha1.SnapshotStatus, attempt *sandboxv1alpha1.SnapshotAttemptStatus, freezeCount int32) {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, env); err != nil {
		t.Fatalf("Get(%s) before mustSetFrozenEnv: %v", key, err)
	}
	env.Status.Phase = phase
	env.Status.FreezeCount = freezeCount
	env.Status.Snapshot = snapshot
	env.Status.SnapshotAttempt = attempt
	if err := k8s.Status().Update(ctx, env); err != nil {
		t.Fatalf("mustSetFrozenEnv Status().Update(%s): %v", key, err)
	}
}

// mustCreateOwnedPVC creates the workspace PVC for env, owned by env via a
// controller owner reference -- the exact shape ensureResources renders.
func mustCreateOwnedPVC(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      render.ChildNames(env.Name).PVC,
			Namespace: env.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupVersion.String(),
				Kind:       "SandboxEnvironment",
				Name:       env.Name,
				UID:        env.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := k8s.Create(ctx, pvc); err != nil {
		t.Fatalf("creating PVC %s: %v", pvc.Name, err)
	}
	return pvc
}

// mustCreateForeignPVC creates a PVC squatting on env's workspace name with
// NO owner reference -- the "manually pre-created object squatting on a name
// this operator would otherwise render" case ownedByEnv exists to reject.
func mustCreateForeignPVC(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      render.ChildNames(env.Name).PVC,
			Namespace: env.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := k8s.Create(ctx, pvc); err != nil {
		t.Fatalf("creating foreign PVC %s: %v", pvc.Name, err)
	}
	return pvc
}

// pvcReclaimed reports whether env's workspace PVC has been reclaimed:
// either gone entirely (NotFound) or terminating (deletionTimestamp set).
// envtest runs no kube-controller-manager, so the kubernetes.io/pvc-protection
// finalizer is never removed and a deleted PVC stays visible with a
// deletionTimestamp -- the deletionTimestamp IS the observable delete in
// this suite (in production the API server removes the object once the
// finalizer is cleared).
func pvcReclaimed(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment) bool {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := k8s.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: render.ChildNames(env.Name).PVC}, &pvc)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatalf("Get PVC for %s: %v", env.Name, err)
	}
	return !pvc.DeletionTimestamp.IsZero()
}

// newWarmCacheGC builds a WarmCacheGC wired to the envtest suite's own k8s
// client for BOTH Client and Reader (envtest's direct client is already
// uncached, so it is a faithful test double for the production Reader),
// with a fake clock at now and a namespace scope.
func newWarmCacheGC(t *testing.T, ns string, now time.Time) *WarmCacheGC {
	t.Helper()
	clk := newFakeClock(now)
	return &WarmCacheGC{
		Client:    k8s,
		Reader:    k8s,
		Interval:  50 * time.Millisecond,
		Namespace: ns,
		Clock:     clk.Now,
	}
}

// TestWarmCacheGC_EligibilityMatrix is the issue's "Testing" line made
// concrete: every condition in eligible is independently required, and each
// is exercised here as its own case. Each case sets up one environment (and
// its workspace PVC) in a namespace scoped to this test, runs exactly one
// GC pass, and asserts whether the PVC was reclaimed. Earlier cases' PVCs
// cannot pollute a later case's stats: a reclaimed PVC is gone (NotFound is
// a no-op on the next pass) and an unreclaimed one is still ineligible.
func TestWarmCacheGC_EligibilityMatrix(t *testing.T) {
	mustCreateClass(t) // "default": S3, no warmCacheTTL -> 30m default
	mustCreateClassWithTTL(t, "short-ttl", sandboxv1alpha1.StorageBackendTypeS3, "1h")
	mustCreateClassWithTTL(t, "disabled", sandboxv1alpha1.StorageBackendTypeS3, "0s")
	mustCreateClassWithTTL(t, "pvc-backed", sandboxv1alpha1.StorageBackendTypePVC, "30m")

	ns := testNamespace(t)
	now := fixedStart
	old := now.Add(-2 * time.Hour)       // well past every TTL in play
	recent := now.Add(-30 * time.Minute) // past the 30m default, not past 1h

	cases := []struct {
		name        string
		className   string
		phase       sandboxv1alpha1.Phase
		snapshot    *sandboxv1alpha1.SnapshotStatus
		attempt     *sandboxv1alpha1.SnapshotAttemptStatus
		freezeCount int32
		foreignPVC  bool
		noPVC       bool
		wantDeleted bool
	}{
		{"waiting-past-ttl-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, true},
		{"running-never-deletes", "default", sandboxv1alpha1.PhaseRunning,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, false},
		{"restoring-never-deletes", "default", sandboxv1alpha1.PhaseRestoring,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, false},
		{"freezing-never-deletes", "default", sandboxv1alpha1.PhaseFreezing,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, false},
		{"no-snapshot-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			nil, nil, 1, false, false, false},
		{"snapshot-no-takenat-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0}, nil, 1, false, false, false},
		{"stale-seq-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 2, false, false, false},
		{"attempt-in-progress-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 1, TakenAt: &metav1.Time{Time: old}},
			&sandboxv1alpha1.SnapshotAttemptStatus{Seq: 1, Phase: sandboxv1alpha1.SnapshotAttemptInProgress}, 2, false, false, false},
		{"attempt-failed-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 1, TakenAt: &metav1.Time{Time: old}},
			&sandboxv1alpha1.SnapshotAttemptStatus{Seq: 1, Phase: sandboxv1alpha1.SnapshotAttemptFailed}, 2, false, false, false},
		{"attempt-succeeded-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 1, TakenAt: &metav1.Time{Time: old}},
			&sandboxv1alpha1.SnapshotAttemptStatus{Seq: 1, Phase: sandboxv1alpha1.SnapshotAttemptSucceeded}, 2, false, false, true},
		{"not-past-ttl-never-deletes", "short-ttl", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: recent}}, nil, 1, false, false, false},
		{"ttl-disabled-never-deletes", "disabled", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, false},
		{"pvc-backed-never-deletes", "pvc-backed", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, false, false},
		{"foreign-pvc-never-deletes", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, true, false, false},
		{"pvc-already-gone-noop", "default", sandboxv1alpha1.PhaseWaiting,
			&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: old}}, nil, 1, false, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := mustCreateEnvInWithClass(t, ns, tc.name, tc.className, 10)
			key := types.NamespacedName{Namespace: ns, Name: tc.name}
			mustSetFrozenEnv(t, key, tc.phase, tc.snapshot, tc.attempt, tc.freezeCount)
			switch {
			case tc.foreignPVC:
				mustCreateForeignPVC(t, env)
			case !tc.noPVC:
				mustCreateOwnedPVC(t, env)
			}

			g := newWarmCacheGC(t, ns, now)
			stats, err := g.RunOnce(ctx)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if stats.Errors != 0 {
				t.Errorf("stats.Errors = %d, want 0", stats.Errors)
			}

			if tc.noPVC {
				// The PVC never existed; the meaningful assertions are that
				// the pass neither deleted nor errored.
				if stats.Deleted != 0 {
					t.Errorf("stats.Deleted = %d, want 0 (no PVC to reclaim)", stats.Deleted)
				}
				return
			}

			if got := pvcReclaimed(t, env); got != tc.wantDeleted {
				t.Errorf("PVC reclaimed = %v, want %v", got, tc.wantDeleted)
			}
			if want := boolToInt(tc.wantDeleted); stats.Deleted != want {
				t.Errorf("stats.Deleted = %d, want %d", stats.Deleted, want)
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestWarmCacheGC_Eligible_DeletionTimestamp covers the one eligibility
// branch that cannot be set up through the API server (a deletionTimestamp
// is only ever written by the API server itself, as a side effect of
// Delete): the pure function must refuse an environment that is being
// deleted, even when every other condition holds.
func TestWarmCacheGC_Eligible_DeletionTimestamp(t *testing.T) {
	mustCreateClass(t)
	class := &sandboxv1alpha1.SandboxClass{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: "default"}, class); err != nil {
		t.Fatalf("Get default class: %v", err)
	}
	now := fixedStart
	env := &sandboxv1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleting",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: now},
		},
		Status: sandboxv1alpha1.SandboxEnvironmentStatus{
			Phase:       sandboxv1alpha1.PhaseWaiting,
			FreezeCount: 1,
			Snapshot:    &sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: now.Add(-2 * time.Hour)}},
		},
	}
	g := &WarmCacheGC{}
	if g.eligible(env, class, now) {
		t.Fatal("eligible = true for an environment being deleted, want false")
	}
}

// wokenReader wraps a client.Reader and, on the first Get of a
// SandboxEnvironment, returns a copy with Phase forced to Restoring --
// simulating the environment waking between the GC's List and its
// pre-delete re-Get. That is the exact race reclaim's live re-Get exists to
// close, and the List (a SandboxEnvironmentList) and the class Get (a
// SandboxClass) pass through untouched.
type wokenReader struct {
	client.Reader
	key  types.NamespacedName
	done bool
}

func (w *wokenReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := w.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if !w.done {
		if env, ok := obj.(*sandboxv1alpha1.SandboxEnvironment); ok && key == w.key {
			env.Status.Phase = sandboxv1alpha1.PhaseRestoring
			w.done = true
		}
	}
	return nil
}

// TestWarmCacheGC_LiveReGetRace proves the "woke between List and Delete"
// window is closed: an environment that is eligible at List time but wakes
// (phase leaves Waiting) before the pre-delete re-Get must NOT have its PVC
// deleted. Before the live re-Get, the delete would have raced the wake and
// destroyed the only copy of the agent's context.
func TestWarmCacheGC_LiveReGetRace(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	env := mustCreateEnvIn(t, ns, "wakes-mid-pass", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	mustSetFrozenEnv(t, key, sandboxv1alpha1.PhaseWaiting,
		&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: now.Add(-2 * time.Hour)}}, nil, 1)
	mustCreateOwnedPVC(t, env)

	g := newWarmCacheGC(t, ns, now)
	g.Reader = &wokenReader{Reader: k8s, key: key}

	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Deleted != 0 {
		t.Errorf("stats.Deleted = %d, want 0 (environment woke between List and re-Get)", stats.Deleted)
	}
	if pvcReclaimed(t, env) {
		t.Fatal("PVC was deleted despite the environment waking mid-pass")
	}
}

// deleteRecorder wraps a client.Client, recording the UID of every object
// passed to Delete, so tests can assert the delete targeted the exact PVC
// the GC observed (the UID precondition's whole point).
type deleteRecorder struct {
	client.Client
	mu   sync.Mutex
	uids []types.UID
}

func (d *deleteRecorder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	d.mu.Lock()
	d.uids = append(d.uids, obj.GetUID())
	d.mu.Unlock()
	return d.Client.Delete(ctx, obj, opts...)
}

func (d *deleteRecorder) deletedUIDs() []types.UID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]types.UID(nil), d.uids...)
}

// TestWarmCacheGC_DeleteCarriesUIDPrecondition proves the delete is issued
// against the exact PVC object the GC observed, so a PVC recreated since
// the Get (new UID) can never be hit: the API server enforces the
// precondition, and this test pins the wiring that supplies it.
func TestWarmCacheGC_DeleteCarriesUIDPrecondition(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	env := mustCreateEnvIn(t, ns, "uid-precondition", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	mustSetFrozenEnv(t, key, sandboxv1alpha1.PhaseWaiting,
		&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: now.Add(-2 * time.Hour)}}, nil, 1)
	pvc := mustCreateOwnedPVC(t, env)

	rec := &deleteRecorder{Client: k8s}
	g := newWarmCacheGC(t, ns, now)
	g.Client = rec

	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("stats.Deleted = %d, want 1", stats.Deleted)
	}
	uids := rec.deletedUIDs()
	if len(uids) != 1 {
		t.Fatalf("Delete called %d times, want 1", len(uids))
	}
	if uids[0] != pvc.UID {
		t.Errorf("Delete targeted UID %s, want the observed PVC's UID %s", uids[0], pvc.UID)
	}
}

// TestWarmCacheGC_ManagerWiring is the manager-integration smoke test: a
// real manager starts the runnable (immediate pass, then ticker), and an
// eligible frozen environment's PVC is reclaimed by the loop alone -- no
// test code calls RunOnce. Mirrors the SlotScheduler manager test's shape.
func TestWarmCacheGC_ManagerWiring(t *testing.T) {
	ns := testNamespace(t)
	mustCreateNamespace(t, ns)
	mustCreateClass(t)

	mgr, err := ctrl.NewManager(k8sCfg, ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.Add(&WarmCacheGC{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Interval:  50 * time.Millisecond,
		Namespace: ns,
	}); err != nil {
		t.Fatalf("mgr.Add(WarmCacheGC): %v", err)
	}

	mgrCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- mgr.Start(mgrCtx) }()
	if !mgr.GetCache().WaitForCacheSync(mgrCtx) {
		t.Fatalf("cache did not sync")
	}

	env := mustCreateEnvIn(t, ns, "gc-me", 10)
	key := types.NamespacedName{Namespace: ns, Name: env.Name}
	mustSetFrozenEnv(t, key, sandboxv1alpha1.PhaseWaiting,
		&sandboxv1alpha1.SnapshotStatus{Seq: 0, TakenAt: &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}}, nil, 1)
	mustCreateOwnedPVC(t, env)

	deadline := time.Now().Add(30 * time.Second)
	for !pvcReclaimed(t, env) {
		if time.Now().After(deadline) {
			t.Fatal("PVC was not reclaimed by the manager-driven loop within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("mgr.Start returned error after shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("mgr.Start did not return within 30s of context cancellation")
	}
}

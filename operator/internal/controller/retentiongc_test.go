package controller

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// fakeGCBackend is an in-memory storage.Backend double for retentiongc_test.go
// -- RetentionGC must never dial a real S3 endpoint in this suite, so every
// test injects one of these via RetentionGC.BackendFor.
type fakeGCBackend struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeGCBackend() *fakeGCBackend { return &fakeGCBackend{data: map[string][]byte{}} }

func (b *fakeGCBackend) Put(_ context.Context, key string, r io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf, err := io.ReadAll(r)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	b.data[key] = buf
	return storage.ObjectInfo{Key: key, Size: int64(len(buf))}, nil
}

func (b *fakeGCBackend) Get(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	buf, ok := b.data[key]
	if !ok {
		return nil, storage.ObjectInfo{}, &storage.Error{Op: "Get", Backend: "fake", Key: key, Kind: storage.ErrNotFound}
	}
	return io.NopCloser(bytes.NewReader(buf)), storage.ObjectInfo{Key: key, Size: int64(len(buf))}, nil
}

func (b *fakeGCBackend) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	_, info, err := b.Get(ctx, key)
	return info, err
}

func (b *fakeGCBackend) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []storage.ObjectInfo
	for k, v := range b.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectInfo{Key: k, Size: int64(len(v))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (b *fakeGCBackend) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.data, key)
	return nil
}

func (b *fakeGCBackend) DeletePrefix(_ context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, &storage.Error{Op: "DeletePrefix", Backend: "fake", Kind: storage.ErrInvalid}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k := range b.data {
		if strings.HasPrefix(k, prefix) {
			delete(b.data, k)
			n++
		}
	}
	return n, nil
}

func (b *fakeGCBackend) has(prefix string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.data {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// seedRoot writes one marker object under the environment root identified
// by (clusterID, ns, name, uid), returning the root prefix (as
// storage.Layout.Root() would compute it) for assertions.
func seedRoot(t *testing.T, be *fakeGCBackend, prefix, clusterID, ns, name, uid string) string {
	t.Helper()
	layout, err := storage.NewLayout(prefix, storage.Identity{ClusterID: clusterID, Namespace: ns, EnvName: name, EnvUID: uid})
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	if _, err := be.Put(context.Background(), layout.ArchiveRun(), strings.NewReader("{}"), storage.PutOptions{}); err != nil {
		t.Fatalf("seeding root %s: %v", layout.Root(), err)
	}
	return layout.Root()
}

// mustSetArchive sets status.archive.finishedAt on key directly, bypassing
// the reconciler -- for setting up RetentionGC-test fixtures without a real
// archive Job.
func mustSetArchive(t *testing.T, key types.NamespacedName, finishedAt time.Time) {
	t.Helper()
	env := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, key, env); err != nil {
		t.Fatalf("Get(%s) before mustSetArchive: %v", key, err)
	}
	ts := metav1.NewTime(finishedAt)
	env.Status.Archive = &sandboxv1alpha1.ArchiveStatus{URI: "s3://sandbox-snapshots/fake", FinishedAt: &ts}
	if err := k8s.Status().Update(ctx, env); err != nil {
		t.Fatalf("mustSetArchive Status().Update(%s): %v", key, err)
	}
}

// newRetentionGC builds a RetentionGC wired to the envtest suite's own k8s
// client for Reader (envtest's direct client is already uncached, mirroring
// newWarmCacheGC), with a fake clock and an injected fake backend so no
// sweep ever dials a real S3 endpoint.
func newRetentionGC(t *testing.T, ns string, now time.Time, be *fakeGCBackend, ttl time.Duration, dryRun bool) *RetentionGC {
	t.Helper()
	clk := newFakeClock(now)
	return &RetentionGC{
		Client:          k8s,
		Reader:          k8s,
		SecretNamespace: "default",
		ClusterID:       "test-cluster",
		TTL:             ttl,
		DryRun:          dryRun,
		Interval:        50 * time.Millisecond,
		Namespace:       ns,
		Clock:           clk.Now,
		BackendFor:      func(context.Context, *sandboxv1alpha1.SandboxClass) (storage.Backend, error) { return be, nil },
	}
}

func TestRetentionGC_TTLSelection(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	old := mustCreateEnvIn(t, ns, "old-archive", 0)
	mustSetArchive(t, types.NamespacedName{Namespace: ns, Name: old.Name}, now.Add(-2*time.Hour))
	fresh := mustCreateEnvIn(t, ns, "fresh-archive", 0)
	mustSetArchive(t, types.NamespacedName{Namespace: ns, Name: fresh.Name}, now.Add(-10*time.Minute))

	be := newFakeGCBackend()
	oldRoot := seedRoot(t, be, "", "test-cluster", ns, old.Name, string(old.UID))
	freshRoot := seedRoot(t, be, "", "test-cluster", ns, fresh.Name, string(fresh.UID))

	g := newRetentionGC(t, ns, now, be, time.Hour, false)
	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", stats.Deleted)
	}
	if be.has(oldRoot) {
		t.Error("past-TTL archive root still present, want deleted")
	}
	if !be.has(freshRoot) {
		t.Error("before-TTL archive root was deleted, want kept")
	}
}

func TestRetentionGC_TTLZero_OnlyOrphansRun(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	old := mustCreateEnvIn(t, ns, "old-archive-ttl0", 0)
	mustSetArchive(t, types.NamespacedName{Namespace: ns, Name: old.Name}, now.Add(-999*time.Hour))

	be := newFakeGCBackend()
	oldRoot := seedRoot(t, be, "", "test-cluster", ns, old.Name, string(old.UID))

	g := newRetentionGC(t, ns, now, be, 0, false)
	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0 (TTL=0 disables retention)", stats.Deleted)
	}
	// old is still live (its UID matches a real environment), so orphan
	// cleanup must not touch it either.
	if !be.has(oldRoot) {
		t.Error("root was deleted despite TTL=0 and a live, matching UID")
	}
}

func TestRetentionGC_DryRun_DeletesNothing(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	old := mustCreateEnvIn(t, ns, "dryrun-archive", 0)
	mustSetArchive(t, types.NamespacedName{Namespace: ns, Name: old.Name}, now.Add(-2*time.Hour))

	be := newFakeGCBackend()
	root := seedRoot(t, be, "", "test-cluster", ns, old.Name, string(old.UID))
	// An orphan too, so dry-run's WouldDelete covers both sweeps.
	orphanRoot := seedRoot(t, be, "", "test-cluster", ns, "long-gone", "dead-uid")

	g := newRetentionGC(t, ns, now, be, time.Hour, true)
	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Deleted != 0 || stats.OrphansDeleted != 0 {
		t.Errorf("Deleted=%d OrphansDeleted=%d, want 0/0 in dry-run", stats.Deleted, stats.OrphansDeleted)
	}
	if stats.WouldDelete != 2 {
		t.Errorf("WouldDelete = %d, want 2 (one retention + one orphan)", stats.WouldDelete)
	}
	if !be.has(root) {
		t.Error("dry-run deleted the past-TTL root; want it left alone")
	}
	if !be.has(orphanRoot) {
		t.Error("dry-run deleted the orphan root; want it left alone")
	}
}

func TestRetentionGC_OrphanCleanup(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	live := mustCreateEnvIn(t, ns, "still-here", 0)

	be := newFakeGCBackend()
	liveRoot := seedRoot(t, be, "", "test-cluster", ns, live.Name, string(live.UID))
	orphanRoot := seedRoot(t, be, "", "test-cluster", ns, "long-gone", "dead-uid-1")

	g := newRetentionGC(t, ns, now, be, 0, false) // TTL=0: only the orphan sweep can act
	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.OrphansDeleted != 1 {
		t.Errorf("OrphansDeleted = %d, want 1", stats.OrphansDeleted)
	}
	if be.has(orphanRoot) {
		t.Error("orphan root still present, want deleted")
	}
	if !be.has(liveRoot) {
		t.Error("live environment's root was deleted, want kept")
	}
}

// TestRetentionGC_RecreatedEnv_SameNameDifferentUID proves the "delete then
// recreate with the same name" safety property: a stale root left behind by
// a since-deleted environment is treated as an orphan and reclaimed, while a
// freshly created environment that happens to share the OLD environment's
// namespace/name (but has a different UID, and therefore a disjoint root) is
// never touched.
func TestRetentionGC_RecreatedEnv_SameNameDifferentUID(t *testing.T) {
	mustCreateClass(t)
	ns := testNamespace(t)
	now := fixedStart

	recreated := mustCreateEnvIn(t, ns, "recreated", 0)

	be := newFakeGCBackend()
	// The OLD incarnation's root: same namespace/name, a UID that belongs to
	// no live object (the original was deleted and recreated).
	staleRoot := seedRoot(t, be, "", "test-cluster", ns, "recreated", "stale-uid-from-before")
	newRoot := seedRoot(t, be, "", "test-cluster", ns, recreated.Name, string(recreated.UID))

	if staleRoot == newRoot {
		t.Fatalf("setup: stale and new roots collided (%s) -- fixture is not exercising different UIDs", staleRoot)
	}

	g := newRetentionGC(t, ns, now, be, 0, false)
	stats, err := g.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.OrphansDeleted != 1 {
		t.Errorf("OrphansDeleted = %d, want 1", stats.OrphansDeleted)
	}
	if be.has(staleRoot) {
		t.Error("stale (pre-recreate) root still present, want deleted as an orphan")
	}
	if !be.has(newRoot) {
		t.Error("recreated environment's own root was deleted, want kept")
	}
}

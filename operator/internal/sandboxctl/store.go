package sandboxctl

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// ErrWaitAlreadyDeclared is returned by DeclareWait when status.waitFor is
// already non-nil.
var ErrWaitAlreadyDeclared = errors.New("wait already declared")

// ErrResultAlreadyReported is returned by ReportDone when status.agentResult
// is already set to a DIFFERING outcome/message/exitCode than the one
// requested. An IDENTICAL repeat is not an error -- see ReportDone's doc
// comment.
var ErrResultAlreadyReported = errors.New("result already reported with a different outcome")

// ErrFreezing is returned by any mutating Store method once the sidecar has
// latched freezing (see poller.go).
var ErrFreezing = errors.New("environment is freezing")

// ErrSnapshotSeqRegression is returned by RecordSnapshot when r.Seq is lower
// than the sequence number already recorded in status.snapshot -- the
// monotonicity acceptance criterion enforced structurally, not by
// convention.
var ErrSnapshotSeqRegression = errors.New("snapshot sequence number would regress")

// SnapshotAttempt is the value form of a status.snapshotAttempt patch.
type SnapshotAttempt struct {
	Seq      int
	Phase    v1alpha1.SnapshotAttemptPhase
	Attempts int
	Reason   string
	Message  string
}

// SnapshotRecord is the value form of a status.snapshot patch.
type SnapshotRecord struct {
	Seq            int
	URI            string
	SizeBytes      int64
	SHA256         string
	TakenAt        time.Time
	DurationMillis int64
}

// Result is the store's form of a /v1/done report.
type Result struct {
	Outcome  v1alpha1.AgentOutcome
	Message  string
	ExitCode *int32
}

// Snapshot is the cached, poller-refreshed view of the sidecar's own
// SandboxEnvironment. Served directly by GET /v1/status -- never a live API
// call per request, so the agent cannot hammer the API server through this
// endpoint (see limits.go's rate limit on /v1/status regardless).
type Snapshot struct {
	Environment EnvironmentRef
	Phase       v1alpha1.Phase
	WaitFor     *v1alpha1.WaitForStatus
	Result      *v1alpha1.AgentResultStatus
	FreezeCount int32
	WakeCount   int32
	// Generation is env.Generation, populated by Refresh, for
	// storage.Manifest.Source.Generation.
	Generation int64
	ObservedAt time.Time
	// Fresh is true once at least one poll has succeeded.
	Fresh bool
}

// Store is sandboxctl's seam onto the SandboxEnvironment's status
// subresource. The only implementation is envStore; tests use a fake for
// handlers_test.go/poller_test.go so those tests don't need envtest.
type Store interface {
	// Snapshot returns the last cached observation. Never blocks, never
	// calls the API server.
	Snapshot() Snapshot
	// Refresh performs a live Get of the sidecar's own environment and
	// updates the cache. Called by the poller, not by request handlers.
	Refresh(ctx context.Context) error
	// DeclareWait validates and patches p into status.waitFor.
	DeclareWait(ctx context.Context, p WaitProbe, at time.Time) error
	// ReportDone patches r into status.agentResult. idempotent is true when
	// an IDENTICAL result was already reported (no patch issued, not an
	// error); a DIFFERING repeat returns ErrResultAlreadyReported.
	ReportDone(ctx context.Context, r Result, at time.Time) (idempotent bool, err error)
	// RecordSnapshotAttempt patches status.snapshotAttempt. Idempotent.
	RecordSnapshotAttempt(ctx context.Context, a SnapshotAttempt, at time.Time) error
	// RecordSnapshot patches status.snapshot AND flips status.snapshotAttempt
	// to Succeeded in the SAME patch, so the two can never disagree. REFUSES
	// a lower sequence number than the one already recorded
	// (ErrSnapshotSeqRegression).
	RecordSnapshot(ctx context.Context, r SnapshotRecord, at time.Time) error
}

// envStore is the real Store, backed by a direct (uncached) client.Client
// built once in run.go from rest.InClusterConfig().
type envStore struct {
	client client.Client
	key    types.NamespacedName

	mu   sync.RWMutex
	snap Snapshot
}

// NewStore builds a Store for the environment named name in namespace ns,
// backed by c. c must be a direct, uncached client (see doc.go).
func NewStore(c client.Client, namespace, name string) Store {
	return newEnvStore(c, namespace, name)
}

func newEnvStore(c client.Client, namespace, name string) *envStore {
	return &envStore{
		client: c,
		key:    types.NamespacedName{Namespace: namespace, Name: name},
	}
}

func (s *envStore) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func (s *envStore) setSnapshot(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

// Refresh performs a live, uncached Get of the sidecar's own environment
// and updates the cached snapshot. A failure leaves the previous snapshot
// in place (see poller.go: staleness, not a hard error, is how this
// surfaces on /v1/status) and is returned to the caller for logging.
func (s *envStore) Refresh(ctx context.Context) error {
	var env v1alpha1.SandboxEnvironment
	if err := s.client.Get(ctx, s.key, &env); err != nil {
		return err
	}
	s.setSnapshot(Snapshot{
		Environment: EnvironmentRef{Name: env.Name, Namespace: env.Namespace, UID: string(env.UID)},
		Phase:       env.Status.Phase,
		WaitFor:     env.Status.WaitFor,
		Result:      env.Status.AgentResult,
		FreezeCount: env.Status.FreezeCount,
		WakeCount:   env.Status.WakeCount,
		Generation:  env.Generation,
		ObservedAt:  time.Now(),
		Fresh:       true,
	})
	return nil
}

// RecordSnapshotAttempt patches status.snapshotAttempt.
func (s *envStore) RecordSnapshotAttempt(ctx context.Context, a SnapshotAttempt, at time.Time) error {
	ts := metav1.NewTime(at)
	return s.patchStatus(ctx, func(env *v1alpha1.SandboxEnvironment) error {
		started := &ts
		if existing := env.Status.SnapshotAttempt; existing != nil && int(existing.Seq) == a.Seq && existing.StartedAt != nil {
			started = existing.StartedAt
		}
		env.Status.SnapshotAttempt = &v1alpha1.SnapshotAttemptStatus{
			Seq:       int32(a.Seq), //nolint:gosec // G115: seq is an internal, small, monotonically increasing counter
			Phase:     a.Phase,
			Attempts:  int32(a.Attempts), //nolint:gosec // G115: bounded retry counter
			Reason:    a.Reason,
			Message:   a.Message,
			StartedAt: started,
			UpdatedAt: &ts,
		}
		return nil
	})
}

// RecordSnapshot patches status.snapshot and flips status.snapshotAttempt to
// Succeeded in the same patch. Refuses a regression against the currently
// recorded status.snapshot.Seq.
func (s *envStore) RecordSnapshot(ctx context.Context, r SnapshotRecord, at time.Time) error {
	takenAt := metav1.NewTime(r.TakenAt)
	ts := metav1.NewTime(at)
	return s.patchStatus(ctx, func(env *v1alpha1.SandboxEnvironment) error {
		if existing := env.Status.Snapshot; existing != nil && int(existing.Seq) > r.Seq {
			return ErrSnapshotSeqRegression
		}
		env.Status.Snapshot = &v1alpha1.SnapshotStatus{
			Seq:            int32(r.Seq), //nolint:gosec // G115: seq is an internal, small, monotonically increasing counter
			URI:            r.URI,
			SizeBytes:      r.SizeBytes,
			SHA256:         r.SHA256,
			TakenAt:        &takenAt,
			DurationMillis: r.DurationMillis,
		}
		var startedAt *metav1.Time
		if existing := env.Status.SnapshotAttempt; existing != nil {
			startedAt = existing.StartedAt
		}
		env.Status.SnapshotAttempt = &v1alpha1.SnapshotAttemptStatus{
			Seq:       int32(r.Seq), //nolint:gosec // G115: seq is an internal, small, monotonically increasing counter
			Phase:     v1alpha1.SnapshotAttemptSucceeded,
			StartedAt: startedAt,
			UpdatedAt: &ts,
		}
		return nil
	})
}

// DeclareWait refuses if status.waitFor is already non-nil (ErrWaitAlreadyDeclared),
// otherwise patches p into it, stamping DeclaredAt from at (never time.Now()
// directly -- matches Reconciler.now()'s injected-clock pattern elsewhere in
// this repo).
func (s *envStore) DeclareWait(ctx context.Context, p WaitProbe, at time.Time) error {
	declaredAt := metav1.NewTime(at)
	return s.patchStatus(ctx, func(env *v1alpha1.SandboxEnvironment) error {
		if env.Status.WaitFor != nil {
			return ErrWaitAlreadyDeclared
		}
		env.Status.WaitFor = &v1alpha1.WaitForStatus{
			Type:       p.Type,
			Reason:     p.Reason,
			Params:     p.Params,
			DeclaredAt: &declaredAt,
		}
		return nil
	})
}

// ReportDone refuses/idempotently-succeeds against status.agentResult. It
// also refuses (ErrWaitAlreadyDeclared) if a wait was already declared: a
// run that has asked to be frozen has, by definition, not finished.
func (s *envStore) ReportDone(ctx context.Context, r Result, at time.Time) (bool, error) {
	reportedAt := metav1.NewTime(at)
	idempotent := false
	err := s.patchStatus(ctx, func(env *v1alpha1.SandboxEnvironment) error {
		if env.Status.WaitFor != nil {
			return ErrWaitAlreadyDeclared
		}
		if existing := env.Status.AgentResult; existing != nil {
			if resultEqual(existing, r) {
				idempotent = true
				return nil // no-op: patchStatus's DeepEqual check skips the write
			}
			return ErrResultAlreadyReported
		}
		env.Status.AgentResult = &v1alpha1.AgentResultStatus{
			Outcome:    r.Outcome,
			Message:    r.Message,
			ExitCode:   r.ExitCode,
			ReportedAt: &reportedAt,
		}
		return nil
	})
	return idempotent, err
}

func resultEqual(existing *v1alpha1.AgentResultStatus, r Result) bool {
	if existing.Outcome != r.Outcome || existing.Message != r.Message {
		return false
	}
	switch {
	case existing.ExitCode == nil && r.ExitCode == nil:
		return true
	case existing.ExitCode == nil || r.ExitCode == nil:
		return false
	default:
		return *existing.ExitCode == *r.ExitCode
	}
}

// patchStatus applies mutate to a freshly-read copy and sends it as a JSON
// merge patch with an OPTIMISTIC LOCK.
//   - Patch, not Update: the sidecar Role grants get+patch on
//     sandboxenvironments/status, NOT update. A merge patch also sends only
//     the fields we touched, so it can never clobber a field the operator
//     wrote between our Get and our write.
//   - MergeFromWithOptimisticLock, not a bare MergeFrom: the plain form
//     would silently win every race. Including resourceVersion turns a
//     concurrent operator write into a 409, which RetryOnConflict re-reads
//     and re-applies.
//
// Mirrors internal/controller/status.go's own RetryOnConflict pattern.
func (s *envStore) patchStatus(ctx context.Context, mutate func(*v1alpha1.SandboxEnvironment) error) error {
	return retryOnRetryable(func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var env v1alpha1.SandboxEnvironment
			if err := s.client.Get(ctx, s.key, &env); err != nil {
				return err
			}
			base := env.DeepCopy()
			if err := mutate(&env); err != nil {
				return err
			}
			if apiequality.Semantic.DeepEqual(&base.Status, &env.Status) {
				return nil // idempotent: nothing to write
			}
			return s.client.Status().Patch(ctx, &env, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		})
	})
}

// retryOnRetryable is a bounded OUTER loop (3 attempts, 200ms/400ms/800ms)
// over transient API-server/network errors, NOT over IsForbidden/IsInvalid/
// IsNotFound (permanent, must surface immediately to the caller, mapped to
// a 502 by handlers.go carrying the API server's own message) and not over
// the sentinel Err* values above (also permanent from this loop's
// perspective -- retrying them would just re-observe the same conflict).
func retryOnRetryable(fn func() error) error {
	backoffs := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isRetryable(err) || attempt >= len(backoffs) {
			return err
		}
		time.Sleep(backoffs[attempt])
	}
}

func isRetryable(err error) bool {
	if apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/metrics"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// defaultProbeBackoff is the per-probe evaluation interval schedule (#30),
// mirroring internal/sandboxctl/retrystep.go's delay shape: the Nth
// non-satisfied evaluation schedules the next one Backoff[min(N-1, len-1)]
// later, so a probe that keeps answering "not yet" is re-checked at 1s, 2s,
// 4s, 8s, then every 8s -- never hammered on every reconcile.
func defaultProbeBackoff() []time.Duration {
	return []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
}

// defaultMaxConsecutiveErrors is the number of consecutive unevaluatable
// failures (bad URL, auth failure, unparseable response) that fails the
// environment rather than keeping it Waiting forever.
const defaultMaxConsecutiveErrors = 3

// ProbeEvaluator evaluates an environment's declared wait (status.waitFor)
// against the real world. It is the #30 counterpart of the pure
// lifecycle/probe.go predicates: those decide what "satisfied" means, this
// does the I/O and produces the attempt record lifecycle threads into
// status.probeAttempt.
//
// Every field is injectable so tests run without a cluster or a wall clock:
//   - HTTPClient: the client for gitProxyCheck/httpGet. Defaults to a
//     timeout-bounded client.
//   - Clock: the evaluator's "now". Defaults to time.Now.
//   - MaxConsecutiveErrors: the unevaluatable-failure threshold. Defaults to
//     defaultMaxConsecutiveErrors.
//   - Backoff: the per-probe interval schedule. Defaults to
//     defaultProbeBackoff.
//   - NewS3Backend: constructs the storage backend for S3ObjectExists probes.
//     Defaults to storage.NewS3; tests substitute a fake so no S3 endpoint
//     is needed.
type ProbeEvaluator struct {
	HTTPClient           *http.Client
	Clock                func() time.Time
	MaxConsecutiveErrors int32
	Backoff              []time.Duration
	NewS3Backend         func(cfg storage.S3Config, creds storage.Credentials) (storage.Backend, error)

	// Metrics records probe_evaluations_total by wait type and outcome
	// (#33). Nil-guarded, matching Reconciler.Metrics.
	Metrics *metrics.Collectors
}

// NewProbeEvaluator returns a ProbeEvaluator with every field defaulted.
func NewProbeEvaluator() *ProbeEvaluator {
	return &ProbeEvaluator{}
}

func (e *ProbeEvaluator) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

func (e *ProbeEvaluator) httpClient() *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (e *ProbeEvaluator) maxConsecutiveErrors() int32 {
	if e.MaxConsecutiveErrors > 0 {
		return e.MaxConsecutiveErrors
	}
	return defaultMaxConsecutiveErrors
}

func (e *ProbeEvaluator) backoff() []time.Duration {
	if len(e.Backoff) > 0 {
		return e.Backoff
	}
	return defaultProbeBackoff()
}

func (e *ProbeEvaluator) newS3Backend(cfg storage.S3Config, creds storage.Credentials) (storage.Backend, error) {
	if e.NewS3Backend != nil {
		return e.NewS3Backend(cfg, creds)
	}
	return storage.NewS3(cfg, creds)
}

// Evaluate runs one probe evaluation for env's declared wait, if one is due.
// It returns the probe-relevant ClusterFacts fields (ProbeObserved,
// WaitProbeSatisfied, WaitProbeFailure) and the attempt record to thread into
// status.probeAttempt. The returned error is reserved for failures that must
// fail the RECONCILE (none today); probe failures are reported through
// WaitProbeFailure instead, so a bad probe can never hot-loop a reconcile.
//
// The skip path: when status.probeAttempt.NextEligibleAt is in the future,
// the evaluator does NO I/O and returns attempt=nil -- the safe zero reading
// that leaves status.probeAttempt (with its NextEligibleAt) untouched, so
// the reconciler's requeue (see lifecycle/timeouts.go) wakes it exactly at
// the next eligible instant. ProbeObserved stays true on the skip path: the
// evaluator ran and decided to skip, so the WaitSatisfied condition keeps
// its stable False/ProbePending reading rather than flipping to Unknown.
func (e *ProbeEvaluator) Evaluate(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, creds render.Credentials) (lifecycle.ClusterFacts, *v1alpha1.ProbeAttemptStatus, error) {
	w := env.Status.WaitFor
	if w == nil {
		return lifecycle.ClusterFacts{}, nil, nil
	}
	now := e.now()
	if prev := env.Status.ProbeAttempt; prev != nil && prev.NextEligibleAt != nil && now.Before(prev.NextEligibleAt.Time) {
		e.Metrics.RecordProbeEvaluation(w.Type, metrics.ResultSkipped)
		return lifecycle.ClusterFacts{ProbeObserved: true}, nil, nil
	}

	attempt := &v1alpha1.ProbeAttemptStatus{
		Type:          w.Type,
		Phase:         v1alpha1.ProbeAttemptPending,
		LastResult:    "pending",
		LastAttemptAt: newMetaTime(now),
	}
	if prev := env.Status.ProbeAttempt; prev != nil {
		attempt.Attempts = prev.Attempts
		attempt.ConsecutiveErrors = prev.ConsecutiveErrors
	}
	attempt.Attempts++

	f := lifecycle.ClusterFacts{ProbeObserved: true}
	satisfied, err := e.evaluateOne(ctx, env, w, class, creds)
	// result is this evaluation's OWN outcome, computed explicitly here --
	// never read back from attempt.LastResult, which the err!=nil branch
	// below may downgrade from "error" to "pending" once the attempt is
	// still within the retry budget. That downgrade is about what the NEXT
	// evaluation should do; it must not retroactively relabel this one.
	result := metrics.ResultPending
	switch {
	case err != nil:
		result = metrics.ResultError
	case satisfied:
		result = metrics.ResultSatisfied
	}
	defer func() { e.Metrics.RecordProbeEvaluation(w.Type, result) }()
	switch {
	case err != nil:
		unevaluatable, reason, message := lifecycle.ClassifyError(err)
		attempt.Phase = v1alpha1.ProbeAttemptFailed
		attempt.LastResult = "error"
		attempt.Reason = reason
		attempt.Message = message
		// ConsecutiveErrors counts CONSECUTIVE UNEVALUATABLE results only --
		// the field is documented as "consecutive unevaluatable results" and
		// the threshold is the bounded number of attempts after which an
		// unevaluatable probe fails the environment. Counting transient
		// errors (5xx/transport) here would let N transient hiccups followed
		// by a single unevaluatable error cross the threshold and fail the
		// environment prematurely (transient×3 + unevaluatable×1 -> fail),
		// defeating the bounded-attempts guarantee. Transient errors neither
		// increment nor reset the streak; only a successful evaluation
		// (satisfied or pending) resets it, below.
		if unevaluatable {
			attempt.ConsecutiveErrors++
		}
		if unevaluatable && attempt.ConsecutiveErrors >= e.maxConsecutiveErrors() {
			f.WaitProbeFailure = &lifecycle.StepFailure{Reason: reason, Message: message}
		} else {
			// Under the threshold (or merely transient): still pending, back
			// off and re-check at the next eligible instant.
			attempt.Phase = v1alpha1.ProbeAttemptPending
			attempt.LastResult = "pending"
			attempt.NextEligibleAt = e.nextEligible(now, attempt)
		}
	case satisfied:
		attempt.ConsecutiveErrors = 0
		attempt.Phase = v1alpha1.ProbeAttemptSatisfied
		attempt.LastResult = "satisfied"
		f.WaitProbeSatisfied = true
	default:
		attempt.ConsecutiveErrors = 0
		attempt.NextEligibleAt = e.nextEligible(now, attempt)
	}
	return f, attempt, nil
}

// evaluateOne dispatches on the wait type and performs the probe's I/O. It
// returns satisfied=true when the wait's condition is met, or a typed
// ProbeError (see lifecycle.ClassifyError) when the probe could not be
// evaluated. A "not yet" answer (e.g. git-proxy overall:"pending") is
// satisfied=false with a nil error.
func (e *ProbeEvaluator) evaluateOne(ctx context.Context, env *v1alpha1.SandboxEnvironment, w *v1alpha1.WaitForStatus, class *v1alpha1.SandboxClass, creds render.Credentials) (bool, error) {
	switch w.Type {
	case v1alpha1.WaitTypeNotBefore:
		declaredAt := time.Time{}
		if w.DeclaredAt != nil {
			declaredAt = w.DeclaredAt.Time
		}
		return lifecycle.NotBeforeSatisfied(w.Params, declaredAt, e.now()), nil
	case v1alpha1.WaitTypeGitProxyCheck:
		return e.gitProxyCheck(ctx, env, w, class, creds)
	case v1alpha1.WaitTypeHTTPGet:
		return e.httpGet(ctx, w)
	case v1alpha1.WaitTypeS3ObjectExists:
		return e.s3ObjectExists(ctx, w, class, creds)
	default:
		// The API server's enum rejects unknown types, but an object built
		// before CRD validation ran could carry one. Fail-safe: unevaluatable.
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("unknown wait type %q", w.Type)}
	}
}

// gitProxyCheck evaluates a GitProxyCheck wait: GET
// {brokerURL}/{repo}/checks/{ref} with the class's bearer token, satisfied
// when the check summary's overall is terminal (see
// lifecycle.GitProxyCheckSatisfied). repo defaults to spec.repo.
func (e *ProbeEvaluator) gitProxyCheck(ctx context.Context, env *v1alpha1.SandboxEnvironment, w *v1alpha1.WaitForStatus, class *v1alpha1.SandboxClass, creds render.Credentials) (bool, error) {
	if class == nil || class.Spec.Services.GitProxy == nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: "class declares no gitProxy service"}
	}
	repo := w.Params["repo"]
	if repo == "" {
		repo = env.Spec.Repo
	}
	ref := w.Params["ref"]
	if ref == "" {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: "GitProxyCheck requires a ref param"}
	}
	u := strings.TrimRight(class.Spec.Services.GitProxy.BrokerURL, "/") + "/" + repo + "/checks/" + ref

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("building check request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+creds.GitProxyToken)

	resp, err := e.httpClient().Do(req)
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("checking %s: %v", u, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("check endpoint returned %s", resp.Status)}
	}
	if resp.StatusCode == http.StatusNotFound {
		// 404 means no check run exists for this ref yet -- the very thing
		// the wait is waiting to see start. That is the "not satisfied yet"
		// reading (satisfied=false, no error), not an unevaluatable failure:
		// failing the environment after three 404s would defeat a wait
		// declared before CI has started. The waiting timeout (#30 criterion
		// 4) bounds how long this can hold. A genuinely wrong repo/ref that
		// the broker can never resolve surfaces as a persistent 404, which
		// the waiting timeout eventually turns into a clear failure too.
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("check endpoint returned %s", resp.Status)}
	}
	var summary struct {
		Overall string `json:"overall"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("decoding check summary: %v", err)}
	}
	return lifecycle.GitProxyCheckSatisfied(summary.Overall), nil
}

// httpGet evaluates an HTTPGet wait: GET the declared url, satisfied when the
// response matches the declared expectation (see lifecycle.HTTPGetSatisfied).
// The body is read bounded so a huge response cannot exhaust memory.
func (e *ProbeEvaluator) httpGet(ctx context.Context, w *v1alpha1.WaitForStatus) (bool, error) {
	u := w.Params["url"]
	if u == "" {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: "HTTPGet requires a url param"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("building GET request: %v", err)}
	}
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("GET %s: %v", u, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("reading GET %s: %v", u, err)}
	}
	if resp.StatusCode >= 500 {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("GET %s returned %s", u, resp.Status)}
	}
	if resp.StatusCode >= 400 {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("GET %s returned %s", u, resp.Status)}
	}
	return lifecycle.HTTPGetSatisfied(resp.StatusCode, string(body), w.Params), nil
}

// s3ObjectExists evaluates an S3ObjectExists wait: the declared key (relative
// to the class's backend prefix) must exist in the class's S3 bucket.
// ErrNotFound is a definite "not yet" (satisfied=false, no error); an
// unreachable backend is transient; anything else is unevaluatable.
func (e *ProbeEvaluator) s3ObjectExists(ctx context.Context, w *v1alpha1.WaitForStatus, class *v1alpha1.SandboxClass, creds render.Credentials) (bool, error) {
	key := w.Params["key"]
	if key == "" {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: "S3ObjectExists requires a key param"}
	}
	if class == nil || class.Spec.Storage.Backend.Type != v1alpha1.StorageBackendTypeS3 || class.Spec.Storage.Backend.S3 == nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: "class storage backend is not S3"}
	}
	cfg, err := storage.FromS3Backend(class.Spec.Storage.Backend.S3)
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: err.Error()}
	}
	backend, err := e.newS3Backend(cfg, storage.Credentials{
		AccessKeyID:     creds.SnapshotAccessKeyID,
		SecretAccessKey: storage.Secret(creds.SnapshotSecretAccessKey),
		SessionToken:    storage.Secret(creds.SnapshotSessionToken),
	})
	if err != nil {
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("building s3 backend: %v", err)}
	}
	fullKey := joinKey(storage.LayoutPrefix(class.Spec.Storage.Backend), key)
	_, err = backend.Stat(ctx, fullKey)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrNotFound):
		return false, nil
	case errors.Is(err, storage.ErrUnreachable):
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrTransient, Message: fmt.Sprintf("stat %s: %v", fullKey, err)}
	default:
		return false, &lifecycle.ProbeError{Kind: lifecycle.ProbeErrUnevaluatable, Message: fmt.Sprintf("stat %s: %v", fullKey, err)}
	}
}

// joinKey joins a backend prefix and a probe key with "/", never path.Join
// (which would silently collapse a ".." segment -- the sandboxctl validation
// already rejects "..", but the fail-safe reading is to keep it visible).
func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return strings.TrimRight(prefix, "/") + "/" + key
}

// nextEligible computes the NextEligibleAt for a non-satisfied attempt: now
// plus the backoff delay for this attempt's count, with +/-20% jitter (the
// same shape internal/sandboxctl/retrystep.go uses for snapshot retries).
//
// Deliberately NOT Rfc3339Copy'd: that normalization truncates to whole
// seconds, and the 1s backoff with jitter is sub-second -- truncating would
// round a 0.8s delay down to 0 and collapse the backoff entirely. A deadline
// keeps its full precision; only display timestamps are normalized.
func (e *ProbeEvaluator) nextEligible(now time.Time, attempt *v1alpha1.ProbeAttemptStatus) *metav1.Time {
	delays := e.backoff()
	idx := int(attempt.Attempts) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(delays) {
		idx = len(delays) - 1
	}
	delay := jittered(delays[idx], 0.2, rand.Float64)
	t := metav1.NewTime(now.Add(delay))
	return &t
}

// jittered applies +/-frac jitter to d using rnd() (expected to return a
// value uniformly distributed in [0, 1)), mirroring internal/storage/retry.go's
// jittered. frac=0.2 yields a delay in [0.8d, 1.2d].
func jittered(d time.Duration, frac float64, rnd func() float64) time.Duration {
	if frac <= 0 {
		return d
	}
	r := (rnd()*2 - 1) * frac
	out := time.Duration(float64(d) * (1 + r))
	if out < 0 {
		out = 0
	}
	return out
}

// newMetaTime is the controller package's copy of lifecycle's helper: an
// RFC3339-normalized metav1.Time pointer.
func newMetaTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t).Rfc3339Copy()
	return &m
}

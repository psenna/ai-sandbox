package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

var probeFixedNow = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func probeEnv(w *v1alpha1.WaitForStatus) *v1alpha1.SandboxEnvironment {
	return &v1alpha1.SandboxEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "env-1", Namespace: "default", Generation: 1},
		Spec:       v1alpha1.SandboxEnvironmentSpec{Repo: "org/repo"},
		Status:     v1alpha1.SandboxEnvironmentStatus{WaitFor: w},
	}
}

func gitProxyClass(brokerURL string) *v1alpha1.SandboxClass {
	return &v1alpha1.SandboxClass{
		Spec: v1alpha1.SandboxClassSpec{
			Services: v1alpha1.ServicesSpec{
				GitProxy: &v1alpha1.GitProxyService{BrokerURL: brokerURL},
			},
		},
	}
}

func probeCreds() render.Credentials {
	return render.Credentials{GitProxyToken: "test-token"}
}

func gitProxyWait(ref string) *v1alpha1.WaitForStatus {
	return &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeGitProxyCheck, Params: map[string]string{"ref": ref}}
}

// TestProbeEvaluator_GitProxyCheck covers the GitProxyCheck probe's I/O
// contract: the request path (repo defaulting to spec.repo), the Bearer
// auth, and the satisfied/unevaluatable/transient classification of every
// response shape.
func TestProbeEvaluator_GitProxyCheck(t *testing.T) {
	cases := []struct {
		name          string
		repoParam     string
		status        int
		body          string
		wantSatisfied bool
		wantErr       bool
		wantErrKind   lifecycle.ProbeErrorKind
	}{
		{"terminal success", "", 200, `{"overall":"success"}`, true, false, 0},
		{"terminal failure", "", 200, `{"overall":"failure"}`, true, false, 0},
		{"terminal neutral", "", 200, `{"overall":"neutral"}`, true, false, 0},
		{"pending keeps waiting", "", 200, `{"overall":"pending"}`, false, false, 0},
		{"unknown keeps waiting", "", 200, `{"overall":"unknown"}`, false, false, 0},
		{"missing overall keeps waiting", "", 200, `{"checks":[]}`, false, false, 0},
		{"401 is unevaluatable", "", 401, ``, false, true, lifecycle.ProbeErrUnevaluatable},
		{"404 keeps waiting (no check yet)", "", 404, ``, false, false, 0},
		{"500 is transient", "", 500, ``, false, true, lifecycle.ProbeErrTransient},
		{"bad json is unevaluatable", "", 200, `not json`, false, true, lifecycle.ProbeErrUnevaluatable},
		{"explicit repo overrides spec.repo", "other/repo", 200, `{"overall":"success"}`, true, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantPath := "/org/repo/checks/refs/heads/feat/x"
			if tc.repoParam != "" {
				wantPath = "/other/repo/checks/refs/heads/feat/x"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wantPath {
					t.Errorf("request path = %q, want %q", r.URL.Path, wantPath)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
					t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			ev := NewProbeEvaluator()
			env := probeEnv(gitProxyWait("refs/heads/feat/x"))
			if tc.repoParam != "" {
				env.Status.WaitFor.Params["repo"] = tc.repoParam
			}
			satisfied, err := ev.gitProxyCheck(context.Background(), env, env.Status.WaitFor, gitProxyClass(srv.URL), probeCreds())
			if satisfied != tc.wantSatisfied {
				t.Errorf("satisfied = %v, want %v", satisfied, tc.wantSatisfied)
			}
			if tc.wantErr {
				var pe *lifecycle.ProbeError
				if !errors.As(err, &pe) {
					t.Fatalf("error = %v, want a ProbeError", err)
				}
				if pe.Kind != tc.wantErrKind {
					t.Errorf("error kind = %v, want %v", pe.Kind, tc.wantErrKind)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	t.Run("missing gitProxy service is unevaluatable", func(t *testing.T) {
		ev := NewProbeEvaluator()
		env := probeEnv(gitProxyWait("refs/heads/feat/x"))
		_, err := ev.gitProxyCheck(context.Background(), env, env.Status.WaitFor, &v1alpha1.SandboxClass{}, probeCreds())
		var pe *lifecycle.ProbeError
		if !errors.As(err, &pe) || pe.Kind != lifecycle.ProbeErrUnevaluatable {
			t.Fatalf("error = %v, want unevaluatable ProbeError", err)
		}
	})

	t.Run("missing ref is unevaluatable", func(t *testing.T) {
		ev := NewProbeEvaluator()
		env := probeEnv(&v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeGitProxyCheck})
		_, err := ev.gitProxyCheck(context.Background(), env, env.Status.WaitFor, gitProxyClass("http://broker"), probeCreds())
		var pe *lifecycle.ProbeError
		if !errors.As(err, &pe) || pe.Kind != lifecycle.ProbeErrUnevaluatable {
			t.Fatalf("error = %v, want unevaluatable ProbeError", err)
		}
	})

	t.Run("transport failure is transient", func(t *testing.T) {
		// A server that accepts the connection then slams it shut.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()
		ev := NewProbeEvaluator()
		env := probeEnv(gitProxyWait("refs/heads/feat/x"))
		_, err := ev.gitProxyCheck(context.Background(), env, env.Status.WaitFor, gitProxyClass(srv.URL), probeCreds())
		var pe *lifecycle.ProbeError
		if !errors.As(err, &pe) || pe.Kind != lifecycle.ProbeErrTransient {
			t.Fatalf("error = %v, want transient ProbeError", err)
		}
	})
}

// TestProbeEvaluator_HTTPGet covers the HTTPGet probe: status/body matching
// against the declared expectation, and the 4xx/5xx classification.
func TestProbeEvaluator_HTTPGet(t *testing.T) {
	cases := []struct {
		name          string
		params        map[string]string
		status        int
		body          string
		wantSatisfied bool
		wantErr       bool
		wantErrKind   lifecycle.ProbeErrorKind
	}{
		{"200 default is satisfied", nil, 200, "ok", true, false, 0},
		{"expectStatus 201 matches", map[string]string{"expectStatus": "201"}, 201, "ok", true, false, 0},
		{"expectStatus mismatch keeps waiting", map[string]string{"expectStatus": "201"}, 200, "ok", false, false, 0},
		{"expectBody substring matches", map[string]string{"expectBody": "hello"}, 200, "say hello world", true, false, 0},
		{"expectBody missing keeps waiting", map[string]string{"expectBody": "hello"}, 200, "goodbye", false, false, 0},
		{"404 is unevaluatable", nil, 404, "", false, true, lifecycle.ProbeErrUnevaluatable},
		{"500 is transient", nil, 500, "", false, true, lifecycle.ProbeErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			params := map[string]string{"url": srv.URL}
			for k, v := range tc.params {
				params[k] = v
			}
			ev := NewProbeEvaluator()
			w := &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeHTTPGet, Params: params}
			satisfied, err := ev.httpGet(context.Background(), w)
			if satisfied != tc.wantSatisfied {
				t.Errorf("satisfied = %v, want %v", satisfied, tc.wantSatisfied)
			}
			if tc.wantErr {
				var pe *lifecycle.ProbeError
				if !errors.As(err, &pe) {
					t.Fatalf("error = %v, want a ProbeError", err)
				}
				if pe.Kind != tc.wantErrKind {
					t.Errorf("error kind = %v, want %v", pe.Kind, tc.wantErrKind)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	t.Run("missing url is unevaluatable", func(t *testing.T) {
		ev := NewProbeEvaluator()
		_, err := ev.httpGet(context.Background(), &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeHTTPGet})
		var pe *lifecycle.ProbeError
		if !errors.As(err, &pe) || pe.Kind != lifecycle.ProbeErrUnevaluatable {
			t.Fatalf("error = %v, want unevaluatable ProbeError", err)
		}
	})
}

// fakeBackend is a storage.Backend whose Stat returns a canned result,
// counting calls so tests can assert exactly one I/O per evaluation.
type fakeBackend struct {
	statErr   error
	statCalls atomic.Int32
}

func (f *fakeBackend) Stat(_ context.Context, _ string) (storage.ObjectInfo, error) {
	f.statCalls.Add(1)
	if f.statErr != nil {
		return storage.ObjectInfo{}, f.statErr
	}
	return storage.ObjectInfo{Key: "k"}, nil
}

func (f *fakeBackend) Put(context.Context, string, io.Reader, storage.PutOptions) (storage.ObjectInfo, error) {
	panic("unused")
}
func (f *fakeBackend) Get(context.Context, string) (io.ReadCloser, storage.ObjectInfo, error) {
	panic("unused")
}
func (f *fakeBackend) List(context.Context, string) ([]storage.ObjectInfo, error) {
	panic("unused")
}
func (f *fakeBackend) Delete(context.Context, string) error { panic("unused") }
func (f *fakeBackend) DeletePrefix(context.Context, string) (int, error) {
	panic("unused")
}

// TestProbeEvaluator_S3ObjectExists covers the S3ObjectExists probe against a
// fake backend: existence is satisfied, ErrNotFound is a definite "not yet",
// ErrUnreachable is transient, and anything else is unevaluatable.
func TestProbeEvaluator_S3ObjectExists(t *testing.T) {
	cases := []struct {
		name          string
		statErr       error
		wantSatisfied bool
		wantErr       bool
		wantErrKind   lifecycle.ProbeErrorKind
	}{
		{"object exists is satisfied", nil, true, false, 0},
		{"not found keeps waiting", storage.ErrNotFound, false, false, 0},
		{"unreachable is transient", storage.ErrUnreachable, false, true, lifecycle.ProbeErrTransient},
		{"permission is unevaluatable", storage.ErrPermission, false, true, lifecycle.ProbeErrUnevaluatable},
		{"other error is unevaluatable", errors.New("boom"), false, true, lifecycle.ProbeErrUnevaluatable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{statErr: tc.statErr}
			ev := &ProbeEvaluator{
				NewS3Backend: func(storage.S3Config, storage.Credentials) (storage.Backend, error) {
					return backend, nil
				},
			}
			class := &v1alpha1.SandboxClass{
				Spec: v1alpha1.SandboxClassSpec{
					Storage: v1alpha1.StorageSpec{
						Backend: v1alpha1.BackendSpec{
							Type: v1alpha1.StorageBackendTypeS3,
							S3:   &v1alpha1.S3Backend{Endpoint: "http://minio:9000", Bucket: "b"},
						},
					},
				},
			}
			w := &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeS3ObjectExists, Params: map[string]string{"key": "dir/obj"}}
			satisfied, err := ev.s3ObjectExists(context.Background(), w, class, probeCreds())
			if satisfied != tc.wantSatisfied {
				t.Errorf("satisfied = %v, want %v", satisfied, tc.wantSatisfied)
			}
			if tc.wantErr {
				var pe *lifecycle.ProbeError
				if !errors.As(err, &pe) {
					t.Fatalf("error = %v, want a ProbeError", err)
				}
				if pe.Kind != tc.wantErrKind {
					t.Errorf("error kind = %v, want %v", pe.Kind, tc.wantErrKind)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if backend.statCalls.Load() != 1 {
				t.Errorf("stat calls = %d, want 1", backend.statCalls.Load())
			}
		})
	}

	t.Run("non-S3 backend is unevaluatable", func(t *testing.T) {
		ev := NewProbeEvaluator()
		class := &v1alpha1.SandboxClass{Spec: v1alpha1.SandboxClassSpec{Storage: v1alpha1.StorageSpec{Backend: v1alpha1.BackendSpec{Type: v1alpha1.StorageBackendTypePVC}}}}
		w := &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeS3ObjectExists, Params: map[string]string{"key": "obj"}}
		_, err := ev.s3ObjectExists(context.Background(), w, class, probeCreds())
		var pe *lifecycle.ProbeError
		if !errors.As(err, &pe) || pe.Kind != lifecycle.ProbeErrUnevaluatable {
			t.Fatalf("error = %v, want unevaluatable ProbeError", err)
		}
	})
}

// TestProbeEvaluator_NotBefore covers the NotBefore probe, which is pure --
// no I/O, just the declared deadline against the evaluator's clock.
func TestProbeEvaluator_NotBefore(t *testing.T) {
	now := probeFixedNow
	cases := []struct {
		name          string
		params        map[string]string
		declaredAt    *metav1.Time
		wantSatisfied bool
	}{
		{"absolute time in the past", map[string]string{"time": now.Add(-time.Minute).Format(time.RFC3339)}, nil, true},
		{"absolute time exactly now", map[string]string{"time": now.Format(time.RFC3339)}, nil, true},
		{"absolute time in the future", map[string]string{"time": now.Add(time.Minute).Format(time.RFC3339)}, nil, false},
		{"duration elapsed", map[string]string{"duration": "30m"}, &metav1.Time{Time: now.Add(-time.Hour)}, true},
		{"duration not elapsed", map[string]string{"duration": "30m"}, &metav1.Time{Time: now.Add(-time.Minute)}, false},
		{"missing params keeps waiting", nil, nil, false},
		{"unparseable time keeps waiting", map[string]string{"time": "not-a-time"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &ProbeEvaluator{Clock: func() time.Time { return now }}
			w := &v1alpha1.WaitForStatus{Type: v1alpha1.WaitTypeNotBefore, Params: tc.params, DeclaredAt: tc.declaredAt}
			satisfied, err := ev.evaluateOne(context.Background(), probeEnv(w), w, nil, probeCreds())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if satisfied != tc.wantSatisfied {
				t.Errorf("satisfied = %v, want %v", satisfied, tc.wantSatisfied)
			}
		})
	}
}

// TestProbeEvaluator_Evaluate_AttemptRecord covers the attempt-record
// contract: counters accumulate across evaluations, unevaluatable errors fail
// the environment at the consecutive-error threshold, a satisfied probe
// resets the counter, and the backoff window skips I/O entirely.
func TestProbeEvaluator_Evaluate_AttemptRecord(t *testing.T) {
	// The four cases are top-level helpers (not inline closures) so gocyclo
	// measures each in isolation rather than summing their branches into this
	// table-of-contents function.
	t.Run("unevaluatable errors fail at the threshold", testAttemptRecord_UnevaluatableThreshold)
	t.Run("transient errors never fail the environment", testAttemptRecord_TransientNeverFails)
	t.Run("transient errors do not count toward the unevaluatable threshold", testAttemptRecord_TransientDoesNotCount)
	t.Run("satisfied probe resets the counter and reports satisfied", testAttemptRecord_SatisfiedResets)
	t.Run("backoff window skips I/O and preserves the attempt", testAttemptRecord_BackoffSkipsIO)
}

// testAttemptRecord_TransientDoesNotCount verifies ConsecutiveErrors tracks
// CONSECUTIVE UNEVALUATABLE results only: transient errors (5xx/transport)
// neither increment nor reset the streak, so N transient hiccups followed by a
// single unevaluatable error must not cross the threshold. This is the
// bounded-attempts guarantee -- without it, transient×3 + unevaluatable×1
// would fail the environment after one unevaluatable error, defeating #30's
// "fail after a bounded number of [unevaluatable] attempts" contract.
func testAttemptRecord_TransientDoesNotCount(t *testing.T) {
	ctx := context.Background()
	// A server that returns 500 (transient) until toggled to 401 (unevaluatable).
	status := int32(500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()
	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client(), MaxConsecutiveErrors: 3}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)

	// Two transient errors: ConsecutiveErrors must stay 0 and the env must
	// not fail.
	for i := 1; i <= 2; i++ {
		f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
		if err != nil {
			t.Fatalf("transient eval %d: unexpected error: %v", i, err)
		}
		if f.WaitProbeFailure != nil {
			t.Fatalf("transient eval %d: unexpected WaitProbeFailure %+v", i, f.WaitProbeFailure)
		}
		if attempt.ConsecutiveErrors != 0 {
			t.Errorf("transient eval %d: ConsecutiveErrors = %d, want 0 (transient must not count)", i, attempt.ConsecutiveErrors)
		}
		if attempt.Phase != v1alpha1.ProbeAttemptPending {
			t.Errorf("transient eval %d: phase = %s, want Pending", i, attempt.Phase)
		}
		env.Status.ProbeAttempt = attempt
		clk.Advance(time.Hour) // clear the backoff window
	}

	// One unevaluatable error: only the FIRST toward the threshold.
	atomic.StoreInt32(&status, 401)
	f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
	if err != nil {
		t.Fatalf("unevaluatable eval: unexpected error: %v", err)
	}
	if f.WaitProbeFailure != nil {
		t.Fatalf("unevaluatable eval: WaitProbeFailure = %+v, want nil (only 1 consecutive unevaluatable, threshold is 3)", f.WaitProbeFailure)
	}
	if attempt.ConsecutiveErrors != 1 {
		t.Errorf("unevaluatable eval: ConsecutiveErrors = %d, want 1", attempt.ConsecutiveErrors)
	}
}

func testAttemptRecord_UnevaluatableThreshold(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client(), MaxConsecutiveErrors: 3}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)

	for i := 1; i <= 2; i++ {
		f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
		if err != nil {
			t.Fatalf("eval %d: unexpected error: %v", i, err)
		}
		if !f.ProbeObserved {
			t.Fatalf("eval %d: ProbeObserved = false", i)
		}
		if f.WaitProbeFailure != nil {
			t.Fatalf("eval %d: unexpected WaitProbeFailure %+v", i, f.WaitProbeFailure)
		}
		if attempt.Attempts != int32(i) || attempt.ConsecutiveErrors != int32(i) {
			t.Fatalf("eval %d: attempt = %+v", i, attempt)
		}
		if attempt.Phase != v1alpha1.ProbeAttemptPending || attempt.LastResult != "pending" {
			t.Fatalf("eval %d: attempt phase/result = %s/%s, want Pending/pending", i, attempt.Phase, attempt.LastResult)
		}
		if attempt.NextEligibleAt == nil {
			t.Fatalf("eval %d: NextEligibleAt is nil", i)
		}
		env.Status.ProbeAttempt = attempt
		clk.Advance(time.Hour) // clear the backoff window
	}

	f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
	if err != nil {
		t.Fatalf("eval 3: unexpected error: %v", err)
	}
	if f.WaitProbeFailure == nil {
		t.Fatalf("eval 3: WaitProbeFailure is nil, want failure at the threshold")
	}
	if f.WaitProbeFailure.Reason != lifecycle.ReasonProbeFailed {
		t.Errorf("failure reason = %q, want %q", f.WaitProbeFailure.Reason, lifecycle.ReasonProbeFailed)
	}
	if attempt.ConsecutiveErrors != 3 {
		t.Errorf("ConsecutiveErrors = %d, want 3", attempt.ConsecutiveErrors)
	}
	if attempt.Phase != v1alpha1.ProbeAttemptFailed || attempt.LastResult != "error" {
		t.Errorf("attempt phase/result = %s/%s, want Failed/error", attempt.Phase, attempt.LastResult)
	}
}

func testAttemptRecord_TransientNeverFails(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client(), MaxConsecutiveErrors: 3}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)

	for i := 1; i <= 5; i++ {
		f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
		if err != nil {
			t.Fatalf("eval %d: unexpected error: %v", i, err)
		}
		if f.WaitProbeFailure != nil {
			t.Fatalf("eval %d: unexpected WaitProbeFailure %+v", i, f.WaitProbeFailure)
		}
		if attempt.Phase != v1alpha1.ProbeAttemptPending {
			t.Fatalf("eval %d: phase = %s, want Pending", i, attempt.Phase)
		}
		env.Status.ProbeAttempt = attempt
		clk.Advance(time.Hour)
	}
}

func testAttemptRecord_SatisfiedResets(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"overall":"success"}`))
	}))
	defer srv.Close()
	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client()}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)

	// One failed evaluation first, so the counter is non-zero.
	env.Status.ProbeAttempt = &v1alpha1.ProbeAttemptStatus{Attempts: 4, ConsecutiveErrors: 2}

	f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !f.WaitProbeSatisfied {
		t.Fatalf("WaitProbeSatisfied = false, want true")
	}
	if f.WaitProbeFailure != nil {
		t.Fatalf("unexpected WaitProbeFailure %+v", f.WaitProbeFailure)
	}
	if attempt.Attempts != 5 {
		t.Errorf("Attempts = %d, want 5", attempt.Attempts)
	}
	if attempt.ConsecutiveErrors != 0 {
		t.Errorf("ConsecutiveErrors = %d, want 0 after satisfaction", attempt.ConsecutiveErrors)
	}
	if attempt.Phase != v1alpha1.ProbeAttemptSatisfied || attempt.LastResult != "satisfied" {
		t.Errorf("attempt phase/result = %s/%s, want Satisfied/satisfied", attempt.Phase, attempt.LastResult)
	}
	if attempt.NextEligibleAt != nil {
		t.Errorf("NextEligibleAt = %v, want nil once satisfied", attempt.NextEligibleAt)
	}
}

func testAttemptRecord_BackoffSkipsIO(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"overall":"pending"}`))
	}))
	defer srv.Close()
	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client()}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)

	f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
	env.Status.ProbeAttempt = attempt

	// Within the backoff window: no I/O, attempt nil, ProbeObserved true.
	clk.Advance(500 * time.Millisecond)
	f, attempt, err = ev.Evaluate(ctx, env, class, probeCreds())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want still 1 inside the backoff window", requests.Load())
	}
	if attempt != nil {
		t.Errorf("attempt = %+v, want nil on the skip path", attempt)
	}
	if !f.ProbeObserved {
		t.Errorf("ProbeObserved = false, want true on the skip path")
	}
	if f.WaitProbeSatisfied || f.WaitProbeFailure != nil {
		t.Errorf("skip path leaked facts: satisfied=%v failure=%+v", f.WaitProbeSatisfied, f.WaitProbeFailure)
	}
}

// TestProbeEvaluator_Backoff_RequestCounting is the criterion-5 test: the
// evaluator makes AT MOST ONE I/O call per evaluation, and a probe that
// keeps answering "not yet" is re-checked on the [1s,2s,4s,8s] schedule
// (capped at 8s), each delay within +/-20% jitter.
func TestProbeEvaluator_Backoff_RequestCounting(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"overall":"pending"}`))
	}))
	defer srv.Close()

	clk := newFakeClock(probeFixedNow)
	ev := &ProbeEvaluator{Clock: clk.Now, HTTPClient: srv.Client()}
	env := probeEnv(gitProxyWait("refs/heads/feat/x"))
	class := gitProxyClass(srv.URL)
	ctx := context.Background()

	// The schedule: each evaluation advances the clock past the previous
	// NextEligibleAt, and the next delay follows 1s, 2s, 4s, 8s, 8s.
	steps := []struct {
		advance time.Duration // clock advance before this evaluation
		base    time.Duration // expected backoff base for the NEXT evaluation
	}{
		{0, 1 * time.Second},
		{1500 * time.Millisecond, 2 * time.Second}, // t0+1.5s: past the 1s window
		{3 * time.Second, 4 * time.Second},         // t0+4.5s: past the 2s window
		{5 * time.Second, 8 * time.Second},         // t0+9.5s: past the 4s window
		{9 * time.Second, 8 * time.Second},         // t0+18.5s: past the 8s window (capped)
	}
	for i, step := range steps {
		clk.Advance(step.advance)
		now := clk.Now()
		f, attempt, err := ev.Evaluate(ctx, env, class, probeCreds())
		if err != nil {
			t.Fatalf("eval %d: unexpected error: %v", i, err)
		}
		if !f.ProbeObserved {
			t.Fatalf("eval %d: ProbeObserved = false", i)
		}
		if attempt == nil {
			t.Fatalf("eval %d: attempt is nil", i)
		}
		if attempt.Attempts != int32(i+1) {
			t.Errorf("eval %d: Attempts = %d, want %d", i, attempt.Attempts, i+1)
		}
		assertNextEligible(t, now, attempt, step.base)
		env.Status.ProbeAttempt = attempt

		// Exactly one request per evaluation, and none inside the window.
		if want := int32(i + 1); requests.Load() != want {
			t.Errorf("eval %d: requests = %d, want %d (at most one I/O call per evaluation)", i, requests.Load(), want)
		}
		clk.Advance(step.base / 2) // safely inside the next window
		_, attempt, err = ev.Evaluate(ctx, env, class, probeCreds())
		if err != nil {
			t.Fatalf("eval %d skip: unexpected error: %v", i, err)
		}
		if attempt != nil {
			t.Errorf("eval %d skip: attempt = %+v, want nil inside the backoff window", i, attempt)
		}
		if want := int32(i + 1); requests.Load() != want {
			t.Errorf("eval %d skip: requests = %d, want %d (no I/O inside the backoff window)", i, requests.Load(), want)
		}
	}
}

// assertNextEligible checks that NextEligibleAt is base +/-20% after now.
func assertNextEligible(t *testing.T, now time.Time, attempt *v1alpha1.ProbeAttemptStatus, base time.Duration) {
	t.Helper()
	if attempt.NextEligibleAt == nil {
		t.Fatalf("NextEligibleAt is nil")
	}
	d := attempt.NextEligibleAt.Sub(now)
	lo := time.Duration(float64(base) * 0.8)
	hi := time.Duration(float64(base) * 1.2)
	if d < lo || d > hi {
		t.Errorf("NextEligibleAt delay = %v, want in [%v, %v]", d, lo, hi)
	}
}

// TestObserveProbe covers the observeCluster integration: the evaluator is
// only consulted while Waiting with a declared wait, and its facts are merged
// into the ClusterFacts.
func TestObserveProbe(t *testing.T) {
	ctx := context.Background()

	t.Run("nil evaluator leaves facts at the safe zero reading", func(t *testing.T) {
		r := &Reconciler{}
		f := lifecycle.ClusterFacts{}
		env := probeEnv(gitProxyWait("refs/heads/feat/x"))
		env.Status.Phase = v1alpha1.PhaseWaiting
		r.observeProbe(ctx, env, gitProxyClass("http://broker"), probeCreds(), &f)
		if f.ProbeObserved || f.WaitProbeSatisfied || f.WaitProbeFailure != nil || f.ProbeAttempt != nil {
			t.Errorf("facts = %+v, want all zero", f)
		}
	})

	t.Run("only consulted while Waiting with a waitFor", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"overall":"pending"}`))
		}))
		defer srv.Close()
		r := &Reconciler{Probes: &ProbeEvaluator{HTTPClient: srv.Client()}}
		class := gitProxyClass(srv.URL)

		// Running with a waitFor: not consulted.
		env := probeEnv(gitProxyWait("refs/heads/feat/x"))
		env.Status.Phase = v1alpha1.PhaseRunning
		f := lifecycle.ClusterFacts{}
		r.observeProbe(ctx, env, class, probeCreds(), &f)
		if calls.Load() != 0 {
			t.Errorf("calls = %d, want 0 outside Waiting", calls.Load())
		}

		// Waiting with a waitFor: consulted, facts merged.
		env.Status.Phase = v1alpha1.PhaseWaiting
		r.observeProbe(ctx, env, class, probeCreds(), &f)
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1 while Waiting", calls.Load())
		}
		if !f.ProbeObserved || f.WaitProbeSatisfied {
			t.Errorf("facts = %+v, want ProbeObserved=true, not satisfied", f)
		}
		if f.ProbeAttempt == nil || f.ProbeAttempt.Attempts != 1 {
			t.Errorf("ProbeAttempt = %+v, want Attempts=1", f.ProbeAttempt)
		}
	})
}

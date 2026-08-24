package sandboxctl

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// handlers holds everything the HTTP handlers need: the Store (patches
// status), the Poller (freeze latch + progress ring buffer + cached
// snapshot), and an injected clock (never time.Now() directly, matching
// this repo's convention elsewhere).
type handlers struct {
	store  Store
	poll   *Poller
	env    EnvironmentRef
	sets   serviceSetApplier // nil for non-k8s-native envs (services apply then 404s)
	execer Execer            // nil for non-k8s-native envs (exec then 404s)
	now    func() time.Time
	log    func(format string, args ...any)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message, field string, allowed []string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{Code: code, Message: message, Field: field, Allowed: allowed}})
}

func writeValidationError(w http.ResponseWriter, status int, ve *ValidationError) {
	writeError(w, status, ve.Code, ve.Message, ve.Field, ve.Allowed)
}

// handleWait handles POST /v1/wait.
func (h *handlers) handleWait(w http.ResponseWriter, r *http.Request) {
	if h.poll.Freezing() {
		writeError(w, http.StatusConflict, CodeFreezing, "environment is freezing; new wait declarations are refused", "", nil)
		return
	}

	var req WaitRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}

	probe := WaitProbe(req)
	if err := probe.Validate(); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			writeValidationError(w, http.StatusBadRequest, ve)
			return
		}
		writeError(w, http.StatusBadRequest, CodeBadJSON, err.Error(), "", nil)
		return
	}

	now := h.now()
	err := h.store.DeclareWait(r.Context(), probe, now)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, WaitResponse{
			Type: probe.Type, Reason: probe.Reason, Params: probe.Params,
			DeclaredAt:  now,
			Environment: h.env.Name,
			Message:     "wait declared; exit now so the environment can be frozen",
		})
	case errors.Is(err, ErrWaitAlreadyDeclared):
		writeError(w, http.StatusConflict, CodeWaitAlreadyDeclared, "a wait has already been declared for this run", "", nil)
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err):
		writeError(w, http.StatusBadGateway, CodeStatusPatchFailed, err.Error(), "", nil)
	default:
		h.log("declare wait failed: %v", err)
		writeError(w, http.StatusBadGateway, CodeStatusPatchFailed, err.Error(), "", nil)
	}
}

// handleDone handles POST /v1/done.
func (h *handlers) handleDone(w http.ResponseWriter, r *http.Request) {
	if h.poll.Freezing() {
		writeError(w, http.StatusConflict, CodeFreezing, "environment is freezing; the result cannot be recorded now", "", nil)
		return
	}

	var req DoneRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}

	outcome, err := parseOutcome(req.Outcome)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidOutcome, err.Error(), "outcome", []string{"success", "failure"})
		return
	}
	message := truncate(req.Message, maxReasonBytes)
	if req.ExitCode != nil && (*req.ExitCode < 0 || *req.ExitCode > 255) {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "exitCode must be between 0 and 255", "exitCode", nil)
		return
	}

	now := h.now()
	idempotent, err := h.store.ReportDone(r.Context(), Result{Outcome: outcome, Message: message, ExitCode: req.ExitCode}, now)
	resp := DoneResponse{Outcome: req.Outcome, Message: message, ExitCode: req.ExitCode, ReportedAt: now}
	switch {
	case err == nil && idempotent:
		writeJSON(w, http.StatusOK, resp)
	case err == nil:
		writeJSON(w, http.StatusAccepted, resp)
	case errors.Is(err, ErrResultAlreadyReported):
		writeError(w, http.StatusConflict, CodeResultAlreadyReported, "a different result was already reported for this run", "", nil)
	case errors.Is(err, ErrWaitAlreadyDeclared):
		writeError(w, http.StatusConflict, CodeWaitAlreadyDeclared, "a wait was already declared; this run has not finished", "", nil)
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err):
		writeError(w, http.StatusBadGateway, CodeStatusPatchFailed, err.Error(), "", nil)
	default:
		h.log("report done failed: %v", err)
		writeError(w, http.StatusBadGateway, CodeStatusPatchFailed, err.Error(), "", nil)
	}
}

// handleProgress handles POST /v1/progress. Deliberately does NOT touch the
// API server: written as a structured log line on stdout/stderr plus an
// in-memory ring buffer, returned by /v1/status. Writing to Kubernetes
// status would churn the object and fight the reconciler; emitting an Event
// would need `create` on events, widening the Role for a breadcrumb.
func (h *handlers) handleProgress(w http.ResponseWriter, r *http.Request) {
	if h.poll.Freezing() {
		writeError(w, http.StatusConflict, CodeFreezing, "environment is freezing; progress is not accepted now", "", nil)
		return
	}

	var req ProgressRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if len(req.Message) == 0 {
		writeError(w, http.StatusBadRequest, CodeMissingParam, "message must not be empty", "message", nil)
		return
	}
	if len(req.Message) > maxProgressBodyBytes {
		writeError(w, http.StatusBadRequest, CodeInvalidParam, "message exceeds the progress payload limit", "message", nil)
		return
	}

	h.log("progress environment=%s message=%q", h.env.Name, req.Message)
	h.poll.AddProgress(req.Message)
	writeJSON(w, http.StatusAccepted, ProgressResponse{Accepted: true})
}

// handleStatus handles GET /v1/status, served entirely from the poller's
// cached snapshot -- never a live API call per request.
func (h *handlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := h.store.Snapshot()
	resp := StatusResponse{
		Environment: h.env,
		Phase:       string(snap.Phase),
		Freezing:    h.poll.Freezing(),
		FreezeCount: snap.FreezeCount,
		WakeCount:   snap.WakeCount,
		Progress:    h.poll.Progress(),
		ObservedAt:  snap.ObservedAt,
		Stale:       h.poll.Stale(snap, h.now()),
	}
	if snap.WaitFor != nil {
		resp.WaitFor = &WaitProbe{Type: snap.WaitFor.Type, Reason: snap.WaitFor.Reason, Params: snap.WaitFor.Params}
	}
	if snap.Result != nil {
		var reportedAt time.Time
		if snap.Result.ReportedAt != nil {
			reportedAt = snap.Result.ReportedAt.Time
		}
		resp.Result = &DoneResponse{
			Outcome:    string(snap.Result.Outcome),
			Message:    snap.Result.Message,
			ExitCode:   snap.Result.ExitCode,
			ReportedAt: reportedAt,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHealthz handles GET /healthz. Not rate-limited (the kubelet probes
// it every second) and keeps answering even once freezing is latched, so
// the kubelet does not kill the container mid-freeze.
func (h *handlers) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthzResponse{Status: "ok"})
}

// handleServicesApply handles POST /v1/services. The agent POSTs a declaration
// (services + runtimes, no environmentName); the server validates it, stamps
// EnvironmentName from its own identity, and upserts the ServiceSet CR owned by
// the environment. The ServiceSetReconciler then reconciles it to Pods.
func (h *handlers) handleServicesApply(w http.ResponseWriter, r *http.Request) {
	if h.sets == nil {
		// Not a k8s-native environment: the sidecar Role grants no servicesets
		// RBAC, so upsert would 403. Surface it as a clean 404 rather than a
		// confusing 502 RBAC message.
		writeError(w, http.StatusNotFound, CodeNotFound, "services are not enabled on this environment (requires the k8s-native engine)", "", nil)
		return
	}
	var req ServicesApplyRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	spec := v1alpha1.ServiceSetSpec{EnvironmentName: h.env.Name, Services: req.Services, Runtimes: req.Runtimes}
	if err := ValidateServiceSet(spec); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			writeValidationError(w, http.StatusUnprocessableEntity, ve)
			return
		}
		writeError(w, http.StatusBadRequest, CodeInvalidDeclaration, err.Error(), "", nil)
		return
	}
	if err := h.sets.Upsert(r.Context(), spec); err != nil {
		h.log("services apply failed: %v", err)
		// All upsert failures (RBAC forbidden, invalid CR, env-not-found, or a
		// transport error) surface as 502: the sidecar could not apply the
		// ServiceSet on the agent's behalf. The error message carries the cause.
		writeError(w, http.StatusBadGateway, CodeServiceSetUpsertFailed, err.Error(), "", nil)
		return
	}
	writeJSON(w, http.StatusOK, ServicesApplyResponse{
		Environment: h.env.Name,
		Services:    len(spec.Services),
		Runtimes:    len(spec.Runtimes),
		Applied:     true,
	})
}

// handleExec handles POST /v1/exec. The agent POSTs {runtime, command, stdin};
// the server one-shot execs into the named runtime pod (SPDY) and returns
// stdout/stderr + a best-effort exit code. A non-zero command exit is NOT a
// server error: stdout/stderr are returned with ExitCode set and Error empty.
// A transport/protocol failure (pod gone, not authorized) returns 200 with
// Error populated so the agent sees the failure message alongside any output.
func (h *handlers) handleExec(w http.ResponseWriter, r *http.Request) {
	if h.execer == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "exec is not enabled on this environment (requires the k8s-native engine)", "", nil)
		return
	}
	var req ExecRequest
	if err := decodeStrict(r.Body, &req); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if req.Runtime == "" {
		writeError(w, http.StatusBadRequest, CodeMissingParam, "runtime must not be empty", "runtime", nil)
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, CodeMissingParam, "command must not be empty", "command", nil)
		return
	}
	stdout, stderr, err := h.execer.Exec(r.Context(), req.Runtime, req.Command, []byte(req.Stdin))
	resp := ExecResponse{Runtime: req.Runtime, Stdout: string(stdout), Stderr: string(stderr), ExitCode: extractExitCode(err)}
	if err != nil && resp.ExitCode < 0 {
		h.log("exec %s failed: %v", req.Runtime, err)
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeDecodeErr(w http.ResponseWriter, err error) {
	if mbErr, ok := isMaxBytesError(err); ok {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			"request body exceeds the limit for this endpoint", "", nil)
		_ = mbErr
		return
	}
	var de *decodeError
	if errors.As(err, &de) {
		writeError(w, http.StatusBadRequest, de.code, de.message, "", nil)
		return
	}
	writeError(w, http.StatusBadRequest, CodeBadJSON, err.Error(), "", nil)
}

func parseOutcome(s string) (v1alpha1.AgentOutcome, error) {
	switch s {
	case "success":
		return v1alpha1.AgentOutcomeSucceeded, nil
	case "failure":
		return v1alpha1.AgentOutcomeFailed, nil
	default:
		return "", errInvalidOutcome(s)
	}
}

type invalidOutcomeError string

func (e invalidOutcomeError) Error() string {
	return `outcome must be "success" or "failure", got "` + string(e) + `"`
}

func errInvalidOutcome(s string) error { return invalidOutcomeError(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

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
	store Store
	poll  *Poller
	env   EnvironmentRef
	now   func() time.Time
	log   func(format string, args ...any)
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

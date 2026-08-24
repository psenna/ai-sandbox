package sandboxctl

import (
	"time"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// ErrorEnvelope is the uniform, actionable error shape returned by every
// non-2xx response.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the payload of ErrorEnvelope.
type ErrorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Field   string   `json:"field,omitempty"`
	Allowed []string `json:"allowed,omitempty"`
}

// Known error codes. See doc.go / the use-sandbox skill for the full,
// human-facing table of codes and corrective actions.
const (
	CodeBadJSON                = "bad_json"
	CodeUnknownField           = "unknown_field"
	CodePayloadTooLarge        = "payload_too_large"
	CodeUnsupportedMediaType   = "unsupported_media_type"
	CodeMethodNotAllowed       = "method_not_allowed"
	CodeNotFound               = "not_found"
	CodeUnknownProbeType       = "unknown_probe_type"
	CodeMissingParam           = "missing_param"
	CodeUnknownParam           = "unknown_param"
	CodeInvalidParam           = "invalid_param"
	CodeInvalidOutcome         = "invalid_outcome"
	CodeWaitAlreadyDeclared    = "wait_already_declared"
	CodeResultAlreadyReported  = "result_already_reported"
	CodeFreezing               = "freezing"
	CodeRateLimited            = "rate_limited"
	CodeStatusPatchFailed      = "status_patch_failed"
	CodeDuplicateEntryName     = "duplicate_entry_name"
	CodeDanglingDependsOn      = "dangling_depends_on"
	CodeInvalidDeclaration     = "invalid_declaration"
	CodeServiceSetUpsertFailed = "serviceset_upsert_failed"
	CodeInternal               = "internal"
)

// WaitRequest is the POST /v1/wait request body.
type WaitRequest struct {
	Type   string            `json:"type"`
	Reason string            `json:"reason"`
	Params map[string]string `json:"params,omitempty"`
}

// WaitResponse is the POST /v1/wait 202 response body.
type WaitResponse struct {
	Type        string            `json:"type"`
	Reason      string            `json:"reason"`
	Params      map[string]string `json:"params,omitempty"`
	DeclaredAt  time.Time         `json:"declaredAt"`
	Environment string            `json:"environment"`
	Message     string            `json:"message"`
}

// DoneRequest is the POST /v1/done request body. Outcome is the wire
// spelling ("success"/"failure"), mapped onto v1alpha1.AgentOutcome by
// store.go.
type DoneRequest struct {
	Outcome  string `json:"outcome"`
	Message  string `json:"message,omitempty"`
	ExitCode *int32 `json:"exitCode,omitempty"`
}

// DoneResponse is the POST /v1/done response body (202 on first report, 200
// on an idempotent repeat).
type DoneResponse struct {
	Outcome    string    `json:"outcome"`
	Message    string    `json:"message,omitempty"`
	ExitCode   *int32    `json:"exitCode,omitempty"`
	ReportedAt time.Time `json:"reportedAt"`
}

// ProgressRequest is the POST /v1/progress request body.
type ProgressRequest struct {
	Message string `json:"message"`
}

// ProgressResponse is the POST /v1/progress 202 response body.
type ProgressResponse struct {
	Accepted bool `json:"accepted"`
}

// EnvironmentRef identifies the environment this sidecar belongs to, echoed
// on /v1/status.
type EnvironmentRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid,omitempty"`
}

// StatusResponse is the GET /v1/status response body, served entirely from
// the poller's cached snapshot (never a live API call per request).
type StatusResponse struct {
	Environment EnvironmentRef `json:"environment"`
	Phase       string         `json:"phase"`
	Freezing    bool           `json:"freezing"`
	WaitFor     *WaitProbe     `json:"waitFor"`
	Result      *DoneResponse  `json:"result"`
	FreezeCount int32          `json:"freezeCount"`
	WakeCount   int32          `json:"wakeCount"`
	Snapshot    *string        `json:"snapshot"`
	Progress    []string       `json:"progress"`
	ObservedAt  time.Time      `json:"observedAt"`
	Stale       bool           `json:"stale"`
}

// HealthzResponse is the GET /healthz response body.
type HealthzResponse struct {
	Status string `json:"status"`
}

// ServicesApplyRequest is the POST /v1/services body: the declaration
// (services + runtimes). EnvironmentName is NOT sent -- the server stamps it
// from its own identity, so a declaration is portable across environments.
type ServicesApplyRequest struct {
	Services []v1alpha1.ServiceSpec `json:"services,omitempty"`
	Runtimes []v1alpha1.RuntimeSpec `json:"runtimes,omitempty"`
}

// ServicesApplyResponse is the POST /v1/services 200/201 body.
type ServicesApplyResponse struct {
	Environment string `json:"environment"`
	Services    int    `json:"services"`
	Runtimes    int    `json:"runtimes"`
	Applied     bool   `json:"applied"`
}

// ExecRequest is the POST /v1/exec body: run cmd in the named runtime pod,
// piping stdin (text) into the process. One-shot (no TTY/streaming).
type ExecRequest struct {
	Runtime string   `json:"runtime"`
	Command []string `json:"command"`
	Stdin   string   `json:"stdin,omitempty"`
}

// ExecResponse is the POST /v1/exec 200 body. stdout/stderr are the command's
// captured output (text; binary output is out of scope). ExitCode is
// best-effort: 0 on success, the command's exit code when extractable from the
// SPDY error, else -1. Error is the transport/protocol error message, empty
// when the command ran (regardless of its exit code).
type ExecResponse struct {
	Runtime  string `json:"runtime"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

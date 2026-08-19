package lifecycle

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Terminal overall values for a git-proxy check summary. GitProxyCheckSatisfied
// returns true when overall reaches any of these -- a green OR failed terminal
// state; pending/unknown/none keep waiting.
const (
	OverallSuccess   = "success"
	OverallFailure   = "failure"
	OverallNeutral   = "neutral"
	OverallCancelled = "cancelled"
	OverallTimedOut  = "timed_out"
)

// NotBeforeDeadline returns the absolute deadline a NotBefore wait declares:
// the parsed "time" param, or declaredAt + "duration". ok=false when the
// params are missing or unparseable -- unreachable in practice, because
// internal/sandboxctl/probe.go validates every declaration fail-closed
// (exactly one of time/duration, both well-formed); the safe reading is
// "not satisfied".
func NotBeforeDeadline(params map[string]string, declaredAt time.Time) (time.Time, bool) {
	if t, ok := params["time"]; ok {
		deadline, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return time.Time{}, false
		}
		return deadline, true
	}
	if d, ok := params["duration"]; ok {
		delay, err := time.ParseDuration(d)
		if err != nil {
			return time.Time{}, false
		}
		return declaredAt.Add(delay), true
	}
	return time.Time{}, false
}

// NotBeforeSatisfied reports whether the NotBefore wait's deadline has
// passed. declaredAt anchors a relative "duration" deadline (the wait's
// DeclaredAt, stamped by the sidecar). A missing or unparseable param is
// treated as unsatisfied -- the safe reading.
func NotBeforeSatisfied(params map[string]string, declaredAt, now time.Time) bool {
	deadline, ok := NotBeforeDeadline(params, declaredAt)
	if !ok {
		return false
	}
	return !now.Before(deadline)
}

// GitProxyCheckSatisfied reports whether a git-proxy check summary's overall
// status is terminal. A check is satisfied when overall reaches a terminal
// value -- green OR failed terminal; pending/unknown/none keep waiting.
func GitProxyCheckSatisfied(overall string) bool {
	switch overall {
	case OverallSuccess, OverallFailure, OverallNeutral, OverallCancelled, OverallTimedOut:
		return true
	default:
		return false
	}
}

// HTTPGetSatisfied reports whether an HTTP probe's response matches the
// declared expectation: status equals expectStatus (default 200) and, when
// expectBody is set, body contains it as a substring. An unparseable
// expectStatus falls back to the 200 default (the sidecar's validation makes
// this unreachable in practice).
func HTTPGetSatisfied(status int, body string, params map[string]string) bool {
	expectStatus := 200
	if s, ok := params["expectStatus"]; ok {
		if n, err := strconv.Atoi(s); err == nil {
			expectStatus = n
		}
	}
	if status != expectStatus {
		return false
	}
	if want, ok := params["expectBody"]; ok && want != "" {
		return strings.Contains(body, want)
	}
	return true
}

// S3ObjectExistsSatisfied reports whether the S3 object the wait names
// exists.
func S3ObjectExistsSatisfied(exists bool) bool { return exists }

// ProbeErrorKind classifies a probe evaluation failure.
type ProbeErrorKind int

const (
	// ProbeErrTransient means the probe could not be evaluated this pass but
	// a later pass may succeed (a transport/DNS failure, a 5xx, a timeout).
	ProbeErrTransient ProbeErrorKind = iota
	// ProbeErrUnevaluatable means the probe can never be evaluated as
	// declared (an auth failure, a 4xx) -- the environment should fail
	// rather than hang once the consecutive-error threshold is reached.
	ProbeErrUnevaluatable
)

// ProbeError is the typed error the evaluator produces for a failed probe
// I/O. Kind drives ClassifyError; Message is safe to surface verbatim (never
// a credential -- see internal/storage/doc.go's no-logging rule).
type ProbeError struct {
	Kind    ProbeErrorKind
	Message string
}

func (e *ProbeError) Error() string { return e.Message }

// ClassifyError maps a probe evaluation failure to its consequence.
// unevaluatable=true means the error counts toward the consecutive-error
// threshold that eventually fails the environment; unevaluatable=false means
// the probe is merely pending this pass. reason is a short stable machine
// reason (always ReasonProbeFailed today -- the only allowlisted probe
// StepFailure reason); message is safe to surface verbatim. An unrecognized
// error type is treated as unevaluatable -- the fail-safe reading: an
// unclassified failure must never be silently treated as "still pending"
// forever.
func ClassifyError(err error) (unevaluatable bool, reason, message string) {
	if err == nil {
		// A nil error should never reach ClassifyError (the evaluator only
		// calls it on failure), but the fail-safe reading is the same as for
		// an unrecognized error: never silently treat a missing failure as
		// "still pending".
		return true, ReasonProbeFailed, ""
	}
	var pe *ProbeError
	if errors.As(err, &pe) {
		return pe.Kind == ProbeErrUnevaluatable, ReasonProbeFailed, pe.Message
	}
	return true, ReasonProbeFailed, err.Error()
}

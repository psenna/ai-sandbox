package api

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope is the uniform error shape returned by every non-2xx
// response, modeled on the K8s operator's sandboxctl.ErrorEnvelope (same
// shape, no field/allowed-values machinery: this API's error space is much
// smaller than sandboxctl's probe/exec surface).
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the payload of ErrorEnvelope.
type ErrorBody struct {
	// Code is a stable, machine-matchable string (see the Code* constants).
	// Code, not the HTTP status alone, is what a caller should switch on.
	Code string `json:"code"`
	// Message is a human-readable explanation. Never includes internal
	// resource names/IDs for a 500 -- see Handler.internalError.
	Message string `json:"message"`
	// Field names the request field the error concerns, e.g. "tail" on an
	// invalid_param. Empty when the error isn't about one specific field.
	Field string `json:"field,omitempty"`
}

// Known error codes.
const (
	CodeBadJSON          = "bad_json"
	CodeMissingField     = "missing_field"
	CodeInvalidParam     = "invalid_param"
	CodeNotFound         = "not_found"
	CodeAtCapacity       = "at_capacity"
	CodeNoAnthropicAuth  = "no_anthropic_auth"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal"
)

// writeJSON writes body as status with a JSON content type. Encode errors
// are not checked: by the time WriteHeader has run there is nothing left to
// do about a broken connection, and every body this package writes
// (store.Agent, a slice of them, ErrorEnvelope) is a plain, always-
// marshalable value.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes an ErrorEnvelope.
func writeError(w http.ResponseWriter, status int, code, message, field string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{Code: code, Message: message, Field: field}})
}

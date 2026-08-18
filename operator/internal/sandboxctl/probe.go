package sandboxctl

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// WaitProbe is the wire form of a wait declaration. Validated fail-closed
// against the allowlist below, then projected into v1alpha1.WaitForStatus.
// UNKNOWN KEYS ARE REJECTED, NEVER DROPPED: silently discarding a param the
// agent believed was honoured is exactly the failure mode this exists to
// prevent.
type WaitProbe struct {
	Type   string            `json:"type"`
	Reason string            `json:"reason"`
	Params map[string]string `json:"params,omitempty"`
}

const (
	maxReasonBytes = 512
	maxParamCount  = 16
	maxParamKey    = 64
	maxParamValue  = 1024
)

// paramSpec describes one permitted parameter for a probe type.
type paramSpec struct {
	required bool
	validate func(string) error
	doc      string
}

// paramSchema IS the allowlist. Its key set must equal the CRD's
// WaitForStatus.Type enum -- asserted by TestAllowlistMatchesCRDEnum.
var paramSchema = map[string]map[string]paramSpec{
	v1alpha1.WaitTypeGitProxyCheck: {
		"repo": {required: false, validate: validateRepo, doc: `"owner/name"; defaults to spec.repo`},
		"ref":  {required: true, validate: validateGitRef, doc: `git ref, e.g. "refs/heads/feat/x"`},
	},
	v1alpha1.WaitTypeHTTPGet: {
		"url":          {required: true, validate: validateHTTPURL, doc: `absolute http(s) URL, no userinfo`},
		"expectStatus": {required: false, validate: validateStatusCode, doc: `100-599; default 200`},
		"expectBody":   {required: false, validate: maxLen(256), doc: `substring the body must contain`},
	},
	v1alpha1.WaitTypeS3ObjectExists: {
		"key": {required: true, validate: validateObjectKey, doc: `object key relative to the environment's backend prefix; no leading "/", no ".."`},
	},
	v1alpha1.WaitTypeNotBefore: {
		"time":     {required: false, validate: validateRFC3339, doc: `RFC3339 timestamp`},
		"duration": {required: false, validate: validateDuration, doc: `Go duration, e.g. "30m"; max 24h`},
	},
}

// AllowedProbeTypes returns the sorted allowlist -- used both by
// error-envelope "allowed" fields and by the skill/doc generation.
func AllowedProbeTypes() []string {
	out := make([]string, 0, len(paramSchema))
	for k := range paramSchema {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValidationError is a structured, actionable rejection of a WaitProbe,
// mapped 1:1 onto the wire error envelope by handlers.go.
type ValidationError struct {
	Code    string
	Message string
	Field   string
	Allowed []string
}

func (e *ValidationError) Error() string { return e.Message }

func validationErr(code, field, format string, args ...any) *ValidationError {
	return &ValidationError{Code: code, Field: field, Message: fmt.Sprintf(format, args...)}
}

// Validate checks p against the allowlist, in order, first failure wins.
func (p WaitProbe) Validate() error {
	schema, ok := paramSchema[p.Type]
	if !ok {
		return &ValidationError{
			Code:    "unknown_probe_type",
			Field:   "type",
			Message: fmt.Sprintf("probe type %q is not allowlisted", p.Type),
			Allowed: AllowedProbeTypes(),
		}
	}

	if p.Reason == "" {
		return validationErr("missing_param", "reason", "reason must not be empty")
	}
	if len(p.Reason) > maxReasonBytes {
		return validationErr("invalid_param", "reason", "reason exceeds %d bytes", maxReasonBytes)
	}
	if hasControlChars(p.Reason) {
		return validationErr("invalid_param", "reason", "reason must not contain control characters")
	}

	if len(p.Params) > maxParamCount {
		return validationErr("invalid_param", "params", "at most %d params are permitted, got %d", maxParamCount, len(p.Params))
	}
	for k, v := range p.Params {
		if len(k) > maxParamKey {
			return validationErr("invalid_param", k, "param key exceeds %d bytes", maxParamKey)
		}
		if len(v) > maxParamValue {
			return validationErr("invalid_param", k, "param value exceeds %d bytes", maxParamValue)
		}
		if hasControlChars(v) {
			return validationErr("invalid_param", k, "param value must not contain control characters")
		}
	}

	// Unknown keys are REJECTED, never dropped.
	for k := range p.Params {
		if _, ok := schema[k]; !ok {
			return &ValidationError{
				Code:    "unknown_param",
				Field:   k,
				Message: fmt.Sprintf("param %q is not permitted for probe type %q", k, p.Type),
				Allowed: permittedParamDocs(schema),
			}
		}
	}

	for key, spec := range schema {
		v, present := p.Params[key]
		if !present {
			if spec.required {
				return validationErr("missing_param", key, "param %q is required for probe type %q (%s)", key, p.Type, spec.doc)
			}
			continue
		}
		if spec.validate != nil {
			if err := spec.validate(v); err != nil {
				return validationErr("invalid_param", key, "param %q invalid: %v", key, err)
			}
		}
	}

	if p.Type == v1alpha1.WaitTypeNotBefore {
		_, hasTime := p.Params["time"]
		_, hasDuration := p.Params["duration"]
		if hasTime == hasDuration {
			return validationErr("invalid_param", "time/duration", "NotBefore requires exactly one of \"time\" or \"duration\"")
		}
	}

	return nil
}

func permittedParamDocs(schema map[string]paramSpec) []string {
	out := make([]string, 0, len(schema))
	for k := range schema {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func maxLen(n int) func(string) error {
	return func(s string) error {
		if len(s) > n {
			return fmt.Errorf("exceeds %d bytes", n)
		}
		return nil
	}
}

func validateRepo(s string) error {
	if !strings.Contains(s, "/") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return fmt.Errorf(`must be "owner/name"`)
	}
	return nil
}

func validateGitRef(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(s, " \t\n~^:?*[\\") || strings.Contains(s, "..") {
		return fmt.Errorf("not a valid git ref")
	}
	return nil
}

func validateHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must have a host")
	}
	if u.User != nil {
		return fmt.Errorf("must not contain userinfo/credentials")
	}
	return nil
}

func validateStatusCode(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a valid integer")
	}
	if n < 100 || n > 599 {
		return fmt.Errorf("must be between 100 and 599")
	}
	return nil
}

func validateObjectKey(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.HasPrefix(s, "/") {
		return fmt.Errorf("must not have a leading slash")
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf(`must not contain ".."`)
	}
	return nil
}

func validateRFC3339(s string) error {
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return fmt.Errorf("not a valid RFC3339 timestamp: %w", err)
	}
	return nil
}

func validateDuration(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("not a valid Go duration: %w", err)
	}
	if d <= 0 {
		return fmt.Errorf("must be positive")
	}
	if d > 24*time.Hour {
		return fmt.Errorf("must be at most 24h")
	}
	return nil
}

package sandboxctl

import (
	"errors"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func mustValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not a *ValidationError: %v (%T)", err, err)
	}
	return ve
}

func TestWaitProbe_Validate_AllowlistedTypes(t *testing.T) {
	cases := []struct {
		name  string
		probe WaitProbe
	}{
		{"GitProxyCheck minimal", WaitProbe{Type: v1alpha1.WaitTypeGitProxyCheck, Reason: "ci", Params: map[string]string{"ref": "refs/heads/main"}}},
		{"GitProxyCheck full", WaitProbe{Type: v1alpha1.WaitTypeGitProxyCheck, Reason: "ci", Params: map[string]string{"ref": "refs/heads/main", "repo": "psenna/ai-sandbox"}}},
		{"HTTPGet minimal", WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "wait for deploy", Params: map[string]string{"url": "https://example.com/health"}}},
		{"HTTPGet full", WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "wait for deploy", Params: map[string]string{"url": "https://example.com/health", "expectStatus": "204", "expectBody": "ok"}}},
		{"S3ObjectExists minimal", WaitProbe{Type: v1alpha1.WaitTypeS3ObjectExists, Reason: "wait for artifact", Params: map[string]string{"key": "builds/1.tar.zst"}}},
		{"NotBefore time", WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "cooldown", Params: map[string]string{"time": "2026-01-01T00:00:00Z"}}},
		{"NotBefore duration", WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "cooldown", Params: map[string]string{"duration": "30m"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.probe.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestWaitProbe_Validate_UnknownType(t *testing.T) {
	p := WaitProbe{Type: "SolarEclipse", Reason: "nope"}
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate(): expected error, got nil")
	}
	ve := mustValidationError(t, err)
	if ve.Code != CodeUnknownProbeType || ve.Field != "type" {
		t.Errorf("code=%q field=%q, want unknown_probe_type/type", ve.Code, ve.Field)
	}
	want := AllowedProbeTypes()
	if len(ve.Allowed) != len(want) {
		t.Errorf("Allowed = %v, want %v", ve.Allowed, want)
	}
	for i := range want {
		if ve.Allowed[i] != want[i] {
			t.Errorf("Allowed = %v, want %v", ve.Allowed, want)
			break
		}
	}
}

func TestWaitProbe_Validate_UnknownParam(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "x", Params: map[string]string{"url": "https://example.com", "nope": "1"}}
	err := p.Validate()
	ve := mustValidationError(t, err)
	if ve.Code != CodeUnknownParam || ve.Field != "nope" {
		t.Errorf("code=%q field=%q, want unknown_param/nope", ve.Code, ve.Field)
	}
}

func TestWaitProbe_Validate_MissingRequiredParam(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "x"}
	err := p.Validate()
	ve := mustValidationError(t, err)
	if ve.Code != CodeMissingParam || ve.Field != "url" {
		t.Errorf("code=%q field=%q, want missing_param/url", ve.Code, ve.Field)
	}
}

func TestWaitProbe_Validate_PerTypeInvalidParam(t *testing.T) {
	cases := []struct {
		name  string
		probe WaitProbe
		field string
	}{
		{"GitProxyCheck bad repo", WaitProbe{Type: v1alpha1.WaitTypeGitProxyCheck, Reason: "x", Params: map[string]string{"ref": "refs/heads/main", "repo": "not-a-repo"}}, "repo"},
		{"GitProxyCheck bad ref", WaitProbe{Type: v1alpha1.WaitTypeGitProxyCheck, Reason: "x", Params: map[string]string{"ref": "refs/heads/../evil"}}, "ref"},
		{"HTTPGet bad scheme", WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "x", Params: map[string]string{"url": "ftp://example.com"}}, "url"},
		{"HTTPGet bad status", WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "x", Params: map[string]string{"url": "https://example.com", "expectStatus": "9999"}}, "expectStatus"},
		{"S3ObjectExists leading slash", WaitProbe{Type: v1alpha1.WaitTypeS3ObjectExists, Reason: "x", Params: map[string]string{"key": "/leading"}}, "key"},
		{"S3ObjectExists traversal", WaitProbe{Type: v1alpha1.WaitTypeS3ObjectExists, Reason: "x", Params: map[string]string{"key": "a/../b"}}, "key"},
		{"NotBefore bad time", WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"time": "not-a-time"}}, "time"},
		{"NotBefore bad duration", WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "not-a-duration"}}, "duration"},
		{"NotBefore too-long duration", WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": "48h"}}, "duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.probe.Validate()
			ve := mustValidationError(t, err)
			if ve.Code != CodeInvalidParam || ve.Field != tc.field {
				t.Errorf("code=%q field=%q, want invalid_param/%s", ve.Code, ve.Field, tc.field)
			}
		})
	}
}

func TestWaitProbe_Validate_HTTPGetRejectsUserinfo(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeHTTPGet, Reason: "x", Params: map[string]string{"url": "https://user:pass@example.com/"}} //nolint:gosec // G101: deliberately fake test fixture value, not a real credential
	err := p.Validate()
	ve := mustValidationError(t, err)
	if ve.Code != CodeInvalidParam || ve.Field != "url" {
		t.Errorf("code=%q field=%q, want invalid_param/url", ve.Code, ve.Field)
	}
}

func TestWaitProbe_Validate_NotBeforeRequiresExactlyOne(t *testing.T) {
	t.Run("neither", func(t *testing.T) {
		p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x"}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error when neither time nor duration is set")
		}
	})
	t.Run("both", func(t *testing.T) {
		p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"time": "2026-01-01T00:00:00Z", "duration": "1h"}}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error when both time and duration are set")
		}
	})
}

func TestWaitProbe_Validate_ReasonRequired(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Params: map[string]string{"duration": "1h"}}
	err := p.Validate()
	ve := mustValidationError(t, err)
	if ve.Field != "reason" {
		t.Errorf("field = %q, want reason", ve.Field)
	}
}

func TestWaitProbe_Validate_ReasonTooLong(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: strings.Repeat("x", maxReasonBytes+1), Params: map[string]string{"duration": "1h"}}
	err := p.Validate()
	ve := mustValidationError(t, err)
	if ve.Field != "reason" {
		t.Errorf("field = %q, want reason", ve.Field)
	}
}

func TestWaitProbe_Validate_ReasonControlChars(t *testing.T) {
	p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "bad\x00reason", Params: map[string]string{"duration": "1h"}}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for control characters in reason")
	}
}

func TestWaitProbe_Validate_TooManyParams(t *testing.T) {
	params := map[string]string{"duration": "1h"}
	for i := 0; i < maxParamCount; i++ {
		params[strings.Repeat("k", 1)+string(rune('a'+i))] = "v"
	}
	p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: params}
	if len(p.Params) <= maxParamCount {
		t.Fatalf("test setup: need > %d params, got %d", maxParamCount, len(p.Params))
	}
	if err := p.Validate(); err == nil {
		t.Fatal("expected error for too many params")
	}
}

func TestWaitProbe_Validate_OversizeKeyValue(t *testing.T) {
	t.Run("key too long", func(t *testing.T) {
		p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{strings.Repeat("k", maxParamKey+1): "v"}}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for oversize key")
		}
	})
	t.Run("value too long", func(t *testing.T) {
		p := WaitProbe{Type: v1alpha1.WaitTypeNotBefore, Reason: "x", Params: map[string]string{"duration": strings.Repeat("1", maxParamValue+1)}}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for oversize value")
		}
	})
}

// TestAllowlistMatchesCRDEnum parses the real, generated CRD YAML and
// asserts set-equality between status.waitFor.type.enum and paramSchema's
// keys, making it structurally impossible to add a wire type without adding
// the CRD enum (and vice versa).
func TestAllowlistMatchesCRDEnum(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/sandbox.psenna.dev_sandboxenvironments.yaml")
	if err != nil {
		t.Fatalf("reading CRD yaml: %v", err)
	}

	var doc struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Status struct {
								Properties struct {
									WaitFor struct {
										Properties struct {
											Type struct {
												Enum []string `json:"enum"`
											} `json:"type"`
										} `json:"properties"`
									} `json:"waitFor"`
								} `json:"properties"`
							} `json:"status"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshaling CRD yaml: %v", err)
	}
	if len(doc.Spec.Versions) == 0 {
		t.Fatal("CRD yaml has no versions")
	}

	crdEnum := map[string]bool{}
	for _, v := range doc.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Status.Properties.WaitFor.Properties.Type.Enum {
		crdEnum[v] = true
	}
	if len(crdEnum) == 0 {
		t.Fatal("could not find status.waitFor.type.enum in the CRD yaml -- test's yaml path may be stale")
	}

	schemaTypes := map[string]bool{}
	for k := range paramSchema {
		schemaTypes[k] = true
	}

	if len(crdEnum) != len(schemaTypes) {
		t.Errorf("CRD enum has %d members %v, paramSchema has %d members %v", len(crdEnum), crdEnum, len(schemaTypes), schemaTypes)
	}
	for k := range crdEnum {
		if !schemaTypes[k] {
			t.Errorf("CRD enum member %q has no paramSchema entry", k)
		}
	}
	for k := range schemaTypes {
		if !crdEnum[k] {
			t.Errorf("paramSchema member %q is not in the CRD enum", k)
		}
	}
}

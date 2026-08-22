package crdref

import (
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const (
	classCRDPath = "../../config/crd/bases/sandbox.psenna.dev_sandboxclasses.yaml"
	envCRDPath   = "../../config/crd/bases/sandbox.psenna.dev_sandboxenvironments.yaml"
)

func loadCRD(t *testing.T, path string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is always classCRDPath or envCRDPath, both hardcoded constants
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("unmarshaling %s: %v", path, err)
	}
	return crd
}

func loadRealCRDs(t *testing.T) []*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	return []*apiextensionsv1.CustomResourceDefinition{
		loadCRD(t, classCRDPath),
		loadCRD(t, envCRDPath),
	}
}

// TestRender_Deterministic calls Render twice on the same loaded CRDs and
// asserts byte equality, mirroring internal/render/determinism_test.go.
func TestRender_Deterministic(t *testing.T) {
	crds := loadRealCRDs(t)

	first, err := Render(crds)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for i := 0; i < 10; i++ {
		got, err := Render(crds)
		if err != nil {
			t.Fatalf("Render iteration %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("Render is not deterministic at iteration %d", i)
		}
	}
}

// TestRender_RealCRDs loads both real CRD files and asserts the rendered
// output contains a hand-picked set of load-bearing strings -- facts about
// the actual schema that must never silently regress.
func TestRender_RealCRDs(t *testing.T) {
	crds := loadRealCRDs(t)

	out, err := Render(crds)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		"`spec.engine.type`",
		"enum: rootless-podman, none",
		"`\"rootless-podman\"`",
		"`spec.storage.backend.type`",
		"enum: s3, pvc",
		"`status.phase`",
		"enum: Pending, Ready, Running, Freezing, Waiting, Restoring, Done, Failed",
		"classRef is immutable",
		"task requires at least one of prompt or issueRef",
		"Freezes",
		"Wakes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output does not contain %q", want)
		}
	}
}

// TestRender_EscapesPipes asserts a "|" in a property description is
// escaped to "\|" and that the row it appears in still has the expected
// column count (i.e. the escape actually prevented the table from
// breaking).
func TestRender_EscapesPipes(t *testing.T) {
	crd := crdWithSpecProperties(map[string]apiextensionsv1.JSONSchemaProps{
		"mode": {
			Type:        "string",
			Description: "One of a|b|c.",
		},
	})

	out, err := Render([]*apiextensionsv1.CustomResourceDefinition{crd})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "spec.mode") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("rendered output has no row for spec.mode:\n%s", out)
	}
	if !strings.Contains(row, `One of a\|b\|c.`) {
		t.Errorf("row does not contain escaped pipes: %q", row)
	}
	// 6 columns -> 7 pipe separators in "| a | b | c | d | e | f |". Strip
	// escaped "\|" sequences first so the pipes INSIDE the description
	// (which must stay escaped, not be counted as extra separators) don't
	// throw off the count -- that's the whole point of escaping them.
	stripped := strings.ReplaceAll(row, `\|`, "")
	if got := strings.Count(stripped, "|"); got != 7 {
		t.Errorf("row has %d unescaped '|' separators, want 7 (6 columns): %q", got, row)
	}
}

// TestRender_SortsProperties asserts properties are emitted in sorted
// (not map-iteration) order at every level -- the single highest-risk
// correctness detail in the generator, per the implementation plan.
func TestRender_SortsProperties(t *testing.T) {
	crd := crdWithSpecProperties(map[string]apiextensionsv1.JSONSchemaProps{
		"zebra": {Type: "string", Description: "z"},
		"alpha": {Type: "string", Description: "a"},
		"mike":  {Type: "string", Description: "m"},
	})

	out, err := Render([]*apiextensionsv1.CustomResourceDefinition{crd})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	idxAlpha := strings.Index(out, "spec.alpha")
	idxMike := strings.Index(out, "spec.mike")
	idxZebra := strings.Index(out, "spec.zebra")
	if idxAlpha < 0 || idxMike < 0 || idxZebra < 0 {
		t.Fatalf("rendered output missing one of the synthetic fields:\n%s", out)
	}
	if idxAlpha >= idxMike || idxMike >= idxZebra {
		t.Errorf("properties not emitted in sorted order: alpha=%d mike=%d zebra=%d", idxAlpha, idxMike, idxZebra)
	}
}

// crdWithSpecProperties builds a minimal, valid single-version CRD whose
// .spec.properties are exactly specProps, for the synthetic renderer
// tests above.
func crdWithSpecProperties(specProps map[string]apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.test",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:   "Widget",
				Plural: "widgets",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:        "object",
							Description: "Widget is a synthetic fixture CRD for crdref tests.",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:       "object",
									Properties: specProps,
								},
								"status": {
									Type: "object",
								},
							},
						},
					},
				},
			},
		},
	}
}

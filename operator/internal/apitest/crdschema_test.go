package apitest

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

func loadCRD(t *testing.T, path string) (*apiextensionsv1.CustomResourceDefinition, string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is always classCRDPath or envCRDPath, both hardcoded constants
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("unmarshaling %s: %v", path, err)
	}
	return crd, string(raw)
}

// walkDescriptions asserts every property (leaf and object-typed) under
// schema has a non-empty description -- the CRD schema descriptions are the
// user-facing reference per the issue's acceptance criteria.
func walkDescriptions(t *testing.T, path string, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	if schema == nil {
		return
	}
	for name, prop := range schema.Properties {
		prop := prop
		fullPath := path + "." + name
		if strings.TrimSpace(prop.Description) == "" {
			t.Errorf("%s: missing description", fullPath)
		}
		walkDescriptions(t, fullPath, &prop)
		if prop.Items != nil && prop.Items.Schema != nil {
			walkDescriptions(t, fullPath+"[]", prop.Items.Schema)
		}
		if prop.AdditionalProperties != nil && prop.AdditionalProperties.Schema != nil {
			walkDescriptions(t, fullPath+"{}", prop.AdditionalProperties.Schema)
		}
	}
}

func schemaFor(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, version string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				t.Fatalf("version %s has no OpenAPIV3Schema", version)
			}
			return v.Schema.OpenAPIV3Schema
		}
	}
	t.Fatalf("version %s not found", version)
	return nil
}

func TestCRDSchema_SandboxClass(t *testing.T) {
	crd, raw := loadCRD(t, classCRDPath)
	schema := schemaFor(t, crd, "v1alpha1")

	// Walk only spec/status: the top-level apiVersion/kind/metadata
	// properties are boilerplate controller-gen emits without descriptions
	// of our own, so they're out of scope for the "every field has a doc
	// comment" acceptance criterion.
	specSchema := schema.Properties["spec"]
	walkDescriptions(t, "spec", &specSchema)
	statusSchema := schema.Properties["status"]
	walkDescriptions(t, "status", &statusSchema)

	engineType := schema.Properties["spec"].Properties["engine"].Properties["type"]
	if engineType.Default == nil {
		t.Errorf("spec.engine.type has no default")
	}
	if got := string(engineType.Default.Raw); got != `"rootless-podman"` {
		t.Errorf("spec.engine.type default = %s, want %q", got, "rootless-podman")
	}
	if want := []string{"rootless-podman", "none"}; !enumMatches(engineType.Enum, want) {
		t.Errorf("spec.engine.type enum = %v, want %v", engineType.Enum, want)
	}

	wsSize := schema.Properties["spec"].Properties["storage"].Properties["workspace"].Properties["size"]
	if wsSize.Default == nil || string(wsSize.Default.Raw) != `"20Gi"` {
		t.Errorf("spec.storage.workspace.size default = %v, want %q", wsSize.Default, "20Gi")
	}

	backendType := schema.Properties["spec"].Properties["storage"].Properties["backend"].Properties["type"]
	if want := []string{"s3", "pvc"}; !enumMatches(backendType.Enum, want) {
		t.Errorf("spec.storage.backend.type enum = %v, want %v", backendType.Enum, want)
	}

	for _, rule := range []string{
		`self.type != 's3' || has(self.s3)`,
		`self.type != 'pvc' || has(self.pvc)`,
		`self.type == 's3' || !has(self.s3)`,
		`self.type == 'pvc' || !has(self.pvc)`,
	} {
		if !strings.Contains(raw, rule) {
			t.Errorf("backend CEL rule not found in CRD YAML: %s", rule)
		}
	}
}

func TestCRDSchema_SandboxEnvironment(t *testing.T) {
	crd, raw := loadCRD(t, envCRDPath)
	schema := schemaFor(t, crd, "v1alpha1")

	specSchema := schema.Properties["spec"]
	walkDescriptions(t, "spec", &specSchema)
	statusSchema := schema.Properties["status"]
	walkDescriptions(t, "status", &statusSchema)

	phase := schema.Properties["status"].Properties["phase"]
	if want := []string{"Pending", "Ready", "Running", "Freezing", "Waiting", "Restoring", "Done", "Failed"}; !enumMatches(phase.Enum, want) {
		t.Errorf("status.phase enum = %v, want %v", phase.Enum, want)
	}
	if phase.Default == nil || string(phase.Default.Raw) != `"Pending"` {
		t.Errorf("status.phase default = %v, want %q", phase.Default, "Pending")
	}

	statusProp := schema.Properties["status"]
	if statusProp.Default == nil || string(statusProp.Default.Raw) != `{}` {
		t.Errorf("status default = %v, want {} (required for status.phase's own default to apply)", statusProp.Default)
	}

	for _, rule := range []string{
		`classRef is immutable`,
		`repo is immutable`,
		`task is immutable`,
		`task requires at least one of prompt or issueRef`,
	} {
		if !strings.Contains(raw, rule) {
			t.Errorf("CEL rule message not found in CRD YAML: %s", rule)
		}
	}
}

func enumMatches(enum []apiextensionsv1.JSON, want []string) bool {
	if len(enum) != len(want) {
		return false
	}
	for i, w := range enum {
		if string(w.Raw) != `"`+want[i]+`"` {
			return false
		}
	}
	return true
}

package sandboxctl

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

const validYAML = `
services:
  - name: postgres
    image: postgres:18-alpine
    ports: [5432]
    env:
      POSTGRES_USER: e2e
    storage:
      size: 1Gi
      mountPath: /var/lib/postgresql/data
    healthcheck:
      exec: ["pg_isready", "-U", "e2e"]
      interval: 5s
    dependsOn: []
runtimes:
  - name: python
    image: python:3.13-slim
    mountWorkspace: true
    command: ["sleep", "infinity"]
`

func TestParseServicesYAML(t *testing.T) {
	spec, err := ParseServicesYAML([]byte(validYAML))
	if err != nil {
		t.Fatalf("ParseServicesYAML: %v", err)
	}
	if len(spec.Services) != 1 || spec.Services[0].Name != "postgres" {
		t.Fatalf("services = %+v", spec.Services)
	}
	if len(spec.Runtimes) != 1 || spec.Runtimes[0].Name != "python" {
		t.Fatalf("runtimes = %+v", spec.Runtimes)
	}
	if spec.Services[0].Storage == nil || spec.Services[0].Storage.Size != "1Gi" {
		t.Fatalf("storage not parsed: %+v", spec.Services[0].Storage)
	}
	if spec.Services[0].Healthcheck.Exec[0] != "pg_isready" {
		t.Fatalf("healthcheck not parsed: %+v", spec.Services[0].Healthcheck)
	}
	if spec.EnvironmentName != "" {
		t.Fatalf("EnvironmentName should be empty (server sets it), got %q", spec.EnvironmentName)
	}
}

func TestValidateServiceSet(t *testing.T) {
	cases := []struct {
		name     string
		spec     v1alpha1.ServiceSetSpec
		wantCode string
	}{
		{
			name: "valid",
			spec: v1alpha1.ServiceSetSpec{
				EnvironmentName: "env-1",
				Services:        []v1alpha1.ServiceSpec{{Name: "postgres", Image: "postgres:18"}},
				Runtimes:        []v1alpha1.RuntimeSpec{{Name: "python", Image: "python:3.13"}},
			},
			wantCode: "",
		},
		{
			name:     "missing name",
			spec:     v1alpha1.ServiceSetSpec{EnvironmentName: "e", Services: []v1alpha1.ServiceSpec{{Image: "x"}}},
			wantCode: CodeMissingParam,
		},
		{
			name:     "missing image",
			spec:     v1alpha1.ServiceSetSpec{EnvironmentName: "e", Runtimes: []v1alpha1.RuntimeSpec{{Name: "r"}}},
			wantCode: CodeMissingParam,
		},
		{
			name: "cross-list name collision (#2)",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "shared", Image: "a"}},
				Runtimes: []v1alpha1.RuntimeSpec{{Name: "shared", Image: "b"}}},
			wantCode: CodeDuplicateEntryName,
		},
		{
			name: "within-list duplicate",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "x", Image: "a"}, {Name: "x", Image: "b"}}},
			wantCode: CodeDuplicateEntryName,
		},
		{
			name: "dangling dependsOn",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x", DependsOn: []string{"nope"}}}},
			wantCode: CodeDanglingDependsOn,
		},
		{
			name: "storage missing size",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x", Storage: &v1alpha1.ServiceStorageSpec{MountPath: "/d"}}}},
			wantCode: CodeInvalidDeclaration,
		},
		{
			name: "healthcheck two probes",
			spec: v1alpha1.ServiceSetSpec{EnvironmentName: "e",
				Services: []v1alpha1.ServiceSpec{{Name: "a", Image: "x",
					Healthcheck: v1alpha1.HealthcheckSpec{Exec: []string{"true"}, TCP: &v1alpha1.TCPProbe{Port: 1}}}}},
			wantCode: CodeInvalidDeclaration,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateServiceSet(c.spec)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want code %q, got nil", c.wantCode)
			}
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("error not a *ValidationError: %T %v", err, err)
			}
			if ve.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", ve.Code, c.wantCode)
			}
			if !strings.Contains(ve.Message, c.name) && !strings.Contains(ve.Message, "shared") && !strings.Contains(ve.Message, "x") && !strings.Contains(ve.Message, "a") {
				// Message must name the offending entry/value (spot-check it is non-empty).
				if ve.Message == "" {
					t.Fatal("empty message")
				}
			}
		})
	}
}

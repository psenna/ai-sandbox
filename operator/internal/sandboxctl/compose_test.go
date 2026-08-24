package sandboxctl

import (
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	sigsyaml "sigs.k8s.io/yaml"
)

func TestCompose(t *testing.T) {
	spec := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{
			Name: "postgres", Image: "postgres:18-alpine",
			Ports:       []int32{5432},
			Env:         map[string]string{"POSTGRES_USER": "e2e"},
			Storage:     &v1alpha1.ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
			Healthcheck: v1alpha1.HealthcheckSpec{Exec: []string{"pg_isready", "-U", "e2e"}, Interval: "5s"},
		}},
		Runtimes: []v1alpha1.RuntimeSpec{{Name: "python", Image: "python:3.13-slim", MountWorkspace: ptr(true)}},
	}
	out, err := Compose(spec)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	var doc map[string]any
	if err := sigsyaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", err, out)
	}
	services := doc["services"].(map[string]any)

	pg := services["postgres"].(map[string]any)
	if pg["image"] != "postgres:18-alpine" {
		t.Fatalf("postgres image = %v", pg["image"])
	}
	if pg["restart"] != "always" {
		t.Fatalf("postgres restart = %v", pg["restart"])
	}
	ports := pg["ports"].([]any)
	if len(ports) != 1 || ports[0] != "5432:5432" {
		t.Fatalf("postgres ports = %v", ports)
	}
	env := pg["environment"].(map[string]any)
	if env["POSTGRES_USER"] != "e2e" {
		t.Fatalf("postgres env = %v", env)
	}
	vols := pg["volumes"].([]any)
	if vols[0] != "postgres-data:/var/lib/postgresql/data" {
		t.Fatalf("postgres volumes = %v", vols)
	}
	hc := pg["healthcheck"].(map[string]any)
	test := hc["test"].([]any)
	if test[0] != "CMD" || test[1] != "pg_isready" {
		t.Fatalf("postgres healthcheck.test = %v", test)
	}
	if hc["interval"] != "5s" {
		t.Fatalf("postgres healthcheck.interval = %v", hc["interval"])
	}

	py := services["python"].(map[string]any)
	if py["image"] != "python:3.13-slim" {
		t.Fatalf("python image = %v", py["image"])
	}
	cmd := py["command"].([]any)
	if cmd[0] != "sleep" || cmd[1] != "infinity" {
		t.Fatalf("python command = %v (default sleep infinity expected)", cmd)
	}
	pyVols := py["volumes"].([]any)
	if pyVols[0] != "workspace:/workspace" {
		t.Fatalf("python volumes = %v", pyVols)
	}

	volumes := doc["volumes"].(map[string]any)
	if _, ok := volumes["postgres-data"]; !ok {
		t.Fatalf("top-level volumes missing postgres-data: %v", volumes)
	}
	if _, ok := volumes["workspace"]; !ok {
		t.Fatalf("top-level volumes missing workspace: %v", volumes)
	}
}

func TestComposeExposeAndDeterminism(t *testing.T) {
	spec := v1alpha1.ServiceSetSpec{
		Services: []v1alpha1.ServiceSpec{{
			Name: "web", Image: "nginx", Ports: []int32{80, 81}, Expose: ptr[int32](18080),
		}},
	}
	out1, err := Compose(spec)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	out2, _ := Compose(spec)
	if string(out1) != string(out2) {
		t.Fatalf("Compose not deterministic:\n%s\n---\n%s", out1, out2)
	}
	var doc map[string]any
	_ = sigsyaml.Unmarshal(out1, &doc)
	ports := doc["services"].(map[string]any)["web"].(map[string]any)["ports"].([]any)
	if ports[0] != "18080:80" || ports[1] != "81:81" {
		t.Fatalf("expose ports = %v (want [18080:80, 81:81])", ports)
	}
}

// ptr is a tiny test helper for literals.
func ptr[T any](v T) *T { return &v }

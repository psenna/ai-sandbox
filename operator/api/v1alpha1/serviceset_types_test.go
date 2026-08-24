package v1alpha1

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestServiceSet_RoundTripsJSON(t *testing.T) {
	mount := true
	ss := ServiceSet{
		Spec: ServiceSetSpec{
			EnvironmentName: "env-1",
			Services: []ServiceSpec{{
				Name:  "postgres",
				Image: "postgres:18-alpine",
				Ports: []int32{5432},
				Env:   map[string]string{"POSTGRES_USER": "e2e"},
				Storage: &ServiceStorageSpec{Size: "1Gi", MountPath: "/var/lib/postgresql/data"},
				Healthcheck: HealthcheckSpec{Exec: []string{"pg_isready"}, Interval: "5s"},
			}},
			Runtimes: []RuntimeSpec{{
				Name:           "python",
				Image:          "python:3.13-slim",
				MountWorkspace: &mount,
				Command:        []string{"sleep", "infinity"},
			}},
		},
	}
	b, err := json.Marshal(ss)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ServiceSet
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Spec.Services[0].Name != "postgres" || got.Spec.Runtimes[0].Image != "python:3.13-slim" {
		t.Fatalf("round-trip lost fields: %+v", got.Spec)
	}
}

// TestServiceSet_RegisteredInScheme proves the init() block wired it in so
// AddToScheme picks it up (manager.go:Scheme needs no change).
func TestServiceSet_RegisteredInScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if !scheme.Recognizes(GroupVersion.WithKind("ServiceSet")) {
		t.Fatal("ServiceSet not registered; check the init() in serviceset_types.go")
	}
}
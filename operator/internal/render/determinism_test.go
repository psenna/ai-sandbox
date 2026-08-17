package render

import (
	"testing"
)

// TestRender_Deterministic calls Render with identical Inputs 100 times and
// asserts every result marshals to the byte-identical output as the first,
// catching accidental Go map-iteration-order leaking into the rendered
// objects.
func TestRender_Deterministic(t *testing.T) {
	in := Inputs{
		Env:         baseEnv("determinism-env"),
		Class:       fullClass(),
		Credentials: Credentials{GitProxyToken: "fake-token-determinism"},
		ClusterID:   "test-cluster",
	}

	first, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := marshalAll(t, first)

	for i := 0; i < 100; i++ {
		got, err := Render(in)
		if err != nil {
			t.Fatalf("Render iteration %d: %v", i, err)
		}
		gotBytes := marshalAll(t, got)
		if want != gotBytes {
			t.Fatalf("Render is not deterministic at iteration %d", i)
		}
	}
}

func marshalAll(t *testing.T, objs Objects) string {
	t.Helper()
	var b []byte
	b = append(b, marshalForGolden(t, objs.ServiceAccount)...)
	b = append(b, marshalForGolden(t, objs.Role)...)
	b = append(b, marshalForGolden(t, objs.RoleBinding)...)
	b = append(b, marshalForGolden(t, objs.PVC)...)
	b = append(b, marshalForGolden(t, objs.ConfigMap)...)
	b = append(b, marshalForGolden(t, objs.Secret)...)
	return string(b)
}

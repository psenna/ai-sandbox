package sandboxctl

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestNoopFreezeHook_ReturnsNilAndLogsNoBackendConfigured(t *testing.T) {
	var buf bytes.Buffer
	log := logr.FromSlogHandler(slog.NewJSONHandler(&buf, nil))
	hook := NewNoopFreezeHook(log)

	snap := Snapshot{
		Environment: EnvironmentRef{Name: "env-a", Namespace: "ns-a"},
		Phase:       v1alpha1.PhaseFreezing,
	}
	if err := hook.Freeze(context.Background(), snap); err != nil {
		t.Fatalf("Freeze() = %v, want nil", err)
	}

	out := buf.String()
	if !strings.Contains(out, "env-a") {
		t.Errorf("log output does not mention the environment name: %s", out)
	}
	if !strings.Contains(out, "no snapshot backend configured") {
		t.Errorf("log output does not explain that no snapshot backend is configured: %s", out)
	}
}

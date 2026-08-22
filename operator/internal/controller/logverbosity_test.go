package controller

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// jsonLogCapture is logCapture's JSON-rendering sibling: funcr's default
// LogInfoLevel option ("level") renders every Info line's V-value as a
// "level" key, so a JSON line can be parsed back into structured key/value
// pairs -- exactly what asserting "which V-level did this line log at, and
// with which keys" needs. Error lines carry no "level" key at all (funcr's
// FormatError never adds one), which is itself useful: an Error line can
// never be mistaken for an Info line by this capture.
type jsonLogCapture struct {
	mu    sync.Mutex
	lines []map[string]any
}

func newJSONLogCapture() (*jsonLogCapture, logr.Logger) {
	c := &jsonLogCapture{}
	logger := funcr.NewJSON(func(obj string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var m map[string]any
		if err := json.Unmarshal([]byte(obj), &m); err == nil {
			c.lines = append(c.lines, m)
		}
	}, funcr.Options{Verbosity: 10})
	return c, logger
}

// linesAtLevel returns every captured Info line whose "level" key equals
// level. A missing "level" key (an Error line, or a line from a logger
// namespace that never went through FormatInfo) never matches any level.
func (c *jsonLogCapture) linesAtLevel(level float64) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, m := range c.lines {
		if lv, ok := m["level"].(float64); ok && lv == level {
			out = append(out, m)
		}
	}
	return out
}

// TestLogVerbosity_SettledReconcile_NoV0_BoundedConsistentlyKeyedV1 drives a
// SandboxEnvironment through Pending -> Ready across several reconciles
// (the last few of which are settled: nothing changes), asserting:
//  1. zero V(0)/Info lines are ever emitted along this path -- #33's
//     logging convention adds no new V(0) output, and the Reconcile call
//     graph (unlike retentiongc.go's deliberately-untouched audit lines,
//     which this test never exercises) has none to begin with.
//  2. the V(1) "phase transition" line events.go's observeTransition adds
//     fires exactly once per actual phase change (bounded: it must NOT
//     fire again on the settled reconciles that follow), and every
//     occurrence carries the same two keys (phase, previousPhase) with
//     non-empty string values.
func TestLogVerbosity_SettledReconcile_NoV0_BoundedConsistentlyKeyedV1(t *testing.T) {
	mustCreateClass(t)
	env := mustCreateEnv(t, "log-verbosity-settled")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	capture, logger := newJSONLogCapture()
	capturedCtx := logf.IntoContext(ctx, logger)

	r := newResourceReconciler(t, newFakeClock(fixedStart))

	// Reconcile enough times to settle: Pending->Ready (resources become
	// ready), then several more idempotent passes once queued (no slot
	// scheduler is running in this unit test, so it self-loops in Ready).
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(capturedCtx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile #%d: %v", i, err)
		}
	}

	if lines := capture.linesAtLevel(0); len(lines) != 0 {
		t.Errorf("captured %d V(0) Info line(s) along the Reconcile path, want 0: %+v", len(lines), lines)
	}

	transitions := filterByMsg(capture.linesAtLevel(1), "phase transition")
	if len(transitions) != 1 {
		t.Fatalf("captured %d 'phase transition' V(1) line(s) across 5 reconciles of a settling-then-settled environment, want exactly 1 (Pending->Ready): %+v", len(transitions), transitions)
	}
	got := transitions[0]
	phase, _ := got[LogKeyPhase].(string)
	prevPhase, _ := got["previousPhase"].(string)
	if phase != string(sandboxv1alpha1.PhaseReady) {
		t.Errorf("phase transition line: phase = %q, want %q", phase, sandboxv1alpha1.PhaseReady)
	}
	if prevPhase != string(sandboxv1alpha1.PhasePending) {
		t.Errorf("phase transition line: previousPhase = %q, want %q", prevPhase, sandboxv1alpha1.PhasePending)
	}

	final := getEnv(t, key)
	if final.Status.Phase != sandboxv1alpha1.PhaseReady {
		t.Fatalf("setup check: final phase = %s, want Ready (test assumes this settles there with no slot scheduler running)", final.Status.Phase)
	}
}

func filterByMsg(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if m, ok := l["msg"].(string); ok && strings.Contains(m, msg) {
			out = append(out, l)
		}
	}
	return out
}

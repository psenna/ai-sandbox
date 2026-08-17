package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

func TestEngineSecurityCondition(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}

	t.Run("nil class", func(t *testing.T) {
		c := engineSecurityCondition(env, nil, now)
		if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
			t.Errorf("nil class: got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
		}
	})

	t.Run("none engine: no relaxation", func(t *testing.T) {
		class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeNone}}}
		c := engineSecurityCondition(env, class, now)
		if c.Status != metav1.ConditionFalse || c.Reason != ReasonNoRelaxation {
			t.Errorf("none: got Status=%s Reason=%s, want False/%s", c.Status, c.Reason, ReasonNoRelaxation)
		}
	})

	t.Run("unimplemented engine", func(t *testing.T) {
		class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeRootlessPodman}}}
		c := engineSecurityCondition(env, class, now)
		if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
			t.Errorf("rootless-podman: got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
		}
	})

	t.Run("unknown engine type", func(t *testing.T) {
		class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineType("docker")}}}
		c := engineSecurityCondition(env, class, now)
		if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
			t.Errorf("unknown: got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
		}
	})
}

func TestFormatRelaxations_DeterministicAndSorted(t *testing.T) {
	relaxations := []render.Relaxation{
		{Container: "sidecar", Kind: render.RelaxSeccompUnset, Reason: "z"},
		{Container: "agent", Kind: render.RelaxAllowPrivilegeEscalation, Reason: "a"},
		{Container: "agent", Kind: render.RelaxAddCapability, Reason: "b"},
	}
	got1 := formatRelaxations(relaxations)
	got2 := formatRelaxations(relaxations)
	if got1 != got2 {
		t.Fatalf("formatRelaxations is not deterministic: %q != %q", got1, got2)
	}
	// agent/AddCapability sorts before agent/AllowPrivilegeEscalation
	// (lexicographic on Kind), both before sidecar/*.
	wantOrder := []string{"agent: AddCapability", "agent: AllowPrivilegeEscalation", "sidecar: SeccompUnset"}
	for _, w := range wantOrder {
		if !strings.Contains(got1, w) {
			t.Errorf("formatRelaxations output %q missing expected fragment %q", got1, w)
		}
	}
	idxAdd := strings.Index(got1, "AddCapability")
	idxEsc := strings.Index(got1, "AllowPrivilegeEscalation")
	idxSeccomp := strings.Index(got1, "SeccompUnset")
	if idxAdd >= idxEsc || idxEsc >= idxSeccomp {
		t.Errorf("formatRelaxations output not sorted as expected: %q", got1)
	}
}

func TestEngineSecurityCondition_MessageTruncated(t *testing.T) {
	longReason := strings.Repeat("x", 1000)
	relaxations := []render.Relaxation{{Container: "agent", Kind: render.RelaxSeccompUnset, Reason: longReason}}
	got := formatRelaxations(relaxations)
	truncated := truncateMessage(got, maxRelaxationMessageBytes)
	if len(truncated) > maxRelaxationMessageBytes {
		t.Errorf("truncated message length = %d, want <= %d", len(truncated), maxRelaxationMessageBytes)
	}
	if len(got) <= maxRelaxationMessageBytes {
		t.Fatalf("test setup: formatted message not long enough to exercise truncation (%d bytes)", len(got))
	}
}

func TestEngineSecurityConditionIsNotInLifecycleSet(t *testing.T) {
	for _, ct := range lifecycle.ConditionTypes {
		if ct == ConditionEngineSecurity {
			t.Fatalf("ConditionEngineSecurity (%q) must not appear in lifecycle.ConditionTypes -- it is not Next-driven", ConditionEngineSecurity)
		}
	}
}

// ---- envtest: EngineSecurityRelaxed condition surfaces on a real reconcile ----

func TestReconcile_EngineSecurityConditionSurfaced(t *testing.T) {
	mustCreateClassWithEngine(t, sandboxv1alpha1.EngineTypeNone)
	env := mustCreateEnv(t, "engine-security-surfaced")
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}

	r := newResourceReconciler(t, newFakeClock(fixedStart))
	reconcileOnce(t, r, key)

	final := getEnv(t, key)
	c := findCondition(final, ConditionEngineSecurity)
	if c == nil {
		t.Fatal("EngineSecurityRelaxed condition missing after reconcile")
	}
	if c.Status != metav1.ConditionFalse || c.Reason != ReasonNoRelaxation {
		t.Errorf("EngineSecurityRelaxed = %s/%s, want False/%s", c.Status, c.Reason, ReasonNoRelaxation)
	}

	for _, ct := range lifecycle.ConditionTypes {
		if findCondition(final, ct) == nil {
			t.Errorf("condition %s missing after reconcile (EngineSecurityRelaxed must not disturb the lifecycle set)", ct)
		}
	}
}

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
		c := engineSecurityCondition(env, nil, "", now)
		if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
			t.Errorf("nil class: got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
		}
	})

	t.Run("none engine: no relaxation", func(t *testing.T) {
		class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeNone}}}
		c := engineSecurityCondition(env, class, "", now)
		if c.Status != metav1.ConditionFalse || c.Reason != ReasonNoRelaxation {
			t.Errorf("none: got Status=%s Reason=%s, want False/%s", c.Status, c.Reason, ReasonNoRelaxation)
		}
	})

	t.Run("unknown engine type", func(t *testing.T) {
		class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineType("docker")}}}
		c := engineSecurityCondition(env, class, "", now)
		if c.Status != metav1.ConditionUnknown || c.Reason != ReasonEngineUnavailable {
			t.Errorf("unknown: got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonEngineUnavailable)
		}
	})
}

func podmanClassFixture() *sandboxv1alpha1.SandboxClass {
	return &sandboxv1alpha1.SandboxClass{
		Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeRootlessPodman}},
	}
}

func TestEngineSecurityCondition_RootlessPodmanIsRelaxed(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	c := engineSecurityCondition(env, podmanClassFixture(), "", now)
	if c.Status != metav1.ConditionTrue || c.Reason != ReasonEngineRelaxed {
		t.Fatalf("got Status=%s Reason=%s, want True/%s", c.Status, c.Reason, ReasonEngineRelaxed)
	}
	for _, want := range []string{"podman: AppArmorUnconfined", "podman: SeccompUnconfined", "podman: AllowPrivilegeEscalation"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q missing %q", c.Message, want)
		}
	}
}

func TestEngineSecurityCondition_RootlessPodmanMessageIsNotTruncated(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	c := engineSecurityCondition(env, podmanClassFixture(), "", now)
	if len(c.Message) >= maxRelaxationMessageBytes {
		t.Errorf("message length = %d, want < %d", len(c.Message), maxRelaxationMessageBytes)
	}
	full := formatRelaxations(podmanRelaxationsFixture(t))
	if !strings.HasSuffix(c.Message, full[len(full)-10:]) {
		t.Errorf("message %q looks truncated (does not end with the full formatted relaxations' tail)", c.Message)
	}
}

// podmanRelaxationsFixture returns render.EngineRelaxations(rootless-podman),
// failing the test if the engine reports it is not implemented.
func podmanRelaxationsFixture(t *testing.T) []render.Relaxation {
	t.Helper()
	relaxations, ok := render.EngineRelaxations(sandboxv1alpha1.EngineTypeRootlessPodman)
	if !ok {
		t.Fatal("render.EngineRelaxations(rootless-podman) ok = false, want true")
	}
	return relaxations
}

func TestEngineSecurityCondition_RestrictedNamespace(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	c := engineSecurityCondition(env, podmanClassFixture(), render.PodSecurityRestricted, now)
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonNamespacePSSIncompatible {
		t.Fatalf("got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonNamespacePSSIncompatible)
	}
	if !strings.Contains(c.Message, render.PodSecurityEnforceLabel) {
		t.Errorf("message %q missing %q", c.Message, render.PodSecurityEnforceLabel)
	}
}

func TestEngineSecurityCondition_BaselineNamespace(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	c := engineSecurityCondition(env, podmanClassFixture(), render.PodSecurityBaseline, now)
	if c.Status != metav1.ConditionUnknown || c.Reason != ReasonNamespacePSSIncompatible {
		t.Fatalf("got Status=%s Reason=%s, want Unknown/%s", c.Status, c.Reason, ReasonNamespacePSSIncompatible)
	}
	for _, want := range []string{"AppArmorUnconfined", "SeccompUnconfined"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("message %q missing %q", c.Message, want)
		}
	}
}

func TestEngineSecurityCondition_PrivilegedNamespaceIsFine(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	c := engineSecurityCondition(env, podmanClassFixture(), render.PodSecurityPrivileged, now)
	if c.Status != metav1.ConditionTrue || c.Reason != ReasonEngineRelaxed {
		t.Errorf("got Status=%s Reason=%s, want True/%s", c.Status, c.Reason, ReasonEngineRelaxed)
	}
}

func TestEngineSecurityCondition_NoneEngineIgnoresNamespaceLevel(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	env := &sandboxv1alpha1.SandboxEnvironment{}
	class := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeNone}}}
	c := engineSecurityCondition(env, class, render.PodSecurityRestricted, now)
	if c.Status != metav1.ConditionFalse || c.Reason != ReasonNoRelaxation {
		t.Errorf("got Status=%s Reason=%s, want False/%s", c.Status, c.Reason, ReasonNoRelaxation)
	}
}

func TestWarnIfEngineNamespaceIncompatible_FiresWhenIncompatible(t *testing.T) {
	env := &sandboxv1alpha1.SandboxEnvironment{}
	class := podmanClassFixture()
	r := &Reconciler{Recorder: newEventCapture()}
	r.warnIfEngineNamespaceIncompatible(env, class, render.PodSecurityRestricted)
	events := r.Recorder.(*eventCapture).ByReason("EngineNamespaceIncompatible")
	if len(events) != 1 {
		t.Fatalf("got %d EngineNamespaceIncompatible events, want 1", len(events))
	}
	if events[0].EventType != "Warning" {
		t.Errorf("event type = %q, want Warning", events[0].EventType)
	}
}

func TestWarnIfEngineNamespaceIncompatible_SilentForNoneEnginePrivilegedAndEmpty(t *testing.T) {
	env := &sandboxv1alpha1.SandboxEnvironment{}
	podman := podmanClassFixture()
	none := &sandboxv1alpha1.SandboxClass{Spec: sandboxv1alpha1.SandboxClassSpec{Engine: sandboxv1alpha1.EngineSpec{Type: sandboxv1alpha1.EngineTypeNone}}}

	cases := []struct {
		name    string
		class   *sandboxv1alpha1.SandboxClass
		enforce string
	}{
		{"none engine, restricted", none, render.PodSecurityRestricted},
		{"podman, privileged", podman, render.PodSecurityPrivileged},
		{"podman, no label", podman, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{Recorder: newEventCapture()}
			r.warnIfEngineNamespaceIncompatible(env, tc.class, tc.enforce)
			if got := r.Recorder.(*eventCapture).Events(); len(got) != 0 {
				t.Errorf("got %d events, want 0: %+v", len(got), got)
			}
		})
	}
}

func TestFormatRelaxations_DeterministicAndSorted(t *testing.T) {
	relaxations := []render.Relaxation{
		{Container: "sidecar", Kind: render.RelaxSeccompUnconfined, Reason: "z"},
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
	wantOrder := []string{"agent: AddCapability", "agent: AllowPrivilegeEscalation", "sidecar: SeccompUnconfined"}
	for _, w := range wantOrder {
		if !strings.Contains(got1, w) {
			t.Errorf("formatRelaxations output %q missing expected fragment %q", got1, w)
		}
	}
	idxAdd := strings.Index(got1, "AddCapability")
	idxEsc := strings.Index(got1, "AllowPrivilegeEscalation")
	idxSeccomp := strings.Index(got1, "SeccompUnconfined")
	if idxAdd >= idxEsc || idxEsc >= idxSeccomp {
		t.Errorf("formatRelaxations output not sorted as expected: %q", got1)
	}
}

func TestEngineSecurityCondition_MessageTruncated(t *testing.T) {
	longReason := strings.Repeat("x", 1000)
	relaxations := []render.Relaxation{{Container: "agent", Kind: render.RelaxSeccompUnconfined, Reason: longReason}}
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

package render

import (
	"errors"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestCheckNamespacePodSecurity_AllowsUnlabelledAndPrivileged(t *testing.T) {
	podman, ok := EngineRelaxations(v1alpha1.EngineTypeRootlessPodman)
	if !ok {
		t.Fatal("EngineRelaxations(rootless-podman) ok = false, want true")
	}
	for _, enforce := range []string{"", PodSecurityPrivileged} {
		for _, tc := range []struct {
			name        string
			engine      v1alpha1.EngineType
			relaxations []Relaxation
		}{
			{"none", v1alpha1.EngineTypeNone, nil},
			{"rootless-podman", v1alpha1.EngineTypeRootlessPodman, podman},
		} {
			t.Run(enforce+"/"+tc.name, func(t *testing.T) {
				if err := CheckNamespacePodSecurity("ns", tc.engine, enforce, tc.relaxations); err != nil {
					t.Errorf("CheckNamespacePodSecurity(%q, %q) = %v, want nil", enforce, tc.engine, err)
				}
			})
		}
	}
}

func TestCheckNamespacePodSecurity_NoneEngineIsAllowedEverywhere(t *testing.T) {
	for _, enforce := range []string{"", PodSecurityPrivileged, PodSecurityBaseline, PodSecurityRestricted} {
		if err := CheckNamespacePodSecurity("ns", v1alpha1.EngineTypeNone, enforce, nil); err != nil {
			t.Errorf("CheckNamespacePodSecurity(none, %q) = %v, want nil", enforce, err)
		}
	}
}

func TestCheckNamespacePodSecurity_BaselineRejectsPodman(t *testing.T) {
	relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeRootlessPodman)
	if !ok {
		t.Fatal("EngineRelaxations(rootless-podman) ok = false, want true")
	}
	err := CheckNamespacePodSecurity("ns", v1alpha1.EngineTypeRootlessPodman, PodSecurityBaseline, relaxations)
	if err == nil {
		t.Fatal("expected error at baseline, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "AppArmorUnconfined") || !strings.Contains(msg, "SeccompUnconfined") {
		t.Errorf("error %q missing AppArmorUnconfined/SeccompUnconfined", msg)
	}
	if strings.Contains(msg, "AllowPrivilegeEscalation") {
		t.Errorf("error %q should NOT mention AllowPrivilegeEscalation at baseline", msg)
	}
}

func TestCheckNamespacePodSecurity_RestrictedRejectsPodman(t *testing.T) {
	relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeRootlessPodman)
	if !ok {
		t.Fatal("EngineRelaxations(rootless-podman) ok = false, want true")
	}
	err := CheckNamespacePodSecurity("ns", v1alpha1.EngineTypeRootlessPodman, PodSecurityRestricted, relaxations)
	if err == nil {
		t.Fatal("expected error at restricted, got nil")
	}
	var pssErr *PodSecurityIncompatibleError
	if !errors.As(err, &pssErr) {
		t.Fatalf("err = %v, want *PodSecurityIncompatibleError", err)
	}
	// podman's three relaxations, all forbidden at restricted, sorted.
	wantSorted := []string{"AllowPrivilegeEscalation", "AppArmorUnconfined", "SeccompUnconfined"}
	if len(pssErr.Kinds) != len(wantSorted) {
		t.Fatalf("Kinds = %v, want %v", pssErr.Kinds, wantSorted)
	}
	for i, k := range wantSorted {
		if pssErr.Kinds[i] != k {
			t.Errorf("Kinds[%d] = %q, want %q (want sorted: %v)", i, pssErr.Kinds[i], k, wantSorted)
		}
	}
}

func TestCheckNamespacePodSecurity_MessageIsActionable(t *testing.T) {
	relaxations, ok := EngineRelaxations(v1alpha1.EngineTypeRootlessPodman)
	if !ok {
		t.Fatal("EngineRelaxations(rootless-podman) ok = false, want true")
	}
	err := CheckNamespacePodSecurity("my-ns", v1alpha1.EngineTypeRootlessPodman, PodSecurityRestricted, relaxations)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"my-ns", string(v1alpha1.EngineTypeRootlessPodman), PodSecurityEnforceLabel, "enforce=privileged", "spec.engine.type: none"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestForbiddenAt_AddCapability(t *testing.T) {
	netBind := Relaxation{Kind: RelaxAddCapability, Value: "NET_BIND_SERVICE"}
	if got := forbiddenAt(netBind); got != nil {
		t.Errorf("forbiddenAt(NET_BIND_SERVICE) = %v, want nil (allowed everywhere)", got)
	}

	other := Relaxation{Kind: RelaxAddCapability, Value: "SYS_ADMIN"}
	got := forbiddenAt(other)
	foundBaseline, foundRestricted := false, false
	for _, level := range got {
		if level == PodSecurityBaseline {
			foundBaseline = true
		}
		if level == PodSecurityRestricted {
			foundRestricted = true
		}
	}
	if !foundBaseline || !foundRestricted {
		t.Errorf("forbiddenAt(SYS_ADMIN) = %v, want both baseline and restricted", got)
	}
}

func TestRenderPod_RestrictedNamespaceProducesAnActionableError(t *testing.T) {
	class := podmanClass()
	in := Inputs{
		Env: baseEnv("pss-restricted"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test",
		NamespacePodSecurityEnforce: PodSecurityRestricted,
	}
	_, err := RenderPod(in)
	if err == nil {
		t.Fatal("RenderPod: expected error, got nil")
	}
	var pssErr *PodSecurityIncompatibleError
	if !errors.As(err, &pssErr) {
		t.Fatalf("err = %v, want errors.As into *PodSecurityIncompatibleError", err)
	}
	if !strings.Contains(err.Error(), "cannot run in namespace") {
		t.Errorf("error %q missing \"cannot run in namespace\"", err.Error())
	}
}

func TestRenderPod_RestrictedNamespaceIsFineForNoneEngine(t *testing.T) {
	class := withEngine(minimalClass(), v1alpha1.EngineTypeNone)
	in := Inputs{
		Env: baseEnv("pss-restricted-none"), Class: class, ClusterID: "test-cluster", SidecarImage: "test-sidecar:test",
		NamespacePodSecurityEnforce: PodSecurityRestricted,
	}
	if _, err := RenderPod(in); err != nil {
		t.Errorf("RenderPod with engine: none in a restricted namespace: %v, want nil", err)
	}
}

package render

import (
	"testing"

	acrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

func TestRole_ResourceNamesUsesFullEnvironmentName(t *testing.T) {
	longName := "very-long-environment-name-that-definitely-exceeds-the-sixty-three-character-kubernetes-object-name-limit"
	in := Inputs{Env: baseEnv(longName), Class: minimalClass()}
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, rule := range objs.Role.Rules {
		found := false
		for _, rn := range rule.ResourceNames {
			if rn == longName {
				found = true
			}
		}
		if !found {
			t.Errorf("rule %+v does not reference the FULL environment name %q", rule, longName)
		}
	}
}

func TestRole_NoOverbroadVerbsOrResources(t *testing.T) {
	in := Inputs{Env: baseEnv("e"), Class: minimalClass()}
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	forbiddenVerbs := map[string]bool{"list": true, "watch": true, "create": true, "delete": true, "update": true}
	for _, rule := range objs.Role.Rules {
		for _, v := range rule.Verbs {
			if forbiddenVerbs[v] {
				t.Errorf("rule %+v contains forbidden verb %q", rule, v)
			}
		}
		for _, r := range rule.Resources {
			if r == "secrets" {
				t.Errorf("rule %+v references secrets", rule)
			}
		}
		for _, g := range rule.APIGroups {
			// Every referenced group must be sandbox.psenna.dev -- nothing
			// cluster-scoped (e.g. no empty group covering nodes/namespaces,
			// no rbac.authorization.k8s.io, etc).
			if g != "sandbox.psenna.dev" {
				t.Errorf("rule %+v references unexpected API group %q", rule, g)
			}
		}
	}
}

func TestRole_OnlyAllowedVerbs(t *testing.T) {
	in := Inputs{Env: baseEnv("e"), Class: minimalClass()}
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	allowed := map[string]bool{"get": true, "patch": true}
	for _, rule := range objs.Role.Rules {
		for _, v := range rule.Verbs {
			if !allowed[v] {
				t.Errorf("rule %+v contains unexpected verb %q", rule, v)
			}
		}
	}
}

// TestRole_K8sNativeWide asserts that when the class engine is k8s-native the
// sidecar Role is widened with the servicesets + pods/exec rules, and that the
// servicesets grant is SPLIT: get/update are name-pinned to the env's own
// ServiceSet (resourceNames IS honored for those verbs), while create is NOT
// name-pinned (Kubernetes RBAC ignores resourceNames for create -- a pinned
// create rule authorizes nothing, which is the e2e failure this guards).
// pods/exec create is namespace-scoped only (runtime pod names are dynamic and
// cannot be name-pinned). For the none engine, NO rule may reference
// servicesets or pods/exec -- least-privilege gating.
func TestRole_K8sNativeWide(t *testing.T) {
	in := Inputs{Env: baseEnv("e"), Class: withEngine(minimalClass(), v1alpha1.EngineTypeK8sNative)}
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Collect every servicesets rule (there must be two after the split).
	var servicesetRules []*policyRule
	for i := range objs.Role.Rules {
		r := objs.Role.Rules[i]
		if len(r.APIGroups) == 1 && r.APIGroups[0] == "sandbox.psenna.dev" &&
			len(r.Resources) == 1 && r.Resources[0] == "servicesets" {
			servicesetRules = append(servicesetRules, &objs.Role.Rules[i])
		}
	}
	if len(servicesetRules) != 2 {
		t.Fatalf("want exactly 2 servicesets rules (name-pinned get/update + unpinned create), got %d: %+v", len(servicesetRules), servicesetRules)
	}

	// Exactly one servicesets rule is name-pinned and carries get/update.
	var namePinned *policyRule
	var unpinned *policyRule
	for _, r := range servicesetRules {
		if len(r.ResourceNames) > 0 {
			namePinned = r
		} else {
			unpinned = r
		}
	}
	if namePinned == nil {
		t.Fatalf("no name-pinned servicesets rule; rules=%+v", servicesetRules)
	}
	if !contains(namePinned.ResourceNames, "e") {
		t.Errorf("name-pinned servicesets rule ResourceNames=%v, want to contain env name %q", namePinned.ResourceNames, "e")
	}
	for _, want := range []string{"get", "update"} {
		if !contains(namePinned.Verbs, want) {
			t.Errorf("name-pinned servicesets rule Verbs=%v, want to include %q", namePinned.Verbs, want)
		}
	}
	if contains(namePinned.Verbs, "create") {
		t.Errorf("name-pinned servicesets rule must NOT carry create (resourceNames is ignored for create, so it would authorize nothing AND misrepresent the pin): Verbs=%v", namePinned.Verbs)
	}

	// Exactly one servicesets rule is unpinned and carries create only.
	if unpinned == nil {
		t.Fatalf("no unpinned servicesets create rule; rules=%+v", servicesetRules)
	}
	if !contains(unpinned.Verbs, "create") {
		t.Errorf("unpinned servicesets rule Verbs=%v, want [create]", unpinned.Verbs)
	}
	if len(unpinned.ResourceNames) != 0 {
		t.Errorf("unpinned servicesets create rule must NOT be name-pinned (RBAC ignores resourceNames for create); ResourceNames=%v", unpinned.ResourceNames)
	}

	// pods/exec rule: APIGroups=[""], Resources=[pods/exec], Verbs=[create].
	var execRule *policyRule
	for i := range objs.Role.Rules {
		r := objs.Role.Rules[i]
		if len(r.APIGroups) == 1 && r.APIGroups[0] == "" &&
			len(r.Resources) == 1 && r.Resources[0] == "pods/exec" {
			execRule = &objs.Role.Rules[i]
			break
		}
	}
	if execRule == nil {
		t.Fatalf("no pods/exec rule found for k8s-native; rules=%+v", objs.Role.Rules)
	}
	if !contains(execRule.Verbs, "create") {
		t.Errorf("pods/exec rule Verbs=%v, want [create]", execRule.Verbs)
	}
	if len(execRule.ResourceNames) != 0 {
		t.Errorf("pods/exec rule must NOT be name-pinned (pod names are dynamic); ResourceNames=%v", execRule.ResourceNames)
	}

	// none engine: NO rule may reference servicesets or pods/exec.
	noneIn := Inputs{Env: baseEnv("e"), Class: withEngine(minimalClass(), v1alpha1.EngineTypeNone)}
	noneObjs, err := Render(noneIn)
	if err != nil {
		t.Fatalf("Render (none): %v", err)
	}
	for i, r := range noneObjs.Role.Rules {
		for _, res := range r.Resources {
			if res == "servicesets" {
				t.Errorf("none engine rule[%d] references servicesets; rules=%+v", i, noneObjs.Role.Rules)
			}
			if res == "pods/exec" {
				t.Errorf("none engine rule[%d] references pods/exec; rules=%+v", i, noneObjs.Role.Rules)
			}
		}
	}
}

// policyRule is an alias used only for readability of the assertions above;
// it matches the concrete acrbacv1.PolicyRuleApplyConfiguration (value type)
// stored in objs.Role.Rules.
type policyRule = acrbacv1.PolicyRuleApplyConfiguration

// contains reports whether s contains x.
func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestRoleBinding_ReferencesRenderedServiceAccount(t *testing.T) {
	in := Inputs{Env: baseEnv("e"), Class: minimalClass()}
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	names := ChildNames("e")
	if len(objs.RoleBinding.Subjects) != 1 {
		t.Fatalf("RoleBinding has %d subjects, want 1", len(objs.RoleBinding.Subjects))
	}
	subj := objs.RoleBinding.Subjects[0]
	if *subj.Name != names.ServiceAccount {
		t.Errorf("RoleBinding subject name = %q, want %q", *subj.Name, names.ServiceAccount)
	}
	if *subj.Kind != "ServiceAccount" {
		t.Errorf("RoleBinding subject kind = %q, want ServiceAccount", *subj.Kind)
	}
	if *objs.RoleBinding.RoleRef.Name != names.Role {
		t.Errorf("RoleBinding roleRef name = %q, want %q", *objs.RoleBinding.RoleRef.Name, names.Role)
	}
}

package render

import (
	"testing"
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

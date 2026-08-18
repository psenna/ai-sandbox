package render

import (
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// assertGoldenObjects renders in and asserts every one of the six rendered
// objects against its golden file under testdata/<case>.<kind>.yaml. The
// Secret is redacted (see redactSecretData) before comparison.
func assertGoldenObjects(t *testing.T, caseName string, in Inputs) Objects {
	t.Helper()
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	assertGolden(t, caseName+".serviceaccount.yaml", marshalForGolden(t, objs.ServiceAccount))
	assertGolden(t, caseName+".role.yaml", marshalForGolden(t, objs.Role))
	assertGolden(t, caseName+".rolebinding.yaml", marshalForGolden(t, objs.RoleBinding))
	assertGolden(t, caseName+".pvc.yaml", marshalForGolden(t, objs.PVC))
	assertGolden(t, caseName+".configmap.yaml", marshalForGolden(t, objs.ConfigMap))
	assertGolden(t, caseName+".secret.yaml", marshalForGolden(t, redactSecretData(objs.Secret)))
	if objs.SnapshotSecret != nil {
		assertGolden(t, caseName+".snapshotsecret.yaml", marshalForGolden(t, redactSecretData(objs.SnapshotSecret)))
	}
	return objs
}

func TestRender_Full(t *testing.T) {
	in := Inputs{
		Env:   baseEnv("full-env", withPrompt("do the full thing")),
		Class: fullClass(),
		Credentials: Credentials{ //nolint:gosec // G101: deliberately fake test fixture values, not real credentials
			GitProxyToken:           "fake-git-proxy-token-full",
			SnapshotAccessKeyID:     "fake-snapshot-access-key-full",
			SnapshotSecretAccessKey: "fake-snapshot-secret-key-full",
		},
		ClusterID: "test-cluster",
		SpecHash:  "sha256:fakespechashfull",
	}
	assertGoldenObjects(t, "full", in)
}

func TestRender_Minimal(t *testing.T) {
	in := Inputs{
		Env:       baseEnv("minimal-env"),
		Class:     minimalClass(),
		ClusterID: "test-cluster",
	}
	objs := assertGoldenObjects(t, "minimal", in)

	wantKeys := map[string]bool{
		"GITHUB_REPO":                    true,
		"ANTHROPIC_AUTH_TOKEN":           true,
		"ANTHROPIC_API_KEY":              true,
		"CLAUDE_CONFIG_DIR":              true,
		"CLAUDE_CODE_ATTRIBUTION_HEADER": true,
		"SANDBOX_SIDECAR_URL":            true,
	}
	if len(objs.Secret.Data) != len(wantKeys) {
		t.Errorf("minimal Secret has %d keys, want %d: got %v", len(objs.Secret.Data), len(wantKeys), keysOf(objs.Secret.Data))
	}
	for k := range objs.Secret.Data {
		if !wantKeys[k] {
			t.Errorf("minimal Secret has unexpected key %q", k)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestRender_IssueTask(t *testing.T) {
	in := Inputs{
		Env:       baseEnv("issue-task-env", withIssueRef("org/repo", 42)),
		Class:     minimalClass(),
		ClusterID: "test-cluster",
	}
	objs := assertGoldenObjects(t, "issue-task", in)

	taskMD := objs.ConfigMap.Data[TaskFileName]
	if taskMD != "Implement issue #42 in org/repo." {
		t.Errorf("task.md = %q, want deterministic issue instruction", taskMD)
	}
	sandboxJSON := objs.ConfigMap.Data[RunConfigFileName]
	if strings.Contains(sandboxJSON, `"hasPrompt": true`) {
		t.Errorf("sandbox.json hasPrompt should be false for an issue-only task:\n%s", sandboxJSON)
	}
	if !strings.Contains(sandboxJSON, `"repo": "org/repo"`) || !strings.Contains(sandboxJSON, `"number": 42`) {
		t.Errorf("sandbox.json missing issue fields:\n%s", sandboxJSON)
	}
}

func TestRender_LongName(t *testing.T) {
	longName := strings.Repeat("a", 190) + "-" + strings.Repeat("b", 9) // 200 chars, DNS-1123 shaped
	if len(longName) != 200 {
		t.Fatalf("test setup: longName length = %d, want 200", len(longName))
	}
	in := Inputs{
		Env:       baseEnv(longName),
		Class:     minimalClass(),
		ClusterID: "test-cluster",
	}
	objs := assertGoldenObjects(t, "long-name", in)

	names := ChildNames(longName)
	checkName := func(label, name string) {
		t.Helper()
		if len(name) > MaxNameLen {
			t.Errorf("%s name %q has length %d, want <= %d", label, name, len(name), MaxNameLen)
		}
		last := name[len(name)-1]
		if !isAlnum(last) {
			t.Errorf("%s name %q does not end in an alphanumeric character", label, name)
		}
	}
	checkName("PVC", names.PVC)
	checkName("ServiceAccount", names.ServiceAccount)
	checkName("Secret", names.Secret)
	checkName("ConfigMap", names.ConfigMap)
	checkName("Role", names.Role)
	checkName("RoleBinding", names.RoleBinding)

	for _, n := range []string{names.PVC, names.ServiceAccount, names.Secret, names.ConfigMap, names.Role, names.RoleBinding} {
		if !strings.Contains(n, "-") {
			t.Errorf("name %q missing expected hash suffix separator", n)
			continue
		}
	}

	for k, v := range Labels(in.Env) {
		if len(v) > MaxNameLen {
			t.Errorf("label %s=%q has length %d, want <= %d", k, v, len(v), MaxNameLen)
		}
	}

	_ = objs
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func TestRender_StorageClassNilVsEmpty(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		class := minimalClass()
		class.Spec.Storage.Workspace.StorageClassName = nil
		in := Inputs{Env: baseEnv("storage-class-nil-env"), Class: class, ClusterID: "test-cluster"}
		objs, err := Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		assertGolden(t, "storage-class-nil.pvc.yaml", marshalForGolden(t, objs.PVC))
		if objs.PVC.Spec.StorageClassName != nil {
			t.Errorf("StorageClassName = %v, want nil", *objs.PVC.Spec.StorageClassName)
		}
	})

	t.Run("empty", func(t *testing.T) {
		class := minimalClass()
		empty := ""
		class.Spec.Storage.Workspace.StorageClassName = &empty
		in := Inputs{Env: baseEnv("storage-class-empty-env"), Class: class, ClusterID: "test-cluster"}
		objs, err := Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		assertGolden(t, "storage-class-empty.pvc.yaml", marshalForGolden(t, objs.PVC))
		if objs.PVC.Spec.StorageClassName == nil {
			t.Fatalf("StorageClassName = nil, want present-and-empty")
		}
		if *objs.PVC.Spec.StorageClassName != "" {
			t.Errorf("StorageClassName = %q, want empty string", *objs.PVC.Spec.StorageClassName)
		}
	})
}

func TestRender_ErrorsOnMissingInputs(t *testing.T) {
	env := baseEnv("e")
	class := minimalClass()

	if _, err := Render(Inputs{Class: class}); err == nil {
		t.Error("Render with nil Env: expected error, got nil")
	}
	if _, err := Render(Inputs{Env: env}); err == nil {
		t.Error("Render with nil Class: expected error, got nil")
	}
	noUID := baseEnv("e")
	noUID.UID = ""
	if _, err := Render(Inputs{Env: noUID, Class: class}); err == nil {
		t.Error("Render with empty Env.UID: expected error, got nil")
	}
}

func TestRender_BadStorageSizeErrors(t *testing.T) {
	class := minimalClass()
	class.Spec.Storage.Workspace.Size = "not-a-quantity"
	_, err := Render(Inputs{Env: baseEnv("e"), Class: class})
	if err == nil {
		t.Fatal("Render with bad storage size: expected error, got nil")
	}
}

// sanity: ensure v1alpha1 import is used (guards against accidental removal
// if helpers_test.go changes shape).
var _ = v1alpha1.SandboxEnvironment{}

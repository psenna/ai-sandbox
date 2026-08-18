package render

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestChildName_LengthBoundaries(t *testing.T) {
	suffixes := []string{SuffixWorkspace, SuffixServiceAccount, SuffixSecret, SuffixConfigMap, SuffixSidecarRBAC, SuffixSnapshotSecret, SuffixSnapshotJob}
	lengths := []int{62, 63, 64, 100, 253}

	for _, suffix := range suffixes {
		for _, l := range lengths {
			name := strings.Repeat("a", l)
			t.Run(fmt.Sprintf("%s/len%d", suffix, l), func(t *testing.T) {
				got := childName(name, suffix)
				if len(got) > MaxNameLen {
					t.Errorf("childName(%d-char name, %q) = %q, length %d > %d", l, suffix, got, len(got), MaxNameLen)
				}
				if !dns1123LabelRE.MatchString(got) {
					t.Errorf("childName(%d-char name, %q) = %q is not a valid DNS-1123 label", l, suffix, got)
				}
			})
		}
	}
}

func TestChildName_LongNamesSharingPrefixDifferByHash(t *testing.T) {
	base := strings.Repeat("shared-prefix-", 6) // 84 chars, long enough to force truncation
	nameA := base + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	nameB := base + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	gotA := childName(nameA, SuffixWorkspace)
	gotB := childName(nameB, SuffixWorkspace)

	if gotA == gotB {
		t.Fatalf("two different long names with a shared prefix produced the same childName: %q", gotA)
	}
}

func TestChildName_IdempotentShaped(t *testing.T) {
	name := strings.Repeat("x", 200)
	once := childName(name, SuffixWorkspace)
	twice := childName(once, "")
	// Not a real use case, but childName should never panic or produce an
	// invalid result when called again on its own output.
	if len(twice) > MaxNameLen {
		t.Errorf("re-applying childName grew beyond the limit: %q (len %d)", twice, len(twice))
	}
	if !dns1123LabelRE.MatchString(twice) {
		t.Errorf("re-applying childName produced an invalid label: %q", twice)
	}
}

func TestChildNames_SixDistinctStrings(t *testing.T) {
	cases := []string{"short-name", strings.Repeat("a", 200)}
	for _, name := range cases {
		t.Run(name[:min(20, len(name))], func(t *testing.T) {
			n := ChildNames(name)
			all := []string{n.PVC, n.ServiceAccount, n.Secret, n.ConfigMap, n.Role, n.RoleBinding, n.SnapshotSecret, n.SnapshotJob}
			seen := map[string]int{}
			for _, s := range all {
				seen[s]++
			}
			// Role and RoleBinding legitimately share a name (different
			// kinds); every other pair must differ.
			if n.Role != n.RoleBinding {
				t.Errorf("Role %q and RoleBinding %q should be identical strings (same suffix)", n.Role, n.RoleBinding)
			}
			distinctKinds := map[string]string{
				"PVC":              n.PVC,
				"ServiceAccount":   n.ServiceAccount,
				"Secret":           n.Secret,
				"ConfigMap":        n.ConfigMap,
				"Role/RoleBinding": n.Role,
				"SnapshotSecret":   n.SnapshotSecret,
				"SnapshotJob":      n.SnapshotJob,
			}
			byValue := map[string][]string{}
			for k, v := range distinctKinds {
				byValue[v] = append(byValue[v], k)
			}
			for v, ks := range byValue {
				if len(ks) > 1 {
					t.Errorf("names collide across distinct suffixes: %v all produced %q", ks, v)
				}
			}
			for _, s := range all {
				if !dns1123LabelRE.MatchString(s) {
					t.Errorf("name %q is not a valid DNS-1123 label", s)
				}
			}
		})
	}
}

func TestEnvironmentLabelValue_MatchesLabelLimits(t *testing.T) {
	cases := []string{"short", strings.Repeat("z", 253)}
	for _, name := range cases {
		v := EnvironmentLabelValue(name)
		if len(v) > MaxNameLen {
			t.Errorf("EnvironmentLabelValue(%d-char name) = %q, length %d > %d", len(name), v, len(v), MaxNameLen)
		}
		if !dns1123LabelRE.MatchString(v) {
			t.Errorf("EnvironmentLabelValue(%d-char name) = %q is not a valid label value", len(name), v)
		}
	}
}

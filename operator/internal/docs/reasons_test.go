package docs

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/psenna/ai-sandbox/operator/internal/controller"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// reasonConstRe matches a top-level `ReasonXxx = "value"` constant
// declaration, indented by exactly one tab (gofmt's const-block style).
// It deliberately does NOT match ConditionXxx or other non-Reason
// constants in the same files.
var reasonConstRe = regexp.MustCompile(`(?m)^\tReason[A-Za-z]+\s+=\s+"([^"]+)"`)

// constReasonSet parses path and returns the set of string values of every
// `ReasonXxx = "..."` constant declared in it.
func constReasonSet(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is always one of three hardcoded constants below
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	matches := reasonConstRe.FindAllStringSubmatch(string(raw), -1)
	set := make(map[string]bool, len(matches))
	for _, m := range matches {
		set[m[1]] = true
	}
	return set
}

func sliceSet(s []string) map[string]bool {
	set := make(map[string]bool, len(s))
	for _, v := range s {
		set[v] = true
	}
	return set
}

// assertSetEqual reports, individually, every reason present in one set
// but not the other -- so a failure names the exact drifted entry rather
// than just "sets differ".
func assertSetEqual(t *testing.T, label string, fromSource, fromSlice map[string]bool) {
	t.Helper()
	for r := range fromSource {
		if !fromSlice[r] {
			t.Errorf("%s: reason %q is declared as a constant but missing from the slice -- someone added a Reason constant and forgot to add it here", label, r)
		}
	}
	for r := range fromSlice {
		if !fromSource[r] {
			t.Errorf("%s: reason %q is in the slice but no longer declared as a constant -- stale entry, remove it", label, r)
		}
	}
}

// TestAllReasonsIsComplete asserts lifecycle.AllReasons,
// controller.AllNetworkConditionReasons and
// controller.AllEngineSecurityReasons are each exactly the set of
// `ReasonXxx` constants declared in the source file they mirror -- parsed
// from the file itself, not transcribed by hand, so the two can never
// silently drift apart.
func TestAllReasonsIsComplete(t *testing.T) {
	t.Run("lifecycle.AllReasons", func(t *testing.T) {
		source := constReasonSet(t, "../lifecycle/conditions.go")
		assertSetEqual(t, "lifecycle.AllReasons", source, sliceSet(lifecycle.AllReasons))
	})

	t.Run("controller.AllNetworkConditionReasons", func(t *testing.T) {
		source := constReasonSet(t, "../controller/netconditions.go")
		// ReasonCNIProbeFailed is an internal CNIProbeResult.Reason value
		// only, never a reason a Condition itself carries (see
		// cniEnforcementCondition's routing comment) -- excluded here to
		// match AllNetworkConditionReasons's own deliberate exclusion.
		delete(source, controller.ReasonCNIProbeFailed)
		assertSetEqual(t, "controller.AllNetworkConditionReasons", source, sliceSet(controller.AllNetworkConditionReasons))
	})

	t.Run("controller.AllEngineSecurityReasons", func(t *testing.T) {
		source := constReasonSet(t, "../controller/podsecurity.go")
		assertSetEqual(t, "controller.AllEngineSecurityReasons", source, sliceSet(controller.AllEngineSecurityReasons))
	})
}

// TestEveryConditionReasonIsDocumented asserts every reason in
// lifecycle.AllReasons, controller.AllNetworkConditionReasons and
// controller.AllEngineSecurityReasons appears, backticked, in
// operator/docs/operations.md -- issue #35's "every condition reason the
// operator can emit appears in the troubleshooting guide" acceptance
// criterion, enforced mechanically rather than by review.
//
// Backticked (not a bare substring match) so a prose word that happens to
// match a reason name -- e.g. "Running" -- cannot vacuously satisfy this
// for the `Running` reason without an actual documented row.
func TestEveryConditionReasonIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatalf("reading operator/docs/operations.md: %v -- this file must document every condition reason (issue #35); it does not exist yet", err)
	}
	doc := string(raw)

	var all []string
	all = append(all, lifecycle.AllReasons...)
	all = append(all, controller.AllNetworkConditionReasons...)
	all = append(all, controller.AllEngineSecurityReasons...)

	for _, reason := range all {
		if !strings.Contains(doc, "`"+reason+"`") {
			t.Errorf("operations.md does not document condition reason %q (issue #35 acceptance criterion) -- add a row to the \"Condition reasons\" table", reason)
		}
	}
}

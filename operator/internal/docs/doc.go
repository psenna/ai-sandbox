// Package docs holds the tests that mechanically enforce issue #35's
// acceptance criterion "every condition reason the operator can emit
// appears in the troubleshooting guide" (operator/docs/operations.md).
//
// It follows the repo's own "declared list of every member" idiom
// (lifecycle.ConditionTypes, v1alpha1.AllPhases): lifecycle.AllReasons,
// controller.AllNetworkConditionReasons and
// controller.AllEngineSecurityReasons are the declared source lists.
// reasons_test.go asserts, mechanically, both halves of the contract:
//
//   - TestAllReasonsIsComplete: each of those three slices is complete
//     against the const block it mirrors, by parsing the source file --
//     so adding a new Reason constant without adding it to the slice
//     fails CI.
//   - TestEveryConditionReasonIsDocumented: every entry of all three
//     slices appears, backticked, in docs/operations.md -- so a
//     documented-but-unmaintained reason table drifts loudly, not
//     silently.
//
// This package holds no production code and is imported by nothing;
// go test ./internal/docs/... is its only entry point.
package docs

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This spec deliberately does NOT re-run AssertNetworkPolicyEnforced (a
// ~25-30s sequence) -- it just reports on the package-level result
// SynchronizedBeforeSuite's process-1 function already recorded before any
// spec ran. If the gate itself failed, the suite never got this far (see
// suite_test.go).
var _ = Describe("cluster CNI", func() {
	It("enforces NetworkPolicy", func() {
		Expect(networkPolicyGateResult.Enforced).To(BeTrue(), networkPolicyGateResult.Detail)
	})
})

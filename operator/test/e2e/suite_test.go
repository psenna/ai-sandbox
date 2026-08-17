package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// h is this process's Harness, built once in SynchronizedBeforeSuite's
// all-processes function and reused by every spec that process runs.
var h *Harness

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "operator e2e suite")
}

// SynchronizedBeforeSuite: process 1 builds its own Harness, waits for the
// operator/MinIO/doubles to be Ready, then runs AssertNetworkPolicyEnforced
// as a hard gate BEFORE any spec runs anywhere -- every later "isolation"
// assertion in this suite is meaningless if the cluster's CNI does not
// actually enforce NetworkPolicy. Its result is JSON-encoded into the
// returned []byte -- under --procs>1 each Ginkgo process is a SEPARATE OS
// process with no shared memory, so networkPolicyGateResult (read by
// netpolicy_test.go, which may run on ANY process) can only become visible
// suite-wide via this channel, never by process 1 setting the package var
// directly. All processes (including process 1, a second time) then build
// their own Harness and decode the result into their own copy of the var.
var _ = SynchronizedBeforeSuite(func() []byte {
	ctx := context.Background()
	gate := New(ctx)
	gate.WaitForPlatformReady(ctx, 3*time.Minute)
	result := AssertNetworkPolicyEnforced(ctx, gate)
	b, err := json.Marshal(result)
	Expect(err).NotTo(HaveOccurred())
	return b
}, func(b []byte) {
	h = New(context.Background())
	Expect(json.Unmarshal(b, &networkPolicyGateResult)).To(Succeed())
})

// JustAfterEach (NOT AfterEach/ReportAfterEach) is guaranteed to run
// immediately after the It body and BEFORE any AfterEach/DeferCleanup --
// dumping from ReportAfterEach would race the namespace-deletion cleanup
// DeferCleanup performs and produce empty bundles.
var _ = JustAfterEach(func(ctx SpecContext) {
	report := CurrentSpecReport()
	if !report.Failed() {
		return
	}
	h.DumpDiagnostics(ctx, report.FullText())
})

// ReportAfterSuite always writes a report.json, and on suite failure also
// dumps cluster-wide diagnostics -- this fires even if SynchronizedBeforeSuite
// itself failed (e.g. the NetworkPolicy gate), which no JustAfterEach would
// ever see.
var _ = ReportAfterSuite("e2e artifact report", func(report Report) {
	cfg := LoadConfig()
	_ = os.MkdirAll(cfg.ArtifactDir, 0o755)

	b, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(cfg.ArtifactDir, "report.json"), b, 0o644)
	}

	if report.SuiteSucceeded {
		return
	}

	// Best-effort: SynchronizedBeforeSuite may itself have failed (e.g. the
	// platform never became Ready, or the NetworkPolicy gate failed), in
	// which case `h` was never set.
	ctx := context.Background()
	dumper := h
	if dumper == nil {
		dumper = New(ctx)
	}
	dumper.DumpDiagnostics(ctx, "suite-failure")
})

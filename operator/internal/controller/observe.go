package controller

import (
	"context"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// ObserveFunc gathers the ClusterFacts for one environment. It is a field on
// Reconciler so tests can inject facts and so future issues (#19, #20, #21,
// #27, #28, #29, #30, #32) can replace the stub incrementally without
// touching the reconcile loop.
type ObserveFunc func(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error)

// ObserveStub is the only observer that exists today. It reports exactly what
// the operator can honestly see with no subsystems implemented yet:
//
//	SlotGranted       <- status.slot.granted, written by #20's future scheduler.
//	                     A real observation, not a lie: it's precisely the
//	                     field #20 will write, so the seam is unchanged once
//	                     #20 lands.
//	AgentWaitDeclared <- status.waitFor != nil, written by #27's future sidecar.
//	                     Same argument.
//	ResourcesReady    <- false (#19 renders no child resources yet)
//	PodObserved       <- false (#21/#24 create no pod yet) => PodReady=Unknown
//	SnapshotComplete  <- false (#28 takes no snapshot yet)
//	ProbeObserved     <- false (#30 evaluates no probe yet) => WaitSatisfied=Unknown
//	ArchiveWritten    <- false (#32 writes no archive yet)
//
// Consequence, stated so it surprises nobody: with only this issue merged, a
// new environment settles in Pending with Scheduled=False/ResourcesNotReady
// and stays there until #19 lands. It does not crash, does not hot-spin, and
// does not silently claim to be healthy.
func ObserveStub(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error) {
	return lifecycle.ClusterFacts{
		SlotGranted:       env.Status.Slot.Granted,
		AgentWaitDeclared: env.Status.WaitFor != nil,
	}, nil
}

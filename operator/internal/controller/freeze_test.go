package controller

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// mustFreezingEnv puts env into the state freezePod is called in: phase
// Freezing with this freeze's snapshot not yet landed (Snapshot == nil,
// FreezeCount 0).
func mustFreezingEnv(t *testing.T, env *sandboxv1alpha1.SandboxEnvironment) *sandboxv1alpha1.SandboxEnvironment {
	t.Helper()
	got := getEnv(t, types.NamespacedName{Namespace: env.Namespace, Name: env.Name})
	got.Status.Phase = sandboxv1alpha1.PhaseFreezing
	if err := k8s.Status().Update(ctx, got); err != nil {
		t.Fatalf("setting env phase to Freezing: %v", err)
	}
	return got
}

func snapshotJob(t *testing.T, ns, name string) (*batchv1.Job, bool) {
	t.Helper()
	var job batchv1.Job
	err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &job)
	switch {
	case err == nil:
		return &job, true
	case apierrors.IsNotFound(err):
		return nil, false
	default:
		t.Fatalf("Get(snapshot Job %s/%s): %v", ns, name, err)
		return nil, false
	}
}

// TestFreezePod_TerminatedPodNoSnapshot_CreatesRecoveryJob covers the race
// that parks an environment in Freezing forever.
//
// terminal()'s freeze detour is gated on observing the pod as Running
// (pod.go's PodAliveForArchive), but the freeze SIGNAL is the phase write,
// which the sandboxctl sidecar has to poll. The sidecar exits when the agent
// container exits -- in the e2e collections both terminated within the same
// second the agent reported success -- so a detour decided against a Running
// observation can land phase=Freezing after the only process that could take
// the snapshot is gone.
//
// Nothing then recovers: nextFreezing just waits on SnapshotComplete, and
// freezePod's recovery Job used to be reachable only when the pod object was
// ABSENT. A pod sitting in Succeeded is, for snapshot purposes, exactly as
// gone as a deleted one -- so the recovery Job must run for it too.
func TestFreezePod_TerminatedPodNoSnapshot_CreatesRecoveryJob(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			class := mustCreateClass(t)
			env := mustCreateEnv(t, "freeze-recovery-"+strings.ToLower(string(phase)))
			mustCreateOwnedPod(t, env, phase)
			env = mustFreezingEnv(t, env)

			r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
			if err := r.freezePod(ctx, env, class); err != nil {
				t.Fatalf("freezePod: %v", err)
			}

			names := render.ChildNames(env.Name)
			if _, ok := snapshotJob(t, env.Namespace, names.SnapshotJob); !ok {
				t.Errorf("no recovery snapshot Job for a %s pod; the sidecar has exited, so nothing else can take this freeze's snapshot and the environment parks in Freezing forever", phase)
			}
		})
	}
}

// TestFreezePod_RunningPodNoSnapshot_NoRecoveryJob is the control: while the
// pod is still Running its sidecar is alive and polling status.phase, so it
// owns the snapshot and the recovery Job must NOT be created -- two writers
// for one freeze.
func TestFreezePod_RunningPodNoSnapshot_NoRecoveryJob(t *testing.T) {
	class := mustCreateClass(t)
	env := mustCreateEnv(t, "freeze-running-no-recovery")
	mustCreateOwnedPod(t, env, corev1.PodRunning)
	env = mustFreezingEnv(t, env)

	r := newReconciler(t, newFakeClock(fixedStart), newFactsStore())
	if err := r.freezePod(ctx, env, class); err != nil {
		t.Fatalf("freezePod: %v", err)
	}

	names := render.ChildNames(env.Name)
	if _, ok := snapshotJob(t, env.Namespace, names.SnapshotJob); ok {
		t.Error("recovery snapshot Job created while the pod is Running; its sidecar owns this freeze's snapshot")
	}
}

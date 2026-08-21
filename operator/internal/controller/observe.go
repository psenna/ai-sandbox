package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/scheduler"
)

// ObserveFunc gathers the ClusterFacts for one environment. It is a field on
// Reconciler so tests can inject facts and so future issues (#20, #21, #27,
// #28, #29, #30, #32) can replace observeCluster incrementally without
// touching the reconcile loop.
type ObserveFunc func(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error)

// resourceCheck names one child object this environment expects to exist.
type resourceCheck struct {
	kind string
	name string
	obj  client.Object
}

// observeCluster is the real observer, replacing the earlier ObserveStub. It
// keeps the two honest readings the stub already had (SlotGranted,
// AgentWaitDeclared read straight off status -- SlotGranted now reads back
// what #20's SlotScheduler authoritatively writes, not a degenerate stub)
// and adds real child-resource observation (#19).
//
// ResourcesReady deliberately does NOT require the workspace PVC to be
// Bound. The default volumeBindingMode on most real StorageClasses
// (including k3s's local-path) is WaitForFirstConsumer: the PVC stays
// Pending until a pod that mounts it is scheduled, and no pod exists until
// #21 lands -- which itself only runs after Ready, which requires
// ResourcesReady. Requiring Bound here would deadlock Pending -> Ready
// forever on any normal cluster. Lost (the bound PV was destroyed) is a
// real terminal fault and DOES flip readiness false.
//
// ProbeObserved is no longer stubbed -- see observeProbe below.
// SnapshotComplete is no longer stubbed either -- see observeSnapshot in
// freeze.go. ArchiveWritten/ArchiveURI (#32) read status.archive and
// status.archiveURI, written by the sandboxctl archive Job.
func (r *Reconciler) observeCluster(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (lifecycle.ClusterFacts, error) {
	f := lifecycle.ClusterFacts{
		SlotGranted:       env.Status.Slot.Granted,
		AgentWaitDeclared: env.Status.WaitFor != nil,
		ArchiveWritten:    env.Status.Archive != nil || env.Status.ArchiveURI != "",
		ArchiveURI:        env.Status.ArchiveURI,
	}
	if ar := env.Status.AgentResult; ar != nil {
		f.AgentDone = ar.Outcome == v1alpha1.AgentOutcomeSucceeded
		f.AgentFailed = ar.Outcome == v1alpha1.AgentOutcomeFailed
		f.AgentMessage = ar.Message
	}
	r.observeSnapshot(&env.Status, &f)
	if scheduler.IsCandidate(env) {
		r.observeQueuePosition(ctx, env, &f)
	}
	r.observePod(ctx, env, &f)
	r.observeRestore(&env.Status, &f)

	if class == nil {
		return f, nil
	}

	creds, err := r.resolveCredentials(ctx, class)
	if err != nil {
		f.ResourcesProblem = err.Error()
		return f, nil
	}
	// The network peers are resolved here (not just in renderFor) so a
	// misconfigured class -- an external service endpoint with no extraEgress
	// CIDR, or an unresolvable in-cluster Service -- is reported as a
	// ResourcesProblem and holds the environment at Pending with a visible
	// message, exactly like a missing credentials Secret. The peers themselves
	// are only consumed by renderFor; this call is the validation pass.
	if _, err := r.resolveNetworkPeers(ctx, class); err != nil {
		f.ResourcesProblem = err.Error()
		return f, nil
	}
	r.observeProbe(ctx, env, class, creds, &f)

	names := render.ChildNames(env.Name)
	checks := []resourceCheck{
		{"ServiceAccount", names.ServiceAccount, &corev1.ServiceAccount{}},
		{"Role", names.Role, &rbacv1.Role{}},
		{"RoleBinding", names.RoleBinding, &rbacv1.RoleBinding{}},
		{"ConfigMap", names.ConfigMap, &corev1.ConfigMap{}},
		{"Secret", names.Secret, &corev1.Secret{}},
	}
	// The workspace PVC is deliberately NOT checked while Waiting (#29): a
	// frozen environment's PVC may have been deleted by WarmCacheGC (or may
	// be about to be), and nothing mounts it while Waiting anyway -- it is
	// unconditionally re-applied on the very next Waiting->Ready->Restoring
	// pass (withEnsureResources prepends ActionEnsureResources before
	// ActionEnsurePod in the same Decision). Requiring it here would make a
	// healthy frozen environment report a misleading "waiting for
	// PersistentVolumeClaim" problem.
	if env.Status.Phase != v1alpha1.PhaseWaiting {
		checks = append(checks, resourceCheck{"PersistentVolumeClaim", names.PVC, &corev1.PersistentVolumeClaim{}})
	}
	if class.Spec.Storage.Backend.Type == v1alpha1.StorageBackendTypeS3 {
		checks = append(checks, resourceCheck{"Secret", names.SnapshotSecret, &corev1.Secret{}})
	}
	// The NetworkPolicy is expected only under Restricted isolation (#31);
	// under Open isolation its absence is the correct state (and a stale
	// policy is deleted by ensureResources).
	if class.Spec.Network.Isolation == v1alpha1.NetworkIsolationRestricted {
		checks = append(checks, resourceCheck{"NetworkPolicy", names.NetworkPolicy, &networkingv1.NetworkPolicy{}})
	}

	pvc, ok, err := r.observeResources(ctx, env, checks, &f)
	if err != nil || !ok {
		return f, err
	}

	if pvc != nil && pvc.Status.Phase == corev1.ClaimLost {
		f.ResourcesProblem = fmt.Sprintf("workspace PVC %s is Lost", pvc.Name)
		return f, nil
	}

	f.ResourcesReady = true
	return f, nil
}

// observeProbe fills in the four wait-probe facts (#30) by running the
// evaluator against the environment's declared wait, when one exists and the
// evaluator is wired up. A nil r.Probes leaves the facts at their safe zero
// reading (ProbeObserved=false -> WaitSatisfied=Unknown/ProbeNotEvaluated),
// which is exactly the honest state before #30 ships. The evaluator's own
// skip path (the backoff window) reports ProbeObserved=true with a nil
// attempt, so status.probeAttempt -- and its NextEligibleAt requeue -- is
// preserved untouched.
func (r *Reconciler) observeProbe(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass, creds render.Credentials, f *lifecycle.ClusterFacts) {
	if r.Probes == nil {
		return
	}
	if env.Status.Phase != v1alpha1.PhaseWaiting || env.Status.WaitFor == nil {
		return
	}
	pf, attempt, err := r.Probes.Evaluate(ctx, env, class, creds)
	if err != nil {
		// Evaluate's error is reserved for failures that must fail the
		// reconcile; none exist today. Swallow defensively so a probe bug can
		// never wedge the reconcile loop.
		return
	}
	f.ProbeObserved = pf.ProbeObserved
	f.WaitProbeSatisfied = pf.WaitProbeSatisfied
	if pf.WaitProbeFailure != nil {
		f.WaitProbeFailure = &lifecycle.StepFailure{
			Reason:  pf.WaitProbeFailure.Reason,
			Message: truncateMessage(pf.WaitProbeFailure.Message, podFailureMessageBytes),
		}
	}
	f.ProbeAttempt = attempt
}

// observeResources looks up every child in checks, filling in f.ResourcesProblem
// and returning ok=false on the first missing or foreign-owned object. On
// success it returns the observed PVC (for the caller's Bound/Lost check).
func (r *Reconciler) observeResources(ctx context.Context, env *v1alpha1.SandboxEnvironment, checks []resourceCheck, f *lifecycle.ClusterFacts) (*corev1.PersistentVolumeClaim, bool, error) {
	var pvc *corev1.PersistentVolumeClaim
	for _, c := range checks {
		err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: c.name}, c.obj)
		if apierrors.IsNotFound(err) {
			f.ResourcesProblem = fmt.Sprintf("waiting for %s %s", c.kind, c.name)
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if !ownedByEnv(c.obj, env) {
			f.ResourcesProblem = fmt.Sprintf("%s %s is owned by another object", c.kind, c.name)
			return nil, false, nil
		}
		if p, ok := c.obj.(*corev1.PersistentVolumeClaim); ok {
			pvc = p
		}
	}
	return pvc, true, nil
}

// observeQueuePosition fills in the advisory queue position from the
// CACHED client: it only feeds a condition message, so slight staleness is
// harmless and a List against the informer costs no API call. (The
// authoritative admission decision itself uses a live, uncached read -- see
// SlotScheduler.) A List error is swallowed: position stays 0, the message
// stays empty, and the reconcile proceeds normally -- queue position must
// never fail a reconcile.
func (r *Reconciler) observeQueuePosition(ctx context.Context, env *v1alpha1.SandboxEnvironment, f *lifecycle.ClusterFacts) {
	var list v1alpha1.SandboxEnvironmentList
	if err := r.List(ctx, &list, client.InNamespace(r.WatchNamespace)); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("queue position unavailable", "error", err.Error())
		return
	}
	_, candidates := scheduler.Partition(list.Items)
	f.QueuePosition, f.QueueDepth = scheduler.QueuePosition(candidates, env.UID)
}

// ownedByEnv reports whether obj's controller owner reference matches env's
// UID. This is the actual collision-freedom enforcement for the (rare, hash
// suffix notwithstanding) case of two environments' truncated names
// colliding, or of a manually pre-created object squatting on a name this
// operator would otherwise render: identical names alone are never treated
// as "this environment's child".
func ownedByEnv(obj metav1.Object, env *v1alpha1.SandboxEnvironment) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == env.UID {
			return true
		}
	}
	return false
}

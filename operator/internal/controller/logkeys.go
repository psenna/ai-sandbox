package controller

import (
	"context"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// Structured logging keys and the verbosity convention this package follows
// (#33):
//
//   - Error (log.Error(err, ...)): a failed operation a human may need to
//     act on.
//   - V(0)/Info: rare, significant, irreversible events -- process start/
//     stop, permanent storage deletion (retentiongc.go's own archive/orphan
//     deletion and dry-run lines are deliberately audit-grade Info and are
//     NOT touched by #33 -- see that file's doc comments).
//   - V(1): per-reconcile/per-pass detail, including phase transitions.
//     #33 adds exactly one new V(1) line for this: the status-write path's
//     phase-transition log (events.go's observeTransition).
//
// #33 adds no new V(0) lines: new default-verbosity operator output comes
// from Kubernetes Events (events.go), not logs -- an Event is durable,
// queryable with `kubectl get events`, and scoped to the object it concerns,
// which a log line is not.
const (
	LogKeyEnvironment = "environment"
	LogKeyNamespace   = "namespace"
	LogKeyUID         = "uid"
	LogKeyPhase       = "phase"
	LogKeySnapshotSeq = "snapshotSeq"
	LogKeyClass       = "class"
	LogKeyChildKind   = "childKind"
	LogKeyChildName   = "childName"
)

// envLogValues returns the key/value pairs logForEnv enriches the reconcile
// logger with: uid, phase, and (when a snapshot exists) snapshotSeq.
// Deliberately does NOT include LogKeyEnvironment/LogKeyNamespace:
// controller-runtime's own default LogConstructor (see
// SetupWithManager/ctrl.NewControllerManagedBy) already injects "namespace"
// and "name" into the logger ctrl.LoggerFrom(ctx) returns at the point
// Reconcile runs, for every reconcile of this controller -- duplicating them
// under a second key would double every line's namespace/name pair rather
// than adding information. Runnables (WarmCacheGC, RetentionGC,
// CNIProbeRunnable) have no such per-object injection -- their own log
// sites use LogKeyEnvironment/LogKeyNamespace explicitly instead.
func envLogValues(env *v1alpha1.SandboxEnvironment) []any {
	vals := []any{LogKeyUID, env.UID, LogKeyPhase, env.Status.Phase}
	if s := env.Status.Snapshot; s != nil {
		vals = append(vals, LogKeySnapshotSeq, s.Seq)
	}
	return vals
}

// logForEnv returns ctx's logger enriched with envLogValues(env). Called
// once in Reconcile, immediately after the successful Get, then put back
// into ctx (ctrl.LoggerInto) so every downstream call in that reconcile
// inherits the enrichment without repeating it.
func logForEnv(ctx context.Context, env *v1alpha1.SandboxEnvironment) logr.Logger {
	return ctrl.LoggerFrom(ctx).WithValues(envLogValues(env)...)
}

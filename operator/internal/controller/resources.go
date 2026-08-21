package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
	"github.com/psenna/ai-sandbox/operator/internal/storage"
)

// fieldManager is the stable field manager name used for every server-side
// apply this reconciler performs.
const fieldManager = render.FieldManager

// resolveCredentials reads class-referenced Secrets from r.ClassSecretNamespace
// and projects the values internal/render needs. Returns a zero Credentials
// (not an error) when the class declares no gitProxy service -- that is a
// valid configuration, not a problem. Any returned error names
// namespace/name/key only -- NEVER a value, so it is always safe to surface
// verbatim in a condition message or a log line.
//
// It also resolves the S3 snapshot credentials (storage/doc.go's gap G1):
// CredentialsSecretRef is a single-key SecretKeyRef, but S3 needs two or
// three values, so .Key is IGNORED for S3 and the FIXED data keys
// internal/storage exports are read instead. A class declaring
// backend.type: s3 whose credentials Secret is missing or malformed is a
// REAL error here (unlike the gitProxy case above) -- it blocks the
// environment at Pending with a clear ResourcesProblem instead of silently
// proceeding toward a freeze that could never work.
func (r *Reconciler) resolveCredentials(ctx context.Context, class *v1alpha1.SandboxClass) (render.Credentials, error) {
	var creds render.Credentials

	if gp := class.Spec.Services.GitProxy; gp != nil {
		key := gp.TokenSecretRef.Key
		if key == "" {
			key = "token" // matches SecretKeyRef's CRD default
		}

		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.ClassSecretNamespace, Name: gp.TokenSecretRef.Name}, &secret); err != nil {
			return render.Credentials{}, fmt.Errorf("reading class-referenced secret %s/%s (key %q): %w", r.ClassSecretNamespace, gp.TokenSecretRef.Name, key, err)
		}

		val, ok := secret.Data[key]
		if !ok || len(val) == 0 {
			return render.Credentials{}, fmt.Errorf("secret %s/%s has no non-empty key %q", r.ClassSecretNamespace, gp.TokenSecretRef.Name, key)
		}

		creds.GitProxyToken = string(val)
	}

	if b := class.Spec.Storage.Backend; b.Type == v1alpha1.StorageBackendTypeS3 && b.S3 != nil {
		var sec corev1.Secret
		ref := b.S3.CredentialsSecretRef
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.ClassSecretNamespace, Name: ref.Name}, &sec); err != nil {
			return render.Credentials{}, fmt.Errorf("reading class-referenced s3 credentials secret %s/%s: %w", r.ClassSecretNamespace, ref.Name, err)
		}
		ak := sec.Data[storage.SecretKeyAccessKeyID]
		sk := sec.Data[storage.SecretKeySecretAccessKey]
		if len(ak) == 0 || len(sk) == 0 {
			return render.Credentials{}, fmt.Errorf("secret %s/%s must have non-empty keys %q and %q",
				r.ClassSecretNamespace, ref.Name, storage.SecretKeyAccessKeyID, storage.SecretKeySecretAccessKey)
		}
		creds.SnapshotAccessKeyID = string(ak)
		creds.SnapshotSecretAccessKey = string(sk)
		creds.SnapshotSessionToken = string(sec.Data[storage.SecretKeySessionToken]) // optional
	}

	return creds, nil
}

// archiveBackend builds the S3 storage.Backend for class's storage
// configuration, reusing the exact credential resolution every other S3
// call site (ensureSnapshotJob, ensureArchiveJob, renderFor) already goes
// through. Returns an error for a non-S3 class -- callers must check
// class.Spec.Storage.Backend.Type themselves first; this is a defensive
// re-check, not the primary guard -- or when credentials cannot be resolved
// or the S3 client cannot be constructed. NewS3 dials nothing itself, so a
// genuinely unreachable endpoint is NOT detected here, only obviously-bad
// configuration (a missing/malformed credentials Secret, an empty bucket).
func (r *Reconciler) archiveBackend(ctx context.Context, class *v1alpha1.SandboxClass) (storage.Backend, error) {
	b := class.Spec.Storage.Backend
	if b.Type != v1alpha1.StorageBackendTypeS3 || b.S3 == nil {
		return nil, fmt.Errorf("archiveBackend: class %s does not declare an s3 storage backend", class.Name)
	}
	creds, err := r.resolveCredentials(ctx, class)
	if err != nil {
		return nil, err
	}
	forcePathStyle := true
	if b.S3.ForcePathStyle != nil {
		forcePathStyle = *b.S3.ForcePathStyle
	}
	be, err := storage.NewS3(storage.S3Config{
		Endpoint:       b.S3.Endpoint,
		Bucket:         b.S3.Bucket,
		Region:         b.S3.Region,
		ForcePathStyle: forcePathStyle,
	}, storage.Credentials{
		AccessKeyID:     creds.SnapshotAccessKeyID,
		SecretAccessKey: storage.Secret(creds.SnapshotSecretAccessKey),
		SessionToken:    storage.Secret(creds.SnapshotSessionToken),
	})
	if err != nil {
		return nil, fmt.Errorf("building s3 backend: %w", err)
	}
	return be, nil
}

// buildS3BackendForClass builds the S3 storage.Backend for class directly
// from a plain client.Client and secretNamespace, with no Reconciler
// involved. It exists for RetentionGC (retentiongc.go), which -- like
// WarmCacheGC -- is a manager.Runnable, not a reconciler, and so has no
// Reconciler receiver to hang archiveBackend off of. Reads only the S3
// credentials Secret (never the gitProxy one resolveCredentials also
// handles), since that is all a backend construction ever needs.
func buildS3BackendForClass(ctx context.Context, c client.Client, secretNamespace string, class *v1alpha1.SandboxClass) (storage.Backend, error) {
	b := class.Spec.Storage.Backend
	if b.Type != v1alpha1.StorageBackendTypeS3 || b.S3 == nil {
		return nil, fmt.Errorf("buildS3BackendForClass: class %s does not declare an s3 storage backend", class.Name)
	}
	var sec corev1.Secret
	ref := b.S3.CredentialsSecretRef
	if err := c.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: ref.Name}, &sec); err != nil {
		return nil, fmt.Errorf("reading class-referenced s3 credentials secret %s/%s: %w", secretNamespace, ref.Name, err)
	}
	ak := sec.Data[storage.SecretKeyAccessKeyID]
	sk := sec.Data[storage.SecretKeySecretAccessKey]
	if len(ak) == 0 || len(sk) == 0 {
		return nil, fmt.Errorf("secret %s/%s must have non-empty keys %q and %q",
			secretNamespace, ref.Name, storage.SecretKeyAccessKeyID, storage.SecretKeySecretAccessKey)
	}
	forcePathStyle := true
	if b.S3.ForcePathStyle != nil {
		forcePathStyle = *b.S3.ForcePathStyle
	}
	be, err := storage.NewS3(storage.S3Config{
		Endpoint:       b.S3.Endpoint,
		Bucket:         b.S3.Bucket,
		Region:         b.S3.Region,
		ForcePathStyle: forcePathStyle,
	}, storage.Credentials{
		AccessKeyID:     string(ak),
		SecretAccessKey: storage.Secret(sk),
		SessionToken:    storage.Secret(sec.Data[storage.SecretKeySessionToken]),
	})
	if err != nil {
		return nil, fmt.Errorf("building s3 backend: %w", err)
	}
	return be, nil
}

// renderFor resolves credentials and network peers, then renders every child
// object for env against class. Rendering is pure and cheap -- callers must
// re-render on every reconcile rather than caching or reusing a
// previously-applied object (see applyOne's doc comment on why a rendered
// object must never be reused after an Apply call).
func (r *Reconciler) renderFor(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (render.Objects, error) {
	creds, err := r.resolveCredentials(ctx, class)
	if err != nil {
		return render.Objects{}, err
	}
	netPeers, err := r.resolveNetworkPeers(ctx, class)
	if err != nil {
		return render.Objects{}, err
	}
	specHash, err := storage.SpecHash(&class.Spec, &env.Spec)
	if err != nil {
		return render.Objects{}, fmt.Errorf("computing spec hash: %w", err)
	}
	return render.Render(render.Inputs{
		Env:         env,
		Class:       class,
		Credentials: creds,
		ClusterID:   r.ClusterID,
		SpecHash:    specHash,
		Network:     netPeers,
	})
}

// ensureResources is the ActionEnsureResources handler: render every child
// object and apply it via server-side apply, reclaiming ownership of the
// fields this operator manages. Permanent misconfiguration (a render error,
// or the API server rejecting an immutable-field change on an existing
// object) is logged at V(1) without any value and does NOT fail the
// reconcile -- the observer reports the resulting non-ready state via
// ClusterFacts.ResourcesProblem on the next pass, rather than hot-looping a
// reconcile that can never succeed.
func (r *Reconciler) ensureResources(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) error {
	if class == nil {
		return nil
	}
	log := ctrl.LoggerFrom(ctx)

	objs, err := r.renderFor(ctx, env, class)
	if err != nil {
		log.V(1).Info("child resources not renderable", "reason", err.Error())
		return nil
	}

	names := render.ChildNames(env.Name)

	if err := r.applyOne(ctx, env, "ServiceAccount", client.ObjectKey{Namespace: env.Namespace, Name: names.ServiceAccount}, &corev1.ServiceAccount{}, objs.ServiceAccount); err != nil {
		return err
	}
	if err := r.applyOne(ctx, env, "Role", client.ObjectKey{Namespace: env.Namespace, Name: names.Role}, &rbacv1.Role{}, objs.Role); err != nil {
		return err
	}
	if err := r.applyOne(ctx, env, "RoleBinding", client.ObjectKey{Namespace: env.Namespace, Name: names.RoleBinding}, &rbacv1.RoleBinding{}, objs.RoleBinding); err != nil {
		return err
	}
	// The PVC apply is skipped while Waiting (#29): nothing mounts the
	// workspace while frozen, and WarmCacheGC's whole purpose is to delete a
	// frozen environment's PVC once its snapshot is older than the class's
	// warmCacheTTL -- re-applying it here (withEnsureResources prepends
	// ActionEnsureResources for every non-terminal phase, Waiting included)
	// would recreate the just-deleted PVC within a second and defeat the GC
	// entirely. The PVC is unconditionally re-applied on the very next
	// Waiting->Ready->Restoring pass, since withEnsureResources prepends
	// ActionEnsureResources before ActionEnsurePod in the SAME Decision.
	if env.Status.Phase != v1alpha1.PhaseWaiting {
		if err := r.applyOne(ctx, env, "PersistentVolumeClaim", client.ObjectKey{Namespace: env.Namespace, Name: names.PVC}, &corev1.PersistentVolumeClaim{}, objs.PVC); err != nil {
			return err
		}
	}
	if err := r.applyOne(ctx, env, "ConfigMap", client.ObjectKey{Namespace: env.Namespace, Name: names.ConfigMap}, &corev1.ConfigMap{}, objs.ConfigMap); err != nil {
		return err
	}
	if err := r.applyOne(ctx, env, "Secret", client.ObjectKey{Namespace: env.Namespace, Name: names.Secret}, &corev1.Secret{}, objs.Secret); err != nil {
		return err
	}
	if objs.SnapshotSecret != nil {
		if err := r.applyOne(ctx, env, "Secret", client.ObjectKey{Namespace: env.Namespace, Name: names.SnapshotSecret}, &corev1.Secret{}, objs.SnapshotSecret); err != nil {
			return err
		}
	}
	// The NetworkPolicy is nil for Open isolation (or an unrenderable class):
	// apply it when present, delete any stale policy otherwise. The delete is
	// ownership-guarded and idempotent, exactly like deletePod.
	if objs.NetworkPolicy != nil {
		if err := r.applyOne(ctx, env, "NetworkPolicy", client.ObjectKey{Namespace: env.Namespace, Name: names.NetworkPolicy}, &networkingv1.NetworkPolicy{}, objs.NetworkPolicy); err != nil {
			return err
		}
	} else {
		r.deleteOwnedChild(ctx, env, "NetworkPolicy", names.NetworkPolicy, &networkingv1.NetworkPolicy{})
	}
	return nil
}

// deleteOwnedChild deletes a child object this environment owns, idempotently
// (no-op if already gone or already terminating) and only when the existing
// object's controller owner reference points at env -- a foreign object
// squatting the name is never touched. Mirrors deletePod's ownership-guarded
// delete for the non-Pod children that can legitimately disappear (the
// NetworkPolicy when a class switches from Restricted to Open isolation).
func (r *Reconciler) deleteOwnedChild(ctx context.Context, env *v1alpha1.SandboxEnvironment, kind, name string, obj client.Object) {
	log := ctrl.LoggerFrom(ctx)
	key := client.ObjectKey{Namespace: env.Namespace, Name: name}
	if err := r.Get(ctx, key, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			log.V(1).Info("delete check failed", LogKeyChildKind, kind, LogKeyChildName, name, "error", err.Error())
		}
		return
	}
	if !ownedByEnv(obj, env) {
		log.V(1).Info("skipping delete: object is not owned by this environment", LogKeyChildKind, kind, LogKeyChildName, name)
		return
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return
	}
	uid := obj.GetUID()
	if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		log.V(1).Info("delete failed", LogKeyChildKind, kind, LogKeyChildName, name, "error", err.Error())
	}
}

// applyOne applies a single rendered child object, guarded by an explicit
// pre-apply ownership check: if an object with this name already exists and
// its controller owner reference does not point at env, the apply is
// skipped entirely rather than attempted. This is required, not just
// belt-and-braces: server-side apply with ForceOwnership reclaims *field*
// ownership from whatever field manager currently holds a given field, but
// it does not know or care about a Kubernetes ownerReference -- an apply
// against a same-named foreign object would happily rewrite its labels and
// ownerReferences to point at this environment, silently annexing an
// unrelated object. The observer (observeCluster) performs the same
// ownership check, but only after the fact; this guard prevents the
// clobbering from happening in the first place.
//
// existing is decoded into (a struct pointer of the concrete type), then
// discarded; ac is the apply-configuration to apply, and is mutated in
// place by a successful Apply call (the client decodes the server's
// response back into it) -- callers must never reuse ac afterwards.
func (r *Reconciler) applyOne(ctx context.Context, env *v1alpha1.SandboxEnvironment, kind string, key client.ObjectKey, existing client.Object, ac runtime.ApplyConfiguration) error {
	log := ctrl.LoggerFrom(ctx)

	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		if !ownedByEnv(existing, env) {
			log.V(1).Info("skipping apply: existing object is not owned by this environment", LogKeyChildKind, kind, LogKeyChildName, key.Name)
			return nil
		}
	case apierrors.IsNotFound(err):
		// Nothing to guard against; Apply will create it.
	default:
		return fmt.Errorf("checking existing %s %s: %w", kind, key.Name, err)
	}

	if err := r.Apply(ctx, ac, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			log.V(1).Info("child resource apply rejected (permanent, e.g. immutable field)", LogKeyChildKind, kind, LogKeyChildName, key.Name)
			return nil
		}
		return fmt.Errorf("applying %s %s: %w", kind, key.Name, err)
	}
	return nil
}

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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

// renderFor resolves credentials and renders every child object for env
// against class. Rendering is pure and cheap -- callers must re-render on
// every reconcile rather than caching or reusing a previously-applied
// object (see applyOne's doc comment on why a rendered object must never be
// reused after an Apply call).
func (r *Reconciler) renderFor(ctx context.Context, env *v1alpha1.SandboxEnvironment, class *v1alpha1.SandboxClass) (render.Objects, error) {
	creds, err := r.resolveCredentials(ctx, class)
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
	if err := r.applyOne(ctx, env, "PersistentVolumeClaim", client.ObjectKey{Namespace: env.Namespace, Name: names.PVC}, &corev1.PersistentVolumeClaim{}, objs.PVC); err != nil {
		return err
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
	return nil
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
			log.V(1).Info("skipping apply: existing object is not owned by this environment", "kind", kind, "name", key.Name)
			return nil
		}
	case apierrors.IsNotFound(err):
		// Nothing to guard against; Apply will create it.
	default:
		return fmt.Errorf("checking existing %s %s: %w", kind, key.Name, err)
	}

	if err := r.Apply(ctx, ac, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			log.V(1).Info("child resource apply rejected (permanent, e.g. immutable field)", "kind", kind, "name", key.Name)
			return nil
		}
		return fmt.Errorf("applying %s %s: %w", kind, key.Name, err)
	}
	return nil
}

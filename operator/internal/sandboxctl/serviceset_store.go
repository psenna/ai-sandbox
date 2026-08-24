package sandboxctl

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// serviceSetApplier upserts the environment's ServiceSet CR. The only real
// implementation is *serviceSetStore; handler tests use a fake.
type serviceSetApplier interface {
	Upsert(ctx context.Context, spec v1alpha1.ServiceSetSpec) error
}

// serviceSetStore upserts the ServiceSet CR named after the environment,
// owned by the environment, with Spec.EnvironmentName == env.Name. The CR name
// == env.Name (one per environment) and the workspace PVC the runtimes mount is
// <env.Name>-workspace (owned by the env controller, referenced here by name).
type serviceSetStore struct {
	c   client.Client
	env types.NamespacedName // the SandboxEnvironment key (Name == ServiceSet name)
}

func newServiceSetStore(c client.Client, env EnvironmentRef) *serviceSetStore {
	return &serviceSetStore{c: c, env: types.NamespacedName{Name: env.Name, Namespace: env.Namespace}}
}

// Upsert sets spec.EnvironmentName from the store's env identity, Gets the
// SandboxEnvironment for its UID (the OwnerReference needs it; the env get is
// granted by the sidecar Role), then creates-or-updates the ServiceSet CR
// named env.Name. Update is by Get-then-Update (the sidecar Role grants
// get+create+update on servicesets resourceNames=[env.Name]).
func (s *serviceSetStore) Upsert(ctx context.Context, spec v1alpha1.ServiceSetSpec) error {
	spec.EnvironmentName = s.env.Name

	var env v1alpha1.SandboxEnvironment
	if err := s.c.Get(ctx, s.env, &env); err != nil {
		return fmt.Errorf("getting environment %s for ServiceSet ownership: %w", s.env.Name, err)
	}
	ownerRef := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "SandboxEnvironment",
		Name:               env.Name,
		UID:                env.UID,
		Controller:         k8sptr.To(true),
		BlockOwnerDeletion: k8sptr.To(true),
	}

	key := types.NamespacedName{Name: s.env.Name, Namespace: s.env.Namespace}
	var existing v1alpha1.ServiceSet
	err := s.c.Get(ctx, key, &existing)
	switch {
	case err == nil:
		existing.Spec = spec
		return s.c.Update(ctx, &existing)
	case apierrors.IsNotFound(err):
		ss := &v1alpha1.ServiceSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:            s.env.Name,
				Namespace:       s.env.Namespace,
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: spec,
		}
		return s.c.Create(ctx, ss)
	default:
		return fmt.Errorf("getting ServiceSet %s: %w", s.env.Name, err)
	}
}

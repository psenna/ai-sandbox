package operator

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/psenna/ai-sandbox/operator/internal/config"
	"github.com/psenna/ai-sandbox/operator/internal/controller"
)

// SetupControllers registers every controller with mgr. Separate from New so
// manager construction stays testable without a cluster (manager_test.go
// starts a manager against a fake host; a registered controller's informer
// would never sync there).
func SetupControllers(mgr manager.Manager, cfg config.Config) error {
	return (&controller.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr)
}

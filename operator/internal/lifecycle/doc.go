// Package lifecycle implements the SandboxEnvironment phase state machine as
// a pure function of observed facts: Next(current, observed, now) Decision.
//
// This package is the seam between "what the cluster looks like" and "what
// the operator should do about it". It is deliberately isolated from
// Kubernetes client machinery so the entire state machine is table-testable
// with no cluster, no informer, no client, and no wall-clock dependency
// (time is always passed in).
//
// Contract: this package (including its tests) MUST import only "time",
// "fmt", "k8s.io/api/core/v1" (as corev1), "k8s.io/apimachinery/pkg/apis/meta/v1"
// (as metav1), "k8s.io/apimachinery/pkg/api/meta" (as apimeta), and
// "github.com/psenna/ai-sandbox/operator/api/v1alpha1". It must NEVER import
// "sigs.k8s.io/controller-runtime" or any other client-runtime package. The
// internal/controller package is the only place that talks to a real
// cluster; it calls Next and Apply from here and does the I/O.
package lifecycle

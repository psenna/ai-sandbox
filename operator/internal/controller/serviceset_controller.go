package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
	"github.com/psenna/ai-sandbox/operator/internal/render"
)

// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.psenna.dev,resources=servicesets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// ServiceSetReconciler reconciles a ServiceSet to native Pods/Services/PVCs.
type ServiceSetReconciler struct {
	client.Client
}

const (
	// specHashAnnotation records the hash of the pod-affecting spec; a mismatch
	// triggers Pod recreation (Task 6).
	specHashAnnotation = "ai-sandbox.io/spec-hash"
	// owner labels identify a ServiceSet's children (used by prune + Service selector).
	labelServiceset = "ai-sandbox.io/serviceset"
	labelEntry      = "ai-sandbox.io/entry"
	labelKind       = "ai-sandbox.io/kind"
)

func (r *ServiceSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ss sandboxv1alpha1.ServiceSet
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Defense-in-depth guard for the #2 defect: a service and runtime sharing
	// a name both target Pod/<name>, which would storm the reconciler (two
	// ensurePod calls delete+recreate each other's pod in a loop). Admission
	// (Task 2/5) rejects this at the control API, but a hand-crafted CR that
	// bypasses admission must still fail safe. Detect the collision BEFORE
	// reconciling children, write Ready=False reason DuplicateEntryName, and
	// return nil -- no children created, no storm. The user fixes the spec; the
	// next reconcile proceeds.
	if dup := duplicateEntryName(&ss); dup != "" {
		if err := r.writeDuplicateStatus(ctx, &ss, dup); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // no child reconcile: a colliding name would
		// storm (two ensurePod calls targeting the same Pod/<name>), so we
		// refuse to create children and wait for the spec to be fixed. Stale
		// children from before the collision are owned by the env and GC'd on
		// env deletion; the next reconcile after the fix prunes + reconciles.
	}
	for i := range ss.Spec.Services {
		if err := r.reconcileService(ctx, &ss, &ss.Spec.Services[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	for i := range ss.Spec.Runtimes {
		if err := r.reconcileRuntime(ctx, &ss, &ss.Spec.Runtimes[i]); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.writeStatus(ctx, &ss); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneChildren(ctx, &ss); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ServiceSetReconciler) writeStatus(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) error {
	// Capture the live object's status before mutation so the patch sends only
	// the status diff (not spec/metadata), avoiding resourceVersion churn.
	base := ss.DeepCopy()
	ready := r.computeReady(ctx, ss)
	entries := make([]sandboxv1alpha1.EntryStatus, 0, len(ss.Spec.Services)+len(ss.Spec.Runtimes))
	allReady := true
	for _, s := range ss.Spec.Services {
		ok, reason := ready(s.Name)
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: s.Name, Kind: "service", Ready: ok, Reason: reason})
		if !ok {
			allReady = false
		}
	}
	for _, rt := range ss.Spec.Runtimes {
		ok, reason := ready(rt.Name)
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: rt.Name, Kind: "runtime", Ready: ok, Reason: reason})
		if !ok {
			allReady = false
		}
	}
	cond := metav1.Condition{Type: "Ready", LastTransitionTime: metav1.Now(), ObservedGeneration: ss.Generation}
	if allReady {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "AllEntriesReady"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "EntriesNotReady"
	}
	apimeta.SetStatusCondition(&ss.Status.Conditions, cond)
	ss.Status.Entries = entries
	return r.Status().Patch(ctx, ss, client.MergeFrom(base))
}

// duplicateEntryName returns the first entry name that appears more than once
// across services+runtimes (the #2 defect: the CRD pins uniqueness within each
// list only, so a service+runtime sharing a name passes the API server but would
// storm the reconciler). "" means no collision.
func duplicateEntryName(ss *sandboxv1alpha1.ServiceSet) string {
	count := map[string]int{}
	for _, s := range ss.Spec.Services {
		count[s.Name]++
	}
	for _, rt := range ss.Spec.Runtimes {
		count[rt.Name]++
	}
	for name, n := range count {
		if n > 1 {
			return name
		}
	}
	return ""
}

// writeDuplicateStatus marks every entry NotReady and the ServiceSet Ready=False
// with reason DuplicateEntryName, without reconciling any children. It is the
// defense-in-depth counterpart to admission's ValidateServiceSet: a hand-crafted
// CR that bypasses admission still fails safe rather than storming.
func (r *ServiceSetReconciler) writeDuplicateStatus(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, dup string) error {
	base := ss.DeepCopy()
	entries := make([]sandboxv1alpha1.EntryStatus, 0, len(ss.Spec.Services)+len(ss.Spec.Runtimes))
	for _, s := range ss.Spec.Services {
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: s.Name, Kind: "service", Ready: false, Reason: "DuplicateEntryName"})
	}
	for _, rt := range ss.Spec.Runtimes {
		entries = append(entries, sandboxv1alpha1.EntryStatus{Name: rt.Name, Kind: "runtime", Ready: false, Reason: "DuplicateEntryName"})
	}
	apimeta.SetStatusCondition(&ss.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "DuplicateEntryName",
		Message:            fmt.Sprintf("entry name %q is duplicated across services+runtimes", dup),
		LastTransitionTime: metav1.Now(), ObservedGeneration: ss.Generation,
	})
	ss.Status.Entries = entries
	return r.Status().Patch(ctx, ss, client.MergeFrom(base))
}

// pruneChildren lists Pods/Services/PVCs in the namespace labeled
// labelServiceset=<ss.Name> (i.e. owned by this ServiceSet) and deletes any
// whose labelEntry name is not in the current services+runtimes set. Run last
// in Reconcile so children belonging to entries removed from the spec are
// garbage-collected. A data PVC carries its service's entry label (set via
// entryLabels in ensurePVC), so it prunes alongside the service's Pod/Service.
func (r *ServiceSetReconciler) pruneChildren(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) error {
	want := map[string]struct{}{}
	for _, s := range ss.Spec.Services {
		want[s.Name] = struct{}{}
	}
	for _, rt := range ss.Spec.Runtimes {
		want[rt.Name] = struct{}{}
	}
	selector := k8slabels.SelectorFromSet(map[string]string{labelServiceset: ss.Name})

	listOpts := []client.ListOption{client.InNamespace(ss.Namespace), client.MatchingLabelsSelector{Selector: selector}}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, listOpts...); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if _, ok := want[p.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, listOpts...); err != nil {
		return err
	}
	for i := range svcs.Items {
		s := &svcs.Items[i]
		if _, ok := want[s.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, listOpts...); err != nil {
		return err
	}
	for i := range pvcs.Items {
		v := &pvcs.Items[i]
		// A data PVC carries its service's entry label, so it prunes alongside
		// the service's Pod/Service.
		if _, ok := want[v.Labels[labelEntry]]; ok {
			continue
		}
		if err := r.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// readyMap is a closure that resolves an entry's readiness (pod ready + deps ready).
type readyMap func(name string) (bool, string)

func (r *ServiceSetReconciler) computeReady(ctx context.Context, ss *sandboxv1alpha1.ServiceSet) readyMap {
	// first pass: pod readiness by name (independent of deps)
	podReady := map[string]bool{}
	for _, s := range ss.Spec.Services {
		podReady[s.Name] = r.isPodReady(ctx, ss.Namespace, s.Name)
	}
	for _, rt := range ss.Spec.Runtimes {
		podReady[rt.Name] = r.isPodReady(ctx, ss.Namespace, rt.Name)
	}
	depMap := map[string][]string{}
	for _, s := range ss.Spec.Services {
		depMap[s.Name] = s.DependsOn
	}
	for _, rt := range ss.Spec.Runtimes {
		depMap[rt.Name] = rt.DependsOn
	}
	resolve := func(name string) (bool, string) {
		// Path-based cycle guard: seen tracks the names on the CURRENT DFS path,
		// not all-ever-visited. Backtracking (delete after the dep loop) keeps a
		// diamond (a→b→d, a→c→d) from false-positiving as a cycle: once d resolves
		// via b and the b-branch unwinds, d leaves seen, so reaching d via c is a
		// normal revisit, not a cycle. Each top-level resolve(name) call from
		// writeStatus starts with a fresh seen, so path state never leaks across
		// entries.
		seen := map[string]struct{}{}
		var visit func(n string) (bool, string)
		visit = func(n string) (bool, string) {
			if _, ok := seen[n]; ok {
				return false, "CircularDependency"
			}
			if !podReady[n] {
				return false, "PodNotReady"
			}
			seen[n] = struct{}{}
			for _, dep := range depMap[n] {
				if ok, _ := visit(dep); !ok {
					return false, "DependenciesNotReady"
				}
			}
			delete(seen, n)
			return true, ""
		}
		return visit(name)
	}
	return resolve
}

func (r *ServiceSetReconciler) isPodReady(ctx context.Context, ns, name string) bool {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &pod); err != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ServiceSetReconciler) reconcileRuntime(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, rt *sandboxv1alpha1.RuntimeSpec) error {
	labels := entryLabels(ss, rt.Name, "runtime")
	mount := true
	if rt.MountWorkspace != nil {
		mount = *rt.MountWorkspace
	}
	command := rt.Command
	if len(command) == 0 {
		command = []string{"sleep", "infinity"}
	}
	var mounts []corev1.VolumeMount
	if mount {
		mounts = []corev1.VolumeMount{{Name: ss.Spec.EnvironmentName + "-workspace", MountPath: "/workspace"}}
	}
	return r.ensurePod(ctx, ss, rt.Name, rt.Image, command, rt.Args, rt.Env, nil,
		rt.Resources, "", rt.RunAsUser, rt.Healthcheck, labels, mounts, nil)
}

func (r *ServiceSetReconciler) reconcileService(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, s *sandboxv1alpha1.ServiceSpec) error {
	labels := entryLabels(ss, s.Name, "service")
	if s.Storage != nil {
		if err := r.ensurePVC(ctx, ss, s.Name+"-data", s.Storage.Size, labels); err != nil {
			return fmt.Errorf("pvc %s: %w", s.Name, err)
		}
	}
	if err := r.ensureService(ctx, ss, s, labels); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	if err := r.ensurePod(ctx, ss, s.Name, s.Image, s.Command, s.Args, s.Env, s.EnvFromSecret,
		s.Resources, s.ImagePullPolicy, s.RunAsUser, s.Healthcheck, labels, serviceVolumeMounts(s), serviceEnvFrom(s)); err != nil {
		return fmt.Errorf("pod %s: %w", s.Name, err)
	}
	return nil
}

func entryLabels(ss *sandboxv1alpha1.ServiceSet, name, kind string) map[string]string {
	return map[string]string{
		labelServiceset:                  ss.Name,
		labelEntry:                       name,
		labelKind:                        kind,
		"sandbox.psenna.dev/environment": render.EnvironmentLabelValue(ss.Spec.EnvironmentName),
	}
}

func serviceVolumeMounts(s *sandboxv1alpha1.ServiceSpec) []corev1.VolumeMount {
	if s.Storage == nil {
		return nil
	}
	return []corev1.VolumeMount{{Name: s.Name + "-data", MountPath: s.Storage.MountPath}}
}

func serviceEnvFrom(s *sandboxv1alpha1.ServiceSpec) *corev1.EnvFromSource {
	if s.EnvFromSecret == nil {
		return nil
	}
	return &corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: *s.EnvFromSecret}}}
}

func (r *ServiceSetReconciler) ensurePVC(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, name, size string, labels map[string]string) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ss.Namespace}, &existing)
	if err == nil {
		return nil // retained by name across recreates; do not mutate (Task 6 may grow size, left out of Plan 1 scope)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ss.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  pvcVolumeMode(),
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}
	return r.Create(ctx, pvc)
}

func pvcVolumeMode() *corev1.PersistentVolumeMode {
	m := corev1.PersistentVolumeFilesystem
	return &m
}

func (r *ServiceSetReconciler) ensureService(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, s *sandboxv1alpha1.ServiceSpec, labels map[string]string) error {
	// A Service with no ports has nothing to expose; skip creation (a portless
	// ClusterIP Service is rejected by the API server anyway). If a Service
	// exists from a prior spec that had ports, remove it now — pruneChildren
	// won't, because the entry is still in the spec (labelEntry still in `want`).
	// Tolerate IsNotFound.
	if len(s.Ports) == 0 {
		var existing corev1.Service
		if err := r.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: ss.Namespace}, &existing); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return r.Delete(ctx, &existing)
	}
	var existing corev1.Service
	key := types.NamespacedName{Name: s.Name, Namespace: ss.Namespace}
	err := r.Get(ctx, key, &existing)
	svcType := corev1.ServiceTypeClusterIP
	var nodePort int32
	if s.Expose != nil && len(s.Ports) > 0 {
		svcType = corev1.ServiceTypeNodePort
		nodePort = *s.Expose
	}
	desiredPorts := make([]corev1.ServicePort, 0, len(s.Ports))
	for i, p := range s.Ports {
		sp := corev1.ServicePort{Name: fmt.Sprintf("p%d", i), Port: p, TargetPort: intstr.FromInt32(p)}
		if nodePort != 0 && i == 0 {
			sp.NodePort = nodePort
		}
		desiredPorts = append(desiredPorts, sp)
	}
	if err == nil {
		existing.Spec.Ports = desiredPorts
		existing.Spec.Type = svcType
		existing.Spec.Selector = map[string]string{labelServiceset: ss.Name, labelEntry: s.Name}
		if existing.Labels == nil {
			existing.Labels = labels
		}
		return r.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: ss.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Ports:    desiredPorts,
			Selector: map[string]string{labelServiceset: ss.Name, labelEntry: s.Name},
		},
	}
	return r.Create(ctx, svc)
}

func (r *ServiceSetReconciler) ensurePod(ctx context.Context, ss *sandboxv1alpha1.ServiceSet, name, image string, command, args []string, env map[string]string, envFromSecret *string, resources corev1.ResourceRequirements, pullPolicy corev1.PullPolicy, runAsUser *int64, hc sandboxv1alpha1.HealthcheckSpec, labels map[string]string, mounts []corev1.VolumeMount, envFrom *corev1.EnvFromSource) error {
	hash := podSpecHash(image, command, args, env, envFromSecret, resources, pullPolicy, runAsUser, hc, mounts)
	key := types.NamespacedName{Name: name, Namespace: ss.Namespace}
	var existing corev1.Pod
	err := r.Get(ctx, key, &existing)
	if err == nil {
		if existing.GetAnnotations()[specHashAnnotation] == hash {
			return nil // unchanged
		}
		// Changed pod-affecting spec: recreate (Task 6). PVC retained (separate object).
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ss.Namespace, Labels: labels,
			Annotations:     map[string]string{specHashAnnotation: hash},
			OwnerReferences: []metav1.OwnerReference{ownerRef(ss)}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:            name,
				Image:           image,
				Command:         command,
				Args:            args,
				Env:             toEnvVars(env),
				EnvFrom:         envFromSlice(envFrom),
				Resources:       resources,
				ImagePullPolicy: pullPolicy,
				VolumeMounts:    mounts,
				ReadinessProbe:  readinessProbe(hc),
			}},
			Volumes: podVolumes(mounts),
		},
	}
	// The dep/runtime pods hold no credential -- mirror the agent pod's
	// invariant and explicitly disable service-account token automount.
	pod.Spec.AutomountServiceAccountToken = ptr.To(false)
	applySecurityContext(pod, runAsUser)
	return r.Create(ctx, pod)
}

// podVolumes turns each volume mount into a PVC-backed volume. Every mount in
// this plan references a PVC by name (a service data PVC "<name>-data", or the
// shared workspace PVC "<environmentName>-workspace"), so each volume's claim
// name equals its mount name.
func podVolumes(mounts []corev1.VolumeMount) []corev1.Volume {
	vols := make([]corev1.Volume, 0, len(mounts))
	for _, m := range mounts {
		vols = append(vols, corev1.Volume{Name: m.Name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.Name}}})
	}
	return vols
}

func applySecurityContext(pod *corev1.Pod, runAsUser *int64) {
	if runAsUser == nil {
		return
	}
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: runAsUser}
}

func toEnvVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

func envFromSlice(e *corev1.EnvFromSource) []corev1.EnvFromSource {
	if e == nil {
		return nil
	}
	return []corev1.EnvFromSource{*e}
}

func readinessProbe(hc sandboxv1alpha1.HealthcheckSpec) *corev1.Probe {
	interval := 5 * time.Second
	if hc.Interval != "" {
		if d, err := time.ParseDuration(hc.Interval); err == nil {
			interval = d
		}
	}
	p := &corev1.Probe{PeriodSeconds: int32(interval.Seconds()), TimeoutSeconds: 2}
	switch {
	case len(hc.Exec) > 0:
		p.ProbeHandler = corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: hc.Exec}}
	case hc.HTTP != nil:
		p.ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: hc.HTTP.Path, Port: intstr.FromInt32(hc.HTTP.Port)}}
	case hc.TCP != nil:
		p.ProbeHandler = corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(hc.TCP.Port)}}
	default:
		return nil
	}
	return p
}

func ownerRef(ss *sandboxv1alpha1.ServiceSet) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: sandboxv1alpha1.GroupVersion.String(), Kind: "ServiceSet", Name: ss.Name, UID: ss.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
}

// podSpecHash is the recreate-detection key: any change to a pod-affecting
// field yields a different hash, so ensurePod deletes+recreates the Pod.
func podSpecHash(image string, command, args []string, env map[string]string, envFromSecret *string, resources corev1.ResourceRequirements, pullPolicy corev1.PullPolicy, runAsUser *int64, hc sandboxv1alpha1.HealthcheckSpec, mounts []corev1.VolumeMount) string {
	type spec struct {
		Image     string
		Command   []string
		Args      []string
		Env       map[string]string
		EnvFrom   *string
		Resources corev1.ResourceRequirements
		Pull      corev1.PullPolicy
		RunAsUser *int64
		HC        sandboxv1alpha1.HealthcheckSpec
		Mounts    []corev1.VolumeMount
	}
	b, _ := json.Marshal(spec{image, command, args, env, envFromSecret, resources, pullPolicy, runAsUser, hc, mounts})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (r *ServiceSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxv1alpha1.ServiceSet{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("serviceset").
		Complete(r)
}

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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
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
	return ctrl.Result{}, nil
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
		labelServiceset: ss.Name,
		labelEntry:      name,
		labelKind:       kind,
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

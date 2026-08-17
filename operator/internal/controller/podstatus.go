package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// unschedulableGracePeriod is how long a pod must have been continuously
// unschedulable before it's treated as a terminal failure rather than a
// transient scheduling delay (e.g. a cluster-autoscaler scale-up in
// progress). Without this window, a normal scale-up would fail the
// environment; without ANY window, an unschedulable pod would hang until
// the generic RunningTimeoutExceeded fires, which is exactly the
// non-actionable reason this issue's acceptance criteria forbid.
const unschedulableGracePeriod = 2 * time.Minute

// podFailureMessageBytes bounds a PodFailure.Message before it is stored in
// ClusterFacts, matching lifecycle's own maxMessageBytes bound on
// AgentMessage.
const podFailureMessageBytes = 512

// podReady reports whether pod's Ready condition is True. Hand-written
// (k8s.io/kubernetes/pkg/api/v1/pod's IsPodReady is not importable from a
// non-Kubernetes-core module).
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podFailure maps a Pod's observed status to a terminal failure reason, or
// nil if the pod is not (yet, or ever) in a terminal failure state. Ordered,
// first match wins, so a specific cause is never masked by the generic
// phase==Failed fallback. now is injected (never time.Now()) so the
// unschedulable grace window is testable with a fake clock.
//
// ReasonRestoreVerificationFailed is NEVER produced here -- reserved for
// #29's wake/restore verification.
func podFailure(pod *corev1.Pod, now time.Time) *lifecycle.PodFailure {
	if !pod.DeletionTimestamp.IsZero() {
		return nil // deliberately being deleted (Done/freeze/suspend), not a failure
	}
	if f := unschedulableFailure(pod, now); f != nil {
		return f
	}
	if f := imagePullFailure(pod); f != nil {
		return f
	}
	return genericFailure(pod)
}

func unschedulableFailure(pod *corev1.Pod, now time.Time) *lifecycle.PodFailure {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			if now.Sub(c.LastTransitionTime.Time) >= unschedulableGracePeriod {
				return &lifecycle.PodFailure{Reason: lifecycle.ReasonUnschedulable, Message: c.Message}
			}
			return nil // inside the grace window
		}
	}
	return nil
}

func imagePullFailure(pod *corev1.Pod) *lifecycle.PodFailure {
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if cs.State.Waiting == nil {
			continue
		}
		// ErrImagePull, ImageInspectError, RegistryUnavailable deliberately
		// excluded: the kubelet emits these as the FIRST attempt's
		// transient state and converges to ImagePullBackOff within
		// seconds -- treating them as terminal would fail an environment
		// on a single registry blip. InvalidImageName is included because
		// it never reaches backoff and is permanently fatal.
		switch cs.State.Waiting.Reason {
		case "ImagePullBackOff", "InvalidImageName":
			return &lifecycle.PodFailure{
				Reason:  lifecycle.ReasonImagePullFailure,
				Message: fmt.Sprintf("%s: %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message),
			}
		}
	}
	return nil
}

func genericFailure(pod *corev1.Pod) *lifecycle.PodFailure {
	if pod.Status.Phase != corev1.PodFailed {
		return nil
	}
	msg := pod.Status.Message
	if msg == "" {
		msg = firstTerminatedContainerMessage(pod)
	}
	if pod.Status.Reason != "" {
		msg = pod.Status.Reason + ": " + msg
	}
	return &lifecycle.PodFailure{Reason: lifecycle.ReasonPodFailed, Message: msg}
}

func firstTerminatedContainerMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("container %s exited with code %d (%s)", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	return ""
}

// truncateMessage bounds s to podFailureMessageBytes, matching the
// conservative truncation applied to every other free-text message this
// operator surfaces into a condition.
func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

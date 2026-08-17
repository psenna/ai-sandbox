package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/psenna/ai-sandbox/operator/internal/lifecycle"
)

// TestPodReady covers podReady's Ready-condition lookup.
func TestPodReady(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"no conditions", &corev1.Pod{}, false},
		{"ready true", &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}}}, true},
		{"ready false", &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podReady(tc.pod); got != tc.want {
				t.Errorf("podReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPodFailure is the core table test over podFailure's ordered mapping.
func TestPodFailure(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		pod        *corev1.Pod
		wantNil    bool
		wantReason string
	}{
		{
			name:    "pending no conditions",
			pod:     &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			wantNil: true,
		},
		{
			name: "unschedulable inside grace window",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionFalse,
					Reason:             corev1.PodReasonUnschedulable,
					LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Second)),
				},
			}}},
			wantNil: true,
		},
		{
			name: "unschedulable past grace window",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionFalse,
					Reason:             corev1.PodReasonUnschedulable,
					Message:            "0/3 nodes are available",
					LastTransitionTime: metav1.NewTime(now.Add(-5 * time.Minute)),
				},
			}}},
			wantReason: lifecycle.ReasonUnschedulable,
		},
		{
			name: "PodScheduled false with a different reason",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionFalse,
					Reason:             "SchedulerError",
					LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute)),
				},
			}}},
			wantNil: true,
		},
		{
			name: "ErrImagePull is transient, excluded",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
			}}},
			wantNil: true,
		},
		{
			name: "ImagePullBackOff",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "back-off pulling image"}}},
			}}},
			wantReason: lifecycle.ReasonImagePullFailure,
		},
		{
			name: "InvalidImageName",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "InvalidImageName"}}},
			}}},
			wantReason: lifecycle.ReasonImagePullFailure,
		},
		{
			name: "init container ImagePullBackOff is scanned",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "restore", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			}}},
			wantReason: lifecycle.ReasonImagePullFailure,
		},
		{
			name:    "running and ready",
			pod:     &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
			wantNil: true,
		},
		{
			name:    "running not ready",
			pod:     &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}},
			wantNil: true,
		},
		{
			name:    "succeeded",
			pod:     &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			wantNil: true,
		},
		{
			name: "failed with terminated container exit code",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
			}}},
			wantReason: lifecycle.ReasonPodFailed,
		},
		{
			name: "failed AND ImagePullBackOff: specific beats generic",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			}}},
			wantReason: lifecycle.ReasonImagePullFailure,
		},
		{
			name: "deletion timestamp set suppresses any failure",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{Time: now}},
				Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
					{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
				}},
			},
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podFailure(tc.pod, now)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("podFailure() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("podFailure() = nil, want Reason %q", tc.wantReason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("podFailure().Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			allowed := false
			for _, r := range lifecycle.PodFailureReasons {
				if got.Reason == r {
					allowed = true
				}
			}
			if !allowed {
				t.Errorf("podFailure().Reason = %q is not in lifecycle.PodFailureReasons", got.Reason)
			}
			if got.Reason == lifecycle.ReasonRestoreVerificationFailed {
				t.Errorf("podFailure() must never produce ReasonRestoreVerificationFailed (reserved for #29)")
			}
			sanitized := lifecycle.SanitizeReason(got.Reason, lifecycle.PodFailureReasons, lifecycle.ReasonPodFailed)
			if sanitized != got.Reason {
				t.Errorf("SanitizeReason changed %q to %q -- podFailure produced a reason not in the allowlist", got.Reason, sanitized)
			}
		})
	}
}

func TestPodFailure_UnschedulableMessagePreserved(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
		{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/3 nodes are available: insufficient cpu",
			LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute)),
		},
	}}}
	got := podFailure(pod, now)
	if got == nil {
		t.Fatal("podFailure() = nil, want non-nil")
	}
	if got.Message != "0/3 nodes are available: insufficient cpu" {
		t.Errorf("podFailure().Message = %q, want the scheduling condition's message", got.Message)
	}
}

func TestTruncateMessage(t *testing.T) {
	short := "short message"
	if got := truncateMessage(short, 512); got != short {
		t.Errorf("truncateMessage(short) = %q, want unchanged", got)
	}
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateMessage(string(long), 512)
	if len(got) != 512 {
		t.Errorf("truncateMessage(long) length = %d, want 512", len(got))
	}
}

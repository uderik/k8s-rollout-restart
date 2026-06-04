package reporter

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestGetNodeStatus(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want string
	}{
		{
			name: "ready node",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}}},
			want: "Ready",
		},
		{
			name: "not ready node",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			}}},
			want: "NotReady",
		},
		{
			name: "no ready condition",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			}}},
			want: "Unknown",
		},
		{
			name: "no conditions",
			node: &corev1.Node{},
			want: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getNodeStatus(tt.node); got != tt.want {
				t.Errorf("getNodeStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready pod",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}}},
			want: true,
		},
		{
			name: "not ready pod",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			}}},
			want: false,
		},
		{
			name: "no ready condition",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			}}},
			want: false,
		},
		{
			name: "no conditions",
			pod:  &corev1.Pod{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodReady(tt.pod); got != tt.want {
				t.Errorf("isPodReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetTotalRestarts(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want int32
	}{
		{
			name: "no containers",
			pod:  &corev1.Pod{},
			want: 0,
		},
		{
			name: "single container",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 3},
			}}},
			want: 3,
		},
		{
			name: "multiple containers sum",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 2},
				{RestartCount: 5},
			}}},
			want: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getTotalRestarts(tt.pod); got != tt.want {
				t.Errorf("getTotalRestarts() = %d, want %d", got, tt.want)
			}
		})
	}
}

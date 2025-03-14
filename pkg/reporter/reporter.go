package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterState represents the state of cluster components
type ClusterState struct {
	Timestamp  string                `json:"timestamp"`
	Nodes      []NodeState           `json:"nodes"`
	Pods       map[string][]PodState `json:"pods"`
	Components ComponentState        `json:"components"`
}

// NodeState represents the state of a node
type NodeState struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	Unschedulable bool   `json:"unschedulable"`
}

// PodState represents the state of a pod
type PodState struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Age      string `json:"age"`
}

// ComponentState represents the state of cluster components
type ComponentState struct {
	Deployments  int `json:"deployments"`
	StatefulSets int `json:"statefulSets"`
	DaemonSets   int `json:"daemonSets"`
	Kafka        int `json:"kafka"`
}

// Reporter handles cluster state reporting
type Reporter struct {
	client *kubernetes.Clientset
}

// NewReporter creates a new Reporter instance
func NewReporter(client *kubernetes.Clientset) *Reporter {
	return &Reporter{
		client: client,
	}
}

// GenerateReport generates a report of the current cluster state
// If namespace is not empty, report will include only information about that namespace
func (r *Reporter) GenerateReport(ctx context.Context, namespace string) (*ClusterState, error) {
	state := &ClusterState{
		Timestamp: time.Now().Format(time.RFC3339),
		Pods:      make(map[string][]PodState),
	}

	// Get nodes state
	nodes, err := r.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		nodeState := NodeState{
			Name:          node.Name,
			Status:        getNodeStatus(&node),
			Unschedulable: node.Spec.Unschedulable,
		}
		state.Nodes = append(state.Nodes, nodeState)
	}

	// Get pods state for each namespace
	if namespace != "" {
		// If namespace specified, get pods only for that namespace
		if err := r.collectNamespacePods(ctx, namespace, state.Pods); err != nil {
			return nil, err
		}
	} else {
		// Get all namespaces
		namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, ns := range namespaces.Items {
			if err := r.collectNamespacePods(ctx, ns.Name, state.Pods); err != nil {
				return nil, err
			}
		}
	}

	// Get component counts
	if err := r.collectComponentCounts(ctx, &state.Components, namespace); err != nil {
		return nil, err
	}

	return state, nil
}

// collectNamespacePods collects pod information for a specific namespace
func (r *Reporter) collectNamespacePods(ctx context.Context, namespace string, podMap map[string][]PodState) error {
	pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	var podStates []PodState
	for _, pod := range pods.Items {
		podState := PodState{
			Name:     pod.Name,
			Status:   string(pod.Status.Phase),
			Ready:    isPodReady(&pod),
			Restarts: getTotalRestarts(&pod),
			Age:      time.Since(pod.CreationTimestamp.Time).Round(time.Second).String(),
		}
		podStates = append(podStates, podState)
	}
	podMap[namespace] = podStates

	return nil
}

// collectComponentCounts collects counts of different component types, optionally filtered by namespace
func (r *Reporter) collectComponentCounts(ctx context.Context, state *ComponentState, namespace string) error {
	// Get deployments
	deployments, err := r.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}
	state.Deployments = len(deployments.Items)

	// Get statefulsets
	statefulSets, err := r.client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list statefulsets: %w", err)
	}
	state.StatefulSets = len(statefulSets.Items)

	// Get daemonsets
	daemonSets, err := r.client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list daemonsets: %w", err)
	}
	state.DaemonSets = len(daemonSets.Items)

	// Count Kafka clusters using the REST client
	kafkaPath := "/apis/kafka.strimzi.io/v1beta2"
	if namespace != "" {
		response, err := r.client.RESTClient().
			Get().
			AbsPath(kafkaPath).
			Namespace(namespace).
			Resource("kafkas").
			Do(ctx).
			Raw()
		if err == nil && len(response) > 0 {
			var result struct {
				Items []interface{} `json:"items"`
			}
			if err := json.Unmarshal(response, &result); err == nil {
				state.Kafka = len(result.Items)
			}
		}
	} else {
		// Try to count all Kafka clusters in all namespaces
		response, err := r.client.RESTClient().
			Get().
			AbsPath(kafkaPath).
			Resource("kafkas").
			Do(ctx).
			Raw()
		if err == nil && len(response) > 0 {
			var result struct {
				Items []interface{} `json:"items"`
			}
			if err := json.Unmarshal(response, &result); err == nil {
				state.Kafka = len(result.Items)
			}
		}
	}

	return nil
}

// ToJSON converts the cluster state to JSON format
func (s *ClusterState) ToJSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// Helper functions

func getNodeStatus(node *corev1.Node) string {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			if condition.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func getTotalRestarts(pod *corev1.Pod) int32 {
	var total int32
	for _, containerStatus := range pod.Status.ContainerStatuses {
		total += containerStatus.RestartCount
	}
	return total
}

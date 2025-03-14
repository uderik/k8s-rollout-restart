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
	Postgresql   int `json:"postgresql"`
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
// If namespaces is empty, report on all namespaces
func (r *Reporter) GenerateReport(ctx context.Context, namespaces []string) (*ClusterState, error) {
	report := &ClusterState{
		Timestamp: time.Now().Format(time.RFC3339),
		Pods:      make(map[string][]PodState),
	}

	// Get all nodes
	nodes, err := r.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Process node info
	for _, node := range nodes.Items {
		nodeInfo := NodeState{
			Name:          node.Name,
			Status:        getNodeStatus(&node),
			Unschedulable: node.Spec.Unschedulable,
		}
		report.Nodes = append(report.Nodes, nodeInfo)
	}

	// If no namespaces provided, get all namespaces
	namespacesToCheck := namespaces
	if len(namespacesToCheck) == 0 {
		nsList, err := r.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		for _, ns := range nsList.Items {
			namespacesToCheck = append(namespacesToCheck, ns.Name)
		}
	}

	// For each namespace, get pods and other resources
	for _, ns := range namespacesToCheck {
		// Get pods in namespace
		pods, err := r.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list pods in namespace %s: %w", ns, err)
		}

		// Process pod info for this namespace
		var podInfos []PodState
		for _, pod := range pods.Items {
			podInfo := PodState{
				Name:     pod.Name,
				Status:   string(pod.Status.Phase),
				Ready:    isPodReady(&pod),
				Restarts: getTotalRestarts(&pod),
				Age:      time.Since(pod.CreationTimestamp.Time).Round(time.Second).String(),
			}
			podInfos = append(podInfos, podInfo)
		}
		report.Pods[ns] = podInfos

		// Count components in this namespace
		deployments, err := r.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list deployments in namespace %s: %w", ns, err)
		}
		report.Components.Deployments += len(deployments.Items)

		statefulsets, err := r.client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list statefulsets in namespace %s: %w", ns, err)
		}
		report.Components.StatefulSets += len(statefulsets.Items)

		// Count Kafka resources if available
		kafkaList, err := r.client.RESTClient().Get().
			AbsPath("/apis/kafka.strimzi.io/v1beta2").
			Namespace(ns).
			Resource("kafkas").
			DoRaw(ctx)
		if err == nil {
			var result struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(kafkaList, &result); err == nil {
				report.Components.Kafka += len(result.Items)
			}
		}

		// Count PostgreSQL resources if available
		postgresqlList, err := r.client.RESTClient().Get().
			AbsPath("/apis/acid.zalan.do/v1").
			Namespace(ns).
			Resource("postgresqls").
			DoRaw(ctx)
		if err == nil {
			var result struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(postgresqlList, &result); err == nil {
				report.Components.Postgresql += len(result.Items)
			}
		}
	}

	return report, nil
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

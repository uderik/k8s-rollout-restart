// Package reporter provides cluster state reporting functionality.
package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"github.com/uderik/k8s-rollout-restart/pkg/operations"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultParallelism bounds concurrent per-namespace requests.
const defaultParallelism = 10

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
	// CordonedByThisRun marks nodes cordoned by this run. They are always
	// part of the report, even when no pod of the target namespaces runs on
	// them any more.
	CordonedByThisRun bool `json:"cordonedByThisRun,omitempty"`
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
	client operations.K8sClient
	log    *logger.Logger
	// kafkaAPIVersion is the Kafka CR group/version (e.g.
	// "kafka.strimzi.io/v1beta2"). Empty or the default means "discover the
	// served version".
	kafkaAPIVersion string
	parallelism     int
	// cordonedNodes returns the nodes cordoned by this run (may be nil).
	cordonedNodes func() []string
}

// Option configures a Reporter.
type Option func(*Reporter)

// WithKafkaAPIVersion sets the Kafka CR group/version used to count Kafka
// clusters (same format as --strimzi-kafka-api).
func WithKafkaAPIVersion(gv string) Option {
	return func(r *Reporter) { r.kafkaAPIVersion = gv }
}

// WithLogger makes the reporter log through l (typically the root logger, so
// warnings follow the configured --output format instead of breaking JSON).
func WithLogger(l *logger.Logger) Option {
	return func(r *Reporter) {
		if l != nil {
			r.log = l
		}
	}
}

// WithParallelism bounds the number of namespaces processed concurrently.
func WithParallelism(n int) Option {
	return func(r *Reporter) {
		if n > 0 {
			r.parallelism = n
		}
	}
}

// WithCordonedNodes makes the report always include the nodes returned by
// nodes (the nodes cordoned by this run), even if they no longer run pods of
// the target namespaces.
func WithCordonedNodes(nodes func() []string) Option {
	return func(r *Reporter) { r.cordonedNodes = nodes }
}

// NewReporter creates a new Reporter instance
func NewReporter(client operations.K8sClient, opts ...Option) *Reporter {
	r := &Reporter{
		client:      client,
		log:         logger.NewLogger(false),
		parallelism: defaultParallelism,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// resolveKafkaAPIPath returns the Kafka API path to use, or "" when the
// Strimzi Kafka API is not installed (or cannot be resolved).
func (r *Reporter) resolveKafkaAPIPath(ctx context.Context) string {
	gv := strings.TrimPrefix(strings.TrimPrefix(r.kafkaAPIVersion, "/"), "apis/")
	group, version := "kafka.strimzi.io", ""
	if gv != "" && gv != operations.DefaultKafkaAPIVersion {
		if i := strings.Index(gv, "/"); i >= 0 {
			group, version = gv[:i], gv[i+1:]
		} else {
			group = gv
		}
	}
	path, present, err := operations.ResolveAPIGroupVersion(ctx, r.client.RESTClient(), group, version, "v1", "v1beta2")
	if err != nil {
		r.log.Warning("Reporter: cannot resolve Kafka API, Kafka clusters will not be counted: %v", err)
		return ""
	}
	if !present {
		return ""
	}
	return path
}

// countCRs lists a custom resource in a namespace and returns the number of
// items. A missing API (NotFound) counts as zero silently; any other error is
// logged and also counts as zero so that reporting never blocks a restart.
func (r *Reporter) countCRs(ctx context.Context, apiPath, namespace, resource string) int {
	raw, err := r.client.RESTClient().Get().
		AbsPath(apiPath).
		Namespace(namespace).
		Resource(resource).
		DoRaw(ctx)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			r.log.Warning("Reporter: failed to list %s in namespace %s: %v", resource, namespace, err)
		}
		return 0
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		r.log.Warning("Reporter: failed to parse %s in namespace %s: %v", resource, namespace, err)
		return 0
	}
	return len(list.Items)
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

	// Create a map to track nodes that have pods in our namespaces
	usedNodes := make(map[string]bool)

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

	kafkaAPIPath := r.resolveKafkaAPIPath(ctx)

	// A fixed pool of workers processes the namespaces (no goroutine per
	// namespace).
	parallelism := r.parallelism
	if parallelism < 1 {
		parallelism = defaultParallelism
	}
	parallelism = min(parallelism, len(namespacesToCheck))

	results := make([]namespaceResult, len(namespacesToCheck))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = r.collectNamespace(ctx, namespacesToCheck[i], kafkaAPIPath)
			}
		}()
	}
	for i := range namespacesToCheck {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}

		report.Pods[result.ns] = result.pods
		report.Components.Deployments += result.deployments
		report.Components.StatefulSets += result.statefulsets
		report.Components.Kafka += result.kafka
		report.Components.Postgresql += result.postgresql

		for nodeName := range result.usedNodes {
			usedNodes[nodeName] = true
		}
	}

	cordoned := make(map[string]bool)
	if r.cordonedNodes != nil {
		for _, name := range r.cordonedNodes() {
			cordoned[name] = true
		}
	}

	// Nodes that have pods in our namespaces, plus every node cordoned by
	// this run (it may have been emptied by the restart).
	for _, node := range nodes.Items {
		if usedNodes[node.Name] || cordoned[node.Name] {
			report.Nodes = append(report.Nodes, NodeState{
				Name:              node.Name,
				Status:            getNodeStatus(&node),
				Unschedulable:     node.Spec.Unschedulable,
				CordonedByThisRun: cordoned[node.Name],
			})
		}
	}

	return report, nil
}

// namespaceResult is what the report collects from one namespace.
type namespaceResult struct {
	ns           string
	pods         []PodState
	deployments  int
	statefulsets int
	kafka        int
	postgresql   int
	usedNodes    map[string]bool // nodes running pods of this namespace
	err          error
}

// collectNamespace gathers the report data of one namespace.
func (r *Reporter) collectNamespace(ctx context.Context, ns, kafkaAPIPath string) (result namespaceResult) {
	result = namespaceResult{ns: ns, usedNodes: make(map[string]bool)}
	defer func() {
		if p := recover(); p != nil {
			result = namespaceResult{ns: ns, err: fmt.Errorf("panic in namespace %s: %v", ns, p)}
		}
	}()
	if err := ctx.Err(); err != nil {
		result.err = fmt.Errorf("namespace %s: %w", ns, err)
		return result
	}

	pods, err := r.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		result.err = fmt.Errorf("failed to list pods in namespace %s: %w", ns, err)
		return result
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != "" {
			result.usedNodes[pod.Spec.NodeName] = true
		}
		result.pods = append(result.pods, PodState{
			Name:     pod.Name,
			Status:   string(pod.Status.Phase),
			Ready:    isPodReady(pod),
			Restarts: getTotalRestarts(pod),
			Age:      time.Since(pod.CreationTimestamp.Time).Round(time.Second).String(),
		})
	}

	deployments, err := r.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		result.err = fmt.Errorf("failed to list deployments in namespace %s: %w", ns, err)
		return result
	}
	result.deployments = len(deployments.Items)

	statefulsets, err := r.client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		result.err = fmt.Errorf("failed to list statefulsets in namespace %s: %w", ns, err)
		return result
	}
	result.statefulsets = len(statefulsets.Items)

	// Count operator-managed resources if their APIs are available
	if kafkaAPIPath != "" {
		result.kafka = r.countCRs(ctx, kafkaAPIPath, ns, "kafkas")
	}
	result.postgresql = r.countCRs(ctx, "/apis/acid.zalan.do/v1", ns, "postgresqls")
	return result
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

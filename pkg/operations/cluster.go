package operations

import (
	"context"
	"fmt"
	"sort"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterOperations handles cluster-wide operations
type ClusterOperations struct {
	client    *kubernetes.Clientset
	parallel  int
	timeout   int
	noFlagger bool
	dryRun    bool
}

// NewClusterOperations creates a new ClusterOperations instance
func NewClusterOperations(client *kubernetes.Clientset, parallel, timeout int, noFlagger bool, dryRun bool) *ClusterOperations {
	return &ClusterOperations{
		client:    client,
		parallel:  parallel,
		timeout:   timeout,
		noFlagger: noFlagger,
		dryRun:    dryRun,
	}
}

// CordonNodes marks selected nodes as unschedulable
// If namespace is provided, only nodes running pods from that namespace will be cordoned
func (c *ClusterOperations) CordonNodes(ctx context.Context, namespace string) error {
	// Get all nodes first
	allNodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// If no namespace specified, cordon all nodes
	if namespace == "" {
		return c.cordonNodeList(ctx, allNodes.Items)
	}

	// If namespace specified, get nodes that have pods from this namespace
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	// Create a map of node names to avoid duplicates
	nodesWithNamespacedPods := make(map[string]corev1.Node)
	for _, pod := range pods.Items {
		// Find the node this pod is running on
		for _, node := range allNodes.Items {
			if node.Name == pod.Spec.NodeName {
				nodesWithNamespacedPods[node.Name] = node
				break
			}
		}
	}

	// Convert map to slice for processing
	var nodesToCordon []corev1.Node
	for _, node := range nodesWithNamespacedPods {
		nodesToCordon = append(nodesToCordon, node)
	}

	// Sort nodes by name for consistent output
	sort.Slice(nodesToCordon, func(i, j int) bool {
		return nodesToCordon[i].Name < nodesToCordon[j].Name
	})

	if len(nodesToCordon) == 0 {
		if c.dryRun {
			fmt.Printf("\nNo nodes have pods from namespace %s, nothing to cordon\n", namespace)
		}
		return nil
	}

	return c.cordonNodeList(ctx, nodesToCordon)
}

// cordonNodeList handles cordoning a specific list of nodes
func (c *ClusterOperations) cordonNodeList(ctx context.Context, nodes []corev1.Node) error {
	if c.dryRun {
		fmt.Printf("\nNodes that would be cordoned:\n")
		for _, node := range nodes {
			if !node.Spec.Unschedulable {
				fmt.Printf("- %s\n", node.Name)
			} else {
				fmt.Printf("- %s (already cordoned)\n", node.Name)
			}
		}
		return nil
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(c.parallel)

	for _, node := range nodes {
		node := node // capture range variable
		g.Go(func() error {
			node.Spec.Unschedulable = true
			_, err := c.client.CoreV1().Nodes().Update(ctx, &node, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to cordon node %s: %w", node.Name, err)
			}
			return nil
		})
	}

	return g.Wait()
}

// UncordonNodes marks selected nodes as schedulable
// If namespace is provided, only nodes running pods from that namespace will be uncordoned
func (c *ClusterOperations) UncordonNodes(ctx context.Context, namespace string) error {
	// Get all nodes first
	allNodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// If no namespace specified, uncordon all nodes
	if namespace == "" {
		return c.uncordonNodeList(ctx, allNodes.Items)
	}

	// If namespace specified, get nodes that have pods from this namespace
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	// Create a map of node names to avoid duplicates
	nodesWithNamespacedPods := make(map[string]corev1.Node)
	for _, pod := range pods.Items {
		// Find the node this pod is running on
		for _, node := range allNodes.Items {
			if node.Name == pod.Spec.NodeName {
				nodesWithNamespacedPods[node.Name] = node
				break
			}
		}
	}

	// Convert map to slice for processing
	var nodesToUncordon []corev1.Node
	for _, node := range nodesWithNamespacedPods {
		nodesToUncordon = append(nodesToUncordon, node)
	}

	// Sort nodes by name for consistent output
	sort.Slice(nodesToUncordon, func(i, j int) bool {
		return nodesToUncordon[i].Name < nodesToUncordon[j].Name
	})

	if len(nodesToUncordon) == 0 {
		if c.dryRun {
			fmt.Printf("\nNo nodes have pods from namespace %s, nothing to uncordon\n", namespace)
		}
		return nil
	}

	return c.uncordonNodeList(ctx, nodesToUncordon)
}

// uncordonNodeList handles uncordoning a specific list of nodes
func (c *ClusterOperations) uncordonNodeList(ctx context.Context, nodes []corev1.Node) error {
	if c.dryRun {
		fmt.Printf("\nNodes that would be uncordoned:\n")
		for _, node := range nodes {
			if node.Spec.Unschedulable {
				fmt.Printf("- %s\n", node.Name)
			} else {
				fmt.Printf("- %s (already schedulable)\n", node.Name)
			}
		}
		return nil
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(c.parallel)

	for _, node := range nodes {
		node := node // capture range variable
		g.Go(func() error {
			node.Spec.Unschedulable = false
			_, err := c.client.CoreV1().Nodes().Update(ctx, &node, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to uncordon node %s: %w", node.Name, err)
			}
			return nil
		})
	}

	return g.Wait()
}

package operations

import (
	"context"
	"fmt"
	"sync"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterOperations handles cluster-related operations
type ClusterOperations struct {
	clientset  K8sClient
	parallel   int
	timeout    int
	nodes      map[string]bool // map of node names that were cordoned
	nodesMutex sync.RWMutex    // protects nodes map
	dryRun     bool
	log        *logger.Logger
}

// NewClusterOperations creates a new ClusterOperations instance
func NewClusterOperations(clientset K8sClient, parallel, timeout int, _, dryRun bool) *ClusterOperations {
	return &ClusterOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		nodes:     make(map[string]bool),
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
	}
}

// CordonNodes cordons all nodes with pods from the namespaces or all nodes if cordonAllNodes is true
func (c *ClusterOperations) CordonNodes(ctx context.Context, namespaces []string, cordonAllNodes bool, nodeLabels []string, excludeLabels []string) error {
	if len(namespaces) == 0 && !cordonAllNodes {
		c.log.Info("No namespaces specified and cordon-all-nodes not set, skipping node cordon")
		return nil
	}

	// Get all nodes
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// Map for node names to check
	nodeNames := make(map[string]bool)

	if cordonAllNodes {
		// Add all nodes to the map if cordonAllNodes is true
		c.log.Info("Cordoning all nodes in the cluster as requested")
		for _, node := range nodes.Items {
			nodeNames[node.Name] = true
		}
	} else {
		// For each namespace, find nodes with pods
		for _, namespace := range namespaces {
			// Get all pods in the namespace
			pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
			}

			// Find nodes with pods from the namespace
			for _, pod := range pods.Items {
				nodeNames[pod.Spec.NodeName] = true
			}
		}
	}

	if len(nodeNames) == 0 {
		c.log.Warning("No nodes found to cordon, skipping cordon operation")
		return nil
	}

	// Parse node labels using helper
	requiredLabels := ParseLabelsOrAnnotations(nodeLabels)

	// Parse exclude labels using helper
	excludedLabels := ParseLabelsOrAnnotations(excludeLabels)

	// Filter nodes by labels
	filteredNodes := make(map[string]bool)
	for nodeName := range nodeNames {
		node, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", nodeName),
		})
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}

		if len(node.Items) == 0 {
			continue
		}

		nodeObj := node.Items[0]
		shouldInclude := true

		// Check required labels
		for key, value := range requiredLabels {
			if nodeObj.Labels[key] != value {
				shouldInclude = false
				break
			}
		}

		// Check excluded labels
		for key, value := range excludedLabels {
			if nodeObj.Labels[key] == value {
				shouldInclude = false
				break
			}
		}

		if shouldInclude {
			filteredNodes[nodeName] = true
		}
	}

	if len(filteredNodes) == 0 {
		c.log.Warning("No nodes found after label filtering, skipping cordon operation")
		return nil
	}

	c.log.Info("Found %d nodes to cordon after label filtering", len(filteredNodes))

	if c.dryRun {
		c.log.Info("Would cordon the following nodes:")
		for node := range filteredNodes {
			c.log.Info("- %s", node)
		}
		return nil
	}

	// Cordon each node
	var wg sync.WaitGroup
	ch := make(chan string, c.parallel)
	errCh := make(chan error, len(nodes.Items))

	for i := 0; i < c.parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for nodeName := range ch {
				if err := c.cordonNode(ctx, nodeName); err != nil {
					errCh <- fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
					return
				}
			}
		}()
	}

	// Send nodes to cordon
	for nodeName := range filteredNodes {
		ch <- nodeName
	}
	close(ch)

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		return err
	}

	return nil
}

// UncordonNodes uncordons all nodes previously cordoned
func (c *ClusterOperations) UncordonNodes(ctx context.Context, namespaces []string) error {
	c.nodesMutex.RLock()
	nodesLen := len(c.nodes)
	c.nodesMutex.RUnlock()

	if c.dryRun {
		c.log.Info("Would uncordon the following nodes:")
		c.nodesMutex.RLock()
		for node := range c.nodes {
			c.log.Info("- %s", node)
		}
		c.nodesMutex.RUnlock()
		return nil
	}

	// If nodes is empty, recreate list from namespaces
	if nodesLen == 0 {
		// Get all nodes
		nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}

		// Get all pods for namespaces
		var allPods []corev1.Pod
		for _, namespace := range namespaces {
			pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
			}
			allPods = append(allPods, pods.Items...)
		}

		// Add cordoned nodes to our tracking map
		c.nodesMutex.Lock()
		for _, node := range nodes.Items {
			if node.Spec.Unschedulable && c.hasPodFromNamespaces(node.Name, allPods, namespaces) {
				c.nodes[node.Name] = true
			}
		}
		c.nodesMutex.Unlock()
	}

	c.nodesMutex.RLock()
	nodesLen = len(c.nodes)
	c.nodesMutex.RUnlock()

	if nodesLen == 0 {
		c.log.Info("No cordoned nodes found, skipping uncordon")
		return nil
	}

	// Uncordon each node
	var wg sync.WaitGroup
	c.nodesMutex.RLock()
	ch := make(chan string, c.parallel)
	errCh := make(chan error, len(c.nodes))
	c.nodesMutex.RUnlock()

	for i := 0; i < c.parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for nodeName := range ch {
				if err := c.uncordonNode(ctx, nodeName); err != nil {
					errCh <- fmt.Errorf("failed to uncordon node %s: %w", nodeName, err)
					return
				}
			}
		}()
	}

	// Send nodes to uncordon
	c.nodesMutex.RLock()
	for nodeName := range c.nodes {
		ch <- nodeName
	}
	c.nodesMutex.RUnlock()
	close(ch)

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		return err
	}

	return nil
}

// cordonNode marks a node as unschedulable
func (c *ClusterOperations) cordonNode(ctx context.Context, nodeName string) error {
	node, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", nodeName),
	})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if len(node.Items) == 0 {
		return fmt.Errorf("node %s not found", nodeName)
	}

	nodeObj := node.Items[0]

	// Skip if already cordoned
	if nodeObj.Spec.Unschedulable {
		c.log.Info("Node %s already cordoned, skipping", nodeName)
		return nil
	}

	// Mark node as unschedulable
	nodeObj.Spec.Unschedulable = true

	// Update node
	_, err = c.clientset.CoreV1().Nodes().Update(ctx, &nodeObj, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node: %w", err)
	}

	// Mark node as cordoned in our tracking map
	c.nodesMutex.Lock()
	c.nodes[nodeName] = true
	c.nodesMutex.Unlock()

	c.log.Success("Node %s cordoned successfully", nodeName)
	return nil
}

// uncordonNode marks a node as schedulable
func (c *ClusterOperations) uncordonNode(ctx context.Context, nodeName string) error {
	node, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", nodeName),
	})
	if err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if len(node.Items) == 0 {
		return fmt.Errorf("node %s not found", nodeName)
	}

	nodeObj := node.Items[0]

	// Skip if already uncordoned
	if !nodeObj.Spec.Unschedulable {
		c.log.Info("Node %s already uncordoned, skipping", nodeName)
		return nil
	}

	// Mark node as schedulable
	nodeObj.Spec.Unschedulable = false

	// Update node
	_, err = c.clientset.CoreV1().Nodes().Update(ctx, &nodeObj, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node: %w", err)
	}

	// Remove node from our tracking map
	c.nodesMutex.Lock()
	delete(c.nodes, nodeName)
	c.nodesMutex.Unlock()

	c.log.Success("Node %s uncordoned successfully", nodeName)
	return nil
}

// hasPodFromNamespaces checks if a node has pods from any of the namespaces
func (c *ClusterOperations) hasPodFromNamespaces(nodeName string, pods []corev1.Pod, namespaces []string) bool {
	// Create a map for faster lookups
	nsMap := make(map[string]bool)
	for _, ns := range namespaces {
		nsMap[ns] = true
	}

	for _, pod := range pods {
		if pod.Spec.NodeName == nodeName && nsMap[pod.Namespace] {
			return true
		}
	}
	return false
}

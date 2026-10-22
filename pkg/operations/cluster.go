package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// CordonedByAnnotation marks a node as cordoned by a particular run of this
	// tool. It is written atomically together with spec.unschedulable=true, so
	// operators (and autoscaler debugging) can tell from the node object
	// itself which run cordoned it. The tool never uncordons nodes: cordoned
	// nodes are meant to be drained and removed (e.g. by Karpenter).
	CordonedByAnnotation = "k8s-rollout-restart/cordoned-by"

	// maxNodePatchAttempts bounds the read/patch retries on 409 Conflict.
	maxNodePatchAttempts = 5

	// verifyTimeout bounds the fresh-context read used to resolve the outcome
	// of a node PATCH whose response was lost (cancelled ctx, broken
	// connection, 5xx).
	verifyTimeout = 30 * time.Second

	// hostnameLabel is the well-known node label local PVs are pinned to.
	hostnameLabel = "kubernetes.io/hostname"
)

// ClusterOperations handles cluster-related operations
type ClusterOperations struct {
	clientset K8sClient
	parallel  int
	timeout   int
	// runID identifies this run; it is the value of CordonedByAnnotation on
	// every node this run cordons.
	runID string
	// nodes maps the names of nodes THIS run cordoned to their UID. Nodes
	// that were already unschedulable (cordoned by an admin, autoscaler,
	// drain, ...) are never added. It is only used to report the nodes this
	// run cordoned (and a manual rollback hint).
	nodes      map[string]types.UID
	nodesMutex sync.Mutex // protects nodes map
	dryRun     bool
	log        *logger.Logger
}

// NewClusterOperations creates a new ClusterOperations instance
func NewClusterOperations(clientset K8sClient, parallel, timeout int, _, dryRun bool) *ClusterOperations {
	return &ClusterOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		runID:     newRunID(),
		nodes:     make(map[string]types.UID),
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
	}
}

// newRunID returns a unique, annotation-value-safe identifier for this run.
func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// SetLogger replaces the logger (e.g. with the root logger so JSON output
// stays consistent).
func (c *ClusterOperations) SetLogger(l *logger.Logger) {
	if l != nil {
		c.log = l
	}
}

// RunID returns the identifier written to CordonedByAnnotation by this run.
func (c *ClusterOperations) RunID() string { return c.runID }

// CordonCandidate is a node selected for cordon.
type CordonCandidate struct {
	Name string
	UID  types.UID
	// AlreadyUnschedulable is true when the node was unschedulable at
	// selection time; such a node is left as is and not claimed by this run.
	AlreadyUnschedulable bool
}

// LocalVolumeUse describes a pod whose PVC is bound to a node-local PV on a
// node selected for cordon.
type LocalVolumeUse struct {
	Pod  string // namespace/name
	PVC  string
	PV   string
	Node string
}

func (u LocalVolumeUse) String() string {
	return fmt.Sprintf("pod %s (PVC %s -> local PV %s) on node %s", u.Pod, u.PVC, u.PV, u.Node)
}

// CordonPlan is the result of node selection, shared by dry-run and execute.
type CordonPlan struct {
	Nodes []CordonCandidate // sorted by name
	// LocalVolumes lists pods that cannot be rescheduled to another node
	// because their storage is pinned to a node that will be cordoned.
	LocalVolumes []LocalVolumeUse
	// LocalVolumeCheckErr is set when the local-PV check could not run
	// (e.g. no RBAC permission to list PersistentVolumes).
	LocalVolumeCheckErr error
	// LocalVolumeCheckIncomplete is set when pods of other namespaces on the
	// selected nodes could not be listed (e.g. no cluster-wide pod list
	// permission); only pods of the target namespaces were checked.
	LocalVolumeCheckIncomplete error
}

// PlanCordon selects the nodes CordonNodes would cordon. It is used by both
// dry-run and execute so that the preview matches what execute does.
func (c *ClusterOperations) PlanCordon(ctx context.Context, namespaces []string, cordonAllNodes bool, nodeLabels []string, excludeLabels []string) (*CordonPlan, error) {
	plan := &CordonPlan{}
	if len(namespaces) == 0 && !cordonAllNodes {
		return plan, nil
	}

	requiredLabels, err := ParseLabelsOrAnnotationsStrict(nodeLabels)
	if err != nil {
		return nil, fmt.Errorf("invalid node label filter: %w", err)
	}
	excludedLabels, err := ParseLabelsOrAnnotationsStrict(excludeLabels)
	if err != nil {
		return nil, fmt.Errorf("invalid node exclude-label filter: %w", err)
	}

	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Pods of the target namespaces: they decide which nodes to cordon (unless
	// cordon-all) and are checked for node-local storage.
	var pods []corev1.Pod
	for _, namespace := range namespaces {
		list, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
		}
		pods = append(pods, list.Items...)
	}

	candidates := make(map[string]bool)
	if cordonAllNodes {
		for _, node := range nodes.Items {
			candidates[node.Name] = true
		}
	} else {
		for _, pod := range pods {
			// Pending pods have no node yet.
			if pod.Spec.NodeName != "" {
				candidates[pod.Spec.NodeName] = true
			}
		}
	}

	selected := make(map[string]bool)
	for _, node := range nodes.Items {
		if !candidates[node.Name] || !nodeMatchesFilters(node.Labels, requiredLabels, excludedLabels) {
			continue
		}
		selected[node.Name] = true
		plan.Nodes = append(plan.Nodes, CordonCandidate{
			Name:                 node.Name,
			UID:                  node.UID,
			AlreadyUnschedulable: node.Spec.Unschedulable,
		})
	}
	sort.Slice(plan.Nodes, func(i, j int) bool { return plan.Nodes[i].Name < plan.Nodes[j].Name })

	if len(selected) > 0 {
		// Every pod on a node to be cordoned matters, not only the pods of
		// the target namespaces: e.g. db/postgres-0 with a local PV on a
		// node selected via -n app.
		nodePods, err := c.podsOnNodes(ctx, selected)
		if err != nil {
			plan.LocalVolumeCheckIncomplete = err
			nodePods = pods
		}
		plan.LocalVolumes, plan.LocalVolumeCheckErr = c.findLocalVolumeUses(ctx, nodePods, selected)
	}
	return plan, nil
}

// podsOnNodes returns the pods of ALL namespaces running on the given nodes.
// It uses one cluster-wide pod List (RBAC: list pods cluster-wide) and
// filters by spec.nodeName.
func (c *ClusterOperations) podsOnNodes(ctx context.Context, nodes map[string]bool) ([]corev1.Pod, error) {
	list, err := c.clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in all namespaces: %w", err)
	}
	var out []corev1.Pod
	for i := range list.Items {
		if nodes[list.Items[i].Spec.NodeName] {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// nodeMatchesFilters reports whether a node with the given labels passes the
// include (all must match) and exclude (none may match) filters. A filter
// "key=" requires the key to be present with an empty value; a missing key
// never matches.
func nodeMatchesFilters(labels, required, excluded map[string]string) bool {
	if !MatchesRequirements(labels, required) {
		return false
	}
	for key, want := range excluded {
		if v, ok := labels[key]; ok && v == want {
			return false
		}
	}
	return true
}

// findLocalVolumeUses returns pods running on selected nodes whose PVCs are
// bound to node-local PVs (spec.local, or nodeAffinity pinned to a hostname).
// Such pods cannot move to another node: their replacement stays Pending on a
// cordoned node, and the data is lost if the node is removed afterwards.
func (c *ClusterOperations) findLocalVolumeUses(ctx context.Context, pods []corev1.Pod, selected map[string]bool) ([]LocalVolumeUse, error) {
	type claimRef struct{ namespace, name string }
	claims := make(map[claimRef][]*corev1.Pod)
	for i := range pods {
		pod := &pods[i]
		if !selected[pod.Spec.NodeName] {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				ref := claimRef{pod.Namespace, v.PersistentVolumeClaim.ClaimName}
				claims[ref] = append(claims[ref], pod)
			}
		}
	}
	if len(claims) == 0 {
		return nil, nil
	}

	lister, ok := c.clientset.CoreV1().(PersistentVolumeLister)
	if !ok {
		return nil, errors.New("client does not support listing PersistentVolumes")
	}
	pvs, err := lister.PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PersistentVolumes: %w", err)
	}

	var uses []LocalVolumeUse
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.ClaimRef == nil || !isNodeLocalPV(pv) {
			continue
		}
		for _, pod := range claims[claimRef{pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name}] {
			uses = append(uses, LocalVolumeUse{
				Pod:  pod.Namespace + "/" + pod.Name,
				PVC:  pv.Spec.ClaimRef.Name,
				PV:   pv.Name,
				Node: pod.Spec.NodeName,
			})
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].Pod != uses[j].Pod {
			return uses[i].Pod < uses[j].Pod
		}
		return uses[i].PVC < uses[j].PVC
	})
	return uses, nil
}

// isNodeLocalPV reports whether a PV is bound to a single node: a "local"
// volume, or one whose required node affinity pins kubernetes.io/hostname.
func isNodeLocalPV(pv *corev1.PersistentVolume) bool {
	if pv.Spec.Local != nil || pv.Spec.HostPath != nil {
		return true
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return false
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == hostnameLabel && expr.Operator == corev1.NodeSelectorOpIn {
				return true
			}
		}
		for _, f := range term.MatchFields {
			if f.Key == "metadata.name" && f.Operator == corev1.NodeSelectorOpIn {
				return true
			}
		}
	}
	return false
}

// logPlanWarnings reports local-volume findings of a plan.
func (c *ClusterOperations) logPlanWarnings(plan *CordonPlan) {
	if plan.LocalVolumeCheckErr != nil {
		c.log.Warning("Could not check for pods using node-local PersistentVolumes: %v", plan.LocalVolumeCheckErr)
	}
	if plan.LocalVolumeCheckIncomplete != nil && plan.LocalVolumeCheckErr == nil {
		c.log.Warning("Node-local PersistentVolume check is INCOMPLETE: only pods of the target namespaces were checked, "+
			"pods of other namespaces on the nodes to be cordoned were not (%v)", plan.LocalVolumeCheckIncomplete)
	}
	if len(plan.LocalVolumes) == 0 {
		return
	}
	c.log.Warning("%d pod(s) use PVCs bound to node-local PersistentVolumes on nodes that will be cordoned. "+
		"Their replacements can only start on the same (cordoned) node and will stay Pending; "+
		"if the node is then removed (e.g. by Karpenter) the local data is lost:", len(plan.LocalVolumes))
	for _, u := range plan.LocalVolumes {
		c.log.Warning("  - %s", u)
	}
}

// CordonNodes cordons all nodes with pods from the namespaces or all nodes if cordonAllNodes is true.
// Only nodes that were schedulable and actually got cordoned by this call are
// recorded (see CordonedNodes). Nodes are intentionally never uncordoned by
// this tool.
func (c *ClusterOperations) CordonNodes(ctx context.Context, namespaces []string, cordonAllNodes bool, nodeLabels []string, excludeLabels []string) error {
	if len(namespaces) == 0 && !cordonAllNodes {
		c.log.Info("No namespaces specified and cordon-all-nodes not set, skipping node cordon")
		return nil
	}
	if cordonAllNodes {
		c.log.Info("Cordoning all nodes in the cluster as requested")
	}

	plan, err := c.PlanCordon(ctx, namespaces, cordonAllNodes, nodeLabels, excludeLabels)
	if err != nil {
		return err
	}
	c.logPlanWarnings(plan)

	if len(plan.Nodes) == 0 {
		c.log.Warning("No nodes found to cordon after label filtering, skipping cordon operation")
		return nil
	}
	c.log.Info("Found %d nodes to cordon after label filtering", len(plan.Nodes))

	if c.dryRun {
		c.log.Info("Would cordon the following nodes:")
		for _, node := range plan.Nodes {
			if node.AlreadyUnschedulable {
				c.log.Info("- %s (already cordoned, would be left as is)", node.Name)
			} else {
				c.log.Info("- %s", node.Name)
			}
		}
		return nil
	}

	names := make([]string, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		names = append(names, node.Name)
	}
	return c.forEachNode(ctx, names, func(ctx context.Context, nodeName string) error {
		if err := c.cordonNode(ctx, nodeName); err != nil {
			return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
		}
		return nil
	})
}

// CordonedNodes returns the sorted names of the nodes cordoned by this run.
func (c *ClusterOperations) CordonedNodes() []string {
	c.nodesMutex.Lock()
	defer c.nodesMutex.Unlock()

	names := make([]string, 0, len(c.nodes))
	for name := range c.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c *ClusterOperations) track(name string, uid types.UID) {
	c.nodesMutex.Lock()
	c.nodes[name] = uid
	c.nodesMutex.Unlock()
}

// forEachNode runs fn for every node with at most c.parallel concurrent calls.
// A failing node does not stop the others; all errors are collected and
// returned joined. Once ctx is cancelled no new nodes are dispatched.
func (c *ClusterOperations) forEachNode(ctx context.Context, nodeNames []string, fn func(context.Context, string) error) error {
	var (
		mu   sync.Mutex
		errs []error
	)
	addErr := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	g := new(errgroup.Group)
	g.SetLimit(max(c.parallel, 1))

	for i, nodeName := range nodeNames {
		if err := ctx.Err(); err != nil {
			addErr(fmt.Errorf("%d node(s) not processed: %w", len(nodeNames)-i, err))
			break
		}
		g.Go(func() error {
			if err := fn(ctx, nodeName); err != nil {
				addErr(err)
			}
			return nil
		})
	}

	_ = g.Wait()
	return errors.Join(errs...)
}

// getNode reads a node by name. It uses a field-selector List (same RBAC as
// the initial listing) and returns (nil, nil) when the node does not exist.
func (c *ClusterOperations) getNode(ctx context.Context, name string) (*corev1.Node, error) {
	list, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// nodePatch builds a JSON merge patch that sets spec.unschedulable and the
// ownership annotation (nil removes it). metadata.resourceVersion makes the
// API server reject the patch with 409 Conflict if the node changed since it
// was read, so the decision "this node is schedulable / carries our marker"
// cannot be invalidated by a concurrent writer.
func nodePatch(resourceVersion string, unschedulable bool, marker *string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": resourceVersion,
			"annotations":     map[string]any{CordonedByAnnotation: marker},
		},
		"spec": map[string]any{"unschedulable": unschedulable},
	})
}

// isUncertainErr reports whether a failed mutation may nevertheless have been
// applied by the API server: the request was cancelled, the connection broke
// or the server failed/timed out. Definite API rejections (4xx such as 403,
// 404, 409, 422) are not uncertain.
func isUncertainErr(err error) bool {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		return code == 0 || code >= 500 || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
	}
	return true
}

// cordonNode cordons a schedulable node and records it as cordoned by this
// run. The cordon and the ownership marker are written in one conditional
// PATCH; on conflict the node is re-read and re-evaluated, so a node that
// someone else cordoned in the meantime is never claimed.
func (c *ClusterOperations) cordonNode(ctx context.Context, nodeName string) error {
	for attempt := 1; ; attempt++ {
		node, err := c.getNode(ctx, nodeName)
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}
		if node == nil {
			c.log.Warning("Node %s no longer exists, skipping cordon", nodeName)
			return nil
		}

		if node.Spec.Unschedulable {
			if node.Annotations[CordonedByAnnotation] == c.runID {
				// Our own earlier cordon (e.g. an earlier attempt whose response was lost).
				c.track(nodeName, node.UID)
				c.log.Info("Node %s already cordoned by this run", nodeName)
				return nil
			}
			// Cordoned by someone else: deliberately NOT tracked, it must stay
			// unschedulable after the run.
			c.log.Info("Node %s already cordoned, skipping (not claimed by this run)", nodeName)
			return nil
		}

		marker := c.runID
		patch, err := nodePatch(node.ResourceVersion, true, &marker)
		if err != nil {
			return err
		}
		_, err = c.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
		switch {
		case err == nil:
			c.track(nodeName, node.UID)
			c.log.Success("Node %s cordoned successfully", nodeName)
			return nil
		case apierrors.IsConflict(err):
			if attempt >= maxNodePatchAttempts {
				return fmt.Errorf("node kept changing, giving up after %d attempts: %w", attempt, err)
			}
			c.log.Info("Node %s changed concurrently, re-reading before cordon", nodeName)
			continue
		case apierrors.IsNotFound(err):
			c.log.Warning("Node %s no longer exists, skipping cordon", nodeName)
			return nil
		case isUncertainErr(err):
			if c.verifyOwnCordon(nodeName, node.UID) {
				c.log.Warning("Cordon of node %s returned %v, but the node carries this run's marker; counting it as cordoned by this run", nodeName, err)
				if ctx.Err() != nil {
					return fmt.Errorf("failed to patch node: %w", err)
				}
				return nil
			}
			return fmt.Errorf("failed to patch node: %w", err)
		default:
			return fmt.Errorf("failed to patch node: %w", err)
		}
	}
}

// verifyOwnCordon resolves an uncertain cordon PATCH with a fresh, bounded
// context (the operation context may already be cancelled). It tracks the node
// and returns true only when the node carries this run's marker AND is
// cordoned (spec.unschedulable=true).
func (c *ClusterOperations) verifyOwnCordon(nodeName string, uid types.UID) bool {
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	node, err := c.getNode(ctx, nodeName)
	if err != nil {
		c.log.Warning("Could not verify cordon of node %s: %v (look for %s=%s)", nodeName, err, CordonedByAnnotation, c.runID)
		return false
	}
	if node == nil || node.UID != uid || node.Annotations[CordonedByAnnotation] != c.runID {
		return false
	}
	if !node.Spec.Unschedulable {
		// The marker alone is not enough: someone uncordoned the node after
		// our PATCH was applied (the annotation stays). The node is not
		// cordoned, so the cordon must fail.
		c.log.Warning("Node %s carries this run's marker (%s=%s) but is schedulable (spec.unschedulable=false): "+
			"it was uncordoned concurrently", nodeName, CordonedByAnnotation, c.runID)
		return false
	}
	c.track(nodeName, node.UID)
	return true
}

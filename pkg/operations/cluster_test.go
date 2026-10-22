package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// clusterFakeClient is a minimal in-memory K8sClient for node cordon tests.
// Only CoreV1 Nodes()/Pods() (and optionally PersistentVolumes()) are
// implemented. Node patches honour metadata.resourceVersion like the API
// server does (409 Conflict on mismatch).
type clusterFakeClient struct {
	mu    sync.Mutex
	nodes map[string]*corev1.Node
	pods  map[string][]corev1.Pod // by namespace
	pvs   []corev1.PersistentVolume
	// withPVs makes CoreV1() implement PersistentVolumeLister.
	withPVs bool
	// allNamespacesErr, if set, is returned by a cluster-wide pod List.
	allNamespacesErr error
	rv               int

	// patchHook, if set, is called before a patch is applied; a non-nil
	// error aborts the patch.
	patchHook func(ctx context.Context, name string, unschedulable bool) error
	// afterPatchErr, if set, is called after a patch WAS applied; a non-nil
	// error is returned to the caller (lost response).
	afterPatchErr func(name string) error
	patches       []string // "name=true|false" in call order (applied only)
}

func newClusterFakeClient() *clusterFakeClient {
	return &clusterFakeClient{
		nodes: make(map[string]*corev1.Node),
		pods:  make(map[string][]corev1.Pod),
	}
}

func (f *clusterFakeClient) nextRV() string {
	f.rv++
	return strconv.Itoa(f.rv)
}

func (f *clusterFakeClient) addNode(name string, unschedulable bool, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[name] = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, UID: types.UID("uid-" + name), ResourceVersion: f.nextRV()},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
}

// setUnschedulable simulates a concurrent writer (e.g. an admin's kubectl cordon).
func (f *clusterFakeClient) setUnschedulable(name string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[name].Spec.Unschedulable = v
	f.nodes[name].ResourceVersion = f.nextRV()
}

func (f *clusterFakeClient) deleteNode(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, name)
}

func (f *clusterFakeClient) addPod(namespace, name, nodeName string, claims ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
	}
	for _, c := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         c,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}},
		})
	}
	f.pods[namespace] = append(f.pods[namespace], pod)
}

func (f *clusterFakeClient) node(name string) *corev1.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes[name].DeepCopy()
}

func (f *clusterFakeClient) unschedulable(name string) bool {
	return f.node(name).Spec.Unschedulable
}

func (f *clusterFakeClient) marker(name string) string {
	return f.node(name).Annotations[CordonedByAnnotation]
}

func (f *clusterFakeClient) CoreV1() CoreV1Interface {
	if f.withPVs {
		return &clusterFakeCoreV1PV{clusterFakeCoreV1{f: f}}
	}
	return &clusterFakeCoreV1{f: f}
}
func (f *clusterFakeClient) AppsV1() AppsV1Interface { return nil }
func (f *clusterFakeClient) RESTClient() rest.Interface {
	return nil
}
func (f *clusterFakeClient) ClearCache() {}

type clusterFakeCoreV1 struct{ f *clusterFakeClient }

func (c *clusterFakeCoreV1) Pods(namespace string) PodInterface {
	return &clusterFakePods{f: c.f, namespace: namespace}
}
func (c *clusterFakeCoreV1) Nodes() NodeInterface           { return &clusterFakeNodes{f: c.f} }
func (c *clusterFakeCoreV1) Namespaces() NamespaceInterface { return nil }

type clusterFakeCoreV1PV struct{ clusterFakeCoreV1 }

func (c *clusterFakeCoreV1PV) PersistentVolumes() PersistentVolumeInterface {
	return clusterFakePVs{f: c.f}
}

type clusterFakePVs struct{ f *clusterFakeClient }

func (p clusterFakePVs) List(context.Context, metav1.ListOptions) (*corev1.PersistentVolumeList, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return &corev1.PersistentVolumeList{Items: append([]corev1.PersistentVolume(nil), p.f.pvs...)}, nil
}

type clusterFakePods struct {
	f         *clusterFakeClient
	namespace string
}

func (p *clusterFakePods) List(ctx context.Context, _ metav1.ListOptions) (*corev1.PodList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.namespace == metav1.NamespaceAll {
		if p.f.allNamespacesErr != nil {
			return nil, p.f.allNamespacesErr
		}
		var all []corev1.Pod
		for _, pods := range p.f.pods {
			all = append(all, pods...)
		}
		return &corev1.PodList{Items: all}, nil
	}
	return &corev1.PodList{Items: append([]corev1.Pod(nil), p.f.pods[p.namespace]...)}, nil
}

type clusterFakeNodes struct{ f *clusterFakeClient }

func (n *clusterFakeNodes) List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.f.mu.Lock()
	defer n.f.mu.Unlock()

	want := strings.TrimPrefix(opts.FieldSelector, "metadata.name=")
	list := &corev1.NodeList{}
	for name, node := range n.f.nodes {
		if opts.FieldSelector != "" && name != want {
			continue
		}
		list.Items = append(list.Items, *node.DeepCopy())
	}
	return list, nil
}

var nodesGR = schema.GroupResource{Resource: "nodes"}

func (n *clusterFakeNodes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, _ metav1.PatchOptions) (*corev1.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pt != types.MergePatchType {
		return nil, fmt.Errorf("unexpected patch type %s", pt)
	}
	var patch struct {
		Metadata struct {
			ResourceVersion string             `json:"resourceVersion"`
			Annotations     map[string]*string `json:"annotations"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(data, &patch); err != nil {
		return nil, err
	}
	if len(patch.Spec) != 1 {
		return nil, fmt.Errorf("patch must touch only spec.unschedulable, got %s", data)
	}
	unschedulable, ok := patch.Spec["unschedulable"].(bool)
	if !ok {
		return nil, fmt.Errorf("patch must set spec.unschedulable, got %s", data)
	}
	if patch.Metadata.ResourceVersion == "" {
		return nil, fmt.Errorf("patch must carry a resourceVersion precondition, got %s", data)
	}

	if hook := n.f.patchHook; hook != nil {
		if err := hook(ctx, name, unschedulable); err != nil {
			return nil, err
		}
	}

	n.f.mu.Lock()
	node, ok := n.f.nodes[name]
	if !ok {
		n.f.mu.Unlock()
		return nil, apierrors.NewNotFound(nodesGR, name)
	}
	if node.ResourceVersion != patch.Metadata.ResourceVersion {
		n.f.mu.Unlock()
		return nil, apierrors.NewConflict(nodesGR, name, errors.New("the object has been modified"))
	}
	node.Spec.Unschedulable = unschedulable
	for k, v := range patch.Metadata.Annotations {
		if v == nil {
			delete(node.Annotations, k)
			continue
		}
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[k] = *v
	}
	node.ResourceVersion = n.f.nextRV()
	n.f.patches = append(n.f.patches, fmt.Sprintf("%s=%t", name, unschedulable))
	out := node.DeepCopy()
	n.f.mu.Unlock()

	if after := n.f.afterPatchErr; after != nil {
		if err := after(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// runWithTimeout fails the test if fn does not return within d (i.e. it hangs).
func runWithTimeout(t *testing.T, d time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("operation did not finish within %v (deadlock/hang)", d)
		return nil
	}
}

func TestCordonNodes_ManyNodesMarkedAndRecorded(t *testing.T) {
	const parallel = 2
	const nodeCount = 5*parallel + 1

	f := newClusterFakeClient()
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%02d", i)
		f.addNode(name, false, nil)
		f.addPod("app", "pod-"+name, name)
	}
	c := NewClusterOperations(f, parallel, 10, false, false)

	err := runWithTimeout(t, 5*time.Second, func() error {
		return c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil)
	})
	if err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := len(c.CordonedNodes()); got != nodeCount {
		t.Fatalf("recorded %d nodes, want %d", got, nodeCount)
	}
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%02d", i)
		if !f.unschedulable(name) {
			t.Errorf("node %s not cordoned", name)
		}
		if got := f.marker(name); got != c.RunID() {
			t.Errorf("node %s marker = %q, want %q", name, got, c.RunID())
		}
	}
}

func TestCordonNodes_AllWorkersErrorNoHang(t *testing.T) {
	const parallel = 2
	const nodeCount = 4*parallel + 1

	f := newClusterFakeClient()
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%02d", i)
		f.addNode(name, false, nil)
		f.addPod("app", "pod-"+name, name)
	}
	c := NewClusterOperations(f, parallel, 10, false, false)

	errBoom := errors.New("boom")
	f.patchHook = func(context.Context, string, bool) error { return errBoom }

	err := runWithTimeout(t, 5*time.Second, func() error {
		return c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil)
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("CordonNodes error = %v, want %v", err, errBoom)
	}
	// Every node must have been attempted and reported, not just the first.
	if got := strings.Count(err.Error(), "failed to cordon node"); got != nodeCount {
		t.Errorf("got %d cordon errors, want %d: %v", got, nodeCount, err)
	}
	if got := len(c.CordonedNodes()); got != 0 {
		t.Errorf("recorded %d nodes after failed cordon, want 0", got)
	}
}

func TestCordonNodes_PreCordonedNodesNotClaimed(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("ours", false, nil)
	f.addNode("admin-cordoned", true, nil)
	f.addPod("app", "p1", "ours")
	f.addPod("app", "p2", "admin-cordoned")
	c := NewClusterOperations(f, 2, 10, false, false)

	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 || got[0] != "ours" {
		t.Fatalf("recorded = %v, want [ours]", got)
	}
	if f.marker("admin-cordoned") != "" {
		t.Errorf("pre-cordoned node must not get our marker")
	}
	for _, p := range f.patches {
		if strings.HasPrefix(p, "admin-cordoned=") {
			t.Errorf("pre-cordoned node was patched: %v", f.patches)
		}
	}
}

// R2: an admin cordons the node between our read and our PATCH. The
// resourceVersion precondition makes our PATCH fail with 409; after re-reading
// the node is already unschedulable and must not be claimed.
func TestCordonNodes_ConcurrentAdminCordonNotClaimed(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 1, 10, false, false)

	var once sync.Once
	f.patchHook = func(_ context.Context, name string, _ bool) error {
		once.Do(func() { f.setUnschedulable(name, true) })
		return nil
	}

	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 0 {
		t.Fatalf("node cordoned concurrently by someone else was claimed: %v", got)
	}
	if f.marker("n1") != "" {
		t.Errorf("concurrently cordoned node must not get our marker")
	}
	if len(f.patches) != 0 {
		t.Errorf("no patch should have been applied, got %v", f.patches)
	}
	if !f.unschedulable("n1") {
		t.Errorf("admin cordon must be preserved")
	}
}

// R2: a conflict caused by an unrelated change (labels, status) is retried.
func TestCordonNodes_ConflictRetried(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 1, 10, false, false)

	var once sync.Once
	f.patchHook = func(_ context.Context, name string, _ bool) error {
		once.Do(func() { f.setUnschedulable(name, false) }) // bumps resourceVersion only
		return nil
	}
	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 {
		t.Fatalf("recorded = %v, want [n1]", got)
	}
	if !f.unschedulable("n1") || f.marker("n1") != c.RunID() {
		t.Errorf("node must be cordoned with our marker after retry")
	}
}

// R3: the PATCH is applied but the response is lost. The fresh-context
// re-check finds our marker and records the node.
func TestCordonNodes_AppliedButResponseLost(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 1, 10, false, false)

	f.afterPatchErr = func(string) error { return errors.New("connection reset by peer") }

	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v (applied cordon should be confirmed by marker)", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("recorded = %v, want [n1]", got)
	}
}

// A definite API rejection is not re-checked and nothing is recorded.
func TestCordonNodes_ForbiddenNotRecorded(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 1, 10, false, false)
	f.patchHook = func(context.Context, string, bool) error {
		return apierrors.NewForbidden(nodesGR, "n1", errors.New("no patch"))
	}
	err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
	if got := c.CordonedNodes(); len(got) != 0 {
		t.Errorf("recorded = %v, want none", got)
	}
}

// Karpenter may remove a node at any time; a missing node is not an error.
func TestCordonNodes_NodeDisappearsIsNotError(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("gone", false, nil)
	f.addNode("stays", false, nil)
	f.addPod("app", "p1", "gone")
	f.addPod("app", "p2", "stays")
	c := NewClusterOperations(f, 1, 10, false, false)

	f.patchHook = func(_ context.Context, name string, _ bool) error {
		if name == "gone" {
			f.deleteNode(name)
		}
		return nil
	}
	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 || got[0] != "stays" {
		t.Fatalf("recorded = %v, want [stays]", got)
	}
}

func TestCordonNodes_ContextCancellation(t *testing.T) {
	const parallel = 2
	const nodeCount = 10

	f := newClusterFakeClient()
	for i := 0; i < nodeCount; i++ {
		f.addNode(fmt.Sprintf("node-%02d", i), false, nil)
	}
	c := NewClusterOperations(f, parallel, 10, false, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The first patch cancels the context and then blocks until the
	// cancellation is observed, simulating a slow API call interrupted by
	// Ctrl-C.
	var once sync.Once
	f.patchHook = func(ctx context.Context, _ string, _ bool) error {
		once.Do(cancel)
		<-ctx.Done()
		return ctx.Err()
	}

	err := runWithTimeout(t, 5*time.Second, func() error {
		return c.CordonNodes(ctx, nil, true, nil, nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CordonNodes error = %v, want context.Canceled", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("no patch should have been applied after cancellation, got %v", f.patches)
	}
	if got := c.CordonedNodes(); len(got) != 0 {
		t.Errorf("recorded = %v, want none", got)
	}
}

func TestCordonNodes_LabelFilters(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("worker", false, map[string]string{"role": "worker"})
	f.addNode("fargate", false, map[string]string{"role": "worker", "eks.amazonaws.com/compute-type": "fargate"})
	f.addNode("infra", false, map[string]string{"role": "infra"})
	for _, n := range []string{"worker", "fargate", "infra"} {
		f.addPod("app", "pod-"+n, n)
	}
	f.addPod("app", "pending", "") // unscheduled pod must be ignored
	c := NewClusterOperations(f, 2, 10, false, false)

	err := c.CordonNodes(context.Background(), []string{"app"}, false,
		[]string{"role=worker"}, []string{"eks.amazonaws.com/compute-type=fargate"})
	if err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 || got[0] != "worker" {
		t.Fatalf("recorded = %v, want [worker]", got)
	}
	if f.unschedulable("fargate") || f.unschedulable("infra") {
		t.Errorf("filtered nodes must not be cordoned")
	}
}

func TestNodeMatchesFilters_EmptyValueRequiresKey(t *testing.T) {
	parse := func(items ...string) map[string]string {
		m, err := ParseLabelsOrAnnotationsStrict(items)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	withEmpty := map[string]string{"spot": ""}
	without := map[string]string{"other": "x"}

	// include "spot=": key must be present with an empty value
	if !nodeMatchesFilters(withEmpty, parse("spot="), nil) {
		t.Errorf("node with spot=\"\" must match include spot=")
	}
	if nodeMatchesFilters(without, parse("spot="), nil) {
		t.Errorf("node without spot must NOT match include spot=")
	}
	if nodeMatchesFilters(map[string]string{"spot": "yes"}, parse("spot="), nil) {
		t.Errorf("node with spot=yes must NOT match include spot=")
	}
	// exclude "spot=": only nodes having the key with an empty value are excluded
	if nodeMatchesFilters(withEmpty, nil, parse("spot=")) {
		t.Errorf("node with spot=\"\" must be excluded by spot=")
	}
	if !nodeMatchesFilters(without, nil, parse("spot=")) {
		t.Errorf("node without spot must NOT be excluded by spot=")
	}
}

func TestCordonNodes_EmptyValueLabelFilterEndToEnd(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("tagged", false, map[string]string{"drain": ""})
	f.addNode("untagged", false, nil)
	f.addPod("app", "p1", "tagged")
	f.addPod("app", "p2", "untagged")
	c := NewClusterOperations(f, 2, 10, false, false)

	if err := c.CordonNodes(context.Background(), []string{"app"}, false, []string{"drain="}, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if got := c.CordonedNodes(); len(got) != 1 || got[0] != "tagged" {
		t.Fatalf("recorded = %v, want [tagged]", got)
	}
}

func TestCordonNodes_DryRunDoesNotPatch(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 2, 10, false, true)

	if err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil); err != nil {
		t.Fatalf("CordonNodes: %v", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("dry-run must not patch nodes, got %v", f.patches)
	}
}

func TestPlanCordon_SameSelectionAsExecute(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("a", false, map[string]string{"role": "worker"})
	f.addNode("b", true, map[string]string{"role": "worker"})
	f.addNode("c", false, map[string]string{"role": "infra"})
	f.addNode("idle", false, map[string]string{"role": "worker"})
	f.addPod("app", "p1", "a")
	f.addPod("app", "p2", "b")
	f.addPod("app", "p3", "c")
	c := NewClusterOperations(f, 2, 10, false, true)

	plan, err := c.PlanCordon(context.Background(), []string{"app"}, false, []string{"role=worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || plan.Nodes[0].Name != "a" || plan.Nodes[1].Name != "b" || !plan.Nodes[1].AlreadyUnschedulable {
		t.Fatalf("plan = %+v", plan.Nodes)
	}

	plan, err = c.PlanCordon(context.Background(), []string{"app"}, true, []string{"role=worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 3 { // a, b, idle
		t.Fatalf("cordon-all plan = %+v", plan.Nodes)
	}

	if _, err := c.PlanCordon(context.Background(), []string{"app"}, false, []string{"bad"}, nil); err == nil {
		t.Errorf("malformed node label filter must be rejected")
	}
}

func localPV(name, claimNS, claim string, spec corev1.PersistentVolumeSpec) corev1.PersistentVolume {
	spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNS, Name: claim}
	return corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

func hostnameAffinity(host string) *corev1.VolumeNodeAffinity {
	return &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: hostnameLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{host}}},
	}}}}
}

func TestPlanCordon_LocalVolumeWarnings(t *testing.T) {
	f := newClusterFakeClient()
	f.withPVs = true
	f.addNode("n1", false, nil)
	f.addNode("n2", false, map[string]string{"skip": "yes"})
	f.addPod("db", "local-pod", "n1", "data-local")
	f.addPod("db", "pinned-pod", "n1", "data-pinned")
	f.addPod("db", "ebs-pod", "n1", "data-ebs")
	f.addPod("db", "other-node", "n2", "data-n2")
	zone := &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
	}}}}
	f.pvs = []corev1.PersistentVolume{
		localPV("pv-local", "db", "data-local", corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/mnt"}},
			NodeAffinity:           hostnameAffinity("n1"),
		}),
		localPV("pv-pinned", "db", "data-pinned", corev1.PersistentVolumeSpec{NodeAffinity: hostnameAffinity("n1")}),
		localPV("pv-ebs", "db", "data-ebs", corev1.PersistentVolumeSpec{NodeAffinity: zone}),
		localPV("pv-n2", "db", "data-n2", corev1.PersistentVolumeSpec{NodeAffinity: hostnameAffinity("n2")}),
	}
	c := NewClusterOperations(f, 2, 10, false, false)

	plan, err := c.PlanCordon(context.Background(), []string{"db"}, false, nil, []string{"skip=yes"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalVolumeCheckErr != nil {
		t.Fatalf("unexpected check error: %v", plan.LocalVolumeCheckErr)
	}
	if len(plan.LocalVolumes) != 2 || plan.LocalVolumes[0].Pod != "db/local-pod" || plan.LocalVolumes[1].Pod != "db/pinned-pod" {
		t.Fatalf("local volumes = %+v", plan.LocalVolumes)
	}

	// Without PV support the check reports why it could not run, but the
	// plan itself still succeeds.
	f.withPVs = false
	plan, err = c.PlanCordon(context.Background(), []string{"db"}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalVolumeCheckErr == nil || len(plan.Nodes) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
}

// #9: pods of other namespaces on the nodes to be cordoned are checked too;
// without cluster-wide pod access the check is reported as incomplete.
func TestPlanCordon_LocalVolumeCheckCoversAllNamespaces(t *testing.T) {
	f := newClusterFakeClient()
	f.withPVs = true
	f.addNode("n1", false, nil)
	f.addNode("n2", false, nil)
	f.addPod("app", "web", "n1")
	f.addPod("db", "postgres-0", "n1", "data-pg")
	f.addPod("db", "postgres-1", "n2", "data-pg1") // n2 is not selected
	f.pvs = []corev1.PersistentVolume{
		localPV("pv-pg", "db", "data-pg", corev1.PersistentVolumeSpec{NodeAffinity: hostnameAffinity("n1")}),
		localPV("pv-pg1", "db", "data-pg1", corev1.PersistentVolumeSpec{NodeAffinity: hostnameAffinity("n2")}),
	}
	c := NewClusterOperations(f, 2, 10, false, false)

	plan, err := c.PlanCordon(context.Background(), []string{"app"}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].Name != "n1" {
		t.Fatalf("nodes = %+v", plan.Nodes)
	}
	if plan.LocalVolumeCheckIncomplete != nil || len(plan.LocalVolumes) != 1 || plan.LocalVolumes[0].Pod != "db/postgres-0" {
		t.Fatalf("local volumes = %+v (incomplete: %v)", plan.LocalVolumes, plan.LocalVolumeCheckIncomplete)
	}

	f.allNamespacesErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("cluster-wide list denied"))
	plan, err = c.PlanCordon(context.Background(), []string{"app"}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LocalVolumeCheckIncomplete == nil || len(plan.LocalVolumes) != 0 {
		t.Fatalf("expected an incomplete check, got %+v (incomplete: %v)", plan.LocalVolumes, plan.LocalVolumeCheckIncomplete)
	}
}

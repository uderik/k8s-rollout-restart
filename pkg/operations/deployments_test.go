package operations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
)

// fakeClient adapts a client-go fake clientset to the K8sClient interface.
type fakeClient struct{ cs *fake.Clientset }

func newFakeClient(objs ...runtime.Object) *fakeClient {
	return &fakeClient{cs: fake.NewSimpleClientset(objs...)}
}

func (f *fakeClient) CoreV1() CoreV1Interface    { return fakeCoreV1{f.cs} }
func (f *fakeClient) AppsV1() AppsV1Interface    { return fakeAppsV1{f.cs} }
func (f *fakeClient) RESTClient() rest.Interface { return nil }
func (f *fakeClient) ClearCache()                {}

type fakeCoreV1 struct{ cs *fake.Clientset }

func (c fakeCoreV1) Pods(ns string) PodInterface    { return c.cs.CoreV1().Pods(ns) }
func (c fakeCoreV1) Nodes() NodeInterface           { return fakeNodes{c.cs.CoreV1().Nodes()} }
func (c fakeCoreV1) Namespaces() NamespaceInterface { return c.cs.CoreV1().Namespaces() }

type fakeAppsV1 struct{ cs *fake.Clientset }

func (a fakeAppsV1) Deployments(ns string) DeploymentInterface {
	return fakeDeployments{a.cs.AppsV1().Deployments(ns)}
}
func (a fakeAppsV1) StatefulSets(ns string) StatefulSetInterface {
	return fakeStatefulSets{a.cs.AppsV1().StatefulSets(ns)}
}
func (a fakeAppsV1) ReplicaSets(ns string) ReplicaSetInterface { return a.cs.AppsV1().ReplicaSets(ns) }

// fakeNodes drops the variadic subresources argument of the typed Patch.
type fakeNodes struct {
	typedcorev1.NodeInterface
}

func (f fakeNodes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*corev1.Node, error) {
	return f.NodeInterface.Patch(ctx, name, pt, data, opts)
}

// fakeDeployments drops the variadic subresources argument of the typed Patch.
type fakeDeployments struct {
	typedappsv1.DeploymentInterface
}

func (f fakeDeployments) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error) {
	return f.DeploymentInterface.Patch(ctx, name, pt, data, opts)
}

// fakeStatefulSets drops the variadic subresources argument of the typed Patch.
type fakeStatefulSets struct {
	typedappsv1.StatefulSetInterface
}

func (f fakeStatefulSets) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.StatefulSet, error) {
	return f.StatefulSetInterface.Patch(ctx, name, pt, data, opts)
}

// patchedNames returns "namespace/name" of every object patched via the fake.
func patchedNames(cs *fake.Clientset, resource string) []string {
	var out []string
	for _, a := range cs.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetVerb() == "patch" && a.GetResource().Resource == resource {
			out = append(out, p.GetNamespace()+"/"+p.GetName())
		}
	}
	sort.Strings(out)
	return out
}

func int32Ptr(i int32) *int32 { return &i }

func boolPtr(b bool) *bool { return &b }

// readyDeployment returns a deployment whose rollout is complete.
func readyDeployment(ns, name string, age time.Duration) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			UID:               types.UID("uid-" + ns + "-" + name),
			Generation:        1,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		},
	}
}

func withCanaryOwner(d *appsv1.Deployment) *appsv1.Deployment {
	d.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "flagger.app/v1beta1", Kind: "Canary", Name: strings.TrimSuffix(d.Name, "-primary"),
		UID: "canary-uid", Controller: boolPtr(true),
	}}
	return d
}

func replicaSetFor(d *appsv1.Deployment, name string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: d.Namespace, Name: name, UID: types.UID("uid-rs-" + name),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: boolPtr(true),
		}},
	}}
}

func podFor(ns, name, ownerKind, ownerName string, ownerUID types.UID, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: ownerKind, Name: ownerName, UID: ownerUID, Controller: boolPtr(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1"}}},
	}
}

func newTestDeploymentOps(client K8sClient, opts func(d *DeploymentOperations)) *DeploymentOperations {
	d := &DeploymentOperations{
		clientset:    client,
		parallel:     2,
		timeout:      5,
		log:          logger.NewLogger(false),
		pollInterval: 10 * time.Millisecond,
	}
	if opts != nil {
		opts(d)
	}
	return d
}

func TestDeploymentRolloutStatus(t *testing.T) {
	base := func(mut func(d *appsv1.Deployment)) *appsv1.Deployment {
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3,
			},
		}
		if mut != nil {
			mut(d)
		}
		return d
	}

	tests := []struct {
		name          string
		deployment    *appsv1.Deployment
		minGeneration int64
		wantDone      bool
		wantTerminal  bool
	}{
		{name: "complete without Progressing condition", deployment: base(nil), minGeneration: 2, wantDone: true},
		{
			name: "complete with Progressing=True",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}}
			}),
			minGeneration: 2, wantDone: true,
		},
		{
			name: "mid-rollout surge: desired=3 replicas=4 updated=3 ready=3 available=2",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.Replicas = 4
				d.Status.AvailableReplicas = 2
			}),
			minGeneration: 2,
		},
		{
			name:       "generation from PATCH not observed",
			deployment: base(nil), minGeneration: 3,
		},
		{
			name: "observed generation behind metadata generation",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.ObservedGeneration = 1
			}),
		},
		{
			name: "not all replicas updated",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.UpdatedReplicas = 2
			}),
		},
		{
			name: "previously ready deployment dropped to zero ready",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.ReadyReplicas = 0
				d.Status.AvailableReplicas = 0
			}),
		},
		{
			name: "ready but not yet available",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.AvailableReplicas = 2
			}),
		},
		{
			name: "ProgressDeadlineExceeded is terminal",
			deployment: base(func(d *appsv1.Deployment) {
				d.Status.UpdatedReplicas = 1
				d.Status.Conditions = []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out",
				}}
			}),
			wantTerminal: true,
		},
		{
			name: "nil replicas defaults to 1",
			deployment: base(func(d *appsv1.Deployment) {
				d.Spec.Replicas = nil
				d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
			}),
			wantDone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, reason, terminal := deploymentRolloutStatus(tt.deployment, tt.minGeneration)
			if done != tt.wantDone {
				t.Errorf("done = %v, want %v (reason: %s)", done, tt.wantDone, reason)
			}
			if (terminal != nil) != tt.wantTerminal {
				t.Errorf("terminal = %v, want terminal=%v", terminal, tt.wantTerminal)
			}
			if !done && terminal == nil && reason == "" {
				t.Errorf("expected a reason for a not-ready deployment")
			}
		})
	}
}

func TestWaitForDeploymentsReady_ProgressDeadlineFailsFast(t *testing.T) {
	dep := readyDeployment("ns", "web", time.Hour)
	dep.Status.UpdatedReplicas = 0
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	}}
	d := newTestDeploymentOps(newFakeClient(dep), func(d *DeploymentOperations) { d.timeout = 30 })

	start := time.Now()
	err := d.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "progress deadline exceeded") {
		t.Fatalf("expected progress deadline error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("waiter did not fail fast")
	}
}

func TestWaitForDeploymentsReady_TimeoutAndCancel(t *testing.T) {
	dep := readyDeployment("ns", "web", time.Hour)
	dep.Status.ReadyReplicas = 0
	targets := []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}

	d := newTestDeploymentOps(newFakeClient(dep), func(d *DeploymentOperations) { d.timeout = 1 })
	err := d.waitForDeploymentsReady(context.Background(), targets, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout error, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	d = newTestDeploymentOps(newFakeClient(dep), func(d *DeploymentOperations) { d.timeout = 30 })
	err = d.waitForDeploymentsReady(ctx, targets, nil)
	if err == nil || !strings.Contains(err.Error(), "cancelled") || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation error, got %v", err)
	}
}

func TestRestartDeployments_SkipsPaused(t *testing.T) {
	paused := readyDeployment("ns", "paused", time.Hour)
	paused.Spec.Paused = true
	active := readyDeployment("ns", "active", time.Hour)
	client := newFakeClient(paused, active)
	d := newTestDeploymentOps(client, nil)

	if err := d.RestartDeployments(context.Background(), []string{"ns"}); err != nil {
		t.Fatalf("RestartDeployments: %v", err)
	}
	if got := patchedNames(client.cs, "deployments"); strings.Join(got, ",") != "ns/active" {
		t.Fatalf("patched = %v, want [ns/active]", got)
	}
}

// flaggerScenario builds a namespace exercising the Flagger, age, paused and
// pod-ownership selection rules.
func flaggerScenario() []runtime.Object {
	ns := "ns"
	// Flagger pair whose primary is too young: neither must be restarted.
	web := readyDeployment(ns, "web", time.Hour)
	webPrimary := withCanaryOwner(readyDeployment(ns, "web-primary", time.Second))
	// Flagger pair with old primary: only the primary is restarted.
	app := readyDeployment(ns, "app", time.Hour)
	appPrimary := withCanaryOwner(readyDeployment(ns, "app-primary", time.Hour))
	// "-primary" without a Canary owner: both are ordinary deployments.
	api := readyDeployment(ns, "api", time.Hour)
	apiPrimary := readyDeployment(ns, "api-primary", time.Hour)
	// Paused deployment.
	paused := readyDeployment(ns, "paused", time.Hour)
	paused.Spec.Paused = true
	return []runtime.Object{web, webPrimary, app, appPrimary, api, apiPrimary, paused}
}

func TestDeploymentSelectionParity(t *testing.T) {
	minAge := time.Minute
	tests := []struct {
		name      string
		objects   []runtime.Object
		noFlagger bool
		podLabels []string
		want      []string
	}{
		{
			name:    "flagger relationships built before min-age filter",
			objects: flaggerScenario(),
			want:    []string{"ns/api", "ns/api-primary", "ns/app-primary"},
		},
		{
			name:      "no flagger filter restarts all old enough deployments",
			objects:   flaggerScenario(),
			noFlagger: true,
			want:      []string{"ns/api", "ns/api-primary", "ns/app", "ns/app-primary", "ns/web"},
		},
		{
			name: "pod filter verifies controller chain",
			objects: func() []runtime.Object {
				a := readyDeployment("ns", "a", time.Hour)
				b := readyDeployment("ns", "b", time.Hour)
				// b's selector overlaps a's pods, but the pods are owned by a's ReplicaSet.
				b.Spec.Selector = a.Spec.Selector
				rsA := replicaSetFor(a, "a-1")
				pod := podFor("ns", "a-1-x", "ReplicaSet", rsA.Name, rsA.UID, map[string]string{"app": "a", "tier": "web"})
				return []runtime.Object{a, b, rsA, pod}
			}(),
			podLabels: []string{"tier=web"},
			want:      []string{"ns/a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := func(d *DeploymentOperations) {
				d.minAge = &minAge
				d.noFlagger = tt.noFlagger
				d.podLabels = tt.podLabels
			}

			preview, err := newTestDeploymentOps(newFakeClient(tt.objects...), opts).GetDeploymentsToRestart(context.Background(), []string{"ns"})
			if err != nil {
				t.Fatalf("GetDeploymentsToRestart: %v", err)
			}
			sort.Strings(preview)

			client := newFakeClient(tt.objects...)
			if err := newTestDeploymentOps(client, opts).RestartDeployments(context.Background(), []string{"ns"}); err != nil {
				t.Fatalf("RestartDeployments: %v", err)
			}
			executed := patchedNames(client.cs, "deployments")

			if strings.Join(preview, ",") != strings.Join(tt.want, ",") {
				t.Errorf("preview = %v, want %v", preview, tt.want)
			}
			if strings.Join(executed, ",") != strings.Join(preview, ",") {
				t.Errorf("executed = %v, preview = %v", executed, preview)
			}
		})
	}
}

func TestRestartDeployments_AmbiguousPatchStopsNewLaunches(t *testing.T) {
	client := newFakeClient(
		readyDeployment("ns1", "bad", time.Hour),
		readyDeployment("ns1", "good", time.Hour),
		readyDeployment("ns2", "other", time.Hour),
	)
	client.cs.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() == "bad" {
			return true, nil, errors.New("connection reset") // ambiguous
		}
		return false, nil, nil
	})
	for _, skipWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("skipWait=%v", skipWait), func(t *testing.T) {
			client.cs.ClearActions()
			d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 1; d.skipWait = skipWait })

			err := d.RestartDeployments(context.Background(), []string{"ns1", "ns2"})
			if err == nil || !strings.Contains(err.Error(), "connection reset") ||
				!strings.Contains(err.Error(), "ns1/good: not started due to earlier failure") ||
				!strings.Contains(err.Error(), "ns2/other: not started due to earlier failure") {
				t.Fatalf("expected patch error and not-started targets, got %v", err)
			}
			if got := strings.Join(patchedNames(client.cs, "deployments"), ","); got != "ns1/bad" {
				t.Fatalf("patched = %s, want only ns1/bad", got)
			}
		})
	}
}

func TestRestartDeployments_SkipWaitDefiniteRejectionStops(t *testing.T) {
	client := newFakeClient(readyDeployment("ns", "a", time.Hour), readyDeployment("ns", "b", time.Hour))
	client.cs.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() == "a" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "a", errors.New("denied"))
		}
		return false, nil, nil
	})
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 1; d.skipWait = true })

	err := d.RestartDeployments(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected forbidden error, got %v", err)
	}
	if got := strings.Join(patchedNames(client.cs, "deployments"), ","); got != "ns/a" {
		t.Fatalf("patched = %s: with --skip-wait a definite rejection must stop new launches", got)
	}
	if !strings.Contains(err.Error(), "not started due to earlier failure") {
		t.Fatalf("expected ns/b to be reported as not started, got %v", err)
	}
}

func TestRestartDeployments_PodFilterErrorPropagates(t *testing.T) {
	client := newFakeClient(readyDeployment("ns", "web", time.Hour))
	client.cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.podLabels = []string{"app=web"} })

	if err := d.RestartDeployments(context.Background(), []string{"ns"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected pod list error, got %v", err)
	}
	if _, err := d.GetDeploymentsToRestart(context.Background(), []string{"ns"}); err == nil {
		t.Fatalf("expected GetDeploymentsToRestart to return the error")
	}
	if got := patchedNames(client.cs, "deployments"); len(got) != 0 {
		t.Fatalf("nothing should be patched, got %v", got)
	}
}

// rolloutTracker simulates rollouts on top of the fake clientset: a PATCH
// starts a rollout, which then reports "not ready" for notReadyPolls GETs
// before completing. It records how many rollouts are in progress (patched
// but not yet observed ready) at the same time.
type rolloutTracker struct {
	mu            sync.Mutex
	notReadyPolls int
	failing       map[string]bool // "ns/name" whose rollout fails terminally
	remaining     map[string]int
	inFlight      int
	peak          int
	completed     []string
}

func newRolloutTracker(notReadyPolls int, failing ...string) *rolloutTracker {
	tr := &rolloutTracker{notReadyPolls: notReadyPolls, failing: map[string]bool{}, remaining: map[string]int{}}
	for _, f := range failing {
		tr.failing[f] = true
	}
	return tr
}

func (tr *rolloutTracker) patched(key string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.remaining[key] = tr.notReadyPolls
	tr.inFlight++
	if tr.inFlight > tr.peak {
		tr.peak = tr.inFlight
	}
}

// poll reports whether the rollout of key is complete. Objects that were not
// patched report their stored status.
func (tr *rolloutTracker) poll(key string) (ready, failed bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	n, ok := tr.remaining[key]
	if !ok {
		return true, false
	}
	if tr.failing[key] {
		return false, true
	}
	if n > 0 {
		tr.remaining[key] = n - 1
		return false, false
	}
	delete(tr.remaining, key)
	tr.inFlight--
	tr.completed = append(tr.completed, key)
	return true, false
}

func (tr *rolloutTracker) install(cs *fake.Clientset, resource string, notReady func(obj runtime.Object, failed bool) runtime.Object) {
	cs.PrependReactor("patch", resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr.patched(a.GetNamespace() + "/" + a.(k8stesting.PatchAction).GetName())
		return false, nil, nil
	})
	cs.PrependReactor("get", resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		g := a.(k8stesting.GetAction)
		obj, err := cs.Tracker().Get(a.GetResource(), g.GetNamespace(), g.GetName())
		if err != nil {
			return true, nil, err
		}
		ready, failed := tr.poll(g.GetNamespace() + "/" + g.GetName())
		if ready {
			return true, obj, nil
		}
		return true, notReady(obj.DeepCopyObject(), failed), nil
	})
}

func deploymentNotReady(obj runtime.Object, failed bool) runtime.Object {
	d := obj.(*appsv1.Deployment)
	d.Status.ReadyReplicas = 0
	if failed {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		}}
	}
	return d
}

func manyDeployments(perNS int, namespaces ...string) []runtime.Object {
	var objs []runtime.Object
	for _, ns := range namespaces {
		for i := 0; i < perNS; i++ {
			objs = append(objs, readyDeployment(ns, fmt.Sprintf("d%d", i), time.Hour))
		}
	}
	return objs
}

func TestRestartDeployments_ParallelBoundsInProgressRollouts(t *testing.T) {
	for _, parallel := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			client := newFakeClient(manyDeployments(3, "ns1", "ns2", "ns3")...)
			tr := newRolloutTracker(3)
			tr.install(client.cs, "deployments", deploymentNotReady)
			d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = parallel })

			if err := d.RestartDeployments(context.Background(), []string{"ns1", "ns2", "ns3"}); err != nil {
				t.Fatalf("RestartDeployments: %v", err)
			}
			if tr.peak > parallel {
				t.Errorf("peak in-progress rollouts = %d, exceeds --parallel=%d", tr.peak, parallel)
			}
			if tr.peak < parallel {
				t.Errorf("peak in-progress rollouts = %d, expected workers to use all %d slots", tr.peak, parallel)
			}
			if len(tr.completed) != 9 || tr.inFlight != 0 {
				t.Errorf("completed = %d (in flight %d), want all 9 rollouts finished", len(tr.completed), tr.inFlight)
			}
		})
	}
}

func TestRestartDeployments_FailedRolloutStopsNewLaunches(t *testing.T) {
	client := newFakeClient(manyDeployments(3, "ns1", "ns2")...)
	tr := newRolloutTracker(1, "ns1/d0")
	tr.install(client.cs, "deployments", deploymentNotReady)
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 1 })

	err := d.RestartDeployments(context.Background(), []string{"ns1", "ns2"})
	if err == nil || !strings.Contains(err.Error(), "ns1/d0") || !strings.Contains(err.Error(), "progress deadline exceeded") {
		t.Fatalf("expected ns1/d0 rollout failure, got %v", err)
	}
	if got := strings.Count(err.Error(), "not started due to earlier failure"); got != 5 {
		t.Errorf("not-started targets = %d, want 5: %v", got, err)
	}
	if got := strings.Join(patchedNames(client.cs, "deployments"), ","); got != "ns1/d0" {
		t.Errorf("patched = %s, want only ns1/d0", got)
	}
}

func TestRestartDeployments_TimeoutStopsNewLaunches(t *testing.T) {
	// parallel=1, 1s per-resource timeout, the first rollout never completes:
	// it may still be running in the cluster, so the second deployment must
	// never be patched (otherwise two rollouts would be in progress).
	client := newFakeClient(manyDeployments(2, "ns")...)
	tr := newRolloutTracker(1_000_000)
	tr.install(client.cs, "deployments", deploymentNotReady)
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 1; d.timeout = 1 })

	err := d.RestartDeployments(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "timed out") ||
		!strings.Contains(err.Error(), "deployment ns/d1: not started due to earlier failure") {
		t.Fatalf("expected timeout of d0 and d1 not started, got %v", err)
	}
	if got := strings.Join(patchedNames(client.cs, "deployments"), ","); got != "ns/d0" {
		t.Errorf("patched = %s, want only ns/d0", got)
	}
	if tr.peak != 1 {
		t.Errorf("peak in-progress rollouts = %d, want 1", tr.peak)
	}
}

func TestRestartDeployments_InFlightRolloutsFinishAfterFailure(t *testing.T) {
	// parallel=2: d0 fails quickly while d1 is in progress; d1 must still be
	// waited for, d2/d3 must not be started.
	client := newFakeClient(manyDeployments(4, "ns")...)
	tr := newRolloutTracker(20, "ns/d0")
	tr.install(client.cs, "deployments", deploymentNotReady)
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 2 })

	err := d.RestartDeployments(context.Background(), []string{"ns"})
	if err == nil || strings.Count(err.Error(), "not started due to earlier failure") != 2 {
		t.Fatalf("expected d2, d3 not started, got %v", err)
	}
	if got := strings.Join(patchedNames(client.cs, "deployments"), ","); got != "ns/d0,ns/d1" {
		t.Errorf("patched = %s", got)
	}
	if strings.Join(tr.completed, ",") != "ns/d1" {
		t.Errorf("in-flight rollout ns/d1 must be awaited, completed = %v", tr.completed)
	}
}

func TestRestartDeployments_SkipWait(t *testing.T) {
	client := newFakeClient(manyDeployments(4, "ns1", "ns2")...)
	tr := newRolloutTracker(1_000_000) // rollouts never complete
	tr.install(client.cs, "deployments", deploymentNotReady)
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 2; d.skipWait = true; d.timeout = 30 })

	start := time.Now()
	if err := d.RestartDeployments(context.Background(), []string{"ns1", "ns2"}); err != nil {
		t.Fatalf("RestartDeployments: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("--skip-wait must not wait for readiness")
	}
	if got := len(patchedNames(client.cs, "deployments")); got != 8 {
		t.Errorf("patched %d deployments, want 8", got)
	}
	for _, a := range client.cs.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "deployments" {
			t.Fatalf("--skip-wait must not poll deployment status")
		}
	}
}

// concurrentPatchClient wraps the fake client and records the number of
// concurrent Deployment PATCH calls. The fake clientset serialises reactors,
// so concurrency is measured outside of it.
type concurrentPatchClient struct {
	*fakeClient
	mu        sync.Mutex
	cur, peak int
}

func (c *concurrentPatchClient) AppsV1() AppsV1Interface {
	return concurrentPatchApps{c.fakeClient.AppsV1(), c}
}

type concurrentPatchApps struct {
	AppsV1Interface
	c *concurrentPatchClient
}

func (a concurrentPatchApps) Deployments(ns string) DeploymentInterface {
	return concurrentPatchDeployments{a.AppsV1Interface.Deployments(ns), a.c}
}

type concurrentPatchDeployments struct {
	DeploymentInterface
	c *concurrentPatchClient
}

func (d concurrentPatchDeployments) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error) {
	d.c.mu.Lock()
	d.c.cur++
	d.c.peak = max(d.c.peak, d.c.cur)
	d.c.mu.Unlock()
	defer func() {
		d.c.mu.Lock()
		d.c.cur--
		d.c.mu.Unlock()
	}()
	time.Sleep(20 * time.Millisecond)
	return d.DeploymentInterface.Patch(ctx, name, pt, data, opts)
}

func TestRestartDeployments_SkipWaitBoundsConcurrentPatches(t *testing.T) {
	client := &concurrentPatchClient{fakeClient: newFakeClient(manyDeployments(5, "ns1", "ns2")...)}
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 3; d.skipWait = true })

	if err := d.RestartDeployments(context.Background(), []string{"ns1", "ns2"}); err != nil {
		t.Fatalf("RestartDeployments: %v", err)
	}
	if client.peak > 3 || client.peak < 2 {
		t.Errorf("peak concurrent patches = %d, want <= 3 (and actually concurrent)", client.peak)
	}
	if got := len(patchedNames(client.cs, "deployments")); got != 10 {
		t.Errorf("patched %d deployments, want 10", got)
	}
}

package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func waiting(name, reason, message string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		RestartCount: restarts,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}
}

func crashLooping(name string, restarts int32) corev1.ContainerStatus {
	cs := waiting(name, CrashLoopBackOffReason, "back-off restarting failed container", restarts)
	cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}
	return cs
}

func podWithStatus(init, containers []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Status:     corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: containers},
	}
}

func TestPodHealthPolicy_BrokenPodReason(t *testing.T) {
	def := DefaultPodHealthPolicy()
	terminating := podWithStatus(nil, []corev1.ContainerStatus{crashLooping("app", 10)})
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	running := podWithStatus(nil, []corev1.ContainerStatus{{
		Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}})

	tests := []struct {
		name   string
		policy PodHealthPolicy
		pod    *corev1.Pod
		want   []string // substrings; nil means not broken
	}{
		{name: "running but not ready is slow", policy: def, pod: running},
		{name: "container creating is slow", policy: def, pod: podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "ContainerCreating", "", 0)})},
		{name: "pending without statuses is slow", policy: def, pod: podWithStatus(nil, nil)},
		{name: "crash loop below threshold", policy: def, pod: podWithStatus(nil, []corev1.ContainerStatus{crashLooping("app", 2)})},
		{
			name: "crash loop at threshold", policy: def, pod: podWithStatus(nil, []corev1.ContainerStatus{crashLooping("app", 3)}),
			want: []string{"container app", "CrashLoopBackOff", "3 restarts", "last exit: Error, code 1"},
		},
		{name: "crash loop check disabled", policy: NewPodHealthPolicy(0, DefaultFailFastReasons), pod: podWithStatus(nil, []corev1.ContainerStatus{crashLooping("app", 50)})},
		{
			name: "init container crash loop", policy: def, pod: podWithStatus([]corev1.ContainerStatus{crashLooping("migrate", 4)}, nil),
			want: []string{"init container migrate", "CrashLoopBackOff"},
		},
		{
			name: "image pull back-off", policy: def,
			pod:  podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "ImagePullBackOff", `Back-off pulling image "x:1"`, 0)}),
			want: []string{"container app", "ImagePullBackOff", `Back-off pulling image "x:1"`},
		},
		{
			name: "missing secret", policy: def,
			pod:  podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "CreateContainerConfigError", `secret "db" not found`, 0)}),
			want: []string{"CreateContainerConfigError", `secret "db" not found`},
		},
		{
			name: "custom reason list", policy: NewPodHealthPolicy(3, []string{"ErrImagePull"}),
			pod:  podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "ErrImagePull", "", 0)}),
			want: []string{"ErrImagePull"},
		},
		{
			name: "reason not in custom list", policy: NewPodHealthPolicy(3, []string{"ErrImagePull"}),
			pod: podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "ImagePullBackOff", "", 0)}),
		},
		{name: "empty reason list disables", policy: NewPodHealthPolicy(3, nil), pod: podWithStatus(nil, []corev1.ContainerStatus{waiting("app", "ImagePullBackOff", "", 0)})},
		{name: "terminating pod is ignored", policy: def, pod: terminating},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.brokenPodReason(tt.pod)
			if tt.want == nil {
				if got != "" {
					t.Fatalf("expected not broken, got %q", got)
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("reason %q does not contain %q", got, w)
				}
			}
		})
	}

	if NewPodHealthPolicy(0, nil).enabled() || (PodHealthPolicy{}).enabled() {
		t.Error("a policy with both checks disabled must not be enabled")
	}
}

// brokenRolloutDeployment returns a Deployment at revision 2 whose rollout is
// in progress, its old (revision 1) and new (revision 2) ReplicaSets.
func brokenRolloutDeployment() (*appsv1.Deployment, *appsv1.ReplicaSet, *appsv1.ReplicaSet) {
	d := readyDeployment("ns", "web", time.Hour)
	d.Annotations = map[string]string{deploymentRevisionAnnotation: "2"}
	d.Status.ReadyReplicas = 0
	oldRS := replicaSetFor(d, "web-1")
	oldRS.Labels = map[string]string{"app": "web"}
	oldRS.Annotations = map[string]string{deploymentRevisionAnnotation: "1"}
	newRS := replicaSetFor(d, "web-2")
	newRS.Labels = map[string]string{"app": "web"}
	newRS.Annotations = map[string]string{deploymentRevisionAnnotation: "2"}
	return d, oldRS, newRS
}

func rsPod(rs *appsv1.ReplicaSet, name string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	p := podFor(rs.Namespace, name, "ReplicaSet", rs.Name, rs.UID, map[string]string{"app": "web"})
	p.Status.ContainerStatuses = statuses
	return p
}

func TestWaitForDeploymentsReady_BrokenNewPodFailsFast(t *testing.T) {
	d, oldRS, newRS := brokenRolloutDeployment()
	client := newFakeClient(d, oldRS, newRS, rsPod(newRS, "web-2-a", crashLooping("app", 5)))
	ops := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.timeout = 30 })

	start := time.Now()
	err := ops.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "web-2-a") || !strings.Contains(err.Error(), "CrashLoopBackOff") ||
		strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an immediate CrashLoopBackOff failure, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("broken pod must end the wait early, took %s", time.Since(start))
	}
}

func TestWaitForDeploymentsReady_OldRevisionAndSlowPodsKeepWaiting(t *testing.T) {
	d, oldRS, newRS := brokenRolloutDeployment()
	client := newFakeClient(d, oldRS, newRS,
		rsPod(oldRS, "web-1-a", crashLooping("app", 50)),                    // old revision: ignored
		rsPod(newRS, "web-2-a", waiting("app", "ContainerCreating", "", 0)), // slow
		rsPod(newRS, "web-2-b", crashLooping("app", 1)),                     // below threshold
	)
	ops := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.timeout = 1 })

	err := ops.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "cannot become ready") {
		t.Fatalf("expected a plain timeout, got %v", err)
	}
}

func TestWaitForDeploymentsReady_DisabledPolicySkipsPodChecks(t *testing.T) {
	d, oldRS, newRS := brokenRolloutDeployment()
	client := newFakeClient(d, oldRS, newRS, rsPod(newRS, "web-2-a", crashLooping("app", 5)))
	ops := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.timeout = 1; d.podHealth = NewPodHealthPolicy(0, nil) })

	err := ops.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout with checks disabled, got %v", err)
	}
	for _, a := range client.cs.Actions() {
		if a.GetVerb() == "list" && (a.GetResource().Resource == "pods" || a.GetResource().Resource == "replicasets") {
			t.Fatalf("disabled policy must not list %s", a.GetResource().Resource)
		}
	}
}

func TestWaitForDeploymentsReady_PodListErrorTripsRun(t *testing.T) {
	d, oldRS, newRS := brokenRolloutDeployment()
	client := newFakeClient(d, oldRS, newRS)
	client.cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("pods list denied"))
	})
	ops := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.timeout = 1 })
	rs := NewRunStop()

	err := ops.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, rs.Trip)
	if err == nil || !strings.Contains(err.Error(), "pods list denied") {
		t.Fatalf("expected the pod list error to be reported, got %v", err)
	}
	if !rs.Stopped() {
		t.Error("a pod list error while checking pod health must trip the run-wide stop")
	}
}

func revisionPod(sts *appsv1.StatefulSet, name, revision string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	p := podFor(sts.Namespace, name, "StatefulSet", sts.Name, sts.UID, map[string]string{
		"app": sts.Name, appsv1.ControllerRevisionHashLabelKey: revision,
	})
	p.Status.ContainerStatuses = statuses
	return p
}

func rollingStatefulSet() *appsv1.StatefulSet {
	sts := readyStatefulSet("ns", "db")
	sts.Status.CurrentRevision = "db-r1"
	sts.Status.UpdateRevision = "db-r2"
	sts.Status.ReadyReplicas = 1
	return sts
}

func TestWaitForStatefulSetsReady_BrokenUpdateRevisionPodFailsFast(t *testing.T) {
	sts := rollingStatefulSet()
	client := newFakeClient(sts,
		revisionPod(sts, "db-1", "db-r2", waiting("db", "ImagePullBackOff", `Back-off pulling image "db:1"`, 0)),
		revisionPod(sts, "db-0", "db-r1"),
	)
	ops := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.timeout = 30 })

	start := time.Now()
	err := ops.waitForStatefulSetsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "db", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "db-1") || !strings.Contains(err.Error(), "ImagePullBackOff") ||
		strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an immediate ImagePullBackOff failure, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("broken pod must end the wait early, took %s", time.Since(start))
	}
}

func TestWaitForStatefulSetsReady_OldRevisionPodIgnored(t *testing.T) {
	sts := rollingStatefulSet()
	client := newFakeClient(sts,
		revisionPod(sts, "db-0", "db-r1", crashLooping("db", 50)), // old revision: ignored
		revisionPod(sts, "db-1", "db-r2", waiting("db", "ContainerCreating", "", 0)),
	)
	ops := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.timeout = 1 })

	err := ops.waitForStatefulSetsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "db", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), "cannot become ready") {
		t.Fatalf("expected a plain timeout, got %v", err)
	}
}

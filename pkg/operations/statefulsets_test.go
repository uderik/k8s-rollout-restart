package operations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
)

func readyStatefulSet(ns, name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID("uid-sts-" + ns + "-" + name), Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
			CurrentRevision: "r1", UpdateRevision: "r1",
		},
	}
}

func newTestStatefulSetOps(client K8sClient, opts func(s *StatefulSetOperations)) *StatefulSetOperations {
	s := &StatefulSetOperations{
		clientset:    client,
		parallel:     2,
		timeout:      5,
		log:          logger.NewLogger(false),
		pollInterval: 10 * time.Millisecond,
	}
	if opts != nil {
		opts(s)
	}
	return s
}

func TestStatefulSetRolloutStatus(t *testing.T) {
	tests := []struct {
		name     string
		mut      func(s *appsv1.StatefulSet)
		minGen   int64
		wantDone bool
	}{
		{name: "complete", minGen: 1, wantDone: true},
		{name: "generation from PATCH not observed", minGen: 2},
		{name: "not available yet", mut: func(s *appsv1.StatefulSet) { s.Status.AvailableReplicas = 1 }},
		{name: "not ready", mut: func(s *appsv1.StatefulSet) { s.Status.ReadyReplicas = 1 }},
		{name: "not updated", mut: func(s *appsv1.StatefulSet) { s.Status.UpdatedReplicas = 1 }},
		{name: "revision not converged", mut: func(s *appsv1.StatefulSet) { s.Status.UpdateRevision = "r2" }},
		{name: "nil replicas defaults to 1", mut: func(s *appsv1.StatefulSet) {
			s.Spec.Replicas = nil
			s.Status.Replicas, s.Status.UpdatedReplicas, s.Status.ReadyReplicas, s.Status.AvailableReplicas = 1, 1, 1, 1
		}, wantDone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := readyStatefulSet("ns", "db")
			if tt.mut != nil {
				tt.mut(sts)
			}
			done, reason := statefulSetRolloutStatus(sts, tt.minGen)
			if done != tt.wantDone {
				t.Errorf("done = %v, want %v (reason: %s)", done, tt.wantDone, reason)
			}
		})
	}
}

func TestStatefulSetExclusionHeuristics(t *testing.T) {
	tests := []struct {
		name         string
		mut          func(s *appsv1.StatefulSet)
		wantPostgres bool
		wantElastic  bool
	}{
		{name: "plain statefulset", mut: func(*appsv1.StatefulSet) {}},
		{name: "cluster-name label alone is not Zalando", mut: func(s *appsv1.StatefulSet) {
			s.Labels = map[string]string{"cluster-name": "payments", "team": "x"}
		}},
		{name: "spilo label", mut: func(s *appsv1.StatefulSet) {
			s.Labels = map[string]string{"application": "spilo"}
		}, wantPostgres: true},
		{name: "owned by postgresql", mut: func(s *appsv1.StatefulSet) {
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "acid.zalan.do/v1", Kind: "postgresql", Name: "pg"}}
		}, wantPostgres: true},
		{name: "name containing elasticsearch is not ECK", mut: func(s *appsv1.StatefulSet) {
			s.Name = "my-elasticsearch-exporter"
			s.Labels = map[string]string{"app": "elasticsearch-exporter"}
		}},
		{name: "ECK cluster-name label alone is not ECK", mut: func(s *appsv1.StatefulSet) {
			s.Labels = map[string]string{"elasticsearch.k8s.elastic.co/cluster-name": "es"}
		}},
		{name: "ECK type label", mut: func(s *appsv1.StatefulSet) {
			s.Labels = map[string]string{"common.k8s.elastic.co/type": "elasticsearch"}
		}, wantElastic: true},
		{name: "owned by Elasticsearch", mut: func(s *appsv1.StatefulSet) {
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "elasticsearch.k8s.elastic.co/v1", Kind: "Elasticsearch", Name: "es"}}
		}, wantElastic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := readyStatefulSet("ns", "db")
			tt.mut(sts)
			if got := isPostgresOperatorStatefulSet(sts); got != tt.wantPostgres {
				t.Errorf("isPostgresOperatorStatefulSet = %v, want %v", got, tt.wantPostgres)
			}
			if got := isElasticsearchStatefulSet(sts); got != tt.wantElastic {
				t.Errorf("isElasticsearchStatefulSet = %v, want %v", got, tt.wantElastic)
			}
		})
	}
}

func statefulSetScenario() []runtime.Object {
	ok := readyStatefulSet("ns", "ok")
	onDelete := readyStatefulSet("ns", "ondelete")
	onDelete.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
	partitioned := readyStatefulSet("ns", "partitioned")
	partitioned.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type:          appsv1.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: int32Ptr(1)},
	}
	partitionZero := readyStatefulSet("ns", "partition-zero")
	partitionZero.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type:          appsv1.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: int32Ptr(0)},
	}
	spilo := readyStatefulSet("ns", "pg")
	spilo.Labels = map[string]string{"application": "spilo"}
	return []runtime.Object{ok, onDelete, partitioned, partitionZero, spilo}
}

func TestStatefulSetSelectionParity(t *testing.T) {
	tests := []struct {
		name      string
		objects   []runtime.Object
		podLabels []string
		want      []string
	}{
		{
			name:    "unsupported update strategies and operator-managed sets are skipped",
			objects: statefulSetScenario(),
			want:    []string{"ns/ok", "ns/partition-zero"},
		},
		{
			name: "pod filter verifies controller ownerRef UID",
			objects: func() []runtime.Object {
				a := readyStatefulSet("ns", "a")
				b := readyStatefulSet("ns", "b")
				b.Spec.Selector = a.Spec.Selector
				// A pod named after b's ownership but controlled by a (by UID).
				pod := podFor("ns", "a-0", "StatefulSet", "a", a.UID, map[string]string{"app": "a", "tier": "db"})
				stale := podFor("ns", "b-0", "StatefulSet", "b", "some-other-uid", map[string]string{"app": "a", "tier": "db"})
				return []runtime.Object{a, b, pod, stale}
			}(),
			podLabels: []string{"tier=db"},
			want:      []string{"ns/a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := func(s *StatefulSetOperations) { s.podLabels = tt.podLabels }

			preview, err := newTestStatefulSetOps(newFakeClient(tt.objects...), opts).GetStatefulSetsToRestart(context.Background(), []string{"ns"})
			if err != nil {
				t.Fatalf("GetStatefulSetsToRestart: %v", err)
			}
			sort.Strings(preview)

			client := newFakeClient(tt.objects...)
			if err := newTestStatefulSetOps(client, opts).RestartStatefulSets(context.Background(), []string{"ns"}); err != nil {
				t.Fatalf("RestartStatefulSets: %v", err)
			}
			executed := patchedNames(client.cs, "statefulsets")

			if strings.Join(preview, ",") != strings.Join(tt.want, ",") {
				t.Errorf("preview = %v, want %v", preview, tt.want)
			}
			if strings.Join(executed, ",") != strings.Join(preview, ",") {
				t.Errorf("executed = %v, preview = %v", executed, preview)
			}
		})
	}
}

func TestRestartStatefulSets_ReturnsAllErrors(t *testing.T) {
	client := newFakeClient(readyStatefulSet("ns1", "a"), readyStatefulSet("ns2", "b"))
	failing := func(ns string) {
		client.cs.PrependReactor("patch", "statefulsets", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetNamespace() == ns {
				return true, nil, errors.New("patch failed in " + ns)
			}
			return false, nil, nil
		})
	}
	failing("ns1")
	failing("ns2")

	err := newTestStatefulSetOps(client, nil).RestartStatefulSets(context.Background(), []string{"ns1", "ns2"})
	if err == nil || !strings.Contains(err.Error(), "patch failed in ns1") || !strings.Contains(err.Error(), "patch failed in ns2") {
		t.Fatalf("expected both errors, got %v", err)
	}
}

func statefulSetNotReady(obj runtime.Object, _ bool) runtime.Object {
	s := obj.(*appsv1.StatefulSet)
	s.Status.ReadyReplicas = 0
	return s
}

func manyStatefulSets(perNS int, namespaces ...string) []runtime.Object {
	var objs []runtime.Object
	for _, ns := range namespaces {
		for i := 0; i < perNS; i++ {
			objs = append(objs, readyStatefulSet(ns, fmt.Sprintf("s%d", i)))
		}
	}
	return objs
}

func TestRestartStatefulSets_ParallelBoundsInProgressRollouts(t *testing.T) {
	for _, parallel := range []int{1, 2} {
		t.Run(fmt.Sprintf("parallel=%d", parallel), func(t *testing.T) {
			client := newFakeClient(manyStatefulSets(2, "ns1", "ns2", "ns3")...)
			tr := newRolloutTracker(3)
			tr.install(client.cs, "statefulsets", statefulSetNotReady)
			s := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.parallel = parallel })

			if err := s.RestartStatefulSets(context.Background(), []string{"ns1", "ns2", "ns3"}); err != nil {
				t.Fatalf("RestartStatefulSets: %v", err)
			}
			if tr.peak != parallel {
				t.Errorf("peak in-progress rollouts = %d, want exactly --parallel=%d", tr.peak, parallel)
			}
			if len(tr.completed) != 6 || tr.inFlight != 0 {
				t.Errorf("completed = %d (in flight %d), want all 6 rollouts finished", len(tr.completed), tr.inFlight)
			}
		})
	}
}

func TestRestartStatefulSets_StuckRolloutStopsNewLaunches(t *testing.T) {
	// parallel=2: ns1/s0 never becomes ready and times out after 1s. The
	// other slot keeps working until then; once s0 has failed no new
	// StatefulSet is started.
	client := newFakeClient(manyStatefulSets(3, "ns1", "ns2")...)
	tr := newRolloutTracker(1, "ns1/s0")
	tr.install(client.cs, "statefulsets", statefulSetNotReady)
	s := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.parallel = 2; s.timeout = 1 })

	err := s.RestartStatefulSets(context.Background(), []string{"ns1", "ns2"})
	if err == nil || !strings.Contains(err.Error(), "ns1/s0") {
		t.Fatalf("expected ns1/s0 failure, got %v", err)
	}
	if len(tr.completed) != 5 {
		t.Errorf("completed rollouts = %v, want the 5 healthy ones started before the failure", tr.completed)
	}
}

func TestRestartStatefulSets_TimeoutStopsNewLaunches(t *testing.T) {
	client := newFakeClient(manyStatefulSets(2, "ns")...)
	tr := newRolloutTracker(1_000_000)
	tr.install(client.cs, "statefulsets", statefulSetNotReady)
	s := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.parallel = 1; s.timeout = 1 })

	err := s.RestartStatefulSets(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "timed out") ||
		!strings.Contains(err.Error(), "StatefulSet ns/s1: not started due to earlier failure") {
		t.Fatalf("expected s0 timeout and s1 not started, got %v", err)
	}
	if got := strings.Join(patchedNames(client.cs, "statefulsets"), ","); got != "ns/s0" {
		t.Errorf("patched = %s, want only ns/s0", got)
	}
}

func TestRestartStatefulSets_SkipWait(t *testing.T) {
	client := newFakeClient(manyStatefulSets(3, "ns1", "ns2")...)
	tr := newRolloutTracker(1_000_000)
	tr.install(client.cs, "statefulsets", statefulSetNotReady)
	s := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.skipWait = true; s.timeout = 30 })

	if err := s.RestartStatefulSets(context.Background(), []string{"ns1", "ns2"}); err != nil {
		t.Fatalf("RestartStatefulSets: %v", err)
	}
	if got := len(patchedNames(client.cs, "statefulsets")); got != 6 {
		t.Errorf("patched %d StatefulSets, want 6", got)
	}
	for _, a := range client.cs.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "statefulsets" {
			t.Fatalf("--skip-wait must not poll StatefulSet status")
		}
	}
}

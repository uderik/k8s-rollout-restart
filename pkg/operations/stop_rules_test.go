package operations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// Kafka: after cluster a fails (rollout timeout, or an ambiguous PodSet PATCH
// with --skip-wait) cluster b must not be started and must be reported.
func TestRestartKafkaClusters_StopsAfterFailure(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skipWait=%v", skip), func(t *testing.T) {
			fastPolling(t)
			srv := newCRTestServer(t)
			srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
			srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1Only)
			crs := "/apis/kafka.strimzi.io/v1/namespaces/ns/kafkas"
			sets := "/apis/core.strimzi.io/v1/namespaces/ns/strimzipodsets"
			srv.json("GET", crs, 200, `{"items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`)
			srv.handle("GET", sets, func(w http.ResponseWriter, r *http.Request) {
				name := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "strimzi.io/cluster=")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"items":[{"metadata":{"name":%q,"labels":{"strimzi.io/cluster":%q}}}]}`, name+"-pool", name)
			})
			for _, name := range []string{"a", "b"} {
				srv.json("GET", sets+"/"+name+"-pool", 200, `{"spec":{"pods":[{}]}}`)
				srv.json("GET", crs+"/"+name, 200, `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
				srv.handle("PATCH", sets+"/"+name+"-pool", okJSON)
			}
			if skip {
				srv.json("PATCH", sets+"/a-pool", 503, serverUnavailable)
			}
			// Pods are never replaced: without --skip-wait cluster a times out.
			client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
				name := strings.TrimPrefix(opts.LabelSelector, "strimzi.io/cluster=")
				return &corev1.PodList{Items: []corev1.Pod{testPod(name+"-0", "old-"+name, boolPtr(true),
					metav1.OwnerReference{Kind: "StrimziPodSet", Name: name + "-pool"})}}, nil
			}}
			rs := NewRunStop()
			k := NewKafkaOperations(client, 1, 1, false, nil, skip, "", "")
			k.SetRunStop(rs)
			err := k.RestartKafkaClusters(context.Background(), []string{"ns"})
			if err == nil {
				t.Fatal("expected failure of cluster a")
			}
			if n := len(srv.bodiesFor("PATCH", sets+"/b-pool")); n != 0 {
				t.Errorf("started cluster b after a failed: b PATCH count=%d, err=%v", n, err)
			}
			if !strings.Contains(err.Error(), "kafka ns/b: not started due to earlier failure") {
				t.Errorf("remaining cluster b not reported as not started: %v", err)
			}
			if !rs.Stopped() {
				t.Error("the run-wide stop signal must be tripped")
			}
		})
	}
}

// Kafka/PG honour a stop signal tripped by another resource type: nothing
// is started, every target is reported.
func TestRestartKafkaClusters_SharedStopPreventsLaunch(t *testing.T) {
	srv := newKafkaTestServer(t)
	client := &crTestClient{rc: srv.restClient()}
	rs := NewRunStop()
	rs.Trip(errors.New("deployment failed"))
	k := NewKafkaOperations(client, 2, 1, false, nil, false, "", "")
	k.SetRunStop(rs)
	err := k.RestartKafkaClusters(context.Background(), []string{"kafka"})
	if err == nil || !strings.Contains(err.Error(), "kafka kafka/my-cluster: not started due to earlier failure") {
		t.Fatalf("expected not-started error, got %v", err)
	}
	if strings.Contains(err.Error(), "StrimziPodSet") {
		t.Errorf("the cluster must not be processed after a stop: %v", err)
	}
}

// Cross-type: a Deployment failure stops the StatefulSet launches of the
// same run (shared RunStop).
func TestRunStop_DeploymentFailureStopsStatefulSets(t *testing.T) {
	client := newFakeClient(readyDeployment("ns", "web", time.Hour), readyStatefulSet("ns", "db"))
	client.cs.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("denied"))
	})
	rs := NewRunStop()
	d := newTestDeploymentOps(client, func(d *DeploymentOperations) { d.parallel = 1 })
	d.SetRunStop(rs)
	s := newTestStatefulSetOps(client, func(s *StatefulSetOperations) { s.parallel = 1 })
	s.SetRunStop(rs)

	if err := d.RestartDeployments(context.Background(), []string{"ns"}); err == nil {
		t.Fatal("expected deployment failure")
	}
	if !rs.Stopped() || rs.Cause() == nil {
		t.Fatal("deployment failure must trip the run-wide stop signal")
	}
	err := s.RestartStatefulSets(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "ns/db: not started due to earlier failure") {
		t.Fatalf("expected StatefulSet ns/db not started, got %v", err)
	}
	if got := patchedNames(client.cs, "statefulsets"); len(got) != 0 {
		t.Errorf("StatefulSets patched after a Deployment failure: %v", got)
	}
}

// A selection error (List forbidden in namespace "bad" of -n bad,good) trips
// the run-wide stop before the launch queue starts: nothing in "good" is
// restarted, the selected target is reported as not started and the
// selection error is returned (with and without --skip-wait).
func TestRestartWorkloads_SelectionFailureStopsRun(t *testing.T) {
	for _, kind := range []string{"deployments", "statefulsets"} {
		for _, skip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/skipWait=%t", kind, skip), func(t *testing.T) {
				client := newFakeClient(readyDeployment("good", "a", time.Hour), readyStatefulSet("good", "a"))
				client.cs.PrependReactor("list", kind, func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.GetNamespace() == "bad" {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: kind}, "", errors.New("denied"))
					}
					return false, nil, nil
				})
				rs := NewRunStop()
				var err error
				notStarted := "good/a: not started due to earlier failure"
				if kind == "deployments" {
					d := NewDeploymentOperations(client, 1, 1, true, false, nil, nil, nil, nil, skip)
					d.SetRunStop(rs)
					err = d.RestartDeployments(context.Background(), []string{"bad", "good"})
					notStarted = "deployment " + notStarted
				} else {
					s := NewStatefulSetOperations(client, 1, 1, true, false, nil, nil, nil, nil, skip)
					s.SetRunStop(rs)
					err = s.RestartStatefulSets(context.Background(), []string{"bad", "good"})
					notStarted = "StatefulSet " + notStarted
				}
				if err == nil || !strings.Contains(err.Error(), "namespace bad") || !strings.Contains(err.Error(), notStarted) {
					t.Fatalf("expected the selection error and %q, got %v", notStarted, err)
				}
				if got := patchedNames(client.cs, kind); len(got) != 0 {
					t.Errorf("%s restarted after a failed selection: %v", kind, got)
				}
				if !rs.Stopped() || !strings.Contains(rs.Cause().Error(), "namespace bad") {
					t.Errorf("selection error must trip the run-wide stop: stopped=%t, cause=%v", rs.Stopped(), rs.Cause())
				}
			})
		}
	}
}

// Kafka/PostgreSQL: a failed CR list in namespace "bad" stops the run before
// any cluster in "db" is touched; the cluster is reported as not started.
func TestRestartCRs_SelectionFailureStopsRun(t *testing.T) {
	for _, kind := range []string{"Kafka", "PostgreSQL"} {
		t.Run(kind, func(t *testing.T) {
			srv := newPGTestServer(t)
			rs := NewRunStop()
			var err error
			var notStarted string
			if kind == "Kafka" {
				srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
				srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1Only)
				srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/bad/kafkas", 403, forbidden)
				srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/db/kafkas", 200, `{"items":[{"metadata":{"name":"a"}}]}`)
				sets := "/apis/core.strimzi.io/v1/namespaces/db/strimzipodsets"
				srv.json("GET", sets, 200, `{"items":[{"metadata":{"name":"a-pool","labels":{"strimzi.io/cluster":"a"}}}]}`)
				srv.handle("PATCH", sets+"/a-pool", okJSON)
				k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
				k.SetRunStop(rs)
				err = k.RestartKafkaClusters(context.Background(), []string{"bad", "db"})
				notStarted = "kafka db/a: not started due to earlier failure"
			} else {
				srv.json("GET", "/apis/acid.zalan.do/v1/namespaces/bad/postgresqls", 403, forbidden)
				srv.json("GET", pgCRPath, 200, pgRunning2)
				srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
				srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
				srv.handle("PATCH", pgCRPath, okJSON)
				client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
					return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "old0", boolPtr(true)), testPod("acid-main-1", "old1", boolPtr(true))}}, nil
				}}
				p := newTestPG(client, 1, true)
				p.SetRunStop(rs)
				err = p.RestartPostgresqlClusters(context.Background(), []string{"bad", "db"})
				notStarted = "postgresql db/acid-main: not started due to earlier failure"
			}
			if err == nil || !strings.Contains(err.Error(), "namespace bad") || !strings.Contains(err.Error(), notStarted) {
				t.Fatalf("expected the selection error and %q, got %v", notStarted, err)
			}
			if got := srv.methodCount("PATCH"); got != 0 {
				t.Errorf("%s mutated after a failed selection: %d PATCH", kind, got)
			}
			if !rs.Stopped() || !strings.Contains(rs.Cause().Error(), "namespace bad") {
				t.Errorf("selection error must trip the run-wide stop: stopped=%t, cause=%v", rs.Stopped(), rs.Cause())
			}
		})
	}
}

// PG ambiguous trigger PATCH: the rotation is waited for, but the original
// error is returned (never a silent success) and the stop signal is tripped.
func TestRestartPostgresqlClusters_AmbiguousTriggerReturnsError(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.json("PATCH", pgCRPath, 503, serverUnavailable)
	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true)), testPod("acid-main-1", "new-1", boolPtr(true))},
	)}
	rs := NewRunStop()
	p := newTestPG(client, 1, false)
	p.SetRunStop(rs)
	err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("ambiguous trigger PATCH must be reported as failure, got %v", err)
	}
	if !rs.Stopped() {
		t.Error("ambiguous trigger PATCH must trip the run-wide stop signal")
	}
}

// PG: after a failed (ambiguous) trigger with --skip-wait the next cluster is
// not triggered and is reported as not started.
func TestRestartPostgresqlClusters_StopsAfterFailure(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgListPath, 200, `{"items":[{"metadata":{"name":"acid-main"}},{"metadata":{"name":"acid-next"}}]}`)
	for _, name := range []string{"acid-main", "acid-next"} {
		srv.json("GET", pgListPath+"/"+name, 200, pgRunning2)
		srv.handle("PATCH", pgListPath+"/"+name, okJSON)
		for _, ordinal := range []string{"0", "1"} {
			srv.handle("PATCH", pgPodPath+name+"-"+ordinal, okJSON)
		}
	}
	srv.json("PATCH", pgCRPath, 503, serverUnavailable)
	var mu sync.Mutex
	calls := map[string]int{}
	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		name := strings.TrimPrefix(opts.LabelSelector, postgresqlPodSelector)
		mu.Lock()
		defer mu.Unlock()
		calls[name]++
		pods := []corev1.Pod{testPod(name+"-0", name+"-old-0", boolPtr(true)), testPod(name+"-1", name+"-old-1", boolPtr(true))}
		if calls[name] > 1 {
			for i := range pods {
				pods[i].Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true", PostgresqlMarkedByAnnotation: testPGRunID}
			}
		}
		return &corev1.PodList{Items: pods}, nil
	}}
	err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil {
		t.Fatal("expected trigger failure")
	}
	if n := len(srv.bodiesFor("PATCH", pgListPath+"/acid-next")); n != 0 {
		t.Errorf("triggered next PG cluster after failure: %d PATCH", n)
	}
	for _, ordinal := range []string{"0", "1"} {
		if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-next-"+ordinal)); n != 0 {
			t.Errorf("marked acid-next-%s after failure: %d PATCH", ordinal, n)
		}
	}
	if !strings.Contains(err.Error(), "postgresql db/acid-next: not started due to earlier failure") {
		t.Errorf("remaining PG cluster not reported as not started: %v", err)
	}
}

// PG plan refresh: a pod that lost Ready after preflight (detected when the
// plan is refreshed after a 422) fails the cluster; our markers are removed
// and the operator is never triggered.
func TestRestartPostgresqlClusters_RefreshRechecksReadiness(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgCRPath, okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	var mu sync.Mutex
	patches := 0
	srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		patches++
		n := patches
		mu.Unlock()
		if n == 1 {
			// resourceVersion changed after preflight (the pod lost Ready).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			fmt.Fprint(w, testFailed)
			return
		}
		okJSON(w, r)
	})
	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(false)), markedPod("acid-main-1", "old-1")},
	)}
	err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "acid-main-0 is not Running and Ready") {
		t.Errorf("refreshed plan must re-check readiness, got %v", err)
	}
	if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
		t.Errorf("triggered operator sync despite failed readiness: %d PATCH", n)
	}
	if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+"acid-main-1"), `"op":"remove"`); n != 1 {
		t.Errorf("marker of acid-main-1 must be removed, got %d removals", n)
	}
	if n := patches; n != 1 {
		t.Errorf("NotReady acid-main-0 must not be marked after the refresh, got %d PATCH", n)
	}
}

// PG plan refresh: an instance count change is detected as well.
func TestRestartPostgresqlClusters_RefreshRechecksInstanceCount(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgCRPath, okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.json("PATCH", pgPodPath+"acid-main-0", 422, testFailed)
	extra := testPod("acid-main-2", "new-2", boolPtr(true))
	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), markedPod("acid-main-1", "old-1"), extra},
	)}
	err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "found 3 pod(s), spec.numberOfInstances is 2") {
		t.Errorf("refreshed plan must re-check the instance count, got %v", err)
	}
	if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
		t.Errorf("triggered operator sync despite instance count change: %d PATCH", n)
	}
}

// The manual PG cleanup hint targets the run's --context, shell-quoted.
func TestPostgresqlUnmarkCommand_IncludesContext(t *testing.T) {
	p := newTestPG(&crTestClient{}, 1, false)
	p.SetKubeContext("prod ctx")
	got := p.unmarkCommand("db", []string{"acid-main-0", "acid-main-1"})
	want := "kubectl --context 'prod ctx' annotate pod -n db acid-main-0 acid-main-1 " +
		postgresqlRollingUpdateAnnotation + "- " + PostgresqlMarkedByAnnotation + "-"
	if got != want {
		t.Errorf("unmarkCommand = %q, want %q", got, want)
	}
	p.SetKubeContext("")
	if got := p.unmarkCommand("db", []string{"x"}); strings.Contains(got, "--context") {
		t.Errorf("no --context expected without a context: %q", got)
	}
}

// Cordon after a lost PATCH response: our marker alone is not enough, the
// node must also be unschedulable (someone may have uncordoned it).
func TestCordonNodes_LostResponseRequiresUnschedulable(t *testing.T) {
	f := newClusterFakeClient()
	f.addNode("n1", false, nil)
	f.addPod("app", "p1", "n1")
	c := NewClusterOperations(f, 1, 10, false, false)
	f.afterPatchErr = func(name string) error {
		// A concurrent admin's uncordon leaves the annotation intact.
		f.setUnschedulable(name, false)
		return errors.New("connection reset by peer")
	}
	err := c.CordonNodes(context.Background(), []string{"app"}, false, nil, nil)
	if err == nil {
		t.Errorf("cordon returned success while node unschedulable=%v and marker=%s", f.unschedulable("n1"), f.marker("n1"))
	}
	if got := c.CordonedNodes(); len(got) != 0 {
		t.Errorf("schedulable node must not be recorded as cordoned, got %v", got)
	}
}

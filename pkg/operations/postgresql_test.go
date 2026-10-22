package operations

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	pgListPath = "/apis/acid.zalan.do/v1/namespaces/db/postgresqls"
	pgCRPath   = pgListPath + "/acid-main"
	pgPodPath  = "/api/v1/namespaces/db/pods/"

	pgRunning2 = `{"spec":{"numberOfInstances":2},"status":{"PostgresClusterStatus":"Running"}}`
	forbidden  = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"pods is forbidden"}`
	testFailed = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","code":422,"message":"the server rejected our request: test operation failed"}`
)

func newPGTestServer(t *testing.T) *crTestServer {
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/acid.zalan.do", 200, apiGroupV1Only)
	srv.json("GET", pgListPath, 200,
		`{"items":[{"metadata":{"name":"acid-main","creationTimestamp":"2020-01-01T00:00:00Z"}}]}`)
	return srv
}

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

const testPGRunID = "test-run"

// markedPod is a Ready pod that carries the rolling-update marker set by this
// (test) run.
func markedPod(name, uid string) corev1.Pod {
	pod := testPod(name, uid, boolPtr(true))
	pod.Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true", PostgresqlMarkedByAnnotation: testPGRunID}
	return pod
}

// foreignMarkedPod is a Ready pod marked by the operator or another run.
func foreignMarkedPod(name, uid, owner string) corev1.Pod {
	pod := testPod(name, uid, boolPtr(true))
	pod.Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true"}
	if owner != "" {
		pod.Annotations[PostgresqlMarkedByAnnotation] = owner
	}
	return pod
}

// podSequence returns a pod lister that serves first on the first call
// (preflight) and rest on every later call.
func podSequence(first, rest []corev1.Pod) func(string, metav1.ListOptions) (*corev1.PodList, error) {
	var (
		mu    sync.Mutex
		calls int
	)
	return func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return &corev1.PodList{Items: first}, nil
		}
		return &corev1.PodList{Items: rest}, nil
	}
}

func newTestPG(client K8sClient, timeout int, skipWait bool) *PostgresqlOperations {
	p := NewPostgresqlOperations(client, 1, timeout, false, nil, skipWait)
	p.cleanupTimeout = 5 * time.Second
	p.unmarkRetryDelay = 10 * time.Millisecond
	p.SetRunID(testPGRunID)
	return p
}

func countContaining(bodies []string, sub string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, sub) {
			n++
		}
	}
	return n
}

func TestRestartPostgresqlClusters_MarksPodsAndWaits(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)

	var (
		mu     sync.Mutex
		synced bool
		polls  int
	)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		synced = true
		mu.Unlock()
		okJSON(w, r)
	})
	srv.json("GET", pgCRPath, 200, pgRunning2)

	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		if opts.LabelSelector != "application=spilo,cluster-name=acid-main" {
			t.Errorf("unexpected selector %q", opts.LabelSelector)
		}
		mu.Lock()
		defer mu.Unlock()
		if !synced {
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))}}, nil
		}
		polls++
		switch polls {
		case 1:
			return &corev1.PodList{Items: []corev1.Pod{markedPod("acid-main-0", "old-0"), markedPod("acid-main-1", "old-1")}}, nil
		case 2:
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true)), markedPod("acid-main-1", "old-1")}}, nil
		case 3:
			// All marked instances are gone, but a pod of the cluster still
			// carries the marker: not finished (R4).
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true)), markedPod("acid-main-1", "new-1")}}, nil
		default:
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true)), testPod("acid-main-1", "new-1", boolPtr(true))}}, nil
		}
	}}

	p := newTestPG(client, 10, false)
	if err := p.RestartPostgresqlClusters(context.Background(), []string{"db"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for pod, uid := range map[string]string{"acid-main-0": "old-0", "acid-main-1": "old-1"} {
		bodies := srv.bodiesFor("PATCH", pgPodPath+pod)
		if len(bodies) != 1 || !strings.Contains(bodies[0], `"path":"/metadata/uid","value":"`+uid+`"`) ||
			!strings.Contains(bodies[0], postgresqlRollingUpdateAnnotation) ||
			!strings.Contains(bodies[0], `"`+PostgresqlMarkedByAnnotation+`":"`+testPGRunID+`"`) ||
			!strings.Contains(bodies[0], `"path":"/metadata/resourceVersion"`) {
			t.Errorf("pod %s not marked with a UID-bound patch: %v", pod, bodies)
		}
	}
	if bodies := srv.bodiesFor("PATCH", pgCRPath); len(bodies) != 1 || !strings.Contains(bodies[0], postgresqlRestartRequestAnnotation) {
		t.Errorf("unexpected CR patches: %v", bodies)
	}

	mu.Lock()
	defer mu.Unlock()
	if polls < 4 {
		t.Errorf("finished after %d polls, before all markers were processed", polls)
	}
}

// R4: a pod replaced under the same name between planning and marking must
// not be marked by name; the UID test fails and the plan is refreshed.
func TestRestartPostgresqlClusters_MarkerBoundToUID(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgCRPath, okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)

	var (
		mu       sync.Mutex
		replaced bool
	)
	srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !replaced {
			replaced = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(testFailed))
			return
		}
		okJSON(w, r)
	})
	client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		uid := "old-0"
		if replaced {
			uid = "new-0"
		}
		return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", uid, boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))}}, nil
	}}

	if err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bodies := srv.bodiesFor("PATCH", pgPodPath+"acid-main-0")
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"value":"old-0"`) || !strings.Contains(bodies[1], `"value":"new-0"`) {
		t.Errorf("expected a UID-bound retry against the refreshed plan, got %v", bodies)
	}
	if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-main-1")); n != 1 {
		t.Errorf("acid-main-1 must be marked exactly once, got %d", n)
	}
	// Successful trigger with --skip-wait: the markers intentionally stay.
	for _, pod := range []string{"acid-main-0", "acid-main-1"} {
		if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+pod), `"op":"remove"`); n != 0 {
			t.Errorf("--skip-wait after a successful trigger must keep the markers on %s, got %d removals", pod, n)
		}
	}
}

func TestRestartPostgresqlClusters_PreflightBlocksMarking(t *testing.T) {
	tests := []struct {
		name string
		cr   string
		pods []corev1.Pod
		want string
	}{
		{
			name: "cluster not Running",
			cr:   `{"spec":{"numberOfInstances":2},"status":{"PostgresClusterStatus":"SyncFailed"}}`,
			pods: []corev1.Pod{testPod("acid-main-0", "a", boolPtr(true)), testPod("acid-main-1", "b", boolPtr(true))},
			want: "SyncFailed",
		},
		{
			name: "pod not ready",
			cr:   pgRunning2,
			pods: []corev1.Pod{testPod("acid-main-0", "a", boolPtr(true)), testPod("acid-main-1", "b", boolPtr(false))},
			want: "acid-main-1 is not Running and Ready",
		},
		{
			name: "instance missing",
			cr:   pgRunning2,
			pods: []corev1.Pod{testPod("acid-main-0", "a", boolPtr(true))},
			want: "numberOfInstances is 2",
		},
		{
			name: "operator marker already present",
			cr:   pgRunning2,
			pods: []corev1.Pod{testPod("acid-main-0", "a", boolPtr(true)), foreignMarkedPod("acid-main-1", "b", "")},
			want: "acid-main-1 already carries " + postgresqlRollingUpdateAnnotation,
		},
		{
			name: "marker of another run present",
			cr:   pgRunning2,
			pods: []corev1.Pod{foreignMarkedPod("acid-main-0", "a", "other-run"), testPod("acid-main-1", "b", boolPtr(true))},
			want: "other-run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newPGTestServer(t)
			srv.json("GET", pgCRPath, 200, tt.cr)
			client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
				return &corev1.PodList{Items: tt.pods}, nil
			}}
			err := newTestPG(client, 1, false).RestartPostgresqlClusters(context.Background(), []string{"db"})
			if err == nil || !strings.Contains(err.Error(), "preflight") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected preflight error containing %q, got %v", tt.want, err)
			}
			for _, pod := range []string{"acid-main-0", "acid-main-1"} {
				if n := len(srv.bodiesFor("PATCH", pgPodPath+pod)); n != 0 {
					t.Errorf("pod %s must not be marked, got %d patches", pod, n)
				}
			}
			if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
				t.Errorf("CR must not be touched, got %d patches", n)
			}
		})
	}
}

// R5: marking one pod failed; the marker already set on another pod is
// removed again and the CR is not touched.
func TestRestartPostgresqlClusters_PartialMarkFailureRemovesMarkers(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.json("PATCH", pgPodPath+"acid-main-1", 403, forbidden)

	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{markedPod("acid-main-0", "old-0"), testPod("acid-main-1", "old-1", boolPtr(true))},
	)}

	err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "CR not touched") {
		t.Fatalf("expected forbidden error, got %v", err)
	}
	if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
		t.Errorf("CR must not be touched when marking pods failed, got %d patches", n)
	}
	bodies := srv.bodiesFor("PATCH", pgPodPath+"acid-main-0")
	if len(bodies) != 2 || !strings.Contains(bodies[1], `"op":"remove"`) || !strings.Contains(bodies[1], `"value":"old-0"`) {
		t.Errorf("expected the marker on acid-main-0 to be removed (UID-bound), got %v", bodies)
	}
}

// R5 / (c): the CR touch failed after the pods were marked; the markers are
// removed so that the operator does not rotate the pods on a later resync.
func TestRestartPostgresqlClusters_TouchFailureRemovesMarkers(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.json("PATCH", pgCRPath, 403, forbidden)

	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{markedPod("acid-main-0", "old-0"), markedPod("acid-main-1", "old-1")},
	)}

	err := newTestPG(client, 1, false).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "failed to trigger operator sync") ||
		!strings.Contains(err.Error(), "left untouched (marker removed): [acid-main-0 acid-main-1]") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, pod := range []string{"acid-main-0", "acid-main-1"} {
		if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+pod), `"op":"remove"`); n != 1 {
			t.Errorf("expected marker removal on %s, got %d", pod, n)
		}
	}
}

// (b) + (c): no progress -> the sync is re-triggered (capped), and on timeout
// the remaining markers are removed; the rotated pod is reported.
func TestRestartPostgresqlClusters_StallRetriggersAndCleansUpOnTimeout(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.handle("PATCH", pgCRPath, okJSON)

	var (
		mu    sync.Mutex
		calls int
	)
	client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 { // preflight
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))}}, nil
		}
		// acid-main-1 got rotated, acid-main-0 never does.
		return &corev1.PodList{Items: []corev1.Pod{markedPod("acid-main-0", "old-0"), testPod("acid-main-1", "new-1", boolPtr(true))}}, nil
	}}

	p := newTestPG(client, 1, false)
	p.stallInterval = 50 * time.Millisecond
	err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "timeout") ||
		!strings.Contains(err.Error(), "rotated: [acid-main-1]") ||
		!strings.Contains(err.Error(), "left untouched (marker removed): [acid-main-0]") {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 1+postgresqlMaxRetriggers {
		t.Errorf("expected 1 trigger + %d re-triggers, got %d CR patches", postgresqlMaxRetriggers, n)
	}
	if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+"acid-main-0"), `"op":"remove"`); n != 1 {
		t.Errorf("expected the marker on acid-main-0 to be removed, got %d removals", n)
	}
	if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+"acid-main-1"), `"op":"remove"`); n != 0 {
		t.Errorf("rotated pod must not be patched, got %d removals", n)
	}
}

// (b): SyncFailed during the wait re-triggers the sync right away instead of
// waiting for the operator's resync.
func TestRestartPostgresqlClusters_SyncFailedRetriggers(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)

	var (
		mu       sync.Mutex
		triggers int
		gets     int
	)
	srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		triggers++
		mu.Unlock()
		okJSON(w, r)
	})
	srv.handle("GET", pgCRPath, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		status := "Running"
		if gets > 1 && triggers < 2 { // after the first trigger the sync fails
			status = "SyncFailed"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"spec":{"numberOfInstances":1},"status":{"PostgresClusterStatus":%q}}`, status)
	})
	client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		if triggers == 0 {
			return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true))}}, nil
		}
		return &corev1.PodList{Items: []corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true))}}, nil
	}}

	p := newTestPG(client, 5, false)
	p.stallInterval = 100 * time.Millisecond
	if err := p.RestartPostgresqlClusters(context.Background(), []string{"db"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if triggers != 2 {
		t.Errorf("expected exactly one re-trigger after SyncFailed, got %d CR patches", triggers)
	}
}

// (c): an interrupted run still removes the markers, with a fresh context.
func TestRestartPostgresqlClusters_CancelRemovesMarkers(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, `{"spec":{"numberOfInstances":1},"status":{"PostgresClusterStatus":"Running"}}`)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, r)
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
	})
	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true))},
		[]corev1.Pod{markedPod("acid-main-0", "old-0")},
	)}

	err := newTestPG(client, 60, false).RestartPostgresqlClusters(ctx, []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "left untouched (marker removed): [acid-main-0]") {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+"acid-main-0"), `"op":"remove"`); n != 1 {
		t.Errorf("expected marker removal after cancellation, got %d", n)
	}
}

func TestRestartPostgresqlClusters_NoPodsIsError(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	p := newTestPG(&crTestClient{rc: srv.restClient()}, 1, true)
	if err := p.RestartPostgresqlClusters(context.Background(), []string{"db"}); err == nil {
		t.Fatal("expected error for cluster without pods")
	}
}

func TestRestartPostgresqlClusters_APIMissingIsSkipped(t *testing.T) {
	srv := newCRTestServer(t)
	p := newTestPG(&crTestClient{rc: srv.restClient()}, 1, true)
	if err := p.RestartPostgresqlClusters(context.Background(), []string{"db"}); err != nil {
		t.Fatalf("expected skip, got %v", err)
	}
}

func TestRestartPostgresqlClusters_ListForbiddenIsError(t *testing.T) {
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/acid.zalan.do", 200, apiGroupV1Only)
	srv.json("GET", pgListPath, 403, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`)
	p := newTestPG(&crTestClient{rc: srv.restClient()}, 1, true)
	if err := p.RestartPostgresqlClusters(context.Background(), []string{"db"}); err == nil {
		t.Fatal("expected 403 on list to be an error")
	}
}

const serverUnavailable = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","code":503,"message":"unavailable"}`

// #2: with --skip-wait an ambiguous trigger failure must not leave markers.
func TestRestartPostgresqlClusters_SkipWaitTriggerFailureRemovesMarkers(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgCRPath, 200, pgRunning2)
	srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
	srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
	srv.json("PATCH", pgCRPath, 503, serverUnavailable)

	client := &crTestClient{rc: srv.restClient(), pods: podSequence(
		[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
		[]corev1.Pod{markedPod("acid-main-0", "old-0"), markedPod("acid-main-1", "old-1")},
	)}

	err := newTestPG(client, 1, true).RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "failed to trigger operator sync") ||
		!strings.Contains(err.Error(), "left untouched (marker removed): [acid-main-0 acid-main-1]") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, pod := range []string{"acid-main-0", "acid-main-1"} {
		bodies := srv.bodiesFor("PATCH", pgPodPath+pod)
		if n := countContaining(bodies, `"op":"remove"`); n != 1 {
			t.Errorf("expected marker removal on %s, got %v", pod, bodies)
		}
		if n := countContaining(bodies, testPGRunID); n != 2 {
			t.Errorf("mark and unmark of %s must be bound to the run ID, got %v", pod, bodies)
		}
	}
}

// #3: a remove PATCH answered with 409 is not a confirmation. The pod is
// re-read: still marked by this run -> retried; if it keeps failing, the pod
// is reported as still marked and never as "marker removed".
func TestRemoveMarkers_ConflictIsReReadAndRetried(t *testing.T) {
	conflict := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,"message":"conflict"}`
	t.Run("retry succeeds", func(t *testing.T) {
		srv := newPGTestServer(t)
		var (
			mu    sync.Mutex
			calls int
		)
		srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(conflict))
				return
			}
			okJSON(w, r)
		})
		client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
			return &corev1.PodList{Items: []corev1.Pod{markedPod("acid-main-0", "old-0")}}, nil
		}}
		p := newTestPG(client, 1, false)
		summary := p.removeMarkers("db", postgresqlPodSelector+"acid-main", map[types.UID]string{"old-0": "acid-main-0"})
		if !strings.Contains(summary, "left untouched (marker removed): [acid-main-0]") || strings.Contains(summary, "STILL MARKED") {
			t.Fatalf("summary = %s", summary)
		}
		if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-main-0")); n != 2 {
			t.Errorf("expected 2 remove attempts, got %d", n)
		}
	})
	t.Run("keeps failing", func(t *testing.T) {
		srv := newPGTestServer(t)
		srv.json("PATCH", pgPodPath+"acid-main-0", 409, conflict)
		client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
			return &corev1.PodList{Items: []corev1.Pod{markedPod("acid-main-0", "old-0")}}, nil
		}}
		p := newTestPG(client, 1, false)
		summary := p.removeMarkers("db", postgresqlPodSelector+"acid-main", map[types.UID]string{"old-0": "acid-main-0"})
		if !strings.Contains(summary, "left untouched (marker removed): []") || !strings.Contains(summary, "STILL MARKED") ||
			!strings.Contains(summary, "acid-main-0") {
			t.Fatalf("summary = %s", summary)
		}
		if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-main-0")); n != postgresqlUnmarkAttempts {
			t.Errorf("expected %d bounded attempts, got %d", postgresqlUnmarkAttempts, n)
		}
	})
	t.Run("422 then pod rotated", func(t *testing.T) {
		srv := newPGTestServer(t)
		srv.json("PATCH", pgPodPath+"acid-main-0", 422, testFailed)
		client := &crTestClient{rc: srv.restClient(), pods: podSequence(
			[]corev1.Pod{markedPod("acid-main-0", "old-0")},
			[]corev1.Pod{testPod("acid-main-0", "new-0", boolPtr(true))},
		)}
		p := newTestPG(client, 1, false)
		summary := p.removeMarkers("db", postgresqlPodSelector+"acid-main", map[types.UID]string{"old-0": "acid-main-0"})
		if !strings.Contains(summary, "rotated: [acid-main-0]") || strings.Contains(summary, "STILL MARKED") {
			t.Fatalf("summary = %s", summary)
		}
	})
}

// #5: cleanup never removes a marker that is not owned by this run.
func TestRemoveMarkers_ForeignMarkerLeftAlone(t *testing.T) {
	srv := newPGTestServer(t)
	client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		return &corev1.PodList{Items: []corev1.Pod{foreignMarkedPod("acid-main-0", "old-0", "other-run"), testPod("acid-main-1", "old-1", boolPtr(true))}}, nil
	}}
	p := newTestPG(client, 1, false)
	summary := p.removeMarkers("db", postgresqlPodSelector+"acid-main", map[types.UID]string{"old-0": "acid-main-0", "old-1": "acid-main-1"})
	if !strings.Contains(summary, "marked by someone else (left as is): [acid-main-0]") || !strings.Contains(summary, "never marked: [acid-main-1]") {
		t.Fatalf("summary = %s", summary)
	}
	for _, pod := range []string{"acid-main-0", "acid-main-1"} {
		if n := len(srv.bodiesFor("PATCH", pgPodPath+pod)); n != 0 {
			t.Errorf("pod %s must not be patched, got %d", pod, n)
		}
	}
}

// #13: the dry-run preview lists the concrete CRs, with the same --older-than
// selection as execute.
func TestGetOperatorClustersToRestart_RespectsOlderThan(t *testing.T) {
	young := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	list := `{"items":[{"metadata":{"name":"old","creationTimestamp":"2020-01-01T00:00:00Z"}},` +
		`{"metadata":{"name":"young","creationTimestamp":"` + young + `"}}]}`
	minAge := time.Hour

	srv := newCRTestServer(t)
	srv.json("GET", "/apis/acid.zalan.do", 200, apiGroupV1Only)
	srv.json("GET", pgListPath, 200, list)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
	srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1Only)
	srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/db/kafkas", 200, list)
	client := &crTestClient{rc: srv.restClient()}

	pg := NewPostgresqlOperations(client, 1, 1, true, &minAge, false)
	got, err := pg.GetPostgresqlClustersToRestart(context.Background(), []string{"db"})
	if err != nil || strings.Join(got, ",") != "db/old" {
		t.Fatalf("postgresql = %v, %v", got, err)
	}
	kafka := NewKafkaOperations(client, 1, 1, true, &minAge, false, "", "")
	got, err = kafka.GetKafkaClustersToRestart(context.Background(), []string{"db"})
	if err != nil || strings.Join(got, ",") != "db/old" {
		t.Fatalf("kafka = %v, %v", got, err)
	}
	got, err = NewKafkaOperations(client, 1, 1, true, nil, false, "", "").GetKafkaClustersToRestart(context.Background(), []string{"db"})
	if err != nil || strings.Join(got, ",") != "db/old,db/young" {
		t.Fatalf("kafka without --older-than = %v, %v", got, err)
	}

	// Missing API group: empty preview, no error.
	empty := &crTestClient{rc: newCRTestServer(t).restClient()}
	if got, err := NewPostgresqlOperations(empty, 1, 1, true, nil, false).GetPostgresqlClustersToRestart(context.Background(), []string{"db"}); err != nil || len(got) != 0 {
		t.Fatalf("missing API: %v, %v", got, err)
	}
}

func writeStatus(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// A failed re-trigger PATCH (403 or 503) during the wait trips the run-wide
// stop at once (before the rotation completes), is kept and returned although
// the operator then completes the rotation, and the next cluster is never
// started.
func TestRestartPostgresqlClusters_RetriggerFailureStopsRun(t *testing.T) {
	for _, code := range []int{403, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			fastPolling(t)
			srv := newPGTestServer(t)
			srv.json("GET", pgListPath, 200, `{"items":[{"metadata":{"name":"acid-main"}},{"metadata":{"name":"acid-next"}}]}`)
			var mainTriggers, nextTriggers atomic.Int32
			for _, name := range []string{"acid-main", "acid-next"} {
				srv.json("GET", pgListPath+"/"+name, 200, pgRunning2)
				for _, ordinal := range []string{"0", "1"} {
					srv.handle("PATCH", pgPodPath+name+"-"+ordinal, okJSON)
				}
			}
			srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, r *http.Request) {
				if mainTriggers.Add(1) == 2 {
					body := forbidden
					if code == 503 {
						body = serverUnavailable
					}
					writeStatus(w, code, body)
					return
				}
				okJSON(w, r)
			})
			srv.handle("PATCH", pgListPath+"/acid-next", func(w http.ResponseWriter, r *http.Request) {
				nextTriggers.Add(1)
				okJSON(w, r)
			})
			rs := NewRunStop()
			var replacedSeen, stoppedBeforeDone atomic.Bool
			client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
				name := strings.TrimPrefix(opts.LabelSelector, postgresqlPodSelector)
				triggers := nextTriggers.Load()
				replaced := triggers > 0
				if name == "acid-main" {
					// The operator rotates only after the failed re-trigger.
					triggers = mainTriggers.Load()
					replaced = triggers >= 2
					if replaced && !replacedSeen.Swap(true) {
						stoppedBeforeDone.Store(rs.Stopped())
					}
				}
				var pods []corev1.Pod
				for _, ordinal := range []string{"0", "1"} {
					uid := name + "-old-" + ordinal
					if replaced {
						uid = name + "-new-" + ordinal
					}
					pod := testPod(name+"-"+ordinal, uid, boolPtr(true))
					if triggers > 0 && !replaced {
						pod.Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true", PostgresqlMarkedByAnnotation: testPGRunID}
					}
					pods = append(pods, pod)
				}
				return &corev1.PodList{Items: pods}, nil
			}}
			p := newTestPG(client, 5, false)
			p.stallInterval = time.Nanosecond
			p.SetRunStop(rs)
			err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
			if err == nil || !strings.Contains(err.Error(), "failed to re-trigger operator sync") ||
				!strings.Contains(err.Error(), "the rotation completed nevertheless") {
				t.Fatalf("failed re-trigger must be returned after the completed rotation, got %v", err)
			}
			if !stoppedBeforeDone.Load() {
				t.Error("failed re-trigger must trip the run-wide stop before the rotation completes")
			}
			if !rs.Stopped() || rs.Cause() == nil || !strings.Contains(rs.Cause().Error(), "postgresql db/acid-main") {
				t.Errorf("run stop not tripped by acid-main: stopped=%v, cause=%v", rs.Stopped(), rs.Cause())
			}
			if n := nextTriggers.Load(); n != 0 {
				t.Errorf("next cluster triggered after failed re-trigger: %d PATCH", n)
			}
			if !strings.Contains(err.Error(), "postgresql db/acid-next: not started due to earlier failure") {
				t.Errorf("next cluster not reported as not started: %v", err)
			}
		})
	}
}

// parallel=2, --skip-wait: A's trigger PATCH is rejected (403) and A starts
// removing its markers; B completes while A's cleanup is blocked. The stop
// must already be tripped, so the slot B releases never starts C.
func TestRestartPostgresqlClusters_RejectionStopsBeforeCleanup(t *testing.T) {
	srv := newPGTestServer(t)
	srv.json("GET", pgListPath, 200, `{"items":[{"metadata":{"name":"acid-main"}},{"metadata":{"name":"acid-b"}},{"metadata":{"name":"acid-c"}}]}`)
	const oneInstance = `{"spec":{"numberOfInstances":1},"status":{"PostgresClusterStatus":"Running"}}`
	for _, name := range []string{"acid-main", "acid-b", "acid-c"} {
		srv.json("GET", pgListPath+"/"+name, 200, oneInstance)
		srv.handle("PATCH", pgPodPath+name+"-0", okJSON)
	}
	enteredCleanup := make(chan struct{})
	cTriggered := make(chan struct{})
	var (
		aTriggered, bTriggered atomic.Bool
		enterOnce, cOnce       sync.Once
	)
	srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"op":"remove"`) {
			// Block A's cleanup until C is triggered (the bug) or long
			// enough for B to finish and free its slot.
			enterOnce.Do(func() { close(enteredCleanup) })
			select {
			case <-cTriggered:
			case <-time.After(500 * time.Millisecond):
			}
		}
		okJSON(w, r)
	})
	srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, _ *http.Request) {
		aTriggered.Store(true)
		writeStatus(w, 403, forbidden)
	})
	srv.handle("PATCH", pgListPath+"/acid-b", func(w http.ResponseWriter, r *http.Request) {
		// B finishes only after A detected its failure.
		select {
		case <-enteredCleanup:
		case <-time.After(5 * time.Second):
			t.Error("A never entered cleanup")
		}
		bTriggered.Store(true)
		okJSON(w, r)
	})
	srv.handle("PATCH", pgListPath+"/acid-c", func(w http.ResponseWriter, r *http.Request) {
		cOnce.Do(func() { close(cTriggered) })
		okJSON(w, r)
	})
	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		name := strings.TrimPrefix(opts.LabelSelector, postgresqlPodSelector)
		pod := testPod(name+"-0", name+"-uid", boolPtr(true))
		if name == "acid-main" && aTriggered.Load() {
			pod.Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true", PostgresqlMarkedByAnnotation: testPGRunID}
		}
		return &corev1.PodList{Items: []corev1.Pod{pod}}, nil
	}}
	rs := NewRunStop()
	p := newTestPG(client, 1, true)
	p.parallel = 2
	p.SetRunStop(rs)
	err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !bTriggered.Load() {
		t.Fatalf("bad setup: err=%v, b triggered=%v", err, bTriggered.Load())
	}
	if n := len(srv.bodiesFor("PATCH", pgListPath+"/acid-c")); n != 0 {
		t.Fatalf("cluster C triggered during A's failure cleanup: %d PATCH; err=%v", n, err)
	}
	if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-c-0")); n != 0 {
		t.Errorf("cluster C marked during A's failure cleanup: %d PATCH", n)
	}
	if !strings.Contains(err.Error(), "failed to trigger operator sync") ||
		!strings.Contains(err.Error(), "postgresql db/acid-c: not started due to earlier failure") {
		t.Errorf("A's failure and C not started must be reported, got %v", err)
	}
	if !rs.Stopped() {
		t.Error("rejected trigger must trip the run-wide stop")
	}
}

// A plan refresh (after a 422 on marking) re-reads the CR: a status that is
// no longer Running or a changed spec.numberOfInstances fails the cluster,
// our marker is removed and the operator is never triggered.
func TestRestartPostgresqlClusters_RefreshRechecksCR(t *testing.T) {
	for name, tc := range map[string]struct{ state, want string }{
		"SyncFailed": {
			state: `{"spec":{"numberOfInstances":2},"status":{"PostgresClusterStatus":"SyncFailed"}}`,
			want:  `cluster status is "SyncFailed"`,
		},
		"InstancesChanged": {
			state: `{"spec":{"numberOfInstances":3},"status":{"PostgresClusterStatus":"Running"}}`,
			want:  "found 2 pod(s), spec.numberOfInstances is 3",
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newPGTestServer(t)
			var reads, marks atomic.Int32
			srv.handle("GET", pgCRPath, func(w http.ResponseWriter, _ *http.Request) {
				body := pgRunning2
				if reads.Add(1) > 1 {
					body = tc.state
				}
				writeStatus(w, 200, body)
			})
			srv.handle("PATCH", pgCRPath, okJSON)
			srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
			srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
				if marks.Add(1) == 1 {
					writeStatus(w, 422, testFailed)
					return
				}
				okJSON(w, r)
			})
			client := &crTestClient{rc: srv.restClient(), pods: podSequence(
				[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))},
				[]corev1.Pod{testPod("acid-main-0", "old-0", boolPtr(true)), markedPod("acid-main-1", "old-1")},
			)}
			rs := NewRunStop()
			p := newTestPG(client, 1, true)
			p.SetRunStop(rs)
			err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
			if err == nil || !strings.Contains(err.Error(), "cluster changed while being marked") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refreshed plan must re-check the current CR, got %v (CR reads: %d)", err, reads.Load())
			}
			if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
				t.Errorf("triggered operator sync despite changed CR: %d PATCH", n)
			}
			if n := marks.Load(); n != 1 {
				t.Errorf("acid-main-0 must not be marked after the refresh, got %d PATCH", n)
			}
			if n := countContaining(srv.bodiesFor("PATCH", pgPodPath+"acid-main-1"), `"op":"remove"`); n != 1 {
				t.Errorf("marker of acid-main-1 must be removed, got %d removals", n)
			}
			if !rs.Stopped() {
				t.Error("marking failure must trip the run-wide stop")
			}
		})
	}
}

// parallel=2: cluster A's first marker PATCH fails (403 rejected or 503
// ambiguous) while cluster B is in flight. The run-wide stop must be tripped
// at once: A marks no further pod, and the slot B frees never starts C (with
// and without --skip-wait). Without the immediate stop A would go on to mark
// its next pod (a slow PATCH here), B would finish meanwhile and C would start.
func TestRestartPostgresqlClusters_MarkFailureStopsImmediately(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, code := range []int{403, 503} {
			t.Run(fmt.Sprintf("skipWait=%t/code=%d", skip, code), func(t *testing.T) {
				fastPolling(t)
				rs := NewRunStop()
				srv := newPGTestServer(t)
				srv.json("GET", pgListPath, 200, `{"items":[{"metadata":{"name":"acid-main"}},{"metadata":{"name":"acid-b"}},{"metadata":{"name":"acid-c"}}]}`)
				for _, name := range []string{"acid-main", "acid-b", "acid-c"} {
					srv.json("GET", pgListPath+"/"+name, 200, pgRunning2)
					srv.handle("PATCH", pgListPath+"/"+name, okJSON)
					for _, ordinal := range []string{"0", "1"} {
						srv.handle("PATCH", pgPodPath+name+"-"+ordinal, okJSON)
					}
				}
				isRemove := func(r *http.Request) bool {
					body, _ := io.ReadAll(r.Body)
					return strings.Contains(string(body), `"op":"remove"`)
				}
				srv.handle("PATCH", pgPodPath+"acid-main-0", func(w http.ResponseWriter, r *http.Request) {
					switch {
					case isRemove(r):
						okJSON(w, r)
					case code == 503:
						writeStatus(w, code, serverUnavailable)
					default:
						writeStatus(w, code, forbidden)
					}
				})
				secondMark := make(chan struct{})
				var secondMarkOnce sync.Once
				srv.handle("PATCH", pgPodPath+"acid-main-1", func(w http.ResponseWriter, r *http.Request) {
					if !isRemove(r) {
						secondMarkOnce.Do(func() { close(secondMark) })
						time.Sleep(300 * time.Millisecond)
					}
					okJSON(w, r)
				})
				var bTriggered, cTriggered atomic.Bool
				// B completes only once A's failure is known: the stop was
				// tripped, or A went on to mark its next pod.
				srv.handle("PATCH", pgListPath+"/acid-b", func(w http.ResponseWriter, r *http.Request) {
					deadline := time.After(5 * time.Second)
				wait:
					for !rs.Stopped() {
						select {
						case <-secondMark:
							break wait
						case <-deadline:
							t.Error("A's marker failure was never observed")
							break wait
						case <-time.After(time.Millisecond):
						}
					}
					bTriggered.Store(true)
					okJSON(w, r)
				})
				srv.handle("PATCH", pgListPath+"/acid-c", func(w http.ResponseWriter, r *http.Request) {
					cTriggered.Store(true)
					okJSON(w, r)
				})
				client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
					name := strings.TrimPrefix(opts.LabelSelector, postgresqlPodSelector)
					replaced := name == "acid-b" && bTriggered.Load() || name == "acid-c" && cTriggered.Load()
					var pods []corev1.Pod
					for _, ordinal := range []string{"0", "1"} {
						uid := name + "-old-" + ordinal
						if replaced {
							uid = name + "-new-" + ordinal
						}
						pods = append(pods, testPod(name+"-"+ordinal, uid, boolPtr(true)))
					}
					return &corev1.PodList{Items: pods}, nil
				}}
				p := newTestPG(client, 2, skip)
				p.parallel = 2
				p.SetRunStop(rs)

				err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
				if err == nil || !bTriggered.Load() {
					t.Fatalf("bad setup: err=%v, B triggered=%t", err, bTriggered.Load())
				}
				if cTriggered.Load() || !strings.Contains(err.Error(), "postgresql db/acid-c: not started due to earlier failure") {
					t.Fatalf("C launched after A's marker failure (%d): C triggered=%t, err=%v", code, cTriggered.Load(), err)
				}
				if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-c-0")); n != 0 {
					t.Errorf("C marked after A's marker failure: %d PATCH", n)
				}
				if n := len(srv.bodiesFor("PATCH", pgPodPath+"acid-main-1")); n != 0 {
					t.Errorf("A kept marking pods after its first marker PATCH failed: %d PATCH on acid-main-1", n)
				}
				if n := len(srv.bodiesFor("PATCH", pgCRPath)); n != 0 {
					t.Errorf("A's operator sync triggered after its marker failure: %d PATCH", n)
				}
				if !rs.Stopped() || !strings.Contains(rs.Cause().Error(), "failed to mark pod db/acid-main-0") {
					t.Errorf("stop not tripped by A's marker failure: stopped=%t, cause=%v", rs.Stopped(), rs.Cause())
				}
			})
		}
	}
}

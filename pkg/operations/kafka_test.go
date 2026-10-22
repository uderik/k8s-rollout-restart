package operations

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

func TestNewKafkaOperations_APIPaths(t *testing.T) {
	tests := []struct {
		name           string
		kafkaVersion   string
		podSetVersion  string
		wantKafkaPath  string
		wantPodSetPath string
	}{
		{
			name:           "defaults when empty",
			kafkaVersion:   "",
			podSetVersion:  "",
			wantKafkaPath:  "/apis/" + DefaultKafkaAPIVersion,
			wantPodSetPath: "/apis/" + DefaultStrimziPodSetAPIVersion,
		},
		{
			name:           "custom versions",
			kafkaVersion:   "kafka.strimzi.io/v1beta3",
			podSetVersion:  "core.strimzi.io/v1beta3",
			wantKafkaPath:  "/apis/kafka.strimzi.io/v1beta3",
			wantPodSetPath: "/apis/core.strimzi.io/v1beta3",
		},
		{
			name:           "tolerates leading /apis/ prefix",
			kafkaVersion:   "/apis/kafka.strimzi.io/v1beta2",
			podSetVersion:  "/apis/core.strimzi.io/v1beta2",
			wantKafkaPath:  "/apis/kafka.strimzi.io/v1beta2",
			wantPodSetPath: "/apis/core.strimzi.io/v1beta2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := NewKafkaOperations(nil, 1, 1, false, nil, false, tt.kafkaVersion, tt.podSetVersion)
			if k.kafkaAPIPath != tt.wantKafkaPath {
				t.Errorf("kafkaAPIPath = %q, want %q", k.kafkaAPIPath, tt.wantKafkaPath)
			}
			if k.podSetAPIPath != tt.wantPodSetPath {
				t.Errorf("podSetAPIPath = %q, want %q", k.podSetAPIPath, tt.wantPodSetPath)
			}
		})
	}
}

func TestIsNotFoundErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "unrelated error", err: errStr("connection refused"), want: false},
		{name: "server could not find", err: errStr(`the server could not find the requested resource (get kafkas.kafka.strimzi.io)`), want: true},
		{name: "could not find", err: errStr("could not find the requested resource"), want: true},
		{name: "plain not found", err: errStr(`kafkas.kafka.strimzi.io "x" not found`), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundErr(tt.err); got != tt.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// errStr is a tiny error type for table tests.
type errStr string

func (e errStr) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// Test doubles shared by the operator (Kafka/PostgreSQL) tests.
// ---------------------------------------------------------------------------

// crTestServer is a fake apiserver: handlers are keyed by "METHOD /path".
type crTestServer struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	bodies   map[string][]string
}

func newCRTestServer(t *testing.T) *crTestServer {
	return &crTestServer{t: t, handlers: map[string]http.HandlerFunc{}, bodies: map[string][]string{}}
}

func (s *crTestServer) handle(method, path string, h http.HandlerFunc) {
	s.handlers[method+" "+path] = h
}

func (s *crTestServer) json(method, path string, code int, body string) {
	s.handle(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
}

func (s *crTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies[key] = append(s.bodies[key], string(body))
	h, ok := s.handlers[key]
	s.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"the server could not find the requested resource"}`))
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	h(w, r)
}

func (s *crTestServer) bodiesFor(method, path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies[method+" "+path]...)
}

// methodCount returns the number of requests received with the given method.
func (s *crTestServer) methodCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key, bodies := range s.bodies {
		if strings.HasPrefix(key, method+" ") {
			n += len(bodies)
		}
	}
	return n
}

func (s *crTestServer) restClient() rest.Interface {
	srv := httptest.NewServer(s)
	s.t.Cleanup(srv.Close)
	rc, err := rest.RESTClientFor(&rest.Config{
		Host: srv.URL,
		// No client-side rate limiting: tests poll every few milliseconds.
		QPS: -1,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		},
	})
	if err != nil {
		s.t.Fatalf("RESTClientFor: %v", err)
	}
	return rc
}

// crTestClient implements K8sClient with a fake REST server and a pluggable
// pod lister.
type crTestClient struct {
	rc   rest.Interface
	pods func(namespace string, opts metav1.ListOptions) (*corev1.PodList, error)
}

func (c *crTestClient) CoreV1() CoreV1Interface    { return crTestCoreV1{c} }
func (c *crTestClient) AppsV1() AppsV1Interface    { return nil }
func (c *crTestClient) RESTClient() rest.Interface { return c.rc }
func (c *crTestClient) ClearCache()                {}

type crTestCoreV1 struct{ c *crTestClient }

func (x crTestCoreV1) Pods(ns string) PodInterface    { return crTestPods{c: x.c, ns: ns} }
func (x crTestCoreV1) Nodes() NodeInterface           { return nil }
func (x crTestCoreV1) Namespaces() NamespaceInterface { return nil }

type crTestPods struct {
	c  *crTestClient
	ns string
}

func (p crTestPods) List(_ context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	if p.c.pods == nil {
		return &corev1.PodList{}, nil
	}
	return p.c.pods(p.ns, opts)
}

// testPod builds a running pod with the given UID; ready=nil means "no Ready
// condition at all".
func testPod(name, uid string, ready *bool, owner ...metav1.OwnerReference) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), OwnerReferences: owner},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready != nil {
		status := corev1.ConditionFalse
		if *ready {
			status = corev1.ConditionTrue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	}
	return pod
}

// fastPolling shortens the waiter poll interval for the duration of a test.
func fastPolling(t *testing.T) {
	old := crPollInterval
	crPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { crPollInterval = old })
}

const (
	apiGroupV1Only     = `{"kind":"APIGroup","versions":[{"version":"v1"}]}`
	apiGroupBetaOnly   = `{"kind":"APIGroup","versions":[{"version":"v1beta2"}]}`
	apiGroupV1AndBeta2 = `{"kind":"APIGroup","versions":[{"version":"v1"},{"version":"v1beta2"}]}`
)

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewKafkaOperations_VersionDiscovery(t *testing.T) {
	k := NewKafkaOperations(nil, 1, 1, false, nil, false, DefaultKafkaAPIVersion, "")
	if k.kafkaGroup != "kafka.strimzi.io" || k.kafkaVersion != "" {
		t.Errorf("default kafka API should be auto-discovered, got group=%q version=%q", k.kafkaGroup, k.kafkaVersion)
	}
	if k.podSetGroup != "core.strimzi.io" || k.podSetVersion != "" {
		t.Errorf("default podset API should be auto-discovered, got group=%q version=%q", k.podSetGroup, k.podSetVersion)
	}

	k = NewKafkaOperations(nil, 1, 1, false, nil, false, "kafka.strimzi.io/v1beta2", "/apis/core.strimzi.io/v1beta2")
	if k.kafkaVersion != "v1beta2" || k.podSetVersion != "v1beta2" {
		t.Errorf("explicit versions must be kept, got %q / %q", k.kafkaVersion, k.podSetVersion)
	}
}

func TestResolveAPIGroupVersion(t *testing.T) {
	tests := []struct {
		name        string
		code        int
		body        string
		version     string
		wantPath    string
		wantPresent bool
		wantErr     bool
	}{
		{name: "group missing", code: 0, wantPresent: false},
		{name: "prefers v1", code: 200, body: apiGroupV1AndBeta2, wantPath: "/apis/g.io/v1", wantPresent: true},
		{name: "falls back to v1beta2", code: 200, body: apiGroupBetaOnly, wantPath: "/apis/g.io/v1beta2", wantPresent: true},
		{name: "explicit served", code: 200, body: apiGroupV1AndBeta2, version: "v1beta2", wantPath: "/apis/g.io/v1beta2", wantPresent: true},
		{name: "explicit not served", code: 200, body: apiGroupV1Only, version: "v1beta2", wantPresent: true, wantErr: true},
		{name: "no supported version", code: 200, body: `{"versions":[{"version":"v1alpha1"}]}`, wantPresent: true, wantErr: true},
		{name: "forbidden is an error", code: 403, body: `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newCRTestServer(t)
			if tt.code != 0 {
				srv.json("GET", "/apis/g.io", tt.code, tt.body)
			}
			path, present, err := ResolveAPIGroupVersion(context.Background(), srv.restClient(), "g.io", tt.version, "v1", "v1beta2")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if present != tt.wantPresent {
				t.Errorf("present = %v, want %v", present, tt.wantPresent)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

func TestSelectPodSetsForKafka(t *testing.T) {
	mk := func(name string, labels map[string]string, owners ...metav1.OwnerReference) strimziPodSet {
		var ps strimziPodSet
		ps.Metadata.Name = name
		ps.Metadata.Labels = labels
		ps.Metadata.OwnerReferences = owners
		return ps
	}
	items := []strimziPodSet{
		mk("my-cluster-brokers", map[string]string{strimziClusterLabel: "my-cluster", strimziKindLabel: "Kafka"},
			metav1.OwnerReference{Kind: "Kafka", Name: "my-cluster"}),
		mk("my-cluster-controllers", map[string]string{strimziClusterLabel: "my-cluster", strimziKindLabel: "Kafka"},
			metav1.OwnerReference{Kind: "KafkaNodePool", Name: "controllers"}),
		mk("my-cluster-kafka", map[string]string{strimziClusterLabel: "my-cluster"}),
		mk("other-kafka", map[string]string{strimziClusterLabel: "other"}),
		mk("my-cluster-connect", map[string]string{strimziClusterLabel: "my-cluster", strimziKindLabel: "KafkaConnect"}),
		mk("foreign", map[string]string{strimziClusterLabel: "my-cluster"},
			metav1.OwnerReference{Kind: "Something", Name: "x"}),
	}

	got := selectPodSetsForKafka(items, "my-cluster")
	want := []string{"my-cluster-brokers", "my-cluster-controllers", "my-cluster-kafka"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("selectPodSetsForKafka() = %v, want %v", got, want)
	}
}

func TestListPodSetsForKafka_UsesLabelSelector(t *testing.T) {
	srv := newCRTestServer(t)
	srv.handle("GET", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("labelSelector"); got != "strimzi.io/cluster=my-cluster" {
			t.Errorf("labelSelector = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"my-cluster-pool-a","labels":{"strimzi.io/cluster":"my-cluster","strimzi.io/kind":"Kafka"}}}]}`))
	})
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
	got, err := k.listPodSetsForKafka(context.Background(), "kafka", "my-cluster", "/apis/core.strimzi.io/v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "my-cluster-pool-a" {
		t.Errorf("got %v", got)
	}
}

func TestEvaluatePodReplacement(t *testing.T) {
	old := map[types.UID]string{"old-1": "p-0", "old-2": "p-1"}
	terminating := testPod("p-1", "new-x", boolPtr(true))
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	tests := []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		{
			name: "old pods still present",
			pods: []corev1.Pod{testPod("p-0", "old-1", boolPtr(true)), testPod("p-1", "old-2", boolPtr(true))},
			want: false,
		},
		{
			name: "one old pod remaining",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "old-2", boolPtr(true))},
			want: false,
		},
		{
			name: "new pod without Ready condition is not ready",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "new-2", nil)},
			want: false,
		},
		{
			name: "new pod Ready=False",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "new-2", boolPtr(false))},
			want: false,
		},
		{
			name: "terminating pod does not count",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), terminating},
			want: false,
		},
		{
			name: "fewer pods than expected",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true))},
			want: false,
		},
		{
			name: "extra NotReady pod keeps restart unfinished",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "new-2", boolPtr(true)), testPod("p-2", "new-3", boolPtr(false))},
			want: false,
		},
		{
			name: "more ready pods than desired",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "new-2", boolPtr(true)), testPod("p-2", "new-3", boolPtr(true))},
			want: false,
		},
		{
			name: "all replaced and ready",
			pods: []corev1.Pod{testPod("p-0", "new-1", boolPtr(true)), testPod("p-1", "new-2", boolPtr(true))},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, progress := evaluatePodReplacement(tt.pods, old, 2); got != tt.want {
				t.Errorf("evaluatePodReplacement() = %v (%s), want %v", got, progress, tt.want)
			}
		})
	}
}

func TestWaitForClusterRestart_TimesOutWhileOldPodsRemain(t *testing.T) {
	fastPolling(t)
	client := &crTestClient{pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		return &corev1.PodList{Items: []corev1.Pod{testPod("p-0", "old-1", boolPtr(true))}}, nil
	}}
	spec := clusterWaitSpec{kind: "Test", namespace: "ns", name: "c", selector: "a=b"}
	if err := spec.recordPods(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	err := waitForClusterRestart(context.Background(), client, logger.NewLogger(false), 1, spec, nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestWaitForClusterRestart_RespectsParentContext(t *testing.T) {
	fastPolling(t)
	client := &crTestClient{pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		return &corev1.PodList{Items: []corev1.Pod{testPod("p-0", "old-1", boolPtr(true))}}, nil
	}}
	spec := clusterWaitSpec{kind: "Test", namespace: "ns", name: "c", selector: "a=b"}
	if err := spec.recordPods(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := waitForClusterRestart(ctx, client, logger.NewLogger(false), 60, spec, nil)
	if err == nil {
		t.Fatal("expected error when parent context expires")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waiter ignored parent context deadline")
	}
}

func TestRecordPods_NoPodsIsError(t *testing.T) {
	spec := clusterWaitSpec{kind: "Test", namespace: "ns", name: "c", selector: "a=b"}
	if err := spec.recordPods(context.Background(), &crTestClient{}); err == nil {
		t.Fatal("expected error for cluster without pods")
	}
}

func TestRestartCRTargets_BoundedStopsAndReportsNotStarted(t *testing.T) {
	var (
		mu      sync.Mutex
		running int
		maxSeen int
		started []string
	)
	targets := []crTarget{{"a", "c1"}, {"a", "c2"}, {"b", "c3"}, {"b", "c4"}, {"c", "c5"}}
	rs := NewRunStop()
	err := restartCRTargets(context.Background(), logger.NewLogger(false), "Test", 2, false, rs, targets,
		func(_ context.Context, tg crTarget, _ *RunStop) error {
			mu.Lock()
			running++
			if running > maxSeen {
				maxSeen = running
			}
			started = append(started, tg.name)
			mu.Unlock()
			defer func() {
				mu.Lock()
				running--
				mu.Unlock()
			}()
			if tg.name == "c1" {
				return fmt.Errorf("boom %s", tg.name)
			}
			// c2 is in flight while c1 fails: it must still finish.
			time.Sleep(50 * time.Millisecond)
			return nil
		})
	if maxSeen > 2 {
		t.Errorf("max concurrency = %d, want <= 2", maxSeen)
	}
	if len(started) != 2 {
		t.Errorf("started %v, want only the first two (c1 failed)", started)
	}
	if !rs.Stopped() {
		t.Error("the shared RunStop must be tripped by the failure")
	}
	if err == nil || !strings.Contains(err.Error(), "test a/c1: boom c1") ||
		strings.Count(err.Error(), "not started due to earlier failure") != 3 {
		t.Errorf("expected c1 failure and 3 not-started targets, got %v", err)
	}
}

// newKafkaTestServer serves Strimzi discovery and a single Kafka cluster.
func newKafkaTestServer(t *testing.T) *crTestServer {
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1AndBeta2)
	srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1AndBeta2)
	srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/kafka/kafkas", 200,
		`{"items":[{"metadata":{"name":"my-cluster","creationTimestamp":"2020-01-01T00:00:00Z"}}]}`)
	return srv
}

func TestRestartKafkaClusters_APINotInstalledIsSkipped(t *testing.T) {
	srv := newCRTestServer(t)
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
	if err := k.RestartKafkaClusters(context.Background(), []string{"kafka"}); err != nil {
		t.Fatalf("expected nil when Strimzi is not installed, got %v", err)
	}
}

func TestRestartKafkaClusters_VersionMismatchIsError(t *testing.T) {
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupBetaOnly)
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "kafka.strimzi.io/v1beta3", "")
	if err := k.RestartKafkaClusters(context.Background(), []string{"kafka"}); err == nil {
		t.Fatal("expected error for unserved explicit version")
	}
}

func TestRestartKafkaClusters_ListForbiddenIsError(t *testing.T) {
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1AndBeta2)
	srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1AndBeta2)
	srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/kafka/kafkas", 403,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`)
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
	if err := k.RestartKafkaClusters(context.Background(), []string{"kafka"}); err == nil {
		t.Fatal("expected 403 on list to be an error")
	}
}

func TestRestartKafkaClusters_NoPodSetsIsError(t *testing.T) {
	srv := newKafkaTestServer(t)
	srv.json("GET", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets", 200, `{"items":[]}`)
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
	err := k.RestartKafkaClusters(context.Background(), []string{"kafka"})
	if err == nil || !strings.Contains(err.Error(), "no StrimziPodSets found") {
		t.Fatalf("expected missing PodSets error, got %v", err)
	}
}

func TestRestartKafkaClusters_PatchErrorPropagates(t *testing.T) {
	srv := newKafkaTestServer(t)
	srv.json("GET", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets", 200,
		`{"items":[{"metadata":{"name":"my-cluster-brokers","labels":{"strimzi.io/cluster":"my-cluster"}}}]}`)
	srv.json("PATCH", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets/my-cluster-brokers", 403,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`)
	k := NewKafkaOperations(&crTestClient{rc: srv.restClient()}, 1, 1, false, nil, true, "", "")
	if err := k.RestartKafkaClusters(context.Background(), []string{"kafka"}); err == nil {
		t.Fatal("expected patch error to propagate")
	}
}

func TestRestartKafkaClusters_WaitsForReplacement(t *testing.T) {
	fastPolling(t)
	srv := newKafkaTestServer(t)
	srv.json("GET", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets", 200,
		`{"items":[{"metadata":{"name":"my-cluster-pool-a","labels":{"strimzi.io/cluster":"my-cluster","strimzi.io/kind":"Kafka"}}}]}`)

	var (
		mu      sync.Mutex
		patched bool
		polls   int
	)
	srv.handle("PATCH", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets/my-cluster-pool-a", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		patched = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	srv.json("GET", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets/my-cluster-pool-a", 200,
		`{"metadata":{"name":"my-cluster-pool-a","annotations":{}},"spec":{"pods":[{}]}}`)
	srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/kafka/kafkas/my-cluster", 200,
		`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)

	owner := metav1.OwnerReference{Kind: "StrimziPodSet", Name: "my-cluster-pool-a"}
	// The entity operator shares the cluster label but is not rolled by the
	// PodSet annotation; it must be ignored by the waiter.
	entityOperator := testPod("my-cluster-entity-operator", "eo", boolPtr(true), metav1.OwnerReference{Kind: "ReplicaSet", Name: "x"})
	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		if opts.LabelSelector != "strimzi.io/cluster=my-cluster" {
			t.Errorf("unexpected selector %q", opts.LabelSelector)
		}
		mu.Lock()
		defer mu.Unlock()
		if !patched {
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-pool-a-0", "old", boolPtr(true), owner), entityOperator}}, nil
		}
		polls++
		switch polls {
		case 1: // old pod still there
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-pool-a-0", "old", boolPtr(true), owner), entityOperator}}, nil
		case 2: // replaced, but no Ready condition yet
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-pool-a-0", "new", nil, owner), entityOperator}}, nil
		default:
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-pool-a-0", "new", boolPtr(true), owner), entityOperator}}, nil
		}
	}}

	k := NewKafkaOperations(client, 1, 10, false, nil, false, "", "")
	if err := k.RestartKafkaClusters(context.Background(), []string{"kafka"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls < 3 {
		t.Errorf("restart reported done after %d polls, expected to wait for replacement and readiness", polls)
	}
	if bodies := srv.bodiesFor("PATCH", "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets/my-cluster-pool-a"); len(bodies) != 1 ||
		!strings.Contains(bodies[0], strimziManualRollingUpdate) {
		t.Errorf("unexpected PodSet patches: %v", bodies)
	}
}

// R8: the expected pod count comes from the desired state, not from the
// pre-restart snapshot. The snapshot saw one pod while the PodSet wants two;
// one new Ready pod plus one new NotReady pod is not a finished restart.
func TestWaitForClusterRestart_ExpectedFromDesiredState(t *testing.T) {
	fastPolling(t)
	var (
		mu    sync.Mutex
		polls int
	)
	client := &crTestClient{pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		switch {
		case polls == 1: // snapshot: the second instance is missing
			return &corev1.PodList{Items: []corev1.Pod{testPod("p-0", "old-0", boolPtr(true))}}, nil
		case polls < 5:
			return &corev1.PodList{Items: []corev1.Pod{testPod("p-0", "new-0", boolPtr(true)), testPod("p-1", "new-1", boolPtr(false))}}, nil
		default:
			return &corev1.PodList{Items: []corev1.Pod{testPod("p-0", "new-0", boolPtr(true)), testPod("p-1", "new-1", boolPtr(true))}}, nil
		}
	}}
	spec := clusterWaitSpec{kind: "Test", namespace: "ns", name: "c", selector: "a=b",
		desired: func(context.Context) (int, error) { return 2, nil }}
	if err := spec.recordPods(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if err := waitForClusterRestart(context.Background(), client, logger.NewLogger(false), 5, spec, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls < 5 {
		t.Errorf("restart reported done after %d polls while a desired pod was NotReady", polls)
	}
}

// R5: when one PodSet PATCH fails, the PodSets that were annotated are still
// waited for before the aggregated error is returned.
func TestRestartKafkaClusters_PartialPatchFailureStillWaits(t *testing.T) {
	fastPolling(t)
	srv := newKafkaTestServer(t)
	psPath := "/apis/core.strimzi.io/v1/namespaces/kafka/strimzipodsets"
	srv.json("GET", psPath, 200, `{"items":[`+
		`{"metadata":{"name":"my-cluster-a","labels":{"strimzi.io/cluster":"my-cluster"}}},`+
		`{"metadata":{"name":"my-cluster-b","labels":{"strimzi.io/cluster":"my-cluster"}}}]}`)

	var (
		mu      sync.Mutex
		patched bool
		polls   int
	)
	srv.handle("PATCH", psPath+"/my-cluster-a", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		patched = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	srv.json("PATCH", psPath+"/my-cluster-b", 403,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`)
	srv.json("GET", psPath+"/my-cluster-a", 200, `{"metadata":{"name":"my-cluster-a"},"spec":{"pods":[{}]}}`)
	srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/kafka/kafkas/my-cluster", 200,
		`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)

	ownerA := metav1.OwnerReference{Kind: "StrimziPodSet", Name: "my-cluster-a"}
	ownerB := metav1.OwnerReference{Kind: "StrimziPodSet", Name: "my-cluster-b"}
	client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
		mu.Lock()
		defer mu.Unlock()
		b := testPod("my-cluster-b-0", "old-b", boolPtr(true), ownerB) // never rolled
		if !patched {
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-a-0", "old-a", boolPtr(true), ownerA), b}}, nil
		}
		polls++
		if polls < 3 {
			return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-a-0", "old-a", boolPtr(true), ownerA), b}}, nil
		}
		return &corev1.PodList{Items: []corev1.Pod{testPod("my-cluster-a-0", "new-a", boolPtr(true), ownerA), b}}, nil
	}}

	k := NewKafkaOperations(client, 1, 5, false, nil, false, "", "")
	err := k.RestartKafkaClusters(context.Background(), []string{"kafka"})
	if err == nil || !strings.Contains(err.Error(), "my-cluster-b") {
		t.Fatalf("expected aggregated error for my-cluster-b, got %v", err)
	}
	if strings.Contains(err.Error(), "timeout") {
		t.Fatalf("wait for my-cluster-a must succeed (old pods of the failed PodSet ignored): %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls < 3 {
		t.Errorf("returned after %d polls, before the annotated PodSet finished rolling", polls)
	}
}

func TestMutationDefinitelyFailed(t *testing.T) {
	if mutationDefinitelyFailed(fmt.Errorf("connection reset")) {
		t.Error("transport errors are ambiguous")
	}
}

// With --parallel=2, cluster a's second PodSet PATCH is rejected (403) while
// its first PodSet is already annotated, so a keeps waiting for it. The
// rejection must stop new launches right away, not only once a's wait ends:
// cluster b (already in flight) completes while a still waits, and the slot
// it frees must not start cluster c.
//
// Ordering is forced: b's PATCH is held until a has entered its wait (a pod
// list of a after the rejection), and a's annotated PodSet only finishes
// rolling once c was started or well after b completed.
func TestRestartKafkaClusters_RejectedPatchStopsBeforeWait(t *testing.T) {
	fastPolling(t)
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
	srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1Only)
	crs := "/apis/kafka.strimzi.io/v1/namespaces/ns/kafkas"
	sets := "/apis/core.strimzi.io/v1/namespaces/ns/strimzipodsets"
	srv.json("GET", crs, 200, `{"items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}},{"metadata":{"name":"c"}}]}`)

	var (
		aRejected, bPatched, cStarted atomic.Bool
		bDoneAt                       atomic.Int64 // unix nanos of b's last ready check
		aWaitingOnce                  sync.Once
	)
	aWaiting := make(chan struct{})
	podSetsOf := map[string][]string{"a": {"a-good", "a-reject"}, "b": {"b-pool"}, "c": {"c-pool"}}

	srv.handle("GET", sets, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), strimziClusterLabel+"=")
		if name == "c" {
			cStarted.Store(true)
		}
		items := make([]string, 0, len(podSetsOf[name]))
		for _, ps := range podSetsOf[name] {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":%q,"labels":{%q:%q}}}`, ps, strimziClusterLabel, name))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
	})
	for _, ps := range []string{"a-good", "a-reject", "b-pool", "c-pool"} {
		srv.json("GET", sets+"/"+ps, 200, `{"spec":{"pods":[{}]}}`)
		srv.handle("PATCH", sets+"/"+ps, okJSON)
	}
	srv.handle("PATCH", sets+"/a-reject", func(w http.ResponseWriter, _ *http.Request) {
		aRejected.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbidden))
	})
	srv.handle("PATCH", sets+"/b-pool", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-aWaiting:
		case <-time.After(5 * time.Second):
			t.Error("cluster a never started waiting for its annotated PodSet")
		}
		bPatched.Store(true)
		okJSON(w, r)
	})
	for _, name := range []string{"a", "c"} {
		srv.json("GET", crs+"/"+name, 200, `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	}
	srv.handle("GET", crs+"/b", func(w http.ResponseWriter, _ *http.Request) {
		bDoneAt.Store(time.Now().UnixNano())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`))
	})

	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		name := strings.TrimPrefix(opts.LabelSelector, strimziClusterLabel+"=")
		var fresh bool
		switch name {
		case "a":
			if aRejected.Load() {
				aWaitingOnce.Do(func() { close(aWaiting) })
			}
			done := bDoneAt.Load()
			fresh = cStarted.Load() || (done != 0 && time.Since(time.Unix(0, done)) > 300*time.Millisecond)
		case "b":
			fresh = bPatched.Load()
		case "c": // only reached on regression; finish fast
			fresh = len(srv.bodiesFor("PATCH", sets+"/c-pool")) > 0
		}
		var pods []corev1.Pod
		for _, ps := range podSetsOf[name] {
			uid := ps + "-old"
			// Only a's annotated PodSet (a-good) is rolled.
			if fresh && ps != "a-reject" {
				uid = ps + "-new"
			}
			pods = append(pods, testPod(ps+"-0", uid, boolPtr(true), metav1.OwnerReference{Kind: "StrimziPodSet", Name: ps}))
		}
		return &corev1.PodList{Items: pods}, nil
	}}

	rs := NewRunStop()
	k := NewKafkaOperations(client, 2, 10, false, nil, false, "", "")
	k.SetRunStop(rs)
	err := k.RestartKafkaClusters(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "a-reject") {
		t.Fatalf("expected cluster a's PodSet rejection, got %v", err)
	}
	if cStarted.Load() || len(srv.bodiesFor("PATCH", sets+"/c-pool")) != 0 {
		t.Fatalf("cluster c started after a's PATCH was rejected: %v", err)
	}
	if strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a's annotated PodSet must still be waited for successfully: %v", err)
	}
	if strings.Contains(err.Error(), "kafka ns/b") {
		t.Fatalf("cluster b was in flight and must complete: %v", err)
	}
	if !strings.Contains(err.Error(), "kafka ns/c: not started due to earlier failure") {
		t.Errorf("cluster c not reported as not started: %v", err)
	}
	if !rs.Stopped() || rs.Cause() == nil || !strings.Contains(rs.Cause().Error(), "a-reject") {
		t.Errorf("run stop must be tripped by a's rejection: stopped=%v cause=%v", rs.Stopped(), rs.Cause())
	}
}

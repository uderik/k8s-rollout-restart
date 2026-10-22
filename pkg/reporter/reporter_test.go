package reporter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"github.com/uderik/k8s-rollout-restart/pkg/operations"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// fakeReporterClient implements operations.K8sClient with empty core/apps
// listings and a fake REST server for custom resources.
type fakeReporterClient struct {
	rc       rest.Interface
	podDelay time.Duration

	nodes []corev1.Node

	mu            sync.Mutex
	inFlight      int
	maxInFlight   int
	maxGoroutines int
}

func (c *fakeReporterClient) CoreV1() operations.CoreV1Interface { return fakeCore{c} }
func (c *fakeReporterClient) AppsV1() operations.AppsV1Interface { return fakeApps{} }
func (c *fakeReporterClient) RESTClient() rest.Interface         { return c.rc }
func (c *fakeReporterClient) ClearCache()                        {}

type fakeCore struct{ c *fakeReporterClient }

func (f fakeCore) Pods(string) operations.PodInterface       { return fakePods(f) }
func (f fakeCore) Nodes() operations.NodeInterface           { return fakeNodes(f) }
func (f fakeCore) Namespaces() operations.NamespaceInterface { return nil }

type fakePods struct{ c *fakeReporterClient }

func (p fakePods) List(context.Context, metav1.ListOptions) (*corev1.PodList, error) {
	p.c.mu.Lock()
	p.c.inFlight++
	if p.c.inFlight > p.c.maxInFlight {
		p.c.maxInFlight = p.c.inFlight
	}
	p.c.maxGoroutines = max(p.c.maxGoroutines, runtime.NumGoroutine())
	p.c.mu.Unlock()
	time.Sleep(p.c.podDelay)
	p.c.mu.Lock()
	p.c.inFlight--
	p.c.mu.Unlock()
	return &corev1.PodList{}, nil
}

type fakeNodes struct{ c *fakeReporterClient }

func (n fakeNodes) List(context.Context, metav1.ListOptions) (*corev1.NodeList, error) {
	return &corev1.NodeList{Items: n.c.nodes}, nil
}
func (fakeNodes) Patch(_ context.Context, name string, _ types.PatchType, _ []byte, _ metav1.PatchOptions) (*corev1.Node, error) {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
}

type fakeApps struct{}

func (fakeApps) Deployments(string) operations.DeploymentInterface   { return fakeDeployments{} }
func (fakeApps) StatefulSets(string) operations.StatefulSetInterface { return fakeStatefulSets{} }
func (fakeApps) ReplicaSets(string) operations.ReplicaSetInterface   { return nil }

type fakeDeployments struct{}

func (fakeDeployments) List(context.Context, metav1.ListOptions) (*appsv1.DeploymentList, error) {
	return &appsv1.DeploymentList{}, nil
}
func (fakeDeployments) Get(context.Context, string, metav1.GetOptions) (*appsv1.Deployment, error) {
	return nil, nil
}
func (fakeDeployments) Update(_ context.Context, d *appsv1.Deployment, _ metav1.UpdateOptions) (*appsv1.Deployment, error) {
	return d, nil
}
func (fakeDeployments) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions) (*appsv1.Deployment, error) {
	return nil, nil
}

type fakeStatefulSets struct{}

func (fakeStatefulSets) List(context.Context, metav1.ListOptions) (*appsv1.StatefulSetList, error) {
	return &appsv1.StatefulSetList{}, nil
}
func (fakeStatefulSets) Get(context.Context, string, metav1.GetOptions) (*appsv1.StatefulSet, error) {
	return nil, nil
}
func (fakeStatefulSets) Update(_ context.Context, s *appsv1.StatefulSet, _ metav1.UpdateOptions) (*appsv1.StatefulSet, error) {
	return s, nil
}
func (fakeStatefulSets) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions) (*appsv1.StatefulSet, error) {
	return nil, nil
}

func newFakeREST(t *testing.T, routes map[string]string) rest.Interface {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	rc, err := rest.RESTClientFor(&rest.Config{
		Host: srv.URL,
		QPS:  -1, // no client-side rate limiting
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestGenerateReport_KafkaAPIVersion(t *testing.T) {
	routes := map[string]string{
		"/apis/kafka.strimzi.io":                             `{"versions":[{"version":"v1beta2"}]}`,
		"/apis/kafka.strimzi.io/v1beta2/namespaces/a/kafkas": `{"items":[{"metadata":{"name":"k1"}},{"metadata":{"name":"k2"}}]}`,
		"/apis/acid.zalan.do/v1/namespaces/a/postgresqls":    `{"items":[{"metadata":{"name":"pg"}}]}`,
	}

	// Auto-discovery falls back to v1beta2 when v1 is not served.
	r := NewReporter(&fakeReporterClient{rc: newFakeREST(t, routes)})
	report, err := r.GenerateReport(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Components.Kafka != 2 || report.Components.Postgresql != 1 {
		t.Errorf("unexpected components: %+v", report.Components)
	}

	// An explicitly configured version is honored.
	routes["/apis/kafka.strimzi.io"] = `{"versions":[{"version":"v1"},{"version":"v1beta2"}]}`
	r = NewReporter(&fakeReporterClient{rc: newFakeREST(t, routes)}, WithKafkaAPIVersion("kafka.strimzi.io/v1beta2"))
	report, err = r.GenerateReport(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Components.Kafka != 2 {
		t.Errorf("expected 2 kafkas via configured v1beta2, got %d", report.Components.Kafka)
	}
}

func TestGenerateReport_BoundedParallelism(t *testing.T) {
	client := &fakeReporterClient{rc: newFakeREST(t, nil), podDelay: 20 * time.Millisecond}
	r := NewReporter(client, WithParallelism(2))
	namespaces := []string{"a", "b", "c", "d", "e", "f"}
	if _, err := r.GenerateReport(context.Background(), namespaces); err != nil {
		t.Fatal(err)
	}
	if client.maxInFlight > 2 {
		t.Errorf("max concurrent namespaces = %d, want <= 2", client.maxInFlight)
	}
}

func TestGetNodeStatus(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want string
	}{
		{
			name: "ready node",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}}},
			want: "Ready",
		},
		{
			name: "not ready node",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			}}},
			want: "NotReady",
		},
		{
			name: "no ready condition",
			node: &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			}}},
			want: "Unknown",
		},
		{
			name: "no conditions",
			node: &corev1.Node{},
			want: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getNodeStatus(tt.node); got != tt.want {
				t.Errorf("getNodeStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready pod",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}}},
			want: true,
		},
		{
			name: "not ready pod",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			}}},
			want: false,
		},
		{
			name: "no ready condition",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			}}},
			want: false,
		},
		{
			name: "no conditions",
			pod:  &corev1.Pod{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodReady(tt.pod); got != tt.want {
				t.Errorf("isPodReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetTotalRestarts(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want int32
	}{
		{
			name: "no containers",
			pod:  &corev1.Pod{},
			want: 0,
		},
		{
			name: "single container",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 3},
			}}},
			want: 3,
		},
		{
			name: "multiple containers sum",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 2},
				{RestartCount: 5},
			}}},
			want: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getTotalRestarts(tt.pod); got != tt.want {
				t.Errorf("getTotalRestarts() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNewReporter_Options(t *testing.T) {
	l := logger.NewLogger(false)
	r := NewReporter(&fakeReporterClient{}, WithLogger(l), WithParallelism(3), WithKafkaAPIVersion("kafka.strimzi.io/v1beta2"))
	if r.log != l {
		t.Errorf("WithLogger must set the logger so warnings follow the root output format")
	}
	if r.parallelism != 3 || r.kafkaAPIVersion != "kafka.strimzi.io/v1beta2" {
		t.Errorf("options not applied: parallelism=%d kafka=%q", r.parallelism, r.kafkaAPIVersion)
	}
	if NewReporter(&fakeReporterClient{}, WithLogger(nil)).log == nil {
		t.Errorf("WithLogger(nil) must keep the default logger")
	}
}

func TestGenerateReport_WorkerPoolNotGoroutinePerNamespace(t *testing.T) {
	client := &fakeReporterClient{rc: newFakeREST(t, nil), podDelay: time.Millisecond}
	r := NewReporter(client, WithParallelism(2))
	namespaces := make([]string, 200)
	for i := range namespaces {
		namespaces[i] = fmt.Sprintf("ns-%d", i)
	}
	base := runtime.NumGoroutine()
	if _, err := r.GenerateReport(context.Background(), namespaces); err != nil {
		t.Fatal(err)
	}
	if extra := client.maxGoroutines - base; extra > 20 {
		t.Errorf("%d extra goroutines for 200 namespaces with parallelism 2: not a bounded worker pool", extra)
	}
}

func TestGenerateReport_IncludesNodesCordonedByThisRun(t *testing.T) {
	client := &fakeReporterClient{rc: newFakeREST(t, nil), nodes: []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "emptied"}, Spec: corev1.NodeSpec{Unschedulable: true}},
		{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}},
	}}
	r := NewReporter(client, WithCordonedNodes(func() []string { return []string{"emptied"} }))
	report, err := r.GenerateReport(context.Background(), []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Nodes) != 1 || report.Nodes[0].Name != "emptied" || !report.Nodes[0].CordonedByThisRun || !report.Nodes[0].Unschedulable {
		t.Fatalf("nodes = %+v, want only the emptied node cordoned by this run", report.Nodes)
	}
}

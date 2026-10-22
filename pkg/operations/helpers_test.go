package operations

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
)

func podWith(containers, initContainers []string) *corev1.Pod {
	pod := &corev1.Pod{}
	for _, img := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Image: img})
	}
	for _, img := range initContainers {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Image: img})
	}
	return pod
}

func TestPodMatchesAnyImage(t *testing.T) {
	const proxy127x = "registry.example.com/mirror/istio/proxyv2:1.27.1-distroless"

	tests := []struct {
		name       string
		pod        *corev1.Pod
		substrings []string
		want       bool
	}{
		{
			name:       "no filter matches everything",
			pod:        podWith([]string{"nginx:1.0"}, nil),
			substrings: nil,
			want:       true,
		},
		{
			name:       "matches sidecar by tag substring",
			pod:        podWith([]string{"app:v2", proxy127x}, nil),
			substrings: []string{"proxyv2:1.27.1"},
			want:       true,
		},
		{
			name:       "does not match a different tag",
			pod:        podWith([]string{"app:v2", proxy127x}, nil),
			substrings: []string{"proxyv2:1.27.9"},
			want:       false,
		},
		{
			name:       "matches in init containers",
			pod:        podWith([]string{"app:v2"}, []string{"istio/proxyv2:1.27.1-distroless"}),
			substrings: []string{"proxyv2:1.27.1"},
			want:       true,
		},
		{
			name:       "OR across multiple substrings",
			pod:        podWith([]string{proxy127x}, nil),
			substrings: []string{"proxyv2:1.27.9", "proxyv2:1.27.1"},
			want:       true,
		},
		{
			name:       "no container matches",
			pod:        podWith([]string{"nginx:1.0", "redis:7"}, nil),
			substrings: []string{"proxyv2"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodMatchesAnyImage(tt.pod, tt.substrings); got != tt.want {
				t.Errorf("PodMatchesAnyImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodMatchesAnyImageIgnoresEmpty(t *testing.T) {
	pod := podWith([]string{"nginx:1.0"}, nil)
	if PodMatchesAnyImage(pod, []string{"", "redis"}) {
		t.Errorf("empty substring must not match every image")
	}
	if !PodMatchesAnyImage(pod, []string{""}) {
		t.Errorf("only empty substrings means the filter is inactive")
	}
}

func TestMatchesRequirementsKeyPresence(t *testing.T) {
	tests := []struct {
		name     string
		actual   map[string]string
		required map[string]string
		want     bool
	}{
		{name: "empty value requires key presence (missing)", actual: map[string]string{"a": "1"}, required: map[string]string{"role": ""}, want: false},
		{name: "empty value matches present empty key", actual: map[string]string{"role": ""}, required: map[string]string{"role": ""}, want: true},
		{name: "empty value does not match non-empty value", actual: map[string]string{"role": "x"}, required: map[string]string{"role": ""}, want: false},
		{name: "nil actual with requirement", actual: nil, required: map[string]string{"a": "1"}, want: false},
		{name: "no requirements", actual: nil, required: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchesRequirements(tt.actual, tt.required); got != tt.want {
				t.Errorf("MatchesRequirements() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseLabelsOrAnnotations(t *testing.T) {
	got := ParseLabelsOrAnnotations([]string{"a=1", "foo", "b=", "=x", "c=d=e"})
	want := map[string]string{"a": "1", "b": "", "c": "d=e"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("key %q = %q (present %v), want %q", k, gv, ok, v)
		}
	}

	if _, err := ParseLabelsOrAnnotationsStrict([]string{"a=1", "b="}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	for _, bad := range []string{"foo", "=x"} {
		if _, err := ParseLabelsOrAnnotationsStrict([]string{"a=1", bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestHasPodFiltersUsesParsedValues(t *testing.T) {
	d := &DeploymentOperations{podLabels: []string{"foo"}, containerImages: []string{""}}
	if d.hasPodFilters() {
		t.Errorf("malformed/empty filters must not count as active")
	}
	d = &DeploymentOperations{podLabels: []string{"role="}}
	if !d.hasPodFilters() {
		t.Errorf("key= is an active filter")
	}
	s := &StatefulSetOperations{podAnnotations: []string{"foo"}}
	if s.hasPodFilters() {
		t.Errorf("malformed statefulset filters must not count as active")
	}
}

func TestIsNotFoundErrTyped(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", apierrors.NewNotFound(schema.GroupResource{Group: "kafka.strimzi.io", Resource: "kafkas"}, "x"))
	if !isNotFoundErr(err) {
		t.Errorf("typed NotFound must be recognised")
	}
	if isNotFoundErr(apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", fmt.Errorf("denied"))) {
		t.Errorf("forbidden is not NotFound")
	}
}

func TestForEachBoundedLimitsConcurrency(t *testing.T) {
	var cur, peak int
	var mu sync.Mutex
	visited := make([]bool, 20)
	forEachBounded(3, len(visited), func(i int) {
		mu.Lock()
		cur++
		if cur > peak {
			peak = cur
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		cur--
		visited[i] = true
		mu.Unlock()
	})
	if peak > 3 {
		t.Errorf("peak concurrency %d exceeds limit 3", peak)
	}
	for i, v := range visited {
		if !v {
			t.Errorf("index %d not visited", i)
		}
	}
}

// R6 regression: a target that was ready in an earlier pass but regressed must
// not count as ready just because another target became ready later.
func TestWaitForRollouts_RequiresAllReadyInOnePass(t *testing.T) {
	targets := []rolloutTarget{{namespace: "ns", name: "a"}, {namespace: "ns", name: "b"}}
	var mu sync.Mutex
	calls := map[string]int{}
	check := func(_ context.Context, tg rolloutTarget) (bool, string, error, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[tg.name]++
		n := calls[tg.name]
		if tg.name == "a" { // ready on the first pass only, then regresses
			return n == 1, "pod crashed", nil, nil
		}
		return n >= 2, "starting", nil, nil // b becomes ready from the second pass on
	}
	err := waitForRollouts(context.Background(), logger.NewLogger(false), "deployment", 200*time.Millisecond, 5*time.Millisecond, targets, check, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "ns/a (pod crashed)") {
		t.Fatalf("expected timeout naming ns/a, got %v", err)
	}
	if strings.Contains(err.Error(), "ns/b (") {
		t.Errorf("ns/b was ready in the last pass and must not be reported as not ready: %v", err)
	}
}

func TestWaitForRollouts_SucceedsWhenAllReadyTogether(t *testing.T) {
	targets := []rolloutTarget{{namespace: "ns", name: "a"}, {namespace: "ns", name: "b"}}
	var mu sync.Mutex
	calls := map[string]int{}
	check := func(_ context.Context, tg rolloutTarget) (bool, string, error, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[tg.name]++
		n := calls[tg.name]
		if tg.name == "a" { // ready, not ready, ready again
			return n != 2, "flapping", nil, nil
		}
		return n >= 3, "starting", nil, nil
	}
	err := waitForRollouts(context.Background(), logger.NewLogger(false), "deployment", 5*time.Second, 5*time.Millisecond, targets, check, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls["a"] != 3 || calls["b"] != 3 {
		t.Errorf("expected success on the third pass, calls = %v", calls)
	}
}

func TestWaitForRollouts_TerminalFailureDoesNotHideOthers(t *testing.T) {
	targets := []rolloutTarget{{namespace: "ns", name: "bad"}, {namespace: "ns", name: "good"}}
	check := func(_ context.Context, tg rolloutTarget) (bool, string, error, error) {
		if tg.name == "bad" {
			return false, "", fmt.Errorf("progress deadline exceeded"), nil
		}
		return true, "", nil, nil
	}
	err := waitForRollouts(context.Background(), logger.NewLogger(false), "deployment", 5*time.Second, 5*time.Millisecond, targets, check, nil)
	if err == nil || !strings.Contains(err.Error(), "ns/bad") || strings.Contains(err.Error(), "ns/good") {
		t.Fatalf("expected only the ns/bad failure, got %v", err)
	}
}

// #21: waiters treat only typed NotFound as "workload deleted"; an error whose
// text merely contains "not found" is not terminal: it is polled again (and,
// like every API error while waiting, reported and trips the run).
func TestWaitersUseTypedNotFoundOnly(t *testing.T) {
	client := newFakeClient(readyDeployment("ns", "web", time.Hour))
	var mu sync.Mutex
	gets := 0
	client.cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		if gets == 1 {
			return true, nil, fmt.Errorf("proxy: upstream not found")
		}
		return false, nil, nil
	})
	d := newTestDeploymentOps(client, nil)
	// The untyped error is not terminal: polling goes on until the rollout
	// is ready. Like every API error while waiting it still trips the run
	// at once and fails the wait.
	rs := NewRunStop()
	err := d.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "web", minGeneration: 1}}, rs.Trip)
	if err == nil || strings.Contains(err.Error(), "no longer exists") || !strings.Contains(err.Error(), "completed nevertheless") {
		t.Fatalf("untyped 'not found' must be retried until ready and then reported, got %v", err)
	}
	if !rs.Stopped() {
		t.Error("an API error while waiting must trip the run-wide stop")
	}
	mu.Lock()
	if gets < 2 {
		t.Errorf("the wait must keep polling after the API error, gets=%d", gets)
	}
	mu.Unlock()

	d = newTestDeploymentOps(newFakeClient(), nil)
	err = d.waitForDeploymentsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "gone", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("typed NotFound must be terminal, got %v", err)
	}

	s := newTestStatefulSetOps(newFakeClient(), nil)
	err = s.waitForStatefulSetsReady(context.Background(), []rolloutTarget{{namespace: "ns", name: "gone", minGeneration: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("typed NotFound must be terminal for StatefulSets, got %v", err)
	}
}

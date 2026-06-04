package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newDeployment builds a Deployment whose "version" label we use to observe
// which copy (server vs cached) a Get returns.
func newDeployment(ns, name, version string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{"version": version},
		},
	}
}

func newStatefulSet(ns, name, version string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{"version": version},
		},
	}
}

// fastCacheConfig makes the rate limiter effectively unlimited so tests never
// block on it; only the TTL matters for these tests.
func fastCacheConfig(ttl time.Duration) CacheConfig {
	return CacheConfig{RateLimit: 100000, Burst: 100000, CacheTTL: ttl}
}

// TestCacheManager_ServesStaleWithinTTL documents the cache behavior: within
// the TTL, GetDeployment returns the previously cached object even after the
// object changed on the server. This is exactly why status polling must NOT go
// through the cache.
func TestCacheManager_ServesStaleWithinTTL(t *testing.T) {
	const ns, name = "default", "web"
	clientset := fake.NewSimpleClientset(newDeployment(ns, name, "1"))
	cm := NewCacheManager(clientset, fastCacheConfig(time.Hour))
	ctx := context.Background()

	got, err := cm.GetDeployment(ctx, ns, name)
	if err != nil {
		t.Fatalf("first GetDeployment: %v", err)
	}
	if got.Labels["version"] != "1" {
		t.Fatalf("want version 1, got %q", got.Labels["version"])
	}

	// The object changes on the server.
	if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, newDeployment(ns, name, "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Within the TTL the cache keeps serving the old version.
	got, err = cm.GetDeployment(ctx, ns, name)
	if err != nil {
		t.Fatalf("second GetDeployment: %v", err)
	}
	if got.Labels["version"] != "1" {
		t.Fatalf("cache should serve stale version 1 within TTL, got %q", got.Labels["version"])
	}
}

// TestCacheManager_RefetchesAfterTTL shows the cache refreshes once the TTL
// elapses.
func TestCacheManager_RefetchesAfterTTL(t *testing.T) {
	const ns, name = "default", "web"
	clientset := fake.NewSimpleClientset(newDeployment(ns, name, "1"))
	cm := NewCacheManager(clientset, fastCacheConfig(20*time.Millisecond))
	ctx := context.Background()

	if _, err := cm.GetDeployment(ctx, ns, name); err != nil {
		t.Fatalf("first GetDeployment: %v", err)
	}
	if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, newDeployment(ns, name, "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	time.Sleep(40 * time.Millisecond) // outlive the TTL

	got, err := cm.GetDeployment(ctx, ns, name)
	if err != nil {
		t.Fatalf("GetDeployment after TTL: %v", err)
	}
	if got.Labels["version"] != "2" {
		t.Fatalf("after TTL expiry want version 2, got %q", got.Labels["version"])
	}
}

// TestCacheManager_InvalidateRefetches shows an explicit invalidation forces a
// fresh read on the next Get.
func TestCacheManager_InvalidateRefetches(t *testing.T) {
	const ns, name = "default", "web"
	clientset := fake.NewSimpleClientset(newDeployment(ns, name, "1"))
	cm := NewCacheManager(clientset, fastCacheConfig(time.Hour))
	ctx := context.Background()

	if _, err := cm.GetDeployment(ctx, ns, name); err != nil {
		t.Fatalf("first GetDeployment: %v", err)
	}
	if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, newDeployment(ns, name, "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	cm.InvalidateDeploymentCache(ns, name)

	got, err := cm.GetDeployment(ctx, ns, name)
	if err != nil {
		t.Fatalf("GetDeployment after invalidate: %v", err)
	}
	if got.Labels["version"] != "2" {
		t.Fatalf("after invalidation want version 2, got %q", got.Labels["version"])
	}
}

// TestDeploymentAdapter_GetReadsLive verifies the readiness path: the adapter's
// Get always reflects the current server state, even on back-to-back calls with
// no TTL wait. This is what makes restart verification correct.
func TestDeploymentAdapter_GetReadsLive(t *testing.T) {
	const ns, name = "default", "web"
	clientset := fake.NewSimpleClientset(newDeployment(ns, name, "1"))
	adapter := &deploymentAdapter{deployments: clientset.AppsV1().Deployments(ns)}
	ctx := context.Background()

	got, err := adapter.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if got.Labels["version"] != "1" {
		t.Fatalf("want version 1, got %q", got.Labels["version"])
	}

	if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, newDeployment(ns, name, "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// No wait, no TTL: the readiness loop must see the change immediately.
	got, err = adapter.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got.Labels["version"] != "2" {
		t.Fatalf("readiness Get must read live; want version 2, got %q", got.Labels["version"])
	}
}

// TestStatefulSetAdapter_GetReadsLive mirrors the deployment case for the
// StatefulSet readiness path.
func TestStatefulSetAdapter_GetReadsLive(t *testing.T) {
	const ns, name = "default", "db"
	clientset := fake.NewSimpleClientset(newStatefulSet(ns, name, "1"))
	adapter := &statefulSetAdapter{statefulsets: clientset.AppsV1().StatefulSets(ns)}
	ctx := context.Background()

	got, err := adapter.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if got.Labels["version"] != "1" {
		t.Fatalf("want version 1, got %q", got.Labels["version"])
	}

	if _, err := clientset.AppsV1().StatefulSets(ns).Update(ctx, newStatefulSet(ns, name, "2"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err = adapter.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got.Labels["version"] != "2" {
		t.Fatalf("readiness Get must read live; want version 2, got %q", got.Labels["version"])
	}
}

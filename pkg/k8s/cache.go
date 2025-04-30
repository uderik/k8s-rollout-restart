package k8s

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CacheConfig contains cache configuration parameters
type CacheConfig struct {
	// RateLimit defines the maximum number of requests per second
	RateLimit float64
	// Burst defines the maximum number of simultaneous requests
	Burst int
	// CacheTTL defines the cache's lifetime
	CacheTTL time.Duration
}

// CacheManager manages caching of Kubernetes resources
type CacheManager struct {
	client     kubernetes.Interface
	limiter    *rate.Limiter
	cacheTTL   time.Duration
	cacheMutex sync.RWMutex

	// Cached data
	deploymentsCache  map[string]deploymentCacheEntry
	statefulSetsCache map[string]statefulSetCacheEntry
	nodesCache        map[string]nodeCacheEntry
	namespacesCache   map[string]namespaceCacheEntry
}

type cacheEntry struct {
	data      interface{}
	timestamp time.Time
}

type deploymentCacheEntry struct {
	cacheEntry
	namespace string
}

type statefulSetCacheEntry struct {
	cacheEntry
	namespace string
}

type nodeCacheEntry struct {
	cacheEntry
}

type namespaceCacheEntry struct {
	cacheEntry
}

// NewCacheManager creates a new CacheManager instance
func NewCacheManager(client kubernetes.Interface, config CacheConfig) *CacheManager {
	return &CacheManager{
		client:            client,
		limiter:           rate.NewLimiter(rate.Limit(config.RateLimit), config.Burst),
		cacheTTL:          config.CacheTTL,
		deploymentsCache:  make(map[string]deploymentCacheEntry),
		statefulSetsCache: make(map[string]statefulSetCacheEntry),
		nodesCache:        make(map[string]nodeCacheEntry),
		namespacesCache:   make(map[string]namespaceCacheEntry),
	}
}

// GetDeployment retrieves a Deployment from cache or Kubernetes API
func (cm *CacheManager) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	// Check cache
	cm.cacheMutex.RLock()
	if entry, exists := cm.deploymentsCache[name]; exists {
		if time.Since(entry.timestamp) < cm.cacheTTL {
			cm.cacheMutex.RUnlock()
			return entry.data.(*appsv1.Deployment), nil
		}
	}
	cm.cacheMutex.RUnlock()

	// Apply rate limiting
	if err := cm.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Get data from API
	deployment, err := cm.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Update cache
	cm.cacheMutex.Lock()
	cm.deploymentsCache[name] = deploymentCacheEntry{
		cacheEntry: cacheEntry{
			data:      deployment,
			timestamp: time.Now(),
		},
		namespace: namespace,
	}
	cm.cacheMutex.Unlock()

	return deployment, nil
}

// GetStatefulSet retrieves a StatefulSet from cache or Kubernetes API
func (cm *CacheManager) GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error) {
	// Check cache
	cm.cacheMutex.RLock()
	if entry, exists := cm.statefulSetsCache[name]; exists {
		if time.Since(entry.timestamp) < cm.cacheTTL {
			cm.cacheMutex.RUnlock()
			return entry.data.(*appsv1.StatefulSet), nil
		}
	}
	cm.cacheMutex.RUnlock()

	// Apply rate limiting
	if err := cm.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Get data from API
	statefulSet, err := cm.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Update cache
	cm.cacheMutex.Lock()
	cm.statefulSetsCache[name] = statefulSetCacheEntry{
		cacheEntry: cacheEntry{
			data:      statefulSet,
			timestamp: time.Now(),
		},
		namespace: namespace,
	}
	cm.cacheMutex.Unlock()

	return statefulSet, nil
}

// GetNode retrieves a Node from cache or Kubernetes API
func (cm *CacheManager) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	// Check cache
	cm.cacheMutex.RLock()
	if entry, exists := cm.nodesCache[name]; exists {
		if time.Since(entry.timestamp) < cm.cacheTTL {
			cm.cacheMutex.RUnlock()
			return entry.data.(*corev1.Node), nil
		}
	}
	cm.cacheMutex.RUnlock()

	// Apply rate limiting
	if err := cm.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Get data from API
	node, err := cm.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Update cache
	cm.cacheMutex.Lock()
	cm.nodesCache[name] = nodeCacheEntry{
		cacheEntry: cacheEntry{
			data:      node,
			timestamp: time.Now(),
		},
	}
	cm.cacheMutex.Unlock()

	return node, nil
}

// GetNamespace retrieves a Namespace from cache or Kubernetes API
func (cm *CacheManager) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	// Check cache
	cm.cacheMutex.RLock()
	if entry, exists := cm.namespacesCache[name]; exists {
		if time.Since(entry.timestamp) < cm.cacheTTL {
			cm.cacheMutex.RUnlock()
			return entry.data.(*corev1.Namespace), nil
		}
	}
	cm.cacheMutex.RUnlock()

	// Apply rate limiting
	if err := cm.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Get data from API
	namespace, err := cm.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Update cache
	cm.cacheMutex.Lock()
	cm.namespacesCache[name] = namespaceCacheEntry{
		cacheEntry: cacheEntry{
			data:      namespace,
			timestamp: time.Now(),
		},
	}
	cm.cacheMutex.Unlock()

	return namespace, nil
}

// InvalidateCache clears all cached data
func (cm *CacheManager) InvalidateCache() {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()

	cm.deploymentsCache = make(map[string]deploymentCacheEntry)
	cm.statefulSetsCache = make(map[string]statefulSetCacheEntry)
	cm.nodesCache = make(map[string]nodeCacheEntry)
	cm.namespacesCache = make(map[string]namespaceCacheEntry)
}

// InvalidateDeploymentCache clears cache for a specific Deployment
func (cm *CacheManager) InvalidateDeploymentCache(name string) {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()
	delete(cm.deploymentsCache, name)
}

// InvalidateStatefulSetCache clears cache for a specific StatefulSet
func (cm *CacheManager) InvalidateStatefulSetCache(name string) {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()
	delete(cm.statefulSetsCache, name)
}

// InvalidateNodeCache clears cache for a specific Node
func (cm *CacheManager) InvalidateNodeCache(name string) {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()
	delete(cm.nodesCache, name)
}

// InvalidateNamespaceCache clears cache for a specific Namespace
func (cm *CacheManager) InvalidateNamespaceCache(name string) {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()
	delete(cm.namespacesCache, name)
}

// ClearCache clears all cached data
func (cm *CacheManager) ClearCache() {
	cm.cacheMutex.Lock()
	defer cm.cacheMutex.Unlock()

	cm.deploymentsCache = make(map[string]deploymentCacheEntry)
	cm.statefulSetsCache = make(map[string]statefulSetCacheEntry)
	cm.nodesCache = make(map[string]nodeCacheEntry)
	cm.namespacesCache = make(map[string]namespaceCacheEntry)
}

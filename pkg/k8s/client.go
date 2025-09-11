package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/operations"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps Kubernetes client with additional functionality
type Client struct {
	clientset    kubernetes.Interface
	cacheManager *CacheManager
}

// NewClient creates a new Kubernetes client
func NewClient(context string, qps float32, burst int) (*Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}

	if context != "" {
		configOverrides.CurrentContext = context
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load Kubernetes config: %w", err)
	}

	// Set the QPS and Burst limits for the client
	config.QPS = qps
	config.Burst = burst

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	// Create CacheManager with default settings
	cacheConfig := CacheConfig{
		RateLimit: float64(qps),     // Use the same QPS
		Burst:     burst,            // Use the same Burst
		CacheTTL:  30 * time.Second, // Reduce cache TTL to 30 seconds
	}

	cacheManager := NewCacheManager(client, cacheConfig)

	return &Client{
		clientset:    client,
		cacheManager: cacheManager,
	}, nil
}

// GetDeployment retrieves Deployment through CacheManager
func (c *Client) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return c.cacheManager.GetDeployment(ctx, namespace, name)
}

// GetStatefulSet retrieves StatefulSet through CacheManager
func (c *Client) GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error) {
	return c.cacheManager.GetStatefulSet(ctx, namespace, name)
}

// GetNode retrieves Node through CacheManager
func (c *Client) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return c.cacheManager.GetNode(ctx, name)
}

// GetNamespace retrieves Namespace through CacheManager
func (c *Client) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	return c.cacheManager.GetNamespace(ctx, name)
}

// InvalidateCache clears the cache
func (c *Client) InvalidateCache() {
	c.cacheManager.InvalidateCache()
}

// ClearCache clears the client cache
func (c *Client) ClearCache() {
	if c.cacheManager != nil {
		c.cacheManager.InvalidateCache()
	}
}

// AsK8sClient returns a K8sClient interface implementation
func (c *Client) AsK8sClient() operations.K8sClient {
	return &k8sClientAdapter{
		client: c,
	}
}

// k8sClientAdapter adapts our Client to the operations.K8sClient interface
type k8sClientAdapter struct {
	client *Client
}

func (a *k8sClientAdapter) CoreV1() operations.CoreV1Interface {
	return &coreV1Adapter{
		coreV1:       a.client.clientset.CoreV1(),
		cacheManager: a.client.cacheManager,
	}
}

func (a *k8sClientAdapter) AppsV1() operations.AppsV1Interface {
	return &appsV1Adapter{
		appsV1:       a.client.clientset.AppsV1(),
		cacheManager: a.client.cacheManager,
	}
}

func (a *k8sClientAdapter) RESTClient() rest.Interface {
	return a.client.clientset.Discovery().RESTClient()
}

func (a *k8sClientAdapter) ClearCache() {
	a.client.cacheManager.InvalidateCache()
}

// coreV1Adapter adapts CoreV1Interface
type coreV1Adapter struct {
	coreV1       typedcorev1.CoreV1Interface
	cacheManager *CacheManager
}

func (a *coreV1Adapter) Pods(namespace string) operations.PodInterface {
	return &podAdapter{pods: a.coreV1.Pods(namespace)}
}

func (a *coreV1Adapter) Nodes() operations.NodeInterface {
	return &nodeAdapter{nodes: a.coreV1.Nodes()}
}

func (a *coreV1Adapter) Namespaces() operations.NamespaceInterface {
	return &namespaceAdapter{namespaces: a.coreV1.Namespaces()}
}

// podAdapter adapts PodInterface
type podAdapter struct {
	pods typedcorev1.PodInterface
}

func (a *podAdapter) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	return a.pods.List(ctx, opts)
}

// nodeAdapter adapts NodeInterface
type nodeAdapter struct {
	nodes typedcorev1.NodeInterface
}

func (a *nodeAdapter) List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error) {
	return a.nodes.List(ctx, opts)
}

func (a *nodeAdapter) Update(ctx context.Context, node *corev1.Node, opts metav1.UpdateOptions) (*corev1.Node, error) {
	return a.nodes.Update(ctx, node, opts)
}

// namespaceAdapter adapts NamespaceInterface
type namespaceAdapter struct {
	namespaces typedcorev1.NamespaceInterface
}

func (a *namespaceAdapter) List(ctx context.Context, opts metav1.ListOptions) (*corev1.NamespaceList, error) {
	return a.namespaces.List(ctx, opts)
}

// appsV1Adapter adapts AppsV1Interface
type appsV1Adapter struct {
	appsV1       typedappsv1.AppsV1Interface
	cacheManager *CacheManager
}

func (a *appsV1Adapter) Deployments(namespace string) operations.DeploymentInterface {
	return &deploymentAdapter{
		deployments:  a.appsV1.Deployments(namespace),
		cacheManager: a.cacheManager,
		namespace:    namespace,
	}
}

func (a *appsV1Adapter) StatefulSets(namespace string) operations.StatefulSetInterface {
	return &statefulSetAdapter{
		statefulsets: a.appsV1.StatefulSets(namespace),
		cacheManager: a.cacheManager,
		namespace:    namespace,
	}
}

func (a *appsV1Adapter) ReplicaSets(namespace string) operations.ReplicaSetInterface {
	return &replicaSetAdapter{
		replicasets: a.appsV1.ReplicaSets(namespace),
	}
}

// deploymentAdapter adapts DeploymentInterface
type deploymentAdapter struct {
	deployments  typedappsv1.DeploymentInterface
	cacheManager *CacheManager
	namespace    string
}

func (a *deploymentAdapter) List(ctx context.Context, opts metav1.ListOptions) (*appsv1.DeploymentList, error) {
	return a.deployments.List(ctx, opts)
}

func (a *deploymentAdapter) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error) {
	return a.cacheManager.GetDeployment(ctx, a.namespace, name)
}

func (a *deploymentAdapter) Update(ctx context.Context, deployment *appsv1.Deployment, opts metav1.UpdateOptions) (*appsv1.Deployment, error) {
	return a.deployments.Update(ctx, deployment, opts)
}

func (a *deploymentAdapter) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error) {
	return a.deployments.Patch(ctx, name, pt, data, opts)
}

// statefulSetAdapter adapts StatefulSetInterface
type statefulSetAdapter struct {
	statefulsets typedappsv1.StatefulSetInterface
	cacheManager *CacheManager
	namespace    string
}

func (a *statefulSetAdapter) List(ctx context.Context, opts metav1.ListOptions) (*appsv1.StatefulSetList, error) {
	return a.statefulsets.List(ctx, opts)
}

func (a *statefulSetAdapter) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.StatefulSet, error) {
	return a.cacheManager.GetStatefulSet(ctx, a.namespace, name)
}

func (a *statefulSetAdapter) Update(ctx context.Context, statefulset *appsv1.StatefulSet, opts metav1.UpdateOptions) (*appsv1.StatefulSet, error) {
	return a.statefulsets.Update(ctx, statefulset, opts)
}

func (a *statefulSetAdapter) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.StatefulSet, error) {
	return a.statefulsets.Patch(ctx, name, pt, data, opts)
}

// replicaSetAdapter adapts ReplicaSetInterface
type replicaSetAdapter struct {
	replicasets typedappsv1.ReplicaSetInterface
}

func (a *replicaSetAdapter) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.ReplicaSet, error) {
	return a.replicasets.Get(ctx, name, opts)
}

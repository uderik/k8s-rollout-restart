package k8s

import (
	"context"
	"fmt"

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

// Client represents a Kubernetes API client
type Client struct {
	clientset *kubernetes.Clientset
	config    *rest.Config
}

// NewClient creates a new Kubernetes client
func NewClient(context string) (*Client, error) {
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

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	return &Client{
		clientset: clientset,
		config:    config,
	}, nil
}

// Clientset returns the underlying Kubernetes clientset
func (c *Client) Clientset() *kubernetes.Clientset {
	return c.clientset
}

// RESTConfig returns the REST config
func (c *Client) RESTConfig() *rest.Config {
	return c.config
}

// AsK8sClient returns the Client as an operations.K8sClient interface
func (c *Client) AsK8sClient() operations.K8sClient {
	return &k8sClientAdapter{
		clientset: c.clientset,
	}
}

// k8sClientAdapter adapts a kubernetes.Clientset to the operations.K8sClient interface
type k8sClientAdapter struct {
	clientset *kubernetes.Clientset
}

func (a *k8sClientAdapter) CoreV1() operations.CoreV1Interface {
	return &coreV1Adapter{coreV1: a.clientset.CoreV1()}
}

func (a *k8sClientAdapter) AppsV1() operations.AppsV1Interface {
	return &appsV1Adapter{appsV1: a.clientset.AppsV1()}
}

func (a *k8sClientAdapter) RESTClient() rest.Interface {
	return a.clientset.RESTClient()
}

// coreV1Adapter adapts a CoreV1Client to the operations.CoreV1Interface
type coreV1Adapter struct {
	coreV1 typedcorev1.CoreV1Interface
}

func (a *coreV1Adapter) Pods(namespace string) operations.PodInterface {
	return &podAdapter{pods: a.coreV1.Pods(namespace)}
}

func (a *coreV1Adapter) Nodes() operations.NodeInterface {
	return &nodeAdapter{nodes: a.coreV1.Nodes()}
}

// podAdapter adapts a pod client to operations.PodInterface
type podAdapter struct {
	pods typedcorev1.PodInterface
}

func (a *podAdapter) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	return a.pods.List(ctx, opts)
}

// nodeAdapter adapts a node client to operations.NodeInterface
type nodeAdapter struct {
	nodes typedcorev1.NodeInterface
}

func (a *nodeAdapter) List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error) {
	return a.nodes.List(ctx, opts)
}

func (a *nodeAdapter) Update(ctx context.Context, node *corev1.Node, opts metav1.UpdateOptions) (*corev1.Node, error) {
	return a.nodes.Update(ctx, node, opts)
}

// appsV1Adapter adapts an AppsV1Client to operations.AppsV1Interface
type appsV1Adapter struct {
	appsV1 typedappsv1.AppsV1Interface
}

func (a *appsV1Adapter) Deployments(namespace string) operations.DeploymentInterface {
	return &deploymentAdapter{deployments: a.appsV1.Deployments(namespace)}
}

func (a *appsV1Adapter) StatefulSets(namespace string) operations.StatefulSetInterface {
	return &statefulSetAdapter{statefulsets: a.appsV1.StatefulSets(namespace)}
}

// deploymentAdapter adapts a deployment client to operations.DeploymentInterface
type deploymentAdapter struct {
	deployments typedappsv1.DeploymentInterface
}

func (a *deploymentAdapter) List(ctx context.Context, opts metav1.ListOptions) (*appsv1.DeploymentList, error) {
	return a.deployments.List(ctx, opts)
}

func (a *deploymentAdapter) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error) {
	return a.deployments.Get(ctx, name, opts)
}

func (a *deploymentAdapter) Update(ctx context.Context, deployment *appsv1.Deployment, opts metav1.UpdateOptions) (*appsv1.Deployment, error) {
	return a.deployments.Update(ctx, deployment, opts)
}

func (a *deploymentAdapter) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error) {
	return a.deployments.Patch(ctx, name, pt, data, opts)
}

// statefulSetAdapter adapts a statefulset client to operations.StatefulSetInterface
type statefulSetAdapter struct {
	statefulsets typedappsv1.StatefulSetInterface
}

func (a *statefulSetAdapter) List(ctx context.Context, opts metav1.ListOptions) (*appsv1.StatefulSetList, error) {
	return a.statefulsets.List(ctx, opts)
}

func (a *statefulSetAdapter) Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.StatefulSet, error) {
	return a.statefulsets.Get(ctx, name, opts)
}

func (a *statefulSetAdapter) Update(ctx context.Context, statefulset *appsv1.StatefulSet, opts metav1.UpdateOptions) (*appsv1.StatefulSet, error) {
	return a.statefulsets.Update(ctx, statefulset, opts)
}

func (a *statefulSetAdapter) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.StatefulSet, error) {
	return a.statefulsets.Patch(ctx, name, pt, data, opts)
}

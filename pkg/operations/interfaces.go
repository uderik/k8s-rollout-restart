package operations

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// KafkaOperator defines the interface for Kafka cluster operations
type KafkaOperator interface {
	// RestartKafkaClusters restarts all Kafka clusters in the given namespaces
	RestartKafkaClusters(ctx context.Context, namespaces []string) error
}

// DeploymentOperator defines the interface for deployment operations
type DeploymentOperator interface {
	// RestartDeployments restarts all deployments in the given namespaces
	RestartDeployments(ctx context.Context, namespaces []string) error
}

// StatefulSetOperator defines the interface for statefulset operations
type StatefulSetOperator interface {
	// RestartStatefulSets restarts all statefulsets in the given namespaces
	RestartStatefulSets(ctx context.Context, namespaces []string) error
}

// ClusterOperator defines the interface for cluster operations
type ClusterOperator interface {
	// CordonNodes cordons all nodes with pods from the namespaces
	CordonNodes(ctx context.Context, namespaces []string) error

	// UncordonNodes uncordons all nodes previously cordoned
	UncordonNodes(ctx context.Context, namespaces []string) error
}

// K8sClient defines the interface for Kubernetes client operations
type K8sClient interface {
	// CoreV1 returns the client for core resources
	CoreV1() CoreV1Interface

	// AppsV1 returns the client for apps resources
	AppsV1() AppsV1Interface

	// RESTClient returns the REST client
	RESTClient() rest.Interface
}

// CoreV1Interface defines the interface for core resources
type CoreV1Interface interface {
	// Pods returns the pod client for the given namespace
	Pods(namespace string) PodInterface

	// Nodes returns the node client
	Nodes() NodeInterface

	// Namespaces returns the namespace client
	Namespaces() NamespaceInterface
}

// AppsV1Interface defines the interface for apps resources
type AppsV1Interface interface {
	// Deployments returns the deployment client for the given namespace
	Deployments(namespace string) DeploymentInterface

	// StatefulSets returns the statefulset client for the given namespace
	StatefulSets(namespace string) StatefulSetInterface
}

// PodInterface defines operations on pods
type PodInterface interface {
	// List lists all pods in the given namespace
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)
}

// NodeInterface defines operations on nodes
type NodeInterface interface {
	// List lists all nodes
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error)

	// Update updates the given node
	Update(ctx context.Context, node *corev1.Node, opts metav1.UpdateOptions) (*corev1.Node, error)
}

// DeploymentInterface defines operations on deployments
type DeploymentInterface interface {
	// List lists all deployments in the given namespace
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.DeploymentList, error)

	// Get gets the deployment with the specified name
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error)

	// Update updates the given deployment
	Update(ctx context.Context, deployment *appsv1.Deployment, opts metav1.UpdateOptions) (*appsv1.Deployment, error)

	// Patch applies the patch to the deployment with the given name
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error)
}

// StatefulSetInterface defines operations on statefulsets
type StatefulSetInterface interface {
	// List lists all statefulsets in the given namespace
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.StatefulSetList, error)

	// Get gets the statefulset with the specified name
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.StatefulSet, error)

	// Update updates the given statefulset
	Update(ctx context.Context, statefulset *appsv1.StatefulSet, opts metav1.UpdateOptions) (*appsv1.StatefulSet, error)

	// Patch applies the patch to the statefulset with the given name
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.StatefulSet, error)
}

// NamespaceInterface defines operations on namespaces
type NamespaceInterface interface {
	// List lists all namespaces
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NamespaceList, error)
}

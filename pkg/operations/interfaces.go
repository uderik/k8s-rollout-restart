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
	// GetDeploymentsToRestart returns a list of deployments that would be restarted
	GetDeploymentsToRestart(ctx context.Context, namespaces []string) ([]string, error)
}

// StatefulSetOperator defines the interface for statefulset operations
type StatefulSetOperator interface {
	// RestartStatefulSets restarts all statefulsets in the given namespaces
	RestartStatefulSets(ctx context.Context, namespaces []string) error
	// GetStatefulSetsToRestart returns a list of statefulsets that would be restarted
	GetStatefulSetsToRestart(ctx context.Context, namespaces []string) ([]string, error)
}

// ClusterOperator defines the interface for cluster operations
type ClusterOperator interface {
	// CordonNodes cordons all nodes with pods from the namespaces or all nodes if cordonAllNodes is true
	CordonNodes(ctx context.Context, namespaces []string, cordonAllNodes bool, nodeLabels []string, excludeLabels []string) error

	// UncordonNodes uncordons all nodes previously cordoned
	UncordonNodes(ctx context.Context, namespaces []string) error
}

// K8sClient defines the interface for Kubernetes client operations
type K8sClient interface {
	// CoreV1 returns interface for working with CoreV1 API
	CoreV1() CoreV1Interface
	// AppsV1 returns interface for working with AppsV1 API
	AppsV1() AppsV1Interface
	// RESTClient returns REST client
	RESTClient() rest.Interface
	// ClearCache clears the client cache
	ClearCache()
}

// CoreV1Interface is an interface for CoreV1 API operations
type CoreV1Interface interface {
	// Pods returns interface for working with pods
	Pods(namespace string) PodInterface
	// Nodes returns interface for working with nodes
	Nodes() NodeInterface
	// Namespaces returns interface for working with namespaces
	Namespaces() NamespaceInterface
}

// PodInterface is an interface for pod operations
type PodInterface interface {
	// List returns a list of pods
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)
}

// NodeInterface is an interface for node operations
type NodeInterface interface {
	// List returns a list of nodes
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error)
	// Update updates a node
	Update(ctx context.Context, node *corev1.Node, opts metav1.UpdateOptions) (*corev1.Node, error)
}

// NamespaceInterface is an interface for namespace operations
type NamespaceInterface interface {
	// List returns a list of namespaces
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NamespaceList, error)
}

// AppsV1Interface is an interface for AppsV1 API operations
type AppsV1Interface interface {
	// Deployments returns interface for working with deployments
	Deployments(namespace string) DeploymentInterface
	// StatefulSets returns interface for working with statefulsets
	StatefulSets(namespace string) StatefulSetInterface
	// ReplicaSets returns interface for working with replicasets
	ReplicaSets(namespace string) ReplicaSetInterface
}

// DeploymentInterface is an interface for deployment operations
type DeploymentInterface interface {
	// List returns a list of deployments
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.DeploymentList, error)
	// Get returns a deployment
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.Deployment, error)
	// Update updates a deployment
	Update(ctx context.Context, deployment *appsv1.Deployment, opts metav1.UpdateOptions) (*appsv1.Deployment, error)
	// Patch patches a deployment
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.Deployment, error)
}

// StatefulSetInterface is an interface for statefulset operations
type StatefulSetInterface interface {
	// List returns a list of statefulsets
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.StatefulSetList, error)
	// Get returns a statefulset
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.StatefulSet, error)
	// Update updates a statefulset
	Update(ctx context.Context, statefulset *appsv1.StatefulSet, opts metav1.UpdateOptions) (*appsv1.StatefulSet, error)
	// Patch patches a statefulset
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*appsv1.StatefulSet, error)
}

// ReplicaSetInterface is an interface for replicaset operations
type ReplicaSetInterface interface {
	// Get returns a replicaset
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*appsv1.ReplicaSet, error)
	// List returns a list of replicasets
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.ReplicaSetList, error)
}

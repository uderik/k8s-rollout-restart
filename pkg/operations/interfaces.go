package operations

import "context"

// KafkaOperator defines the interface for Kafka cluster operations
type KafkaOperator interface {
	// RestartKafkaClusters restarts all Kafka clusters in the given namespace
	RestartKafkaClusters(ctx context.Context, namespace string) error
}

// DeploymentOperator defines the interface for deployment operations
type DeploymentOperator interface {
	// RestartDeployments restarts all deployments in the given namespace
	RestartDeployments(ctx context.Context, namespace string) error
}

// ClusterOperator defines the interface for cluster operations
type ClusterOperator interface {
	// CordonNodes cordons all nodes with pods from the namespace
	CordonNodes(ctx context.Context, namespace string) error

	// UncordonNodes uncordons all nodes previously cordoned
	UncordonNodes(ctx context.Context, namespace string) error
}

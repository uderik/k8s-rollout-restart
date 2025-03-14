# k8s-rollout-restart

Kubernetes cluster maintenance automation utility

## Description

A Go console utility for automating Kubernetes cluster maintenance process, including:
- Temporary marking nodes as unschedulable (cordon)
- Restarting all components (Deployments, StatefulSets)
- Restarting Kafka clusters managed by Strimzi operator
- Verification of successful restart of all services
- Generating cluster state report

## Features

- **Namespace Filtering**: Target specific namespaces to limit the scope of operations
- **Dry Run Mode**: Preview all operations without making changes
- **Optional Node Cordoning**: Enable/disable node cordoning with the `--cordon` flag
- **Structured Logging**: Output logs in either human-readable format or structured JSON
- **Rollback Support**: Automatic rollback on errors or interruption
- **Plugin System**: Extensible architecture for supporting additional resources
- **Parallelism Control**: Configure the degree of parallel operations

## Requirements

- Go 1.21+
- Kubernetes cluster with Strimzi operator
- kubectl configured with cluster access

## Installation

```bash
# Install k8s-rollout-restart
go install github.com/k8s-rollout-restart@latest
```

## RBAC Requirements

The utility requires specific RBAC permissions to operate properly. Below are the minimum required permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-rollout-restart
rules:
- apiGroups: [""]
  resources: ["nodes", "pods"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["kafka.strimzi.io"]
  resources: ["kafkas"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["core.strimzi.io"]
  resources: ["strimzipodsets"]
  verbs: ["get", "list", "watch", "patch"]
```

You can apply this role to a specific service account using a RoleBinding (namespace-specific) or ClusterRoleBinding (cluster-wide):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: k8s-rollout-restart
subjects:
- kind: ServiceAccount
  name: k8s-rollout-restart
  namespace: default
roleRef:
  kind: ClusterRole
  name: k8s-rollout-restart
  apiGroup: rbac.authorization.k8s.io
```

## Usage

```bash
# Preview operations without execution
k8s-rollout-restart --dry-run

# Execute operations
k8s-rollout-restart --execute

# Use specific context
k8s-rollout-restart --execute --context=production-cluster

# Limit to namespace
k8s-rollout-restart --execute --namespace=app-namespace

# Configure parallel processing
k8s-rollout-restart --execute --parallel=10 --timeout=600

# Enable node cordoning
k8s-rollout-restart --execute --cordon

# Output logs in JSON format
k8s-rollout-restart --execute --output=json
```

## Kafka Clusters Restart

The utility restarts Kafka clusters managed by Strimzi operator using `strimzi.io/manual-rolling-update` annotations.
Supported components:
- Kafka broker (podset `<cluster_name>-kafka`)
- Zookeeper (podset `<cluster_name>-zookeeper`) 
- Kafka Connect (podset `<cluster_name>-connect`, if exists)
- Kafka Mirror Maker 2 (podset `<cluster_name>-mirrormaker2`, if exists)

## Flagger Deployments

The utility has special handling for deployments managed by [Flagger](https://flagger.app/), a progressive delivery tool for Kubernetes:

- By default, only primary deployments (with `-primary` suffix) that have Flagger Canary owner references will be restarted
- This ensures safe handling of canary deployments, by targeting only the stable production deployments
- The utility identifies Flagger-managed deployments by checking for owner references with:
  ```
  ownerReferences:
    - apiVersion: flagger.app/v1beta1
      kind: Canary
      controller: true
  ```
- Use the `--no-flagger-filter` flag to restart all deployments regardless of Flagger ownership

## Automatic Rollback

The utility includes an automatic rollback mechanism that will:
- Revert changes on error (e.g., uncordon nodes if an operation fails)
- Handle SIGINT/SIGTERM signals gracefully with proper cleanup
- Provide detailed logs of rollback operations

## Plugin System

The utility features an extensible plugin system for supporting additional Kubernetes resources:
- Standardized interface for adding new resource handlers
- Dynamic registration of plugins
- Support for different operation types (restart, cordon, etc.)

## Development

### Running Tests

```bash
# Run unit tests
go test ./...

# Run integration tests (requires a Kubernetes cluster)
go test -tags=integration ./...
```

### Adding New Plugins

To add support for a new resource type:

1. Implement the `ResourceHandler` interface
2. Register your handler with the `PluginManager`
3. Use the plugin in your main code

## Options

```
  -d, --dry-run               Preview operations only
  -e, --execute               Execute operations
  -c, --context string        Kubernetes context
  -n, --namespace string      Namespace
  -p, --parallel int          Parallelism degree (default: 5)
  -t, --timeout int           Timeout in seconds (default: 300)
  -o, --output string         Output format (text|json) (default: text)
  --no-flagger-filter         Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)
  --cordon                    Enable node cordoning
  -h, --help                  Help
``` 
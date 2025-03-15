# k8s-rollout-restart

Kubernetes cluster maintenance automation utility

## Description

A Go console utility for automating Kubernetes cluster maintenance process, including:
- Temporary marking nodes as unschedulable (cordon)
- Restarting all components (Deployments, StatefulSets)
- Restarting Kafka clusters managed by Strimzi operator
- Restarting PostgreSQL clusters managed by Zalando PostgreSQL Operator
- Verification of successful restart of all services
- Generating cluster state report

## Features

- **Namespace Filtering**: Target specific namespaces to limit the scope of operations
- **Dry Run Mode**: Preview all operations without making changes
- **Optional Node Cordoning**: Enable/disable node cordoning with the `--cordon` flag
- **Resource Type Selection**: Specify which resource types to restart (deployments, statefulsets, or all)
- **Structured Logging**: Output logs in either human-readable format or structured JSON
- **Rollback Support**: Automatic rollback on errors or interruption
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
  resources: ["nodes", "pods", "namespaces"]
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
- apiGroups: ["acid.zalan.do"]
  resources: ["postgresqls"]
  verbs: ["get", "list", "watch", "patch"]
```

> **Important**: The `"namespaces"` resource permission with `"list"` verb is required at the cluster scope when running in "all namespaces" mode, especially for PostgreSQL cluster operations.

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

> **Note**: For restricted environments, make sure the service account has at least access to list namespaces or ensure you explicitly specify target namespaces with the `--namespace` flag.

## Usage

```bash
# Preview operations (dry-run mode)
k8s-rollout-restart --dry-run

# Execute operations
k8s-rollout-restart --execute

# Use specific Kubernetes context
k8s-rollout-restart --execute --context=production-cluster

# Limit to namespace
k8s-rollout-restart --execute --namespace=app-namespace

# Process resources across all namespaces
k8s-rollout-restart --execute --all-namespaces

# Configure parallel processing
k8s-rollout-restart --execute --parallel=10 --timeout=600

# Enable node cordoning (only nodes with pods in target namespaces)
k8s-rollout-restart --execute --cordon

# Enable cordoning of all nodes in the cluster
k8s-rollout-restart --execute --cordon --cordon-all-nodes

# JSON output
k8s-rollout-restart --dry-run --output=json

# Restart specific resource types
k8s-rollout-restart --execute --resources=deployments
k8s-rollout-restart --execute --resources=statefulsets
k8s-rollout-restart --execute --resources=strimzi-kafka
k8s-rollout-restart --execute --resources=zalando-postgresql

# Restart multiple resource types
k8s-rollout-restart --execute --resources=deployments,statefulsets

# or restart all types of resources
k8s-rollout-restart --execute --resources=all

# Restart only resources older than 7 days
k8s-rollout-restart --execute --older-than=7d

# Restart only StatefulSets older than 24 hours
k8s-rollout-restart --execute --resources=statefulsets --older-than=24h
```

## Kafka Clusters Restart

The utility restarts Kafka clusters managed by Strimzi operator using `strimzi.io/manual-rolling-update` annotations.
Supported components:
- Kafka broker (podset `<cluster_name>-kafka`)
- Zookeeper (podset `<cluster_name>-zookeeper`) 
- Kafka Connect (podset `<cluster_name>-connect`, if exists)
- Kafka Mirror Maker 2 (podset `<cluster_name>-mirrormaker2`, if exists)

## PostgreSQL Clusters Restart

The utility has special handling for PostgreSQL clusters managed by [Zalando PostgreSQL Operator](https://github.com/zalando/postgres-operator/):

- StatefulSets managed by Postgres Operator are detected by specific labels and annotations
- Instead of directly restarting these StatefulSets, the utility updates the PostgreSQL custom resource
- This triggers a controlled, safe restart through the operator's own mechanisms
- The utility identifies Postgres Operator StatefulSets by checking for:
  - Label `application: spilo`
  - Label `cluster-name` containing PostgreSQL cluster name
  - Labels `team` and `version` when combined with `cluster-name`
- This ensures proper handling of database cluster restart without disrupting connections

## Flagger Deployments

The utility has special handling for deployments managed by [Flagger](https://flagger.app/), a progressive delivery tool for Kubernetes:

- By default, the utility does intelligent deployment selection:
  - For Flagger-managed deployments, only the primary deployments (with `-primary` suffix) will be restarted
  - For deployments without a Flagger primary counterpart, they will be restarted directly
  - Regular deployments that have a `-primary` counterpart will be skipped (as the primary is restarted instead)
- This ensures safe handling of canary deployments, by targeting only the stable production deployments
- The utility identifies Flagger-managed deployments by checking for owner references with:
  ```
  ownerReferences:
    - apiVersion: flagger.app/v1beta1
      kind: Canary
      controller: true
  ```
- Use the `--no-flagger-filter` flag to restart all deployments regardless of this logic
- Note: StatefulSets are always restarted regardless of any Flagger-related owner references

## Automatic Rollback

The utility includes an automatic rollback mechanism that will:
- Revert changes on error (e.g., uncordon nodes if an operation fails)
- Handle SIGINT/SIGTERM signals gracefully with proper cleanup
- Provide detailed logs of rollback operations

## Development

### Running Tests

```bash
# Run unit tests
go test ./...

# Run integration tests (requires a Kubernetes cluster)
go test -tags=integration ./...
```

## Options

```
  -d, --dry-run               Preview operations only
  -e, --execute               Execute operations
  -c, --context string        Kubernetes context
  -n, --namespace strings     Namespaces to target (comma-separated, empty for all namespaces)
  -A, --all-namespaces        Process resources across all namespaces
  -p, --parallel int          Parallelism degree (default: 5)
  -t, --timeout int           Timeout in seconds (default: 300)
  -o, --output string         Output format (text|json) (default: text)
  --resources strings         Resource types to restart (deployments, statefulsets, strimzi-kafka, zalando-postgresql, all) (default: deployments)
  --no-flagger-filter         Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)
  --cordon                    Enable node cordoning
  --cordon-all-nodes          Cordon all nodes in the cluster, not just those with pods from specified namespaces
  --older-than string         Restart only resources older than specified duration (e.g. 24h, 30m, 7d)
  --kube-api-qps float32      The maximum queries-per-second of requests sent to the Kubernetes API (default: 50)
  --kube-api-burst int        The maximum burst queries-per-second of requests sent to the Kubernetes API (default: 300)
  -h, --help                  Help
```

## Older Than Filter

The utility supports restarting only resources that are older than a specified duration:

- Use the `--older-than` flag to specify a minimum age for resources to be restarted
- Format supports various time units:
  - `h` for hours (e.g., `24h` for 24 hours)
  - `m` for minutes (e.g., `30m` for 30 minutes)
  - `d` for days (e.g., `7d` for 7 days)
- Resources newer than the specified duration will be skipped
- This is useful for avoiding restarts of recently deployed or updated resources
- Can be combined with other filters like namespace and resource type 
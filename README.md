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
k8s-rollout-restart --context=my-cluster --dry-run

# Execute operations
k8s-rollout-restart --context=my-cluster --execute

# Use specific Kubernetes context
k8s-rollout-restart --context=production-cluster --execute

# Limit to namespace
k8s-rollout-restart --context=my-cluster --execute --namespace=app-namespace

# Process resources across all namespaces
k8s-rollout-restart --context=my-cluster --execute --all-namespaces

# Ignore specific namespaces
k8s-rollout-restart --context=my-cluster --execute --ignore-namespaces=karpenter,kube-system

# Configure parallel processing
k8s-rollout-restart --context=my-cluster --execute --parallel=10 --timeout=600

# Enable node cordoning (only nodes with pods in target namespaces)
k8s-rollout-restart --context=my-cluster --execute --cordon

# Enable cordoning of all nodes in the cluster
k8s-rollout-restart --context=my-cluster --execute --cordon --cordon-all-nodes

# JSON output
k8s-rollout-restart --context=my-cluster --dry-run --output=json

# Restart specific resource types
k8s-rollout-restart --context=my-cluster --execute --resources=deployments
k8s-rollout-restart --context=my-cluster --execute --resources=statefulsets
k8s-rollout-restart --context=my-cluster --execute --resources=strimzi-kafka
k8s-rollout-restart --context=my-cluster --execute --resources=zalando-postgresql

# Restart multiple resource types
k8s-rollout-restart --context=my-cluster --execute --resources=deployments,statefulsets

# or restart all types of resources
k8s-rollout-restart --context=my-cluster --execute --resources=all

# Restart only resources older than 7 days
k8s-rollout-restart --context=my-cluster --execute --older-than=7d

# Restart only StatefulSets older than 24 hours
k8s-rollout-restart --context=my-cluster --execute --resources=statefulsets --older-than=24h
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

## Development

### Running Tests

```bash
# Run unit tests
go test ./...

# Run integration tests (requires a Kubernetes cluster)
go test -tags=integration ./...
```

## Options

| Flag | Description |
|------|-------------|
| `--config` | Config file (default is $HOME/.k8s-rollout-restart.yaml) |
| `--context`, `-c` | Kubernetes context (required) |
| `--dry-run`, `-d` | Preview operations without execution |
| `--execute`, `-e` | Execute operations |
| `--namespace`, `-n` | Kubernetes namespace(s). Multiple namespaces can be specified comma-separated |
| `--all-namespaces`, `-A` | Process resources across all namespaces |
| `--ignore-namespaces` | Namespaces to ignore. Multiple namespaces can be specified comma-separated (default [karpenter]) |
| `--parallel`, `-p` | Parallelism degree (default 5) |
| `--timeout`, `-t` | Timeout in seconds (default 300) |
| `--output`, `-o` | Output format (text\|json) (default "text") |
| `--no-flagger-filter` | Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones) |
| `--cordon` | Whether to cordon nodes before restart (if not set, nodes will not be cordoned) |
| `--cordon-all-nodes` | Cordon all nodes in the cluster, not just those with pods from specified namespaces |
| `--node-labels` | Only cordon nodes with these labels (format: key=value). Multiple labels can be specified comma-separated |
| `--exclude-node-labels` | Exclude nodes with these labels from cordon (format: key=value). Multiple labels can be specified comma-separated (default [eks.amazonaws.com/compute-type=fargate]) |
| `--resources` | Resource types to restart (deployments, statefulsets, strimzi-kafka, zalando-postgresql, all) (default [deployments]) |
| `--older-than` | Restart only resources older than specified duration (e.g. 24h, 30m, 7d) |
| `--pod-labels` | Only restart resources that have pods with these labels (format: key=value). Multiple labels can be specified comma-separated |
| `--pod-annotations` | Only restart resources that have pods with these annotations (format: key=value). Multiple annotations can be specified comma-separated |
| `--kube-api-qps` | The maximum queries-per-second of requests sent to the Kubernetes API (default 50) |
| `--kube-api-burst` | The maximum burst queries-per-second of requests sent to the Kubernetes API (default 300) |

### Examples

```bash
# Preview operations for a specific namespace
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --dry-run

# Execute operations for a specific namespace
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --execute

# Execute operations for all namespaces
./k8s-rollout-restart --context=my-cluster --all-namespaces --execute

# Execute operations with node cordoning
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --cordon --execute

# Execute operations with node cordoning and label filtering
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --cordon --node-labels=node-role.kubernetes.io/worker=true --execute

# Execute operations with node cordoning and excluding specific labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --cordon --exclude-node-labels=eks.amazonaws.com/compute-type=fargate,node-role.kubernetes.io/master=true --execute

# Restart only resources with pods having specific labels (e.g., Istio canary deployments)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --execute

# Restart only resources with pods having multiple labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp,version=v1.0 --execute

# Restart only resources with pods having specific labels and older than 1 hour
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=default --older-than=1h --execute

# Restart only resources with pods having specific annotations (e.g., Istio canary deployments)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --execute

# Restart only resources with pods having multiple annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary,deployment.kubernetes.io/revision=1 --execute

# Restart only resources with both specific labels and annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp --pod-annotations=istio.io/rev=canary --execute
```

## Pod Label Filter

The utility supports restarting only resources that have pods with specific labels:

- Use the `--pod-labels` flag to specify required pod labels (format: key=value)
- Multiple labels can be specified comma-separated (e.g., `app=myapp,version=v1.0`)
- Only resources with pods matching ALL specified labels will be restarted
- This is particularly useful for:
  - Istio canary deployments (`istio.io/rev=canary` or `istio.io/rev=default`)
  - A/B testing scenarios
  - Environment-specific filtering
  - Version-based filtering
- Can be combined with other filters like namespace, resource type, and age
- The utility will automatically find pods associated with each resource through:
  - Direct label selectors (`app=<resource-name>`)
  - Owner references (for ReplicaSets and StatefulSets)
  - Alternative label patterns

## Pod Annotation Filter

The utility supports restarting only resources that have pods with specific annotations:

- Use the `--pod-annotations` flag to specify required pod annotations (format: key=value)
- Multiple annotations can be specified comma-separated (e.g., `istio.io/rev=canary,deployment.kubernetes.io/revision=1`)
- Only resources with pods matching ALL specified annotations will be restarted
- This is particularly useful for:
  - Istio canary deployments (using annotations instead of labels)
  - Kubernetes deployment revisions (`deployment.kubernetes.io/revision`)
  - Prometheus monitoring annotations (`prometheus.io/scrape=true`)
  - Custom application annotations
  - StatefulSet specific annotations (`statefulset.kubernetes.io/pod-name`)
- Can be combined with other filters like namespace, resource type, age, and pod labels
- The utility will automatically find pods associated with each resource through:
  - Direct label selectors (`app=<resource-name>`)
  - Owner references (for ReplicaSets and StatefulSets)
  - Alternative label patterns
- Both `--pod-labels` and `--pod-annotations` can be used together (AND logic)

## Older Than Filter

The utility supports restarting only resources that are older than a specified duration:

- Use the `--older-than` flag to specify a minimum age for resources to be restarted
- Format supports various time units:
  - `h` for hours (e.g., `24h` for 24 hours)
  - `m` for minutes (e.g., `30m` for 30 minutes)
  - `d` for days (e.g., `7d` for 7 days)
- Resources newer than the specified duration will be skipped
- This is useful for avoiding restarts of recently deployed or updated resources
- Can be combined with other filters like namespace, resource type, and pod labels 
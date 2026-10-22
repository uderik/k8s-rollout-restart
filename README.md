# k8s-rollout-restart

Kubernetes cluster maintenance automation utility

## Description

A Go console utility for automating Kubernetes cluster maintenance, including:
- Optionally marking nodes as unschedulable (cordon) before the restart, so that restarted pods move to new nodes
- Rolling restart of Deployments and StatefulSets
- Restarting Kafka clusters managed by the Strimzi operator
- Restarting PostgreSQL clusters managed by the Zalando PostgreSQL Operator
- Waiting for restarted pods to become ready (can be skipped with `--skip-wait`)
- Generating a cluster state report before and after the maintenance

## Features

- **Namespace Selection**: Target specific namespaces (`-n`) or all namespaces (`-A`)
- **Dry Run Mode**: Preview operations without making changes
- **Optional Node Cordoning**: Enable node cordoning with the `--cordon` flag; cordoned nodes stay cordoned so that Karpenter can remove them (see [Node Cordoning](#node-cordoning))
- **Resource Type Selection**: Choose which resource types to restart
- **Pod Filters**: Restart only workloads whose pods have given labels, annotations or container images
- **Age Filter**: Restart only resources older than a given duration
- **Structured Logging**: Human-readable text or structured JSON logs

## Requirements

- Go 1.25+ (to build from source)
- Access to a Kubernetes cluster via a kubeconfig context
- Optional operators, only if the corresponding resource types are used: Strimzi (Kafka), Zalando Postgres Operator (PostgreSQL)

## Installation

```bash
go install github.com/uderik/k8s-rollout-restart@latest
```

## RBAC Requirements

The repository ships [`scripts/k8s-restricted-acces.sh`](scripts/k8s-restricted-acces.sh), which creates a ServiceAccount, the RBAC objects below and a kubeconfig with a short-lived token.

The permissions are split in two ClusterRoles:

**Cluster-scoped** (`k8s-rollout-restart-cluster`, always bound with a ClusterRoleBinding):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-rollout-restart-cluster
rules:
# Node cordon (PATCH spec.unschedulable + cordoned-by annotation) and node report
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "patch"]
# --all-namespaces discovery and namespace report
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list"]
# Local-PV warning before cordon: pods on the selected nodes are listed in ALL
# namespaces, not only the target ones (optional: without it the tool warns
# "could not check" and continues). Read-only.
- apiGroups: [""]
  resources: ["persistentvolumes", "pods"]
  verbs: ["list"]
```

**Workload verbs** (`k8s-rollout-restart-workloads`):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-rollout-restart-workloads
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "patch"]   # "patch" only needed for zalando-postgresql
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "patch"]
- apiGroups: ["apps"]
  resources: ["replicasets"]
  verbs: ["get", "list"]
- apiGroups: ["kafka.strimzi.io"]
  resources: ["kafkas"]
  verbs: ["get", "list"]
- apiGroups: ["core.strimzi.io"]
  resources: ["strimzipodsets"]
  verbs: ["get", "list", "patch"]
- apiGroups: ["acid.zalan.do"]
  resources: ["postgresqls"]
  verbs: ["get", "list", "patch"]
```

No `watch`, `update` or `delete` verbs are required.

The workload ClusterRole can be bound in two ways:

- **Cluster-wide** (default): a ClusterRoleBinding. Required for `-A`. Note that this grants the permissions in **every** namespace, including `kube-system`.
- **Per namespace**: a RoleBinding in each target namespace referencing the ClusterRole. The tool must then be run with `-n <those namespaces>`; `-A` would fail with 403 in other namespaces.

Re-running `create` reconciles the bindings: RoleBindings of the script (found by its label) in namespaces no longer listed in `TARGET_NAMESPACES` are deleted, switching between per-namespace and cluster-wide mode removes the bindings of the other mode, and the single cluster-wide ClusterRoleBinding `k8s-rollout-restart` created by older versions of the script is removed after verifying that it binds the ClusterRole `k8s-rollout-restart` to this ServiceAccount only (otherwise it is left alone with a warning; see `LEGACY_BINDING_NAME` / `LEGACY_ROLE_NAME`).

Legacy migration is fail-safe:

- Only a confirmed `NotFound` counts as "no legacy binding". Any other error while reading it (`Forbidden`, connection failure, ...) aborts the script with a non-zero exit code and the kubectl error, because skipping the migration would silently keep the old cluster-wide permissions.
- The legacy ClusterRole `k8s-rollout-restart` is deleted only if no other ClusterRoleBinding and no RoleBinding in any namespace references it. If it is still referenced, or if either list cannot be read, the role is kept and the script prints a warning (with the referencing bindings) instead.

```bash
# Cluster-wide binding (default)
./scripts/k8s-restricted-acces.sh create

# Per-namespace binding
TARGET_NAMESPACES=app1,app2 ./scripts/k8s-restricted-acces.sh create
# or
./scripts/k8s-restricted-acces.sh create app1,app2

# Without Zalando PostgreSQL support (drops postgresqls rule and pods "patch")
ENABLE_POSTGRESQL=false ./scripts/k8s-restricted-acces.sh create

# Request a specific token lifetime (the API server may shorten it; the actual expiry is printed)
TOKEN_DURATION=2h ./scripts/k8s-restricted-acces.sh create

# Remove everything the script created (RBAC, ServiceAccount, kubeconfig file)
./scripts/k8s-restricted-acces.sh delete
```

Other settings (`NAMESPACE`, `SERVICE_ACCOUNT_NAME`, `ROLE_PREFIX`, `KUBECONFIG_PATH`, `KUBECTL_CONTEXT`, `K8S_API_SERVER`) are described in `./scripts/k8s-restricted-acces.sh help`. The generated kubeconfig is written with mode `0600` and its context is named `restricted-context`:

```bash
export KUBECONFIG=./restricted-kubeconfig.yaml
k8s-rollout-restart --context=restricted-context -n app1 --dry-run
```

## Usage

One of `--dry-run` / `--execute` and one of `--namespace` / `--all-namespaces` must be specified.

```bash
# Preview operations (dry-run mode)
k8s-rollout-restart --context=my-cluster -n app-namespace --dry-run

# Execute operations in one or more namespaces
k8s-rollout-restart --context=my-cluster -n app-namespace --execute
k8s-rollout-restart --context=my-cluster -n app1,app2 --execute

# Process resources across all namespaces
k8s-rollout-restart --context=my-cluster -A --execute

# All namespaces except some (kube-system and kube-public are always skipped with -A)
k8s-rollout-restart --context=my-cluster -A --execute --ignore-namespaces=karpenter,monitoring

# Configure concurrency and the per-resource rollout wait
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --parallel=10 --timeout=600

# Enable node cordoning (only nodes with pods in target namespaces)
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --cordon

# Cordon all nodes in the cluster
k8s-rollout-restart --context=my-cluster -A --execute --cordon --cordon-all-nodes

# Cordon only nodes with a label, excluding others
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --cordon --node-labels=node-role.kubernetes.io/worker=true --exclude-node-labels=eks.amazonaws.com/compute-type=fargate

# JSON logs
k8s-rollout-restart --context=my-cluster -n app-namespace --dry-run --output=json

# Restart specific resource types
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --resources=deployments
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --resources=statefulsets
k8s-rollout-restart --context=my-cluster -n kafka --execute --resources=strimzi-kafka
k8s-rollout-restart --context=my-cluster -n databases --execute --resources=zalando-postgresql

# Multiple resource types, or all of them
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --resources=deployments,statefulsets
k8s-rollout-restart --context=my-cluster -A --execute --resources=all

# Restart only resources older than 7 days
k8s-rollout-restart --context=my-cluster -A --execute --older-than=7d

# Restart only StatefulSets older than 24 hours
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --resources=statefulsets --older-than=24h

# Restart only workloads whose pods run an old Istio proxy image
k8s-rollout-restart --context=my-cluster -A --execute --container-image=proxyv2:1.27.1

# Do not wait for pods to become ready after restart
k8s-rollout-restart --context=my-cluster -n app-namespace --execute --skip-wait

# Print version
k8s-rollout-restart --version
```

## Options

| Flag | Description |
|------|-------------|
| `--context`, `-c` | Kubernetes context (required unless `--version` is used) |
| `--dry-run`, `-d` | Preview operations without execution (mutually exclusive with `--execute`) |
| `--execute`, `-e` | Execute operations |
| `--namespace`, `-n` | Kubernetes namespace(s), comma-separated (mutually exclusive with `--all-namespaces`) |
| `--all-namespaces`, `-A` | Process all namespaces. `kube-system` and `kube-public` are always skipped |
| `--ignore-namespaces` | Namespaces to skip, comma-separated; only applies with `-A` (default `[karpenter]`) |
| `--parallel`, `-p` | At most N rollouts in progress at the same time across all namespaces (default 5, must be >= 1). Each resource (Deployment, StatefulSet, Kafka or PostgreSQL cluster) is patched and then waited on before the worker takes the next one. With `--skip-wait` nothing is waited on, so the limit only bounds concurrent restart PATCH requests, not rollouts running in the cluster. Also bounds the node cordon workers. See [Failure handling](#failure-handling) |
| `--timeout`, `-t` | Per-resource rollout wait in seconds: how long the tool waits for one restarted resource to become ready (default 300, must be >= 1). It does not limit individual API requests, which have a fixed 60s client timeout each |
| `--output`, `-o` | Log format: `text` or `json` (default `text`). With `json`, stdout contains only JSON lines (one object per line, including operation and report output) |
| `--resources` | Resource types: `deployments`, `statefulsets`, `strimzi-kafka`, `zalando-postgresql`, `all` (default `[deployments]`). `elasticsearch` is rejected: Elasticsearch support was removed |
| `--older-than` | Restart only resources older than the given duration, e.g. `30m`, `24h`, `7d` (must be > 0) |
| `--pod-labels` | Only restart resources that have a pod with all these labels (`key=value`, comma-separated) |
| `--pod-annotations` | Only restart resources that have a pod with all these annotations (`key=value`, comma-separated) |
| `--container-image` | Only restart resources that have a pod running a container whose image contains one of these substrings (comma-separated, OR) |
| `--no-flagger-filter` | Disable the Flagger filter (restart all deployments, not just Flagger primaries) |
| `--cordon` | Cordon nodes before the restart. Nodes are **not** uncordoned afterwards (see [Node Cordoning](#node-cordoning)) |
| `--cordon-all-nodes` | Cordon all nodes in the cluster, not just those with pods from the target namespaces |
| `--node-labels` | Only cordon nodes with these labels (`key=value`, comma-separated) |
| `--exclude-node-labels` | Never cordon nodes with these labels (`key=value`, comma-separated; default `[eks.amazonaws.com/compute-type=fargate]`) |
| `--skip-wait` | Do not wait for pods to become ready after restart |
| `--strimzi-kafka-api` | Strimzi Kafka CR API group/version |
| `--strimzi-podset-api` | Strimzi StrimziPodSet API group/version |
| `--kube-api-qps` | QPS for the Kubernetes API client (default 20, must be > 0) |
| `--kube-api-burst` | Burst for the Kubernetes API client (default 40, must be >= 1) |
| `--version`, `-v` | Show version information |

### Validation

- `--pod-labels`, `--pod-annotations`, `--node-labels` and `--exclude-node-labels` require `key=value` entries. `key=` is allowed and matches only an empty value of an existing key.
- Pod filters (`--pod-labels`, `--pod-annotations`, `--container-image`) work only for `deployments` and `statefulsets` and cannot be combined with operator-managed resource types (`strimzi-kafka`, `zalando-postgresql`, `all`).
- Filter flags that are given explicitly must not be empty: e.g. `--pod-labels=`, `--container-image=` or an empty item in a comma-separated list is rejected instead of silently matching everything.
- `--resources=elasticsearch` fails with `elasticsearch support was removed`; `all` does not include Elasticsearch. ECK-managed StatefulSets are still skipped by the `statefulsets` restart.
- `--older-than` must be a positive duration.
- `--output` accepts only `text` or `json`.

### Timeouts

- Every Kubernetes API request has a fixed client timeout of 60 seconds, independent of `--timeout`.
- `--timeout` is the rollout wait per resource: after a resource has been patched, the tool waits up to `--timeout` seconds for its pods to be replaced and ready. It is not a budget for the whole run.

### Failure handling

**Plan before mutate.** Before anything is changed, the tool builds the restart plan of every selected resource type: API discovery (Strimzi, Zalando) and target selection in every namespace, read-only. `--dry-run` prints exactly this plan (`Restart plan:`), and an execute run restarts exactly the planned targets, so the preview and the run always agree. If planning fails anywhere — e.g. listing one namespace is forbidden with `-n bad,good`, the Kafka API or the StrimziPodSet API cannot be discovered (403, 5xx; also when there is no Kafka cluster), or the PostgreSQL API cannot be discovered — nothing is cordoned and nothing is patched: every planned target of every resource type is reported as **not started due to earlier failure** and the tool exits with a non-zero exit code. A StrimziPodSet API that is simply not installed only matters when there is a Kafka cluster to restart. The initial cluster report is informational; its lookup errors are only warnings.

**Run-wide stop.** If anything fails during the run — a failed cordon, a restart PATCH that is rejected or has an unknown outcome (connection error, 5xx), a rollout that fails or times out, or an API error while waiting for a rollout (e.g. a forbidden pod List) — the tool stops starting new restarts immediately, for the whole run and across all resource types: a Deployment failure also prevents StatefulSet, Kafka and PostgreSQL restarts and vice versa. A timed-out rollout may still be running in the cluster, so its `--parallel` slot is not given to a new target. Restarts already in flight are allowed to finish (each within its own `--timeout`); a restart whose wait saw an API error is still waited for, and the error is reported even if the restart completes afterwards. A PostgreSQL marker PATCH that fails on any pod stops new launches at once; no further pod of that cluster is marked and the markers already set are removed. Every remaining target — including all targets of resource types that come later in the run — is reported as **not started due to earlier failure**, and the tool exits with a non-zero exit code.

With `--skip-wait` rollouts are not observed, so only PATCH results count, but the rule is the same: any failed PATCH (a definite rejection such as 403 or an ambiguous outcome) stops new launches of every resource type. A PostgreSQL trigger whose outcome is unknown is reported as a failure even if the subsequent wait shows the rotation completed.

### Dry run

`--dry-run` makes no changes. It prints the restart plan — the concrete Deployments, StatefulSets, `Kafka` and `postgresql` resources that would be restarted, built by the same planning step as an execute run — and the nodes that would be cordoned. If planning fails, the dry run fails as well, just like an execute run would.

## Kafka Clusters Restart

The utility restarts Kafka clusters managed by the Strimzi operator by annotating their StrimziPodSets with `strimzi.io/manual-rolling-update=true`; the operator then performs the rolling restart. The Strimzi API versions can be overridden with `--strimzi-kafka-api` and `--strimzi-podset-api`.

The restart is complete when every pod that existed before has been replaced, the number of pods equals the desired count from the StrimziPodSets (`spec.pods`), every one of them is Running and Ready, the annotation is gone from the PodSets and the `Kafka` resource is `Ready`. An API error while polling stops new launches at once; the cluster is still waited for and reported as failed even if its restart completes. If annotating one PodSet fails, the PodSets that were (or may have been) annotated are still waited for before the error is reported.

## PostgreSQL Clusters Restart

The utility has special handling for PostgreSQL clusters managed by the [Zalando PostgreSQL Operator](https://github.com/zalando/postgres-operator/):

- StatefulSets managed by the Postgres Operator are detected by their labels (`application: spilo`, `cluster-name`, ...) and skipped by the `statefulsets` restart
- With `--resources=zalando-postgresql` the restart is triggered through the operator (via the `postgresql` custom resource and its pods) instead of restarting the StatefulSet directly

How a cluster is restarted:

1. **Preflight.** The cluster is only touched when the `postgresql` resource reports `PostgresClusterStatus: Running` and exactly `spec.numberOfInstances` pods exist, all Running and Ready. Otherwise the cluster is skipped with an error and nothing is marked. A cluster where any pod already carries the operator's rolling-update marker is also skipped: that is a pending rotation requested by someone else (the operator or another run), and the tool neither takes it over nor removes it.
2. **Marking.** Every pod gets the operator's own marker annotation `zalando-postgres-operator-rolling-update-required=true` and the annotation `k8s-rollout-restart/marked-by=<run-id>`, so the run can tell its own markers from foreign ones. The patch is bound to the pod UID, so a pod that was replaced under the same name in the meantime is never marked by accident; the plan is refreshed instead. A refreshed plan must pass the preflight pod checks again (exactly `numberOfInstances` pods, all Running and Ready, none terminating); otherwise the cluster fails. If marking a pod fails, the markers already set are removed again and the cluster is not touched.
3. **Trigger.** A metadata annotation (`k8s-rollout-restart/restartedAt`) of the `postgresql` resource is updated. The update event makes the operator sync right away and recreate the marked pods (replicas first, then a switchover and the former primary). If the outcome of this PATCH is unknown (connection error, 5xx), no new restart is started anywhere from that moment; the rotation is still waited for (step 4), and the cluster is reported as failed afterwards even if the rotation completed.
4. **Wait.** The restart is complete when every marked pod instance has been replaced, no pod of the cluster carries the marker any more, `numberOfInstances` pods are Running and Ready, and the status is `Running`. An API error while polling (e.g. a forbidden pod List) stops new launches at once; the rotation is still waited for, and the cluster is reported as failed even if it completes.
5. **Re-trigger on stall.** postgres-operator retries a failed sync only on its next resync (`resync_period`, 30 minutes by default), which is usually longer than `--timeout`. If the status becomes `SyncFailed`/`UpdateFailed`, or no marked pod was replaced for `min(60s, --timeout/3)`, the resource annotation is updated again to force an immediate sync (at most 3 times per cluster, each one logged).
6. **Cleanup on failure.** If the restart does not complete (timeout, error, Ctrl-C) or the trigger fails, the marker is removed from the pods that still carry it **and** carry this run's `marked-by` annotation (markers set by anyone else are never removed), so that the operator does not rotate them unexpectedly on a later resync after the tool has exited. The log lists which pods were rotated and which were left untouched; pods whose marker could not be removed are reported explicitly, because the operator will rotate them on its next sync, together with a ready-to-copy `kubectl --context <ctx> annotate pod ...` command that removes the markers (it targets the run's `--context`, values are shell-quoted).

With `--skip-wait` the tool returns after a successful trigger and the markers stay in place: the operator rotates the pods on its own. If the trigger fails, the markers are cleaned up as in step 6.

RBAC: namespaced `patch` on `pods` (workload ClusterRole) is required in addition to `patch` on `postgresqls`; the setup script grants both only with `ENABLE_POSTGRESQL=true`.

## Node Cordoning

`--cordon` is meant for node rotation, for example with [Karpenter](https://karpenter.sh/):

1. The nodes running pods of the target namespaces (or all nodes with `--cordon-all-nodes`, filtered by `--node-labels` / `--exclude-node-labels`) are cordoned. Each node the tool cordons is annotated with `k8s-rollout-restart/cordoned-by=<run-id>`.
2. The workloads are restarted; the new pods cannot be scheduled on the cordoned nodes and land on new nodes.
3. Karpenter may then remove the emptied nodes.

Before cordoning, the tool warns about pods on the selected nodes that use node-local PersistentVolumes (`local` / node-affinity bound volumes): such pods cannot move to a new node, and removing the node loses their data. The check covers pods of **all** namespaces on the selected nodes (e.g. with `-n app --cordon-all-nodes`, a `db/postgres-0` with a local PV is reported too), so it needs cluster-wide `list` on `persistentvolumes` and `pods`; without them the tool logs "could not check" and continues.

The tool **never uncordons** nodes, on any path (success, failure or Ctrl-C): cordoned nodes stay cordoned on purpose so that Karpenter can consolidate them. At the end of the run the tool logs the cordoned nodes and prints a `kubectl uncordon ...` command for a manual rollback; when `--context` was given, the printed commands include the same `--context=<name>`, so copying them cannot hit another cluster through a different current-context. Nodes cordoned by a given run can be found by the annotation, e.g.:

```bash
kubectl get nodes -o json | jq -r '.items[] | select(.metadata.annotations["k8s-rollout-restart/cordoned-by"] != null) | .metadata.name'
```

## Flagger Deployments

The utility has special handling for deployments managed by [Flagger](https://flagger.app/):

- By default:
  - For Flagger-managed deployments, only the primary deployments (with `-primary` suffix) are restarted
  - Deployments without a Flagger primary counterpart are restarted directly
  - Deployments that have a `-primary` counterpart are skipped (the primary is restarted instead)
- Flagger-managed deployments are identified by an owner reference:
  ```
  ownerReferences:
    - apiVersion: flagger.app/v1beta1
      kind: Canary
      controller: true
  ```
- Use `--no-flagger-filter` to restart all deployments regardless of this logic
- StatefulSets are always restarted regardless of Flagger owner references

## Pod Filters

`--pod-labels`, `--pod-annotations` and `--container-image` restrict the restart to Deployments/StatefulSets that have **at least one pod** satisfying **all** active filters:

- Pods are matched to a workload through the controller owner chain, not label selectors: Pod → ReplicaSet → Deployment (by UID) for Deployments, Pod → StatefulSet (by UID) for StatefulSets. Pods that merely share labels with a workload are not counted. A resource without pods is skipped
- Labels and annotations are compared exactly (AND across all given keys)
- `--container-image` matches if any container image (including init and ephemeral containers) contains one of the given substrings (OR)

Examples: [pod labels](examples/pod-labels-example.md), [pod annotations](examples/pod-annotations-example.md).

```bash
# Istio canary workloads
k8s-rollout-restart --context=my-cluster -n my-namespace --pod-labels=istio.io/rev=canary --execute

# Labels and annotations together, only resources older than 1 hour
k8s-rollout-restart --context=my-cluster -n my-namespace --pod-labels=app=myapp --pod-annotations=istio.io/rev=canary --older-than=1h --execute
```

## Older Than Filter

- `--older-than` sets a minimum age for resources to be restarted
- Accepts Go durations (`30m`, `24h`, `1h30m`) plus a day suffix (`7d`, `1d12h`)
- Resources newer than the given duration are skipped
- Can be combined with namespace, resource type and pod filters

## Development

### Running Tests

```bash
go test ./...
```

### Linting

```bash
golangci-lint run ./...
```

# Pod Labels Filter Examples

This document provides examples of using the `--pod-labels` flag to selectively restart Deployments and StatefulSets based on the labels of their pods.

A resource is selected when **at least one** of its pods (pods linked to it through the controller owner UID chain: Pod → ReplicaSet → Deployment, Pod → StatefulSet) has **all** the specified labels with exactly the specified values.

## Basic Usage

### Restart only workloads whose pods use the Istio canary revision
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --execute
```

### Restart only workloads whose pods use the Istio default revision
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=default --execute
```

## Multiple Labels

### Restart resources whose pods have several specific labels
```bash
# Both labels must be present on the same pod (AND logic)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp,version=v1.0 --execute
```

### Environment and version labels
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=env=production,version=v2.1 --execute
```

## Combined Filters

### Pod labels and age filter
```bash
# Restart only resources older than 1 hour whose pods have the label
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --older-than=1h --execute
```

### Pod labels and resource type filter
```bash
# Restart only StatefulSets whose pods have the label
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --resources=statefulsets --pod-labels=app=mydb --execute
```

Pod filters apply only to `deployments` and `statefulsets`; they cannot be combined with operator-managed resource types (`strimzi-kafka`, `zalando-postgresql`, or `all`).

### Pod labels and container image
```bash
# Pods must have the label AND run an image containing "proxyv2:1.27.1"
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=default --container-image=proxyv2:1.27.1 --execute
```

### Pod labels and node cordoning
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --cordon --execute
```

### Across all namespaces
```bash
# kube-system and kube-public are always skipped with -A
./k8s-rollout-restart --context=my-cluster --all-namespaces --pod-labels=istio.io/rev=canary --dry-run
```

## Dry Run

### Preview what would be restarted
```bash
# Lists the Deployments/StatefulSets that match the filter without changing anything
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --dry-run
```

`--output=json` only switches the log format to structured JSON; it does not produce a separate machine-readable list of resources.

## Common Use Cases

### A/B testing
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=variant=A --execute
```

### Blue-green deployments
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=environment=blue --execute
```

### Version-based rollouts
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=version=v1.2.3 --execute
```

## Notes

- Works with Deployments and StatefulSets only.
- Format is `key=value`; values without `=` are rejected. `key=` matches only pods that have the label with an empty value.
- All specified labels must match on the same pod (AND logic); values are compared exactly.
- Pods are matched to a resource through the controller owner chain by UID (Pod → ReplicaSet → Deployment, Pod → StatefulSet), not via `spec.selector` or an assumed `app=<name>` label: pods of another workload that merely share labels are never counted. If a resource currently has no pods, it is skipped.
- Can be combined with `--older-than`, `--pod-annotations`, `--container-image`, `--resources=deployments,statefulsets`, `--cordon`.

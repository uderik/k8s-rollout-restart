# Pod Annotations Filter Examples

This document provides examples of using the `--pod-annotations` flag to selectively restart Deployments and StatefulSets based on the annotations of their pods.

A resource is selected when **at least one** of its pods (pods linked to it through the controller owner UID chain: Pod → ReplicaSet → Deployment, Pod → StatefulSet) has **all** the specified annotations with exactly the specified values.

## Basic Usage

### Restart only workloads whose pods use the Istio canary revision
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --execute
```

### Restart only workloads whose pods use the Istio default revision
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=default --execute
```

## Multiple Annotations

### Restart resources whose pods have several specific annotations
```bash
# All listed annotations must match on the same pod (AND logic)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true,prometheus.io/port=8080 --execute
```

## Combined Filters

### Annotations and age filter
```bash
# Restart only resources older than 1 hour whose pods have the annotation
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --older-than=1h --execute
```

### Annotations and resource type filter
```bash
# Restart only StatefulSets whose pods have the annotation
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --resources=statefulsets --pod-annotations=prometheus.io/scrape=true --execute
```

Pod filters apply only to `deployments` and `statefulsets`; they cannot be combined with operator-managed resource types (`strimzi-kafka`, `zalando-postgresql`, or `all`).

### Labels and annotations together
```bash
# Restart only resources whose pods have the label AND the annotation
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp --pod-annotations=istio.io/rev=canary --execute
```

### Annotations and node cordoning
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --cordon --execute
```

### Across all namespaces
```bash
# kube-system and kube-public are always skipped with -A
./k8s-rollout-restart --context=my-cluster --all-namespaces --pod-annotations=istio.io/rev=canary --dry-run
```

## Dry Run

### Preview what would be restarted
```bash
# Lists the Deployments/StatefulSets that match the filter without changing anything
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --dry-run
```

`--output=json` only switches the log format to structured JSON; it does not produce a separate machine-readable list of resources.

## Common Use Cases

### Istio revisions
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --execute
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=default --execute
```

### Prometheus scraping
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true --execute
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true,prometheus.io/port=8080,prometheus.io/path=/metrics --execute
```

### Custom application annotations
```bash
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=app.company.com/version=v1.2.3 --execute
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=environment=staging,version=v1.0.0 --execute
```

### Annotation present with an empty value
```bash
# Matches only pods that HAVE the annotation "example.com/flag" with an empty value.
# Pods without the annotation do not match.
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=example.com/flag= --dry-run
```

## Notes

- Works with Deployments and StatefulSets only.
- Format is `key=value`; values without `=` are rejected.
- All specified annotations must match on the same pod (AND logic); values are compared exactly and case-sensitively (no prefix/range matching).
- Pods are matched to a resource through the controller owner chain by UID (Pod → ReplicaSet → Deployment, Pod → StatefulSet), not via `spec.selector`: pods of another workload that merely share labels are never counted. If a resource currently has no pods, it is skipped.
- Annotations Kubernetes sets on other objects are not pod annotations: `deployment.kubernetes.io/revision` lives on Deployments/ReplicaSets, and `statefulset.kubernetes.io/pod-name` is a pod **label** (use `--pod-labels` for it).
- Can be combined with `--older-than`, `--pod-labels`, `--container-image`, `--resources=deployments,statefulsets`, `--cordon`.

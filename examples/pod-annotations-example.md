# Pod Annotations Filter Examples

This document provides examples of using the `--pod-annotations` flag to selectively restart resources based on pod annotations.

## Basic Usage

### Restart only Istio canary deployments (using annotations)
```bash
# Restart only deployments with pods annotated as Istio canary
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --execute
```

### Restart only Istio default deployments (using annotations)
```bash
# Restart only deployments with pods annotated as Istio default
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=default --execute
```

## Multiple Annotations

### Restart resources with multiple specific annotations
```bash
# Restart only resources with pods having both istio.io/rev=canary and deployment.kubernetes.io/revision=1
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary,deployment.kubernetes.io/revision=1 --execute
```

### Restart resources with monitoring annotations
```bash
# Restart only resources with pods having Prometheus monitoring enabled
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true,prometheus.io/port=8080 --execute
```

## Combined Filters

### Restart with annotations and age filter
```bash
# Restart only resources older than 1 hour with specific pod annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --older-than=1h --execute
```

### Restart with annotations and resource type filter
```bash
# Restart only StatefulSets with specific pod annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --resources=statefulsets --pod-annotations=statefulset.kubernetes.io/pod-name=mydb-0 --execute
```

### Restart with both labels and annotations
```bash
# Restart only resources with specific labels AND annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp --pod-annotations=istio.io/rev=canary --execute
```

### Restart with annotations and node cordoning
```bash
# Restart resources with specific pod annotations and cordon nodes
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --cordon --execute
```

## Dry Run Examples

### Preview what would be restarted
```bash
# See which resources would be restarted with specific pod annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --dry-run
```

### Preview with JSON output
```bash
# Get detailed information about resources that would be restarted
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --dry-run --output=json
```

## Common Use Cases

### Istio Canary Deployments
```bash
# Restart only canary deployments (using annotations)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --execute

# Restart only default deployments (using annotations)
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=default --execute
```

### Kubernetes Deployment Revisions
```bash
# Restart only deployments with specific revision
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=deployment.kubernetes.io/revision=1 --execute

# Restart only deployments with revision 2 or higher
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=deployment.kubernetes.io/revision=2 --execute
```

### Prometheus Monitoring
```bash
# Restart only resources with Prometheus monitoring enabled
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true --execute

# Restart only resources with specific Prometheus configuration
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=prometheus.io/scrape=true,prometheus.io/port=8080,prometheus.io/path=/metrics --execute
```

### StatefulSet Specific
```bash
# Restart only StatefulSets with specific pod names
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=statefulset.kubernetes.io/pod-name=mydb-0 --execute

# Restart only StatefulSets with specific pod names and app labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=mydb --pod-annotations=statefulset.kubernetes.io/pod-name=mydb-0 --execute
```

### Custom Application Annotations
```bash
# Restart only resources with custom application annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=app.company.com/version=v1.2.3 --execute

# Restart only resources with custom feature flags
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=feature.new-ui=enabled --execute
```

### Environment-specific Filtering
```bash
# Restart only resources in production environment
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=environment=production --execute

# Restart only resources in staging environment with specific version
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=environment=staging,version=v1.0.0 --execute
```

## Troubleshooting

### Check which resources have specific annotations
```bash
# Use dry-run to see which resources match your criteria
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --dry-run
```

### Debug with verbose logging
```bash
# Add more detailed logging to see the filtering process
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-annotations=istio.io/rev=canary --dry-run --output=json
```

### Check both labels and annotations
```bash
# Check resources with both specific labels and annotations
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp --pod-annotations=istio.io/rev=canary --dry-run
```

## Notes

- The `--pod-annotations` flag works with both Deployments and StatefulSets
- All specified annotations must match for a resource to be restarted (AND logic)
- The utility automatically finds pods associated with each resource through various methods:
  - Direct label selectors (`app=<resource-name>`)
  - Owner references (for ReplicaSets and StatefulSets)
  - Alternative label patterns
- If no pods are found for a resource, it will be skipped
- The flag can be combined with other filters like `--older-than`, `--resources`, `--pod-labels`, etc.
- Both `--pod-labels` and `--pod-annotations` can be used together (AND logic)
- Annotations are case-sensitive and must match exactly
- Empty values in annotations are supported (e.g., `key=`)

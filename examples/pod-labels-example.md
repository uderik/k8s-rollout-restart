# Pod Labels Filter Examples

This document provides examples of using the `--pod-labels` flag to selectively restart resources based on pod labels.

## Basic Usage

### Restart only Istio canary deployments
```bash
# Restart only deployments with pods labeled as Istio canary
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --execute
```

### Restart only Istio default deployments
```bash
# Restart only deployments with pods labeled as Istio default
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=default --execute
```

## Multiple Labels

### Restart resources with multiple specific labels
```bash
# Restart only resources with pods having both app=myapp and version=v1.0
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=app=myapp,version=v1.0 --execute
```

### Restart resources with environment and version labels
```bash
# Restart only resources with pods in production environment and specific version
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=env=production,version=v2.1 --execute
```

## Combined Filters

### Restart with pod labels and age filter
```bash
# Restart only resources older than 1 hour with specific pod labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --older-than=1h --execute
```

### Restart with pod labels and resource type filter
```bash
# Restart only StatefulSets with specific pod labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --resources=statefulsets --pod-labels=app=mydb --execute
```

### Restart with pod labels and node cordoning
```bash
# Restart resources with specific pod labels and cordon nodes
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --cordon --execute
```

## Dry Run Examples

### Preview what would be restarted
```bash
# See which resources would be restarted with specific pod labels
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --dry-run
```

### Preview with JSON output
```bash
# Get detailed information about resources that would be restarted
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --dry-run --output=json
```

## Common Use Cases

### A/B Testing
```bash
# Restart only the "A" variant of your application
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=variant=A --execute
```

### Blue-Green Deployments
```bash
# Restart only the "blue" environment
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=environment=blue --execute
```

### Feature Flags
```bash
# Restart only resources with a specific feature flag enabled
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=feature.new-ui=enabled --execute
```

### Version-based Rollouts
```bash
# Restart only resources with a specific version
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=version=v1.2.3 --execute
```

## Troubleshooting

### Check which resources have specific labels
```bash
# Use dry-run to see which resources match your criteria
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --dry-run
```

### Debug with verbose logging
```bash
# Add more detailed logging to see the filtering process
./k8s-rollout-restart --context=my-cluster --namespace=my-namespace --pod-labels=istio.io/rev=canary --dry-run --output=json
```

## Notes

- The `--pod-labels` flag works with both Deployments and StatefulSets
- All specified labels must match for a resource to be restarted (AND logic)
- The utility automatically finds pods associated with each resource through various methods:
  - Direct label selectors (`app=<resource-name>`)
  - Owner references (for ReplicaSets and StatefulSets)
  - Alternative label patterns
- If no pods are found for a resource, it will be skipped
- The flag can be combined with other filters like `--older-than`, `--resources`, etc.

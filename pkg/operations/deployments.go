package operations

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DeploymentOperations implements DeploymentOperator interface
type DeploymentOperations struct {
	clientset            K8sClient
	parallel             int
	timeout              int
	dryRun               bool
	noFlagger            bool
	log                  *logger.Logger
	minAge               *time.Duration
	podLabels            []string
	podAnnotations       []string
	parsedPodLabels      map[string]string // cached parsed pod labels
	parsedPodAnnotations map[string]string // cached parsed pod annotations
	skipWait             bool
}

// NewDeploymentOperations creates a new DeploymentOperations instance
func NewDeploymentOperations(clientset K8sClient, parallel, timeout int, noFlagger, dryRun bool, minAge *time.Duration, podLabels []string, podAnnotations []string, skipWait bool) *DeploymentOperations {
	return &DeploymentOperations{
		clientset:            clientset,
		parallel:             parallel,
		timeout:              timeout,
		dryRun:               dryRun,
		noFlagger:            noFlagger,
		log:                  logger.NewLogger(dryRun),
		minAge:               minAge,
		podLabels:            podLabels,
		podAnnotations:       podAnnotations,
		parsedPodLabels:      ParseLabelsOrAnnotations(podLabels),
		parsedPodAnnotations: ParseLabelsOrAnnotations(podAnnotations),
		skipWait:             skipWait,
	}
}

// RestartDeployments restarts all deployments in the given namespaces
func (d *DeploymentOperations) RestartDeployments(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		d.log.Info("No namespaces specified, skipping deployments restart")
		return nil
	}

	// Use errgroup for better parallel error handling
	g, ctx := errgroup.WithContext(ctx)

	for _, ns := range namespaces {
		g.Go(func() error {
			if err := d.restartDeploymentsInNamespace(ctx, ns); err != nil {
				return fmt.Errorf("failed to restart deployments in namespace %s: %w", ns, err)
			}
			return nil
		})
	}

	// Wait for all goroutines to complete and return first error if any
	return g.Wait()
}

// restartDeploymentsInNamespace restarts all deployments in a single namespace
func (d *DeploymentOperations) restartDeploymentsInNamespace(ctx context.Context, namespace string) error {
	// List all deployments in the namespace
	deployments, err := d.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}

	if len(deployments.Items) == 0 {
		d.log.Info("No deployments found in namespace %s", namespace)
		return nil
	}

	d.log.Info("Found %d deployment(s) in namespace %s", len(deployments.Items), namespace)

	// Maps to track deployments and relationships
	primaryDeployments := make(map[string]appsv1.Deployment)
	regularDeployments := make(map[string]appsv1.Deployment)
	deploymentsToRestart := make([]appsv1.Deployment, 0)
	skippedDeployments := make([]appsv1.Deployment, 0)
	tooYoungDeployments := make([]appsv1.Deployment, 0)

	// First pass: categorize deployments as primary or regular
	for _, deployment := range deployments.Items {
		// Check if deployment is old enough to restart
		if d.minAge != nil {
			if time.Since(deployment.CreationTimestamp.Time) < *d.minAge {
				tooYoungDeployments = append(tooYoungDeployments, deployment)
				continue
			}
		}

		// Check if deployment has pods with required labels or annotations
		if len(d.podLabels) > 0 || len(d.podAnnotations) > 0 {
			hasMatchingPods, err := d.hasPodsWithLabelsOrAnnotations(ctx, namespace, deployment.Name)
			if err != nil {
				d.log.Warning("Failed to check pods for deployment %s: %v", deployment.Name, err)
				continue
			}
			if !hasMatchingPods {
				d.log.Info("Skipping deployment %s: no pods with required labels or annotations", deployment.Name)
				continue
			}
		}

		if d.noFlagger {
			// If --no-flagger-filter is set, restart all deployments
			deploymentsToRestart = append(deploymentsToRestart, deployment)
			continue
		}

		// Check if this is a primary deployment
		if strings.HasSuffix(deployment.Name, "-primary") && d.hasFlaggerCanaryOwner(deployment) {
			// This is a primary deployment managed by Flagger
			primaryDeployments[deployment.Name] = deployment
			continue
		}

		// This is a regular deployment
		regularDeployments[deployment.Name] = deployment
	}

	// If not using --no-flagger-filter, apply special logic
	if !d.noFlagger {
		// Add all primary deployments to the restart list
		for _, deployment := range primaryDeployments {
			deploymentsToRestart = append(deploymentsToRestart, deployment)
		}

		// For regular deployments, check if they have a primary counterpart
		for name, deployment := range regularDeployments {
			primaryName := name + "-primary"
			if _, hasPrimary := primaryDeployments[primaryName]; hasPrimary {
				// This deployment has a primary counterpart, skip it
				skippedDeployments = append(skippedDeployments, deployment)
			} else {
				// This deployment doesn't have a primary counterpart, restart it
				deploymentsToRestart = append(deploymentsToRestart, deployment)
			}
		}
	}

	if len(deploymentsToRestart) == 0 {
		d.log.Info("No deployments found to restart in namespace %s", namespace)
		return nil
	}

	d.log.Info("Will restart %d/%d deployment(s) in namespace %s",
		len(deploymentsToRestart), len(deployments.Items), namespace)

	if d.dryRun {
		d.log.Info("Deployments that would be restarted in namespace %s:", namespace)
		for _, deployment := range deploymentsToRestart {
			var reason string
			if strings.HasSuffix(deployment.Name, "-primary") && d.hasFlaggerCanaryOwner(deployment) {
				reason = "(Flagger primary deployment)"
			} else {
				reason = "(no primary counterpart)"
			}
			d.log.Info("- %s %s", deployment.Name, reason)
		}

		if len(skippedDeployments) > 0 {
			d.log.Info("\nDeployments that would be skipped in namespace %s:", namespace)
			for _, deployment := range skippedDeployments {
				d.log.Info("- %s (has primary counterpart that will be restarted)", deployment.Name)
			}
		}

		if len(tooYoungDeployments) > 0 {
			d.log.Info("\nDeployments that would be skipped because they are too new in namespace %s:", namespace)
			for _, deployment := range tooYoungDeployments {
				d.log.Info("- %s (age: %s, required: %s)", deployment.Name,
					time.Since(deployment.CreationTimestamp.Time).Round(time.Second),
					*d.minAge)
			}
		}

		return nil
	}

	// Create a worker pool for parallel processing
	type restartJob struct {
		namespace  string
		deployment appsv1.Deployment
	}

	jobCh := make(chan restartJob, len(deploymentsToRestart))
	workerErrCh := make(chan error, len(deploymentsToRestart))

	// Start workers
	var workerWg sync.WaitGroup
	for i := 0; i < d.parallel; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for job := range jobCh {
				// Patch deployment to trigger a rolling update
				patchData := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`, time.Now().Format(time.RFC3339))
				_, err := d.clientset.AppsV1().Deployments(job.namespace).Patch(ctx, job.deployment.Name, types.StrategicMergePatchType, []byte(patchData), metav1.PatchOptions{})
				if err != nil {
					workerErrCh <- fmt.Errorf("failed to patch deployment %s/%s: %w", job.namespace, job.deployment.Name, err)
					continue // Continue processing other jobs instead of returning
				}

				d.log.Success("Successfully restarted deployment %s/%s", job.namespace, job.deployment.Name)
			}
		}()
	}

	// Send jobs to workers
	for _, deployment := range deploymentsToRestart {
		var reason string
		if strings.HasSuffix(deployment.Name, "-primary") && d.hasFlaggerCanaryOwner(deployment) {
			reason = "(Flagger primary deployment)"
		} else {
			reason = "(no primary counterpart)"
		}
		d.log.Info("Restarting deployment %s/%s %s", namespace, deployment.Name, reason)
		jobCh <- restartJob{
			namespace:  namespace,
			deployment: deployment,
		}
	}

	close(jobCh)
	workerWg.Wait()
	close(workerErrCh)

	// Collect all errors
	var errs []error
	for err := range workerErrCh {
		errs = append(errs, err)
	}

	// Return combined error if any errors occurred
	if len(errs) > 0 {
		return fmt.Errorf("errors during deployment restart: %v", errs)
	}

	if len(skippedDeployments) > 0 {
		d.log.Info("Skipped %d deployment(s) in namespace %s that have primary counterparts",
			len(skippedDeployments), namespace)
	}

	// Wait for all restarted deployments to be ready
	if !d.skipWait {
		d.log.Info("Waiting for deployments to become ready in namespace %s", namespace)
		if err := d.waitForDeploymentsReady(ctx, namespace, deploymentsToRestart); err != nil {
			return fmt.Errorf("failed waiting for deployments to become ready: %w", err)
		}
	} else {
		d.log.Info("Skipping wait for deployments readiness (--skip-wait flag is set)")
	}

	d.log.Success("Successfully restarted %d deployment(s) in namespace %s",
		len(deploymentsToRestart), namespace)

	return nil
}

// waitForDeploymentsReady waits for all specified deployments to be ready after restart
func (d *DeploymentOperations) waitForDeploymentsReady(ctx context.Context, namespace string, deployments []appsv1.Deployment) error {
	if len(deployments) == 0 {
		return nil
	}

	// Create a map of deployment names for quick lookup with their initial generation
	deploymentGenerations := make(map[string]int64)
	for _, deployment := range deployments {
		deploymentGenerations[deployment.Name] = deployment.Generation
	}

	// Wait for deployments to be ready
	d.log.Info("Waiting for %d deployment(s) in namespace %s to become ready", len(deployments), namespace)

	// Create a timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(d.timeout)*time.Second)
	defer cancel()

	// Check every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Track deployments that were ready in previous check
	previouslyReady := make(map[string]bool)

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for deployments to be ready in namespace %s", namespace)
		case <-ticker.C:
			// Get current state of deployments
			deploymentsReady := 0
			totalDeployments := len(deploymentGenerations)
			notReadyDeployments := make(map[string][]string)

			// Check each deployment
			for deploymentName := range deploymentGenerations {
				// Get deployment status (cache manager will handle freshness with TTL)
				deployment, err := d.clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
				if err != nil {
					d.log.Warning("Failed to get deployment %s/%s: %v", namespace, deploymentName, err)
					notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName], "Failed to get deployment status")
					continue
				}

				// Check if deployment generation increased
				if deployment.Status.ObservedGeneration < deployment.Generation {
					notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName],
						fmt.Sprintf("Generation not updated (observed: %d, current: %d)",
							deployment.Status.ObservedGeneration, deployment.Generation))
					continue
				}

				// Check deployment conditions
				progressing := false
				replicaFailure := false

				for _, condition := range deployment.Status.Conditions {
					switch condition.Type {
					case appsv1.DeploymentProgressing:
						if condition.Status == "True" {
							progressing = true
						}
					case appsv1.DeploymentReplicaFailure:
						if condition.Status == "True" {
							replicaFailure = true
							notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName],
								fmt.Sprintf("Replica failure: %s", condition.Message))
						}
					case appsv1.DeploymentAvailable:
						// DeploymentAvailable condition is informational, no action needed
					}
				}

				if !progressing {
					notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName], "Not progressing")
					continue
				}

				if replicaFailure {
					continue
				}

				// Check if all pods are updated to the new generation
				if deployment.Status.UpdatedReplicas != *deployment.Spec.Replicas {
					// If deployment was previously ready and has the same number of replicas, consider it ready
					if previouslyReady[deploymentName] && deployment.Status.Replicas == *deployment.Spec.Replicas {
						deploymentsReady++
						continue
					}
					notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName],
						fmt.Sprintf("Pods updating (%d/%d updated)",
							deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas))
					continue
				}

				// Check if all updated pods are ready
				if deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
					// If deployment was previously ready and has the same number of replicas, consider it ready
					if previouslyReady[deploymentName] && deployment.Status.Replicas == *deployment.Spec.Replicas {
						deploymentsReady++
						continue
					}
					notReadyDeployments[deploymentName] = append(notReadyDeployments[deploymentName],
						fmt.Sprintf("Pods starting (%d/%d ready)",
							deployment.Status.ReadyReplicas, *deployment.Spec.Replicas))
					continue
				}

				// Mark deployment as ready in this iteration
				previouslyReady[deploymentName] = true
				deploymentsReady++
			}

			// Log progress
			if deploymentsReady == totalDeployments {
				d.log.Success("All deployments are ready in namespace %s", namespace)
				return nil
			}

			d.log.Info("Waiting for deployments to be ready in namespace %s (%d/%d ready)",
				namespace, deploymentsReady, totalDeployments)

			// Log detailed status for not ready deployments
			for name, reasons := range notReadyDeployments {
				d.log.Info("Deployment %s not ready: %s", name, strings.Join(reasons, ", "))
			}
		}
	}
}

// hasFlaggerCanaryOwner checks if the deployment has a Flagger Canary owner reference
func (d *DeploymentOperations) hasFlaggerCanaryOwner(deployment appsv1.Deployment) bool {
	for _, ownerRef := range deployment.OwnerReferences {
		if ownerRef.APIVersion == "flagger.app/v1beta1" &&
			ownerRef.Kind == "Canary" &&
			ownerRef.Controller != nil && *ownerRef.Controller {
			return true
		}
	}
	return false
}

// isDeploymentReady checks if a deployment is ready
func isDeploymentReady(deployment *appsv1.Deployment) bool {
	return deployment.Status.ReadyReplicas == *deployment.Spec.Replicas &&
		deployment.Status.UpdatedReplicas == *deployment.Spec.Replicas &&
		deployment.Status.AvailableReplicas == *deployment.Spec.Replicas
}

// WaitForDeployments waits for all deployments in the specified namespaces to be ready
func (d *DeploymentOperations) WaitForDeployments(ctx context.Context, namespaces []string) error {
	d.log.Info("Waiting for deployments to be ready in namespaces: %v", namespaces)

	// Create a timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(d.timeout)*time.Second)
	defer cancel()

	// Get all deployments first
	allDeployments := make(map[string][]string)
	for _, ns := range namespaces {
		deployments, err := d.clientset.AppsV1().Deployments(ns).List(timeoutCtx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list deployments in namespace %s: %w", ns, err)
		}
		allDeployments[ns] = make([]string, 0, len(deployments.Items))
		for _, deployment := range deployments.Items {
			allDeployments[ns] = append(allDeployments[ns], deployment.Name)
		}
	}

	// Wait for all deployments to be ready
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for _, ns := range namespaces {
		if err := d.waitForNamespaceDeployments(timeoutCtx, ns, allDeployments[ns], ticker); err != nil {
			return err
		}
	}

	return nil
}

// waitForNamespaceDeployments waits for all deployments in a single namespace to be ready
func (d *DeploymentOperations) waitForNamespaceDeployments(ctx context.Context, ns string, deployments []string, ticker *time.Ticker) error {
	if len(deployments) == 0 {
		d.log.Info("No deployments found in namespace %s", ns)
		return nil
	}

	d.log.Info("Waiting for deployments to be ready in namespace %s (%d deployments)", ns, len(deployments))

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for deployments to be ready in namespace %s", ns)
		case <-ticker.C:
			readyCount := 0
			for _, deploymentName := range deployments {
				deployment, err := d.clientset.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("failed to get deployment %s/%s: %w", ns, deploymentName, err)
				}

				if isDeploymentReady(deployment) {
					readyCount++
				}
			}

			if readyCount == len(deployments) {
				d.log.Info("All deployments are ready in namespace %s", ns)
				return nil
			}

			d.log.Info("Waiting for deployments to be ready in namespace %s (%d/%d ready)", ns, readyCount, len(deployments))
		}
	}
}

// hasPodsWithLabelsOrAnnotations checks if a deployment has pods with the required labels or annotations
func (d *DeploymentOperations) hasPodsWithLabelsOrAnnotations(ctx context.Context, namespace, deploymentName string) (bool, error) {
	// Get the deployment to access its selector
	deployment, err := d.clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get deployment %s: %w", deploymentName, err)
	}

	// Use the deployment's selector to find its pods
	labelSelector := metav1.FormatLabelSelector(deployment.Spec.Selector)
	pods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return false, fmt.Errorf("failed to list pods for deployment %s: %w", deploymentName, err)
	}

	// If no pods found, try to find pods by owner reference as fallback
	if len(pods.Items) == 0 {
		allPods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list all pods in namespace %s: %w", namespace, err)
		}

		// Find pods owned by this deployment through ReplicaSets
		for _, pod := range allPods.Items {
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "ReplicaSet" {
					// Check if this ReplicaSet is owned by our deployment
					rs, err := d.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
					if err != nil {
						continue
					}
					for _, rsOwner := range rs.OwnerReferences {
						if rsOwner.Kind == "Deployment" && rsOwner.Name == deploymentName {
							pods.Items = append(pods.Items, pod)
							break
						}
					}
				}
			}
		}
	}

	// Check if any pod has the required labels or annotations
	for _, pod := range pods.Items {
		if d.podHasRequiredLabels(pod.Labels) && d.podHasRequiredAnnotations(pod.Annotations) {
			return true, nil
		}
	}

	return false, nil
}

// podHasRequiredLabels checks if a pod has all the required labels
func (d *DeploymentOperations) podHasRequiredLabels(podLabels map[string]string) bool {
	// Use parsed labels if available (from constructor), otherwise parse on the fly (for tests)
	requiredLabels := d.parsedPodLabels
	if len(requiredLabels) == 0 && len(d.podLabels) > 0 {
		// Fallback: parse labels on the fly for backward compatibility
		requiredLabels = ParseLabelsOrAnnotations(d.podLabels)
	}
	return MatchesRequirements(podLabels, requiredLabels)
}

// podHasRequiredAnnotations checks if a pod has all the required annotations
func (d *DeploymentOperations) podHasRequiredAnnotations(podAnnotations map[string]string) bool {
	// Use parsed annotations if available (from constructor), otherwise parse on the fly (for tests)
	requiredAnnotations := d.parsedPodAnnotations
	if len(requiredAnnotations) == 0 && len(d.podAnnotations) > 0 {
		// Fallback: parse annotations on the fly for backward compatibility
		requiredAnnotations = ParseLabelsOrAnnotations(d.podAnnotations)
	}
	return MatchesRequirements(podAnnotations, requiredAnnotations)
}

// GetDeploymentsToRestart returns a list of deployments that would be restarted
func (d *DeploymentOperations) GetDeploymentsToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	var allDeployments []string

	for _, namespace := range namespaces {
		deployments, err := d.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list deployments in namespace %s: %w", namespace, err)
		}

		// Get all pods in the namespace once for efficient filtering
		var podsWithMatchingLabels []string
		if len(d.podLabels) > 0 || len(d.podAnnotations) > 0 {
			allPods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
			}

			// Get all ReplicaSets in the namespace once to avoid multiple API calls
			allReplicaSets, err := d.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list replicasets in namespace %s: %w", namespace, err)
			}

			// Create a map of ReplicaSet name -> Deployment name
			rsToDeployment := make(map[string]string)
			for _, rs := range allReplicaSets.Items {
				for _, owner := range rs.OwnerReferences {
					if owner.Kind == "Deployment" {
						rsToDeployment[rs.Name] = owner.Name
						break
					}
				}
			}

			// Create a map of deployment names that have matching pods
			deploymentPods := make(map[string]bool)
			for _, pod := range allPods.Items {
				// Check if pod has required labels and annotations
				if d.podHasRequiredLabels(pod.Labels) && d.podHasRequiredAnnotations(pod.Annotations) {
					// Find which deployment this pod belongs to
					for _, owner := range pod.OwnerReferences {
						if owner.Kind == "ReplicaSet" {
							if deploymentName, exists := rsToDeployment[owner.Name]; exists {
								deploymentPods[deploymentName] = true
							}
						}
					}
				}
			}
			podsWithMatchingLabels = make([]string, 0, len(deploymentPods))
			for deploymentName := range deploymentPods {
				podsWithMatchingLabels = append(podsWithMatchingLabels, deploymentName)
			}
		}

		// Apply the same filtering logic as in RestartDeployments
		for _, deployment := range deployments.Items {
			// Check if deployment is old enough to restart
			if d.minAge != nil {
				if time.Since(deployment.CreationTimestamp.Time) < *d.minAge {
					continue
				}
			}

			// Check if deployment has pods with required labels or annotations
			if len(d.podLabels) > 0 || len(d.podAnnotations) > 0 {
				hasMatchingPods := false
				for _, deploymentName := range podsWithMatchingLabels {
					if deploymentName == deployment.Name {
						hasMatchingPods = true
						break
					}
				}
				if !hasMatchingPods {
					continue
				}
			}

			// Apply Flagger logic
			if !d.noFlagger {
				// Check if this is a primary deployment
				if strings.HasSuffix(deployment.Name, "-primary") && d.hasFlaggerCanaryOwner(deployment) {
					allDeployments = append(allDeployments, fmt.Sprintf("%s/%s", namespace, deployment.Name))
					continue
				}

				// Check if this is a regular deployment without a primary counterpart
				if !strings.HasSuffix(deployment.Name, "-primary") {
					// Check if this deployment has a primary counterpart in the already loaded deployments
					primaryName := deployment.Name + "-primary"
					hasPrimary := false
					for _, existingDeployment := range deployments.Items {
						if existingDeployment.Name == primaryName {
							hasPrimary = true
							break
						}
					}
					if !hasPrimary {
						// No primary deployment found, this regular deployment will be restarted
						allDeployments = append(allDeployments, fmt.Sprintf("%s/%s", namespace, deployment.Name))
					}
				}
			} else {
				// If --no-flagger-filter is set, restart all deployments
				allDeployments = append(allDeployments, fmt.Sprintf("%s/%s", namespace, deployment.Name))
			}
		}
	}

	return allDeployments, nil
}

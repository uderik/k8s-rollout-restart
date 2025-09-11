package operations

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DeploymentOperations implements DeploymentOperator interface
type DeploymentOperations struct {
	clientset      K8sClient
	parallel       int
	timeout        int
	dryRun         bool
	noFlagger      bool
	log            *logger.Logger
	minAge         *time.Duration
	podLabels      []string
	podAnnotations []string
}

// NewDeploymentOperations creates a new DeploymentOperations instance
func NewDeploymentOperations(clientset K8sClient, parallel, timeout int, noFlagger, dryRun bool, minAge *time.Duration, podLabels []string, podAnnotations []string) *DeploymentOperations {
	return &DeploymentOperations{
		clientset:      clientset,
		parallel:       parallel,
		timeout:        timeout,
		dryRun:         dryRun,
		noFlagger:      noFlagger,
		log:            logger.NewLogger(dryRun),
		minAge:         minAge,
		podLabels:      podLabels,
		podAnnotations: podAnnotations,
	}
}

// RestartDeployments restarts all deployments in the given namespaces
func (d *DeploymentOperations) RestartDeployments(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		d.log.Info("No namespaces specified, skipping deployments restart")
		return nil
	}

	// For each namespace, restart deployments
	var wg sync.WaitGroup
	errorCh := make(chan error, len(namespaces))

	for _, ns := range namespaces {
		ns := ns // Capture for goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.restartDeploymentsInNamespace(ctx, ns); err != nil {
				errorCh <- fmt.Errorf("failed to restart deployments in namespace %s: %w", ns, err)
			}
		}()
	}

	wg.Wait()
	close(errorCh)

	// Check for errors
	for err := range errorCh {
		return err // Return first error encountered
	}

	return nil
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
			reason := ""
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
					return
				}

				d.log.Success("Successfully restarted deployment %s/%s", job.namespace, job.deployment.Name)
			}
		}()
	}

	// Send jobs to workers
	for _, deployment := range deploymentsToRestart {
		reason := ""
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

	// Check for errors
	for err := range workerErrCh {
		return err // Return first error encountered
	}

	if len(skippedDeployments) > 0 {
		d.log.Info("Skipped %d deployment(s) in namespace %s that have primary counterparts",
			len(skippedDeployments), namespace)
	}

	// Wait for all restarted deployments to be ready
	d.log.Info("Waiting for deployments to become ready in namespace %s", namespace)
	if err := d.waitForDeploymentsReady(ctx, namespace, deploymentsToRestart); err != nil {
		return fmt.Errorf("failed waiting for deployments to become ready: %w", err)
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
				// Disable caching to get fresh data
				deployment, err := d.clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{
					ResourceVersion: "0", // Force fresh data
				})
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

// isFlaggerPrimaryDeployment checks if the deployment is a Flagger primary deployment
// A Flagger primary deployment has a -primary suffix and is owned by a Flagger Canary resource
func (d *DeploymentOperations) isFlaggerPrimaryDeployment(deployment appsv1.Deployment) bool {
	// Check for Flagger Canary owner reference AND -primary suffix
	return d.hasFlaggerCanaryOwner(deployment) && strings.HasSuffix(deployment.Name, "-primary")
}

// hasPrimaryCounterpart checks if a deployment has a primary counterpart
// by looking for a deployment with the same name plus "-primary" suffix
func (d *DeploymentOperations) hasPrimaryCounterpart(ctx context.Context, namespace, name string) (bool, error) {
	primaryName := name + "-primary"
	_, err := d.clientset.AppsV1().Deployments(namespace).Get(ctx, primaryName, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isDeploymentReady checks if a deployment is ready
func isDeploymentReady(deployment *appsv1.Deployment) bool {
	return deployment.Status.ReadyReplicas == *deployment.Spec.Replicas &&
		deployment.Status.UpdatedReplicas == *deployment.Spec.Replicas &&
		deployment.Status.AvailableReplicas == *deployment.Spec.Replicas
}

// WaitForDeployments waits for all deployments in the specified namespaces to be ready
func (o *DeploymentOperations) WaitForDeployments(ctx context.Context, namespaces []string) error {
	o.log.Info("Waiting for deployments to be ready in namespaces: %v", namespaces)

	// Get all deployments first
	allDeployments := make(map[string][]string)
	for _, ns := range namespaces {
		deployments, err := o.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list deployments in namespace %s: %w", ns, err)
		}
		allDeployments[ns] = make([]string, 0, len(deployments.Items))
		for _, deployment := range deployments.Items {
			allDeployments[ns] = append(allDeployments[ns], deployment.Name)
		}
	}

	// Wait for all deployments to be ready
	for _, ns := range namespaces {
		deployments := allDeployments[ns]
		if len(deployments) == 0 {
			o.log.Info("No deployments found in namespace %s", ns)
			continue
		}

		o.log.Info("Waiting for deployments to be ready in namespace %s (%d deployments)", ns, len(deployments))
		readyCount := 0

		for {
			readyCount = 0
			for _, deploymentName := range deployments {
				deployment, err := o.clientset.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("failed to get deployment %s/%s: %w", ns, deploymentName, err)
				}

				if isDeploymentReady(deployment) {
					readyCount++
				}
			}

			if readyCount == len(deployments) {
				o.log.Info("All deployments are ready in namespace %s", ns)
				break
			}

			o.log.Info("Waiting for deployments to be ready in namespace %s (%d/%d ready)", ns, readyCount, len(deployments))
			time.Sleep(5 * time.Second)
		}
	}

	return nil
}

// hasPodsWithLabelsOrAnnotations checks if a deployment has pods with the required labels or annotations
func (d *DeploymentOperations) hasPodsWithLabelsOrAnnotations(ctx context.Context, namespace, deploymentName string) (bool, error) {
	// Get pods for this deployment
	pods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", deploymentName),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list pods for deployment %s: %w", deploymentName, err)
	}

	// If no pods found, try alternative label selectors
	if len(pods.Items) == 0 {
		// Try with deployment name as label
		pods, err = d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("deployment=%s", deploymentName),
		})
		if err != nil {
			return false, fmt.Errorf("failed to list pods for deployment %s: %w", deploymentName, err)
		}
	}

	// If still no pods, try to find pods by owner reference
	if len(pods.Items) == 0 {
		allPods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list all pods in namespace %s: %w", namespace, err)
		}

		// Find pods owned by this deployment
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
	// Parse required labels
	requiredLabels := make(map[string]string)
	for _, label := range d.podLabels {
		parts := strings.Split(label, "=")
		if len(parts) != 2 {
			continue
		}
		requiredLabels[parts[0]] = parts[1]
	}

	// Check if pod has all required labels
	for key, value := range requiredLabels {
		if podLabels[key] != value {
			return false
		}
	}

	return true
}

// podHasRequiredAnnotations checks if a pod has all the required annotations
func (d *DeploymentOperations) podHasRequiredAnnotations(podAnnotations map[string]string) bool {
	// If no annotations required, return true
	if len(d.podAnnotations) == 0 {
		return true
	}

	// Parse required annotations
	requiredAnnotations := make(map[string]string)
	for _, annotation := range d.podAnnotations {
		parts := strings.Split(annotation, "=")
		if len(parts) != 2 {
			continue
		}
		requiredAnnotations[parts[0]] = parts[1]
	}

	// Check if pod has all required annotations
	for key, value := range requiredAnnotations {
		if podAnnotations[key] != value {
			return false
		}
	}

	return true
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

			// Create a map of deployment names that have matching pods
			deploymentPods := make(map[string]bool)
			for _, pod := range allPods.Items {
				// Check if pod has required labels and annotations
				if d.podHasRequiredLabels(pod.Labels) && d.podHasRequiredAnnotations(pod.Annotations) {
					// Find which deployment this pod belongs to
					for _, owner := range pod.OwnerReferences {
						if owner.Kind == "ReplicaSet" {
							rs, err := d.clientset.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
							if err != nil {
								continue
							}
							for _, rsOwner := range rs.OwnerReferences {
								if rsOwner.Kind == "Deployment" {
									deploymentPods[rsOwner.Name] = true
									break
								}
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
					primaryName := deployment.Name + "-primary"
					_, err := d.clientset.AppsV1().Deployments(namespace).Get(ctx, primaryName, metav1.GetOptions{})
					if err != nil {
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

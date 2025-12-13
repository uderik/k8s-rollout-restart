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

// StatefulSetOperations implements StatefulSetOperator interface
type StatefulSetOperations struct {
	clientset      K8sClient
	parallel       int
	timeout        int
	dryRun         bool
	log            *logger.Logger
	minAge         *time.Duration
	podLabels      []string
	podAnnotations []string
	skipWait       bool
}

// NewStatefulSetOperations creates a new StatefulSetOperations instance
func NewStatefulSetOperations(clientset K8sClient, parallel, timeout int, _, dryRun bool, minAge *time.Duration, podLabels []string, podAnnotations []string, skipWait bool) *StatefulSetOperations {
	return &StatefulSetOperations{
		clientset:      clientset,
		parallel:       parallel,
		timeout:        timeout,
		dryRun:         dryRun,
		log:            logger.NewLogger(dryRun),
		minAge:         minAge,
		podLabels:      podLabels,
		podAnnotations: podAnnotations,
		skipWait:       skipWait,
	}
}

// RestartStatefulSets restarts all statefulsets in the given namespaces
func (s *StatefulSetOperations) RestartStatefulSets(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		s.log.Info("No namespaces specified, skipping statefulsets restart")
		return nil
	}

	// For each namespace, restart statefulsets
	var wg sync.WaitGroup
	errorCh := make(chan error, len(namespaces))

	for _, ns := range namespaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.restartStatefulSetsInNamespace(ctx, ns); err != nil {
				errorCh <- fmt.Errorf("failed to restart statefulsets in namespace %s: %w", ns, err)
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

// restartStatefulSetsInNamespace restarts all statefulsets in a single namespace
func (s *StatefulSetOperations) restartStatefulSetsInNamespace(ctx context.Context, namespace string) error {
	s.log.Info("Restarting StatefulSets in namespace: %s", namespace)

	statefulsets, err := s.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list StatefulSets in namespace %s: %w", namespace, err)
	}

	if len(statefulsets.Items) == 0 {
		s.log.Info("No StatefulSets found in namespace: %s", namespace)
		return nil
	}

	// Track which StatefulSets we'll restart and which we'll skip
	var toRestart []appsv1.StatefulSet
	var postgresOperatorStatefulSets []string
	var elasticsearchStatefulSets []string

	// Filter StatefulSets
	for _, statefulset := range statefulsets.Items {
		// Skip StatefulSets managed by Zalando PostgreSQL Operator
		if s.isPostgresOperatorStatefulSet(&statefulset) {
			postgresOperatorStatefulSets = append(postgresOperatorStatefulSets,
				fmt.Sprintf("%s (controlled by Zalando PostgreSQL Operator)", statefulset.Name))
			continue
		}

		// Skip Elasticsearch StatefulSets
		if s.isElasticsearchStatefulSet(&statefulset) {
			elasticsearchStatefulSets = append(elasticsearchStatefulSets,
				fmt.Sprintf("%s (Elasticsearch cluster)", statefulset.Name))
			continue
		}

		// Apply age filter if specified
		if s.minAge != nil {
			age := time.Since(statefulset.CreationTimestamp.Time)
			if age < *s.minAge {
				s.log.Info("Skipping StatefulSet %s/%s: too new (age: %s, required: %s)",
					namespace, statefulset.Name, age.Round(time.Second), *s.minAge)
				continue
			}
		}

		// Check if StatefulSet has pods with required labels or annotations
		if len(s.podLabels) > 0 || len(s.podAnnotations) > 0 {
			hasMatchingPods, err := s.hasPodsWithLabelsOrAnnotations(ctx, namespace, statefulset.Name)
			if err != nil {
				s.log.Warning("Failed to check pods for StatefulSet %s: %v", statefulset.Name, err)
				continue
			}
			if !hasMatchingPods {
				s.log.Info("Skipping StatefulSet %s: no pods with required labels or annotations", statefulset.Name)
				continue
			}
		}

		toRestart = append(toRestart, statefulset)
	}

	// Log the skipped PostgreSQL Operator StatefulSets
	if len(postgresOperatorStatefulSets) > 0 {
		s.log.Info("Skipping the following StatefulSets in namespace %s (will be restarted via PostgreSQL Operator):", namespace)
		for _, name := range postgresOperatorStatefulSets {
			s.log.Info("- %s", name)
		}
	}

	// Log the skipped Elasticsearch StatefulSets
	if len(elasticsearchStatefulSets) > 0 {
		s.log.Info("Skipping the following Elasticsearch StatefulSets in namespace %s:", namespace)
		for _, name := range elasticsearchStatefulSets {
			s.log.Info("- %s", name)
		}
	}

	// If no StatefulSets to restart after filtering
	if len(toRestart) == 0 {
		s.log.Info("No eligible StatefulSets to restart in namespace: %s", namespace)
		return nil
	}

	s.log.Info("Found %d StatefulSet(s) to restart in namespace: %s", len(toRestart), namespace)

	if s.dryRun {
		for _, statefulset := range toRestart {
			s.log.Info("Would restart StatefulSet: %s/%s", namespace, statefulset.Name)
		}
		return nil
	}

	// Process each StatefulSet that passed the filters
	for _, statefulset := range toRestart {
		s.log.Info("Restarting StatefulSet: %s/%s", namespace, statefulset.Name)

		if err := s.triggerStatefulSetRollout(ctx, namespace, statefulset.Name); err != nil {
			return fmt.Errorf("failed to trigger rollout for StatefulSet %s/%s: %w", namespace, statefulset.Name, err)
		}

		s.log.Success("Successfully triggered rollout for StatefulSet: %s/%s", namespace, statefulset.Name)
	}

	// Wait for all StatefulSets to be ready if there are any
	if len(toRestart) > 0 {
		if !s.skipWait {
			s.log.Info("Waiting for all StatefulSets to be ready in namespace: %s", namespace)
			// Extract names of StatefulSets that were restarted
			statefulsetNames := make([]string, len(toRestart))
			for i, sts := range toRestart {
				statefulsetNames[i] = sts.Name
			}
			if err := s.waitForStatefulSetsReady(ctx, namespace, statefulsetNames); err != nil {
				return fmt.Errorf("failed to wait for StatefulSets to be ready: %w", err)
			}
			s.log.Success("All StatefulSets are ready in namespace: %s", namespace)
		} else {
			s.log.Info("Skipping wait for StatefulSets readiness (--skip-wait flag is set)")
		}
	}

	return nil
}

// waitForStatefulSetsReady waits for all specified statefulsets to be ready after restart
func (s *StatefulSetOperations) waitForStatefulSetsReady(ctx context.Context, namespace string, statefulsetNames []string) error {
	if len(statefulsetNames) == 0 {
		return nil
	}

	// Create a map of statefulset names for quick lookup with their initial generation
	statefulsetGenerations := make(map[string]int64)
	for _, name := range statefulsetNames {
		sts, err := s.clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get StatefulSet %s: %w", name, err)
		}
		statefulsetGenerations[name] = sts.Generation
	}

	// Wait for statefulsets to be ready
	s.log.Info("Waiting for %d statefulset(s) in namespace %s to become ready", len(statefulsetNames), namespace)

	// Create a timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(s.timeout)*time.Second)
	defer cancel()

	// Check every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for statefulsets to be ready in namespace %s", namespace)
		case <-ticker.C:
			// Get current state of statefulsets
			statefulSetsReady := 0
			totalStatefulSets := len(statefulsetGenerations)
			notReadyStatefulSets := []string{}

			// Check each statefulset
			for statefulsetName := range statefulsetGenerations {
				statefulset, err := s.clientset.AppsV1().StatefulSets(namespace).Get(ctx, statefulsetName, metav1.GetOptions{})
				if err != nil {
					s.log.Warning("Failed to get statefulset %s/%s: %v", namespace, statefulsetName, err)
					notReadyStatefulSets = append(notReadyStatefulSets, statefulsetName)
					continue
				}

				// Check if statefulset generation increased and all conditions are satisfied
				isReady := true

				// Check if generation was observed
				if statefulset.Status.ObservedGeneration < statefulset.Generation {
					isReady = false
				}

				// Check replicas status
				if statefulset.Status.ReadyReplicas != statefulset.Status.Replicas ||
					statefulset.Status.UpdatedReplicas != statefulset.Status.Replicas {
					isReady = false
				}

				if isReady {
					statefulSetsReady++
				} else {
					notReadyStatefulSets = append(notReadyStatefulSets, statefulsetName)
				}
			}

			// Log progress
			if statefulSetsReady == totalStatefulSets {
				s.log.Success("All statefulsets are ready in namespace %s", namespace)
				return nil
			}

			s.log.Info("Waiting for statefulsets to be ready in namespace %s (%d/%d ready)",
				namespace, statefulSetsReady, totalStatefulSets)

			if len(notReadyStatefulSets) > 0 && len(notReadyStatefulSets) <= 5 {
				s.log.Info("StatefulSets not yet ready: %s", strings.Join(notReadyStatefulSets, ", "))
			}
		}
	}
}

// isPostgresOperatorStatefulSet checks if the statefulset is managed by Zalando PostgreSQL Operator
func (s *StatefulSetOperations) isPostgresOperatorStatefulSet(statefulset *appsv1.StatefulSet) bool {
	// Check for Zalando PostgreSQL Operator labels
	if value, exists := statefulset.Labels["application"]; exists && value == "spilo" {
		return true
	}

	// Check for Zalando PostgreSQL Operator cluster-name label
	if _, exists := statefulset.Labels["cluster-name"]; exists {
		return true
	}

	// Check for Zalando PostgreSQL Operator team label
	if _, exists := statefulset.Labels["team"]; exists {
		if _, exists := statefulset.Labels["cluster-name"]; exists {
			return true
		}
	}

	// Check for Zalando PostgreSQL Operator version label
	if _, exists := statefulset.Labels["version"]; exists {
		if _, exists := statefulset.Labels["cluster-name"]; exists {
			return true
		}
	}

	return false
}

// isElasticsearchStatefulSet checks if the statefulset is part of an Elasticsearch cluster
func (s *StatefulSetOperations) isElasticsearchStatefulSet(statefulset *appsv1.StatefulSet) bool {
	// Check for Elastic Cloud on Kubernetes (ECK) operator labels
	if value, exists := statefulset.Labels["common.k8s.elastic.co/type"]; exists && value == "elasticsearch" {
		return true
	}

	// Check for ECK cluster-name label
	if _, exists := statefulset.Labels["elasticsearch.k8s.elastic.co/cluster-name"]; exists {
		return true
	}

	// Check if StatefulSet name contains "elasticsearch"
	if strings.Contains(strings.ToLower(statefulset.Name), "elasticsearch") {
		return true
	}

	// Check for app label with elasticsearch value
	if value, exists := statefulset.Labels["app"]; exists {
		if strings.Contains(strings.ToLower(value), "elasticsearch") {
			return true
		}
	}

	return false
}

// triggerStatefulSetRollout triggers a rollout for a StatefulSet by adding a restart annotation
func (s *StatefulSetOperations) triggerStatefulSetRollout(ctx context.Context, namespace, name string) error {
	// Patch statefulset to trigger a rolling update
	patchData := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`, time.Now().Format(time.RFC3339))
	_, err := s.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patchData), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch StatefulSet %s/%s: %w", namespace, name, err)
	}
	return nil
}

// hasPodsWithLabelsOrAnnotations checks if a StatefulSet has pods with the required labels or annotations
func (s *StatefulSetOperations) hasPodsWithLabelsOrAnnotations(ctx context.Context, namespace, statefulSetName string) (bool, error) {
	// Get pods for this StatefulSet
	pods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", statefulSetName),
	})
	if err != nil {
		return false, fmt.Errorf("failed to list pods for StatefulSet %s: %w", statefulSetName, err)
	}

	// If no pods found, try alternative label selectors
	if len(pods.Items) == 0 {
		// Try with StatefulSet name as label
		pods, err = s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("statefulset=%s", statefulSetName),
		})
		if err != nil {
			return false, fmt.Errorf("failed to list pods for StatefulSet %s: %w", statefulSetName, err)
		}
	}

	// If still no pods, try to find pods by owner reference
	if len(pods.Items) == 0 {
		allPods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list all pods in namespace %s: %w", namespace, err)
		}

		// Find pods owned by this StatefulSet
		for _, pod := range allPods.Items {
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "StatefulSet" && owner.Name == statefulSetName {
					pods.Items = append(pods.Items, pod)
				}
			}
		}
	}

	// Check if any pod has the required labels or annotations
	for _, pod := range pods.Items {
		if s.podHasRequiredLabels(pod.Labels) && s.podHasRequiredAnnotations(pod.Annotations) {
			return true, nil
		}
	}

	return false, nil
}

// podHasRequiredLabels checks if a pod has all the required labels
func (s *StatefulSetOperations) podHasRequiredLabels(podLabels map[string]string) bool {
	requiredLabels := ParseLabelsOrAnnotations(s.podLabels)
	return MatchesRequirements(podLabels, requiredLabels)
}

// podHasRequiredAnnotations checks if a pod has all the required annotations
func (s *StatefulSetOperations) podHasRequiredAnnotations(podAnnotations map[string]string) bool {
	requiredAnnotations := ParseLabelsOrAnnotations(s.podAnnotations)
	return MatchesRequirements(podAnnotations, requiredAnnotations)
}

// GetStatefulSetsToRestart returns a list of statefulsets that would be restarted
func (s *StatefulSetOperations) GetStatefulSetsToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	var allStatefulSets []string

	for _, namespace := range namespaces {
		statefulsets, err := s.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list StatefulSets in namespace %s: %w", namespace, err)
		}

		// Get all pods in the namespace once for efficient filtering
		var podsWithMatchingLabels []string
		if len(s.podLabels) > 0 || len(s.podAnnotations) > 0 {
			allPods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
			}

			// Create a map of statefulset names that have matching pods
			statefulSetPods := make(map[string]bool)
			for _, pod := range allPods.Items {
				// Check if pod has required labels and annotations
				if s.podHasRequiredLabels(pod.Labels) && s.podHasRequiredAnnotations(pod.Annotations) {
					// Find which statefulset this pod belongs to
					for _, owner := range pod.OwnerReferences {
						if owner.Kind == "StatefulSet" {
							statefulSetPods[owner.Name] = true
						}
					}
				}
			}
			podsWithMatchingLabels = make([]string, 0, len(statefulSetPods))
			for statefulSetName := range statefulSetPods {
				podsWithMatchingLabels = append(podsWithMatchingLabels, statefulSetName)
			}
		}

		// Apply the same filtering logic as in RestartStatefulSets
		for _, statefulset := range statefulsets.Items {
			// Skip StatefulSets managed by Zalando PostgreSQL Operator
			if s.isPostgresOperatorStatefulSet(&statefulset) {
				continue
			}

			// Skip Elasticsearch StatefulSets
			if s.isElasticsearchStatefulSet(&statefulset) {
				continue
			}

			// Apply age filter if specified
			if s.minAge != nil {
				age := time.Since(statefulset.CreationTimestamp.Time)
				if age < *s.minAge {
					continue
				}
			}

			// Check if StatefulSet has pods with required labels or annotations
			if len(s.podLabels) > 0 || len(s.podAnnotations) > 0 {
				hasMatchingPods := false
				for _, statefulSetName := range podsWithMatchingLabels {
					if statefulSetName == statefulset.Name {
						hasMatchingPods = true
						break
					}
				}
				if !hasMatchingPods {
					continue
				}
			}

			allStatefulSets = append(allStatefulSets, fmt.Sprintf("%s/%s", namespace, statefulset.Name))
		}
	}

	return allStatefulSets, nil
}

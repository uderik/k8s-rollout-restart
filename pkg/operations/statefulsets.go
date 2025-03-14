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
	clientset K8sClient
	parallel  int
	timeout   int
	dryRun    bool
	noFlagger bool
	log       *logger.Logger
	minAge    *time.Duration
}

// NewStatefulSetOperations creates a new StatefulSetOperations instance
func NewStatefulSetOperations(clientset K8sClient, parallel, timeout int, noFlagger, dryRun bool, minAge *time.Duration) *StatefulSetOperations {
	return &StatefulSetOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		dryRun:    dryRun,
		noFlagger: noFlagger,
		log:       logger.NewLogger(dryRun),
		minAge:    minAge,
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
		ns := ns // Capture for goroutine
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

	// Filter StatefulSets
	for _, statefulset := range statefulsets.Items {
		// Skip StatefulSets managed by Zalando PostgreSQL Operator
		if s.isPostgresOperatorStatefulSet(&statefulset) {
			postgresOperatorStatefulSets = append(postgresOperatorStatefulSets,
				fmt.Sprintf("%s (controlled by Zalando PostgreSQL Operator)", statefulset.Name))
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

		toRestart = append(toRestart, statefulset)
	}

	// Log the skipped PostgreSQL Operator StatefulSets
	if len(postgresOperatorStatefulSets) > 0 {
		s.log.Info("Skipping the following StatefulSets in namespace %s (will be restarted via PostgreSQL Operator):", namespace)
		for _, name := range postgresOperatorStatefulSets {
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
		s.log.Info("Waiting for all StatefulSets to be ready in namespace: %s", namespace)
		if err := s.waitForStatefulSetsReady(ctx, namespace); err != nil {
			return fmt.Errorf("failed to wait for StatefulSets to be ready: %w", err)
		}
		s.log.Success("All StatefulSets are ready in namespace: %s", namespace)
	}

	return nil
}

// waitForStatefulSetsReady waits for all specified statefulsets to be ready after restart
func (s *StatefulSetOperations) waitForStatefulSetsReady(ctx context.Context, namespace string) error {
	// List all statefulsets in the namespace
	statefulsets, err := s.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list statefulsets: %w", err)
	}

	if len(statefulsets.Items) == 0 {
		return nil
	}

	// Create a map of statefulset names for quick lookup with their initial generation
	statefulsetGenerations := make(map[string]int64)
	for _, statefulset := range statefulsets.Items {
		statefulsetGenerations[statefulset.Name] = statefulset.Generation
	}

	// Wait for statefulsets to be ready
	s.log.Info("Waiting for %d statefulset(s) in namespace %s to become ready", len(statefulsets.Items), namespace)

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

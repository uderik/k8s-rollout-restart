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
	// List all statefulsets in the namespace
	statefulsets, err := s.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list statefulsets: %w", err)
	}

	if len(statefulsets.Items) == 0 {
		s.log.Info("No statefulsets found in namespace %s", namespace)
		return nil
	}

	s.log.Info("Found %d statefulset(s) in namespace %s", len(statefulsets.Items), namespace)

	statefulSetsToRestart := make([]appsv1.StatefulSet, 0)
	tooYoungStatefulSets := make([]appsv1.StatefulSet, 0)

	// Include all statefulsets for restart
	for _, statefulset := range statefulsets.Items {
		// Check if statefulset is old enough to restart
		if s.minAge != nil {
			if time.Since(statefulset.CreationTimestamp.Time) < *s.minAge {
				tooYoungStatefulSets = append(tooYoungStatefulSets, statefulset)
				continue
			}
		}

		statefulSetsToRestart = append(statefulSetsToRestart, statefulset)
	}

	if len(statefulSetsToRestart) == 0 {
		s.log.Info("No statefulsets found to restart in namespace %s", namespace)
		return nil
	}

	s.log.Info("Will restart %d/%d statefulset(s) in namespace %s",
		len(statefulSetsToRestart), len(statefulsets.Items), namespace)

	if s.dryRun {
		s.log.Info("StatefulSets that would be restarted in namespace %s:", namespace)
		for _, statefulset := range statefulSetsToRestart {
			s.log.Info("- %s", statefulset.Name)
		}

		if len(tooYoungStatefulSets) > 0 {
			s.log.Info("\nStatefulSets that would be skipped because they are too new in namespace %s:", namespace)
			for _, statefulset := range tooYoungStatefulSets {
				s.log.Info("- %s (age: %s, required: %s)", statefulset.Name,
					time.Since(statefulset.CreationTimestamp.Time).Round(time.Second),
					*s.minAge)
			}
		}

		return nil
	}

	// Create a worker pool for parallel processing
	type restartJob struct {
		namespace   string
		statefulset appsv1.StatefulSet
	}

	jobCh := make(chan restartJob, len(statefulSetsToRestart))
	workerErrCh := make(chan error, len(statefulSetsToRestart))

	// Start workers
	var workerWg sync.WaitGroup
	for i := 0; i < s.parallel; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for job := range jobCh {
				// Patch statefulset to trigger a rolling update
				patchData := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`, time.Now().Format(time.RFC3339))
				_, err := s.clientset.AppsV1().StatefulSets(job.namespace).Patch(ctx, job.statefulset.Name, types.StrategicMergePatchType, []byte(patchData), metav1.PatchOptions{})
				if err != nil {
					workerErrCh <- fmt.Errorf("failed to patch statefulset %s/%s: %w", job.namespace, job.statefulset.Name, err)
					return
				}

				s.log.Success("Successfully restarted statefulset %s/%s", job.namespace, job.statefulset.Name)
			}
		}()
	}

	// Send jobs to workers
	for _, statefulset := range statefulSetsToRestart {
		s.log.Info("Restarting statefulset %s/%s", namespace, statefulset.Name)
		jobCh <- restartJob{
			namespace:   namespace,
			statefulset: statefulset,
		}
	}

	close(jobCh)
	workerWg.Wait()
	close(workerErrCh)

	// Check for errors
	for err := range workerErrCh {
		return err // Return first error encountered
	}

	// Wait for all restarted statefulsets to be ready
	s.log.Info("Waiting for statefulsets to become ready in namespace %s", namespace)
	if err := s.waitForStatefulSetsReady(ctx, namespace, statefulSetsToRestart); err != nil {
		return fmt.Errorf("failed waiting for statefulsets to become ready: %w", err)
	}

	s.log.Success("Successfully restarted %d statefulset(s) in namespace %s",
		len(statefulSetsToRestart), namespace)

	return nil
}

// waitForStatefulSetsReady waits for all specified statefulsets to be ready after restart
func (s *StatefulSetOperations) waitForStatefulSetsReady(ctx context.Context, namespace string, statefulsets []appsv1.StatefulSet) error {
	if len(statefulsets) == 0 {
		return nil
	}

	// Create a map of statefulset names for quick lookup with their initial generation
	statefulsetGenerations := make(map[string]int64)
	for _, statefulset := range statefulsets {
		statefulsetGenerations[statefulset.Name] = statefulset.Generation
	}

	// Wait for statefulsets to be ready
	s.log.Info("Waiting for %d statefulset(s) in namespace %s to become ready", len(statefulsets), namespace)

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

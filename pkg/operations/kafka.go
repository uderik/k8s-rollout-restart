package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// KafkaOperations implements KafkaOperator interface
type KafkaOperations struct {
	clientset K8sClient
	parallel  int
	timeout   int
	dryRun    bool
	log       *logger.Logger
	minAge    *time.Duration
}

// NewKafkaOperations creates a new KafkaOperations instance
func NewKafkaOperations(clientset K8sClient, parallel, timeout int, dryRun bool, minAge *time.Duration) *KafkaOperations {
	return &KafkaOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
		minAge:    minAge,
	}
}

// RestartKafkaClusters restarts all Kafka clusters in the given namespaces
func (k *KafkaOperations) RestartKafkaClusters(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		k.log.Info("No namespaces specified, skipping Kafka cluster restart")
		return nil
	}

	k.log.Info("Starting Kafka clusters restart in %d namespace(s)", len(namespaces))

	// For each namespace, restart Kafka clusters
	var wg sync.WaitGroup
	errorCh := make(chan error, len(namespaces))

	// Channel to track if any Kafka clusters were actually restarted
	clusterFoundCh := make(chan bool, len(namespaces))

	for _, ns := range namespaces {
		ns := ns // Capture for goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := k.restartKafkaClustersInNamespace(ctx, ns)

			// Signal if Kafka clusters were found
			clusterFoundCh <- found

			// Only send error to errorCh if there was an actual error
			if err != nil {
				// Only send serious errors to error channel
				// Informational messages about no clusters found are handled within restartKafkaClustersInNamespace
				errorCh <- fmt.Errorf("failed to restart Kafka clusters in namespace %s: %w", ns, err)
			}
		}()
	}

	wg.Wait()
	close(errorCh)
	close(clusterFoundCh)

	// Check if any Kafka clusters were actually found and restarted
	kafkaClustersFound := false
	for found := range clusterFoundCh {
		if found {
			kafkaClustersFound = true
			break
		}
	}

	// Check for errors
	hasErrors := false
	for err := range errorCh {
		hasErrors = true
		k.log.Warning("%v", err)
		// Continue with other namespaces even if one fails
	}

	// Only show success message if actual Kafka clusters were found and restarted
	if kafkaClustersFound {
		if !hasErrors {
			k.log.Success("Kafka clusters restart completed successfully")
		} else {
			k.log.Info("Kafka clusters restart completed with some warnings")
		}
	}

	return nil
}

// restartKafkaClustersInNamespace restarts all Kafka clusters in a single namespace
// Returns a boolean indicating if any Kafka clusters were found and restarted, and an error if any
func (k *KafkaOperations) restartKafkaClustersInNamespace(ctx context.Context, namespace string) (bool, error) {
	k.log.Info("Checking for Kafka clusters in namespace %s", namespace)

	// Check if Kafka CRD exists
	_, err := k.clientset.RESTClient().Get().AbsPath("/apis/kafka.strimzi.io/v1beta2").DoRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check for Kafka CRD: %w", err)
	}

	// List Kafka clusters
	kafkaList, err := k.clientset.RESTClient().Get().
		AbsPath("/apis/kafka.strimzi.io/v1beta2").
		Namespace(namespace).
		Resource("kafkas").
		DoRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to list Kafka clusters in namespace %s: %w", namespace, err)
	}

	// Parse kafka clusters from the response to check if any exist
	var kafkaClusters struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal(kafkaList, &kafkaClusters); err != nil {
		return false, fmt.Errorf("failed to parse Kafka clusters: %w", err)
	}

	if len(kafkaClusters.Items) == 0 {
		k.log.Info("No Kafka clusters found in namespace %s, skipping", namespace)
		return false, nil
	}

	// Filter clusters by age if minAge is set
	kafkaClustersToRestart := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	tooYoungClusters := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	for _, kafka := range kafkaClusters.Items {
		if k.minAge != nil {
			age := time.Since(kafka.Metadata.CreationTimestamp)
			if age < *k.minAge {
				tooYoungClusters = append(tooYoungClusters, struct {
					Name              string
					CreationTimestamp time.Time
				}{
					Name:              kafka.Metadata.Name,
					CreationTimestamp: kafka.Metadata.CreationTimestamp,
				})
				continue
			}
		}

		kafkaClustersToRestart = append(kafkaClustersToRestart, struct {
			Name              string
			CreationTimestamp time.Time
		}{
			Name:              kafka.Metadata.Name,
			CreationTimestamp: kafka.Metadata.CreationTimestamp,
		})
	}

	// Log the clusters we're about to restart
	k.log.Info("Found %d Kafka cluster(s) in namespace %s, will restart %d cluster(s)",
		len(kafkaClusters.Items), namespace, len(kafkaClustersToRestart))

	if k.dryRun {
		if len(kafkaClustersToRestart) > 0 {
			k.log.Info("Kafka clusters that would be restarted in namespace %s:", namespace)
			for _, kafka := range kafkaClustersToRestart {
				age := time.Since(kafka.CreationTimestamp).Round(time.Second)
				k.log.Info("- %s/%s (age: %s)", namespace, kafka.Name, age)
			}
		}

		if len(tooYoungClusters) > 0 {
			k.log.Info("\nKafka clusters that would be skipped because they are too new in namespace %s:", namespace)
			for _, kafka := range tooYoungClusters {
				age := time.Since(kafka.CreationTimestamp).Round(time.Second)
				k.log.Info("- %s/%s (age: %s, required: %s)", namespace, kafka.Name,
					age, *k.minAge)
			}
		}

		return len(kafkaClustersToRestart) > 0, nil
	}

	if len(kafkaClustersToRestart) == 0 {
		k.log.Info("No Kafka clusters to restart in namespace %s", namespace)
		return false, nil
	}

	// Check if StrimziPodSet API is available
	if !k.checkStrimziPodSetAPIExists(ctx) {
		return false, fmt.Errorf("StrimziPodSet API is not available, cannot restart Kafka clusters")
	}

	// For each Kafka cluster, handle related StrimziPodSets
	for _, kafka := range kafkaClustersToRestart {
		kafkaName := kafka.Name
		k.log.Info("Processing Kafka cluster: %s/%s", namespace, kafkaName)

		// Common StrimziPodSet patterns for Kafka components
		podSetNames := []string{
			// Kafka brokers
			fmt.Sprintf("%s-kafka", kafkaName),
			// Zookeeper
			fmt.Sprintf("%s-zookeeper", kafkaName),
			// Kafka Connect (optional)
			fmt.Sprintf("%s-connect", kafkaName),
			// Kafka Mirror Maker 2 (optional)
			fmt.Sprintf("%s-mirrormaker2", kafkaName),
		}

		podSetsFound := false

		// Add annotations to each StrimziPodSet
		for _, podSetName := range podSetNames {
			exists, err := k.checkStrimziPodSetExists(ctx, namespace, podSetName)
			if err != nil {
				k.log.Warning("Error checking StrimziPodSet %s/%s: %v", namespace, podSetName, err)
				continue
			}

			if exists {
				podSetsFound = true
				k.log.Info("Adding restart annotation to StrimziPodSet %s/%s", namespace, podSetName)

				if err := k.annotateStrimziPodSet(ctx, namespace, podSetName); err != nil {
					k.log.Warning("Failed to annotate StrimziPodSet %s/%s: %v", namespace, podSetName, err)
				} else {
					k.log.Success("Successfully added restart annotation to StrimziPodSet %s/%s", namespace, podSetName)
				}
			}
		}

		if !podSetsFound {
			k.log.Warning("No StrimziPodSets found for Kafka cluster %s/%s", namespace, kafkaName)
		}
	}

	k.log.Success("Successfully initiated restart of Kafka clusters in namespace %s", namespace)

	// Wait for Kafka pods to be fully ready (not just running)
	k.log.Info("Waiting for Kafka pods to complete restart in namespace %s", namespace)
	if err := k.waitForKafkaRestart(ctx, namespace); err != nil {
		return false, fmt.Errorf("failed to wait for Kafka pods to restart: %w", err)
	}

	return true, nil
}

// waitForKafkaPods waits for all Kafka pods to be running
func (k *KafkaOperations) waitForKafkaPods(ctx context.Context, namespace string) error {
	k.log.Info("Waiting for Kafka pods in namespace %s to be ready", namespace)

	// Wait for up to 10 minutes
	timeout := 10 * time.Minute
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Check every 10 seconds
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for Kafka pods to be ready in namespace %s", namespace)
		case <-ticker.C:
			// Get Kafka pods
			pods, err := k.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "strimzi.io/kind=Kafka",
			})
			if err != nil {
				k.log.Warning("Failed to list Kafka pods: %v", err)
				continue
			}

			// Check if all pods are running
			allRunning := true
			for _, pod := range pods.Items {
				if pod.Status.Phase != "Running" {
					allRunning = false
					break
				}
			}

			if allRunning && len(pods.Items) > 0 {
				k.log.Success("All Kafka pods are running in namespace %s", namespace)
				return nil
			}

			k.log.Info("Still waiting for Kafka pods to be ready in namespace %s", namespace)
		}
	}
}

// checkStrimziPodSetAPIExists checks if the StrimziPodSet API is available
func (k *KafkaOperations) checkStrimziPodSetAPIExists(ctx context.Context) bool {
	_, err := k.clientset.RESTClient().
		Get().
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Resource("strimzipodsets").
		DoRaw(ctx)

	return err == nil
}

// checkStrimziPodSetExists checks if a StrimziPodSet exists
func (k *KafkaOperations) checkStrimziPodSetExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := k.clientset.RESTClient().
		Get().
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Namespace(namespace).
		Resource("strimzipodsets").
		Name(name).
		DoRaw(ctx)

	if err != nil {
		// Check if this is a "not found" type error
		if strings.Contains(err.Error(), "not found") ||
			strings.Contains(err.Error(), "could not find") ||
			strings.Contains(err.Error(), "the server could not find the requested resource") {
			// This is an expected case for optional components
			return false, nil
		}
		// This is an unexpected error
		return false, err
	}

	return true, nil
}

// annotateStrimziPodSet adds the manual rolling update annotation to a StrimziPodSet
func (k *KafkaOperations) annotateStrimziPodSet(ctx context.Context, namespace, name string) error {
	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"strimzi.io/manual-rolling-update": "true",
			},
		},
	}

	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return err
	}

	_, err = k.clientset.RESTClient().
		Patch(types.MergePatchType).
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Namespace(namespace).
		Resource("strimzipodsets").
		Name(name).
		Body(patchBytes).
		Do(ctx).
		Raw()

	return err
}

// waitForKafkaRestart waits for all Kafka clusters to complete their restart
func (k *KafkaOperations) waitForKafkaRestart(ctx context.Context, namespace string) error {
	k.log.Info("Waiting for Kafka clusters in namespace %s to complete restart...", namespace)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(k.timeout)*time.Second)
	defer cancel()

	// Check Kafka status by checking pod status
	checkTicker := time.NewTicker(5 * time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for Kafka clusters restart in namespace %s", namespace)
		case <-checkTicker.C:
			// Check if all pods are in Running state
			pods, err := k.clientset.CoreV1().Pods(namespace).List(timeoutCtx, metav1.ListOptions{
				LabelSelector: "strimzi.io/kind=Kafka",
			})
			if err != nil {
				k.log.Warning("Failed to list Kafka pods: %v", err)
				continue
			}

			if len(pods.Items) == 0 {
				k.log.Info("No Kafka pods found in namespace %s, continuing to wait...", namespace)
				continue
			}

			allReady := true
			notReadyPods := 0
			pendingPods := 0

			for _, pod := range pods.Items {
				// Check pod phase first
				if pod.Status.Phase != "Running" {
					allReady = false
					pendingPods++
					continue
				}

				// Then check all containers are ready
				containerReady := true
				for _, condition := range pod.Status.Conditions {
					if condition.Type == "Ready" && condition.Status != "True" {
						containerReady = false
						break
					}
				}

				if !containerReady {
					allReady = false
					notReadyPods++
				}
			}

			if allReady {
				k.log.Success("All Kafka pods are now running and ready in namespace %s", namespace)
				return nil
			}

			k.log.Info("Waiting for Kafka pods in namespace %s... (%d pods, %d not running, %d running but not ready)",
				namespace, len(pods.Items), pendingPods, notReadyPods)
		}
	}
}

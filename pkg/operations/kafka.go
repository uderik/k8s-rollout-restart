package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/k8s-rollout-restart/pkg/logger"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// KafkaOperations handles Kafka-related operations
type KafkaOperations struct {
	client   *kubernetes.Clientset
	parallel int
	timeout  int
	dryRun   bool
	log      *logger.Logger
}

// NewKafkaOperations creates a new KafkaOperations instance
func NewKafkaOperations(client *kubernetes.Clientset, parallel, timeout int, dryRun bool) *KafkaOperations {
	return &KafkaOperations{
		client:   client,
		parallel: parallel,
		timeout:  timeout,
		dryRun:   dryRun,
		log:      logger.NewLogger(dryRun),
	}
}

// RestartKafkaClusters restarts all Kafka clusters in the given namespace using annotations
func (k *KafkaOperations) RestartKafkaClusters(ctx context.Context, namespace string) error {
	// Check if Kafka CRDs exist by listing them
	response, err := k.client.RESTClient().
		Get().
		AbsPath("/apis/kafka.strimzi.io/v1beta2").
		Namespace(namespace).
		Resource("kafkas").
		Do(ctx).
		Raw()
	if err != nil {
		return fmt.Errorf("failed to list Kafka clusters in namespace %s: %w", namespace, err)
	}

	// Parse kafka clusters from the response
	var kafkaClusters []string
	if len(response) > 0 {
		var result struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(response, &result); err == nil {
			for _, kafka := range result.Items {
				kafkaClusters = append(kafkaClusters, kafka.Metadata.Name)
			}
		}
	}

	if len(kafkaClusters) == 0 {
		return fmt.Errorf("no Kafka clusters found in namespace %s", namespace)
	}

	// First, check for StrimziPodSet API existence to avoid repeated errors
	strimziPodSetAPIExists := k.checkStrimziPodSetAPIExists(ctx)
	if !strimziPodSetAPIExists {
		return fmt.Errorf("StrimziPodSet API not available in the cluster")
	}

	if k.dryRun {
		k.log.Info("\nKafka clusters that would be restarted using annotations:")
		for _, cluster := range kafkaClusters {
			k.log.Info("- %s/%s (components: kafka, zookeeper, connect, mirrormaker2)", namespace, cluster)
		}
		return nil
	}

	// Restart each Kafka cluster using annotations
	for _, clusterName := range kafkaClusters {
		// List of components to restart
		components := []string{"kafka", "zookeeper", "connect", "mirrormaker2"}

		for _, component := range components {
			podsetName := fmt.Sprintf("%s-%s", clusterName, component)

			// Check if the StrimziPodSet exists before trying to annotate it
			exists, err := k.checkStrimziPodSetExists(ctx, namespace, podsetName)
			if err != nil {
				// Only show warnings for unexpected errors
				k.log.Warning("Failed to check if StrimziPodSet %s exists: %v", podsetName, err)
				continue
			}

			if !exists {
				// For non-critical components like connect/mirrormaker2, silently skip
				// Only show messages for core components that should exist
				if component == "kafka" || component == "zookeeper" {
					k.log.Info("StrimziPodSet %s/%s not found, skipping", namespace, podsetName)
				}
				continue
			}

			// Apply the annotation to trigger restart
			err = k.annotateStrimziPodSet(ctx, namespace, podsetName)
			if err != nil {
				// Only show warnings for unexpected errors
				k.log.Warning("Failed to annotate StrimziPodSet %s: %v", podsetName, err)
				continue
			}

			k.log.Success("Successfully triggered restart for %s/%s", namespace, podsetName)
		}
	}

	// Wait for the restart to complete
	return k.waitForKafkaRestart(ctx, namespace)
}

// checkStrimziPodSetAPIExists checks if the StrimziPodSet API is available
func (k *KafkaOperations) checkStrimziPodSetAPIExists(ctx context.Context) bool {
	_, err := k.client.RESTClient().
		Get().
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Resource("strimzipodsets").
		Do(ctx).
		Raw()

	return err == nil
}

// checkStrimziPodSetExists checks if a StrimziPodSet exists
func (k *KafkaOperations) checkStrimziPodSetExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := k.client.RESTClient().
		Get().
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Namespace(namespace).
		Resource("strimzipodsets").
		Name(name).
		Do(ctx).
		Raw()

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

	_, err = k.client.RESTClient().
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
			pods, err := k.client.CoreV1().Pods(namespace).List(timeoutCtx, metav1.ListOptions{
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

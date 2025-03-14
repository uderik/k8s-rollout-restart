package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// ResourceTypeKafka represents Kafka resource type
	ResourceTypeKafka ResourceType = "kafka"
)

// KafkaPlugin is a plugin for managing Kafka clusters
type KafkaPlugin struct {
	client *kubernetes.Clientset
	log    *logger.Logger
}

// NewKafkaPlugin creates a new KafkaPlugin
func NewKafkaPlugin(client *kubernetes.Clientset, log *logger.Logger) *KafkaPlugin {
	return &KafkaPlugin{
		client: client,
		log:    log,
	}
}

// GetResourceType returns the resource type handled by this plugin
func (p *KafkaPlugin) GetResourceType() ResourceType {
	return ResourceTypeKafka
}

// SupportedOperations returns the operations supported by this plugin
func (p *KafkaPlugin) SupportedOperations() []Operation {
	return []Operation{OperationRestart}
}

// Execute executes the given operation on Kafka resources
func (p *KafkaPlugin) Execute(ctx context.Context, op Operation, options PluginOptions) error {
	switch op {
	case OperationRestart:
		return p.restartKafkaClusters(ctx, options)
	default:
		return fmt.Errorf("operation %s not supported by Kafka plugin", op)
	}
}

// restartKafkaClusters restarts all Kafka clusters in the given namespace
func (p *KafkaPlugin) restartKafkaClusters(ctx context.Context, options PluginOptions) error {
	namespace := options.Namespace

	// Check if Kafka CRDs exist by listing them
	response, err := p.client.RESTClient().
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
	strimziPodSetAPIExists := p.checkStrimziPodSetAPIExists(ctx)
	if !strimziPodSetAPIExists {
		return fmt.Errorf("StrimziPodSet API not available in the cluster")
	}

	if options.DryRun {
		p.log.Info("\nKafka clusters that would be restarted using annotations:")
		for _, cluster := range kafkaClusters {
			p.log.Info("- %s/%s (components: kafka, zookeeper, connect, mirrormaker2)", namespace, cluster)
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
			exists, err := p.checkStrimziPodSetExists(ctx, namespace, podsetName)
			if err != nil {
				// Only show warnings for unexpected errors
				p.log.Warning("Failed to check if StrimziPodSet %s exists: %v", podsetName, err)
				continue
			}

			if !exists {
				// For non-critical components like connect/mirrormaker2, silently skip
				// Only show messages for core components that should exist
				if component == "kafka" || component == "zookeeper" {
					p.log.Info("StrimziPodSet %s/%s not found, skipping", namespace, podsetName)
				}
				continue
			}

			// Apply the annotation to trigger restart
			err = p.annotateStrimziPodSet(ctx, namespace, podsetName)
			if err != nil {
				// Only show warnings for unexpected errors
				p.log.Warning("Failed to annotate StrimziPodSet %s: %v", podsetName, err)
				continue
			}

			p.log.Success("Successfully triggered restart for %s/%s", namespace, podsetName)
		}
	}

	// Wait for the restart to complete
	return p.waitForKafkaRestart(ctx, namespace, options.Timeout)
}

// checkStrimziPodSetAPIExists checks if the StrimziPodSet API is available
func (p *KafkaPlugin) checkStrimziPodSetAPIExists(ctx context.Context) bool {
	_, err := p.client.RESTClient().
		Get().
		AbsPath("/apis/core.strimzi.io/v1beta2").
		Resource("strimzipodsets").
		Do(ctx).
		Raw()

	return err == nil
}

// checkStrimziPodSetExists checks if a StrimziPodSet exists
func (p *KafkaPlugin) checkStrimziPodSetExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := p.client.RESTClient().
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
func (p *KafkaPlugin) annotateStrimziPodSet(ctx context.Context, namespace, name string) error {
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

	_, err = p.client.RESTClient().
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
func (p *KafkaPlugin) waitForKafkaRestart(ctx context.Context, namespace string, timeout int) error {
	p.log.Info("Waiting for Kafka clusters in namespace %s to complete restart...", namespace)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
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
			pods, err := p.client.CoreV1().Pods(namespace).List(timeoutCtx, metav1.ListOptions{
				LabelSelector: "strimzi.io/kind=Kafka",
			})
			if err != nil {
				p.log.Warning("Failed to list Kafka pods: %v", err)
				continue
			}

			if len(pods.Items) == 0 {
				p.log.Info("No Kafka pods found in namespace %s, continuing to wait...", namespace)
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
				p.log.Success("All Kafka pods are now running and ready in namespace %s", namespace)
				return nil
			}

			p.log.Info("Waiting for Kafka pods in namespace %s... (%d pods, %d not running, %d running but not ready)",
				namespace, len(pods.Items), pendingPods, notReadyPods)
		}
	}
}

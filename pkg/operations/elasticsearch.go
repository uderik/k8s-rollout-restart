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

// ElasticsearchOperations implements ElasticsearchOperator interface
type ElasticsearchOperations struct {
	clientset K8sClient
	parallel  int
	timeout   int
	dryRun    bool
	log       *logger.Logger
	minAge    *time.Duration
	skipWait  bool
}

// NewElasticsearchOperations creates a new ElasticsearchOperations instance
func NewElasticsearchOperations(clientset K8sClient, parallel, timeout int, dryRun bool, minAge *time.Duration, skipWait bool) *ElasticsearchOperations {
	return &ElasticsearchOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
		minAge:    minAge,
		skipWait:  skipWait,
	}
}

// RestartElasticsearchClusters restarts all Elasticsearch clusters in the given namespaces
func (e *ElasticsearchOperations) RestartElasticsearchClusters(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		e.log.Info("No namespaces specified, skipping Elasticsearch cluster restart")
		return nil
	}

	e.log.Info("Starting Elasticsearch clusters restart in %d namespace(s)", len(namespaces))

	// For each namespace, restart Elasticsearch clusters
	var wg sync.WaitGroup
	errorCh := make(chan error, len(namespaces))

	// Channel to track if any Elasticsearch clusters were actually restarted
	clusterFoundCh := make(chan bool, len(namespaces))

	for _, ns := range namespaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := e.restartElasticsearchClustersInNamespace(ctx, ns)

			// Signal if Elasticsearch clusters were found
			clusterFoundCh <- found

			// Only send error to errorCh if there was an actual error
			if err != nil {
				errorCh <- fmt.Errorf("failed to restart Elasticsearch clusters in namespace %s: %w", ns, err)
			}
		}()
	}

	wg.Wait()
	close(errorCh)
	close(clusterFoundCh)

	// Check if any Elasticsearch clusters were actually found and restarted
	elasticsearchClustersFound := false
	for found := range clusterFoundCh {
		if found {
			elasticsearchClustersFound = true
			break
		}
	}

	// Check for errors
	hasErrors := false
	for err := range errorCh {
		hasErrors = true
		e.log.Warning("%v", err)
		// Continue with other namespaces even if one fails
	}

	// Only show success message if actual Elasticsearch clusters were found and restarted
	if elasticsearchClustersFound {
		if !hasErrors {
			e.log.Success("Elasticsearch clusters restart completed successfully")
		} else {
			e.log.Info("Elasticsearch clusters restart completed with some warnings")
		}
	}

	return nil
}

// restartElasticsearchClustersInNamespace restarts all Elasticsearch clusters in a single namespace
// Returns a boolean indicating if any Elasticsearch clusters were found and restarted, and an error if any
func (e *ElasticsearchOperations) restartElasticsearchClustersInNamespace(ctx context.Context, namespace string) (bool, error) {
	e.log.Info("Checking for Elasticsearch clusters in namespace %s", namespace)

	// Check if Elasticsearch CRD exists (ECK operator)
	_, err := e.clientset.RESTClient().Get().AbsPath("/apis/elasticsearch.k8s.elastic.co/v1").DoRaw(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			e.log.Info("Elasticsearch CRD not found, skipping Elasticsearch operations")
			return false, nil
		}
		return false, fmt.Errorf("failed to check for Elasticsearch CRD: %w", err)
	}

	// List Elasticsearch clusters
	elasticsearchList, err := e.clientset.RESTClient().Get().
		AbsPath("/apis/elasticsearch.k8s.elastic.co/v1").
		Namespace(namespace).
		Resource("elasticsearches").
		DoRaw(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, fmt.Errorf("failed to list Elasticsearch clusters in namespace %s: %w", namespace, err)
	}

	// Parse Elasticsearch clusters from the response to check if any exist
	var elasticsearchClusters struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal(elasticsearchList, &elasticsearchClusters); err != nil {
		return false, fmt.Errorf("failed to parse Elasticsearch clusters: %w", err)
	}

	if len(elasticsearchClusters.Items) == 0 {
		e.log.Info("No Elasticsearch clusters found in namespace %s, skipping", namespace)
		return false, nil
	}

	// Filter clusters by age if minAge is set
	elasticsearchClustersToRestart := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	tooYoungClusters := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	for _, es := range elasticsearchClusters.Items {
		if e.minAge != nil {
			age := time.Since(es.Metadata.CreationTimestamp)
			if age < *e.minAge {
				tooYoungClusters = append(tooYoungClusters, struct {
					Name              string
					CreationTimestamp time.Time
				}{
					Name:              es.Metadata.Name,
					CreationTimestamp: es.Metadata.CreationTimestamp,
				})
				continue
			}
		}

		elasticsearchClustersToRestart = append(elasticsearchClustersToRestart, struct {
			Name              string
			CreationTimestamp time.Time
		}{
			Name:              es.Metadata.Name,
			CreationTimestamp: es.Metadata.CreationTimestamp,
		})
	}

	// Log the clusters we're about to restart
	e.log.Info("Found %d Elasticsearch cluster(s) in namespace %s, will restart %d cluster(s)",
		len(elasticsearchClusters.Items), namespace, len(elasticsearchClustersToRestart))

	if e.dryRun {
		if len(elasticsearchClustersToRestart) > 0 {
			e.log.Info("Elasticsearch clusters that would be restarted in namespace %s:", namespace)
			for _, es := range elasticsearchClustersToRestart {
				age := time.Since(es.CreationTimestamp).Round(time.Second)
				e.log.Info("- %s/%s (age: %s)", namespace, es.Name, age)
			}
		}

		if len(tooYoungClusters) > 0 {
			e.log.Info("\nElasticsearch clusters that would be skipped because they are too new in namespace %s:", namespace)
			for _, es := range tooYoungClusters {
				age := time.Since(es.CreationTimestamp).Round(time.Second)
				e.log.Info("- %s/%s (age: %s, required: %s)", namespace, es.Name,
					age, *e.minAge)
			}
		}

		return len(elasticsearchClustersToRestart) > 0, nil
	}

	if len(elasticsearchClustersToRestart) == 0 {
		e.log.Info("No Elasticsearch clusters to restart in namespace %s", namespace)
		return false, nil
	}

	// For each Elasticsearch cluster, add restart annotation
	for _, es := range elasticsearchClustersToRestart {
		esName := es.Name
		e.log.Info("Processing Elasticsearch cluster: %s/%s", namespace, esName)

		// Apply restart annotation to Elasticsearch cluster
		if err := e.applyRestartAnnotation(ctx, namespace, esName); err != nil {
			e.log.Warning("Failed to restart Elasticsearch cluster %s/%s: %v", namespace, esName, err)
			continue
		}

		e.log.Success("Successfully triggered restart for Elasticsearch cluster %s/%s", namespace, esName)
	}

	// Wait for Elasticsearch pods to be fully ready
	if !e.skipWait {
		e.log.Info("Waiting for Elasticsearch pods to complete restart in namespace %s", namespace)
		if err := e.waitForElasticsearchRestart(ctx, namespace); err != nil {
			return false, fmt.Errorf("failed to wait for Elasticsearch pods to restart: %w", err)
		}
	} else {
		e.log.Info("Skipping wait for Elasticsearch pods readiness (--skip-wait flag is set)")
	}

	return true, nil
}

// applyRestartAnnotation adds the restart annotation to an Elasticsearch cluster
func (e *ElasticsearchOperations) applyRestartAnnotation(ctx context.Context, namespace, name string) error {
	// For ECK operator, we need to get the current Elasticsearch CR first
	// to properly patch all nodeSets
	restartTimestamp := time.Now().Format(time.RFC3339)

	// Get the current Elasticsearch CR to see how many nodeSets it has
	esData, err := e.clientset.RESTClient().
		Get().
		AbsPath("/apis/elasticsearch.k8s.elastic.co/v1").
		Namespace(namespace).
		Resource("elasticsearches").
		Name(name).
		DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("failed to get Elasticsearch cluster: %w", err)
	}

	// Parse the Elasticsearch CR to get nodeSets. Pointers let us detect which
	// of the optional intermediate objects (podTemplate, metadata) are absent,
	// so we can create them as needed.
	var esCR struct {
		Spec struct {
			NodeSets []struct {
				Name        string `json:"name"`
				PodTemplate *struct {
					Metadata *struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
				} `json:"podTemplate"`
			} `json:"nodeSets"`
		} `json:"spec"`
	}

	if err := json.Unmarshal(esData, &esCR); err != nil {
		return fmt.Errorf("failed to parse Elasticsearch cluster: %w", err)
	}

	// Build JSON patch operations for each nodeSet. We use "add" rather than
	// "replace": RFC 6902 "replace" requires the target path to already exist,
	// but podTemplate/metadata/annotations are all optional and are usually
	// absent (especially on the first restart, before our annotation exists),
	// which makes the apiserver reject the whole patch with "doc is missing
	// path". "add" creates the value if missing and replaces it otherwise, but
	// it still requires the parent object to exist, so when an intermediate
	// object is absent we add it (with the annotations nested inside) instead.
	type jsonPatchOperation struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}

	var patchOps []jsonPatchOperation

	for i, nodeSet := range esCR.Spec.NodeSets {
		// Merge any existing annotations with our restart annotation.
		annotations := make(map[string]string)
		if nodeSet.PodTemplate != nil && nodeSet.PodTemplate.Metadata != nil {
			for k, v := range nodeSet.PodTemplate.Metadata.Annotations {
				annotations[k] = v
			}
		}
		annotations["elastic.co/restartedAt"] = restartTimestamp

		switch {
		case nodeSet.PodTemplate == nil:
			// podTemplate absent: create it with the metadata/annotations nested.
			patchOps = append(patchOps, jsonPatchOperation{
				Op:   "add",
				Path: fmt.Sprintf("/spec/nodeSets/%d/podTemplate", i),
				Value: map[string]interface{}{
					"metadata": map[string]interface{}{"annotations": annotations},
				},
			})
		case nodeSet.PodTemplate.Metadata == nil:
			// metadata absent: create it with the annotations nested.
			patchOps = append(patchOps, jsonPatchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/nodeSets/%d/podTemplate/metadata", i),
				Value: map[string]interface{}{"annotations": annotations},
			})
		default:
			// metadata exists: add/replace the annotations map directly.
			patchOps = append(patchOps, jsonPatchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("/spec/nodeSets/%d/podTemplate/metadata/annotations", i),
				Value: annotations,
			})
		}
	}

	patchBytes, err := json.Marshal(patchOps)
	if err != nil {
		return err
	}

	_, err = e.clientset.RESTClient().
		Patch(types.JSONPatchType).
		AbsPath("/apis/elasticsearch.k8s.elastic.co/v1").
		Namespace(namespace).
		Resource("elasticsearches").
		Name(name).
		Body(patchBytes).
		Do(ctx).
		Raw()

	return err
}

// waitForElasticsearchRestart waits for all Elasticsearch clusters to complete their restart
func (e *ElasticsearchOperations) waitForElasticsearchRestart(ctx context.Context, namespace string) error {
	e.log.Info("Waiting for Elasticsearch clusters in namespace %s to complete restart...", namespace)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(e.timeout)*time.Second)
	defer cancel()

	// Check Elasticsearch status by checking pod status
	checkTicker := time.NewTicker(5 * time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for Elasticsearch clusters restart in namespace %s", namespace)
		case <-checkTicker.C:
			// Check if all pods are in Running state
			pods, err := e.clientset.CoreV1().Pods(namespace).List(timeoutCtx, metav1.ListOptions{
				LabelSelector: "common.k8s.elastic.co/type=elasticsearch",
			})
			if err != nil {
				e.log.Warning("Failed to list Elasticsearch pods: %v", err)
				continue
			}

			if len(pods.Items) == 0 {
				e.log.Info("No Elasticsearch pods found in namespace %s, continuing to wait...", namespace)
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
				e.log.Success("All Elasticsearch pods are now running and ready in namespace %s", namespace)
				return nil
			}

			e.log.Info("Waiting for Elasticsearch pods in namespace %s... (%d pods, %d not running, %d running but not ready)",
				namespace, len(pods.Items), pendingPods, notReadyPods)
		}
	}
}

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

// PostgresqlOperator interface for PostgreSQL clusters operations
type PostgresqlOperator interface {
	RestartPostgresqlClusters(ctx context.Context, namespaces []string) error
}

// PostgresqlOperations implements PostgresqlOperator interface
type PostgresqlOperations struct {
	clientset K8sClient
	parallel  int
	timeout   int
	dryRun    bool
	log       *logger.Logger
	minAge    *time.Duration
}

// NewPostgresqlOperations creates a new PostgresqlOperations instance
func NewPostgresqlOperations(clientset K8sClient, parallel, timeout int, dryRun bool, minAge *time.Duration) *PostgresqlOperations {
	return &PostgresqlOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
		minAge:    minAge,
	}
}

// RestartPostgresqlClusters restarts all PostgreSQL clusters in the given namespaces
func (p *PostgresqlOperations) RestartPostgresqlClusters(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		p.log.Info("No namespaces specified, skipping PostgreSQL clusters restart")
		return nil
	}

	p.log.Info("Starting PostgreSQL clusters restart in %d namespace(s)", len(namespaces))

	// For each namespace, restart PostgreSQL clusters
	var wg sync.WaitGroup
	errorCh := make(chan error, len(namespaces))

	// Channel to track if any PostgreSQL clusters were actually restarted
	clusterFoundCh := make(chan bool, len(namespaces))

	// Add a channel to track namespaces without PostgreSQL clusters
	emptyNamespacesCh := make(chan string, len(namespaces))

	for _, ns := range namespaces {
		ns := ns // Capture for goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := p.restartPostgresqlClustersInNamespace(ctx, ns, emptyNamespacesCh)

			// Signal if PostgreSQL clusters were found
			clusterFoundCh <- found

			// Only send error to errorCh if there was an actual error
			if err != nil {
				errorCh <- fmt.Errorf("failed to restart PostgreSQL clusters in namespace %s: %w", ns, err)
			}
		}()
	}

	wg.Wait()
	close(errorCh)
	close(clusterFoundCh)
	close(emptyNamespacesCh)

	// Check if any PostgreSQL clusters were actually found and restarted
	postgresqlClustersFound := false
	for found := range clusterFoundCh {
		if found {
			postgresqlClustersFound = true
			break
		}
	}

	// Collect names of empty namespaces
	emptyNamespaces := make([]string, 0)
	for ns := range emptyNamespacesCh {
		emptyNamespaces = append(emptyNamespaces, ns)
	}

	if len(emptyNamespaces) > 0 && p.dryRun {
		p.log.Info("PostgreSQL clusters not found in %d namespaces (skipped)", len(emptyNamespaces))
	}

	// Check for errors
	hasErrors := false
	for err := range errorCh {
		hasErrors = true
		p.log.Warning("%v", err)
		// Continue with other namespaces even if one fails
	}

	// Only show success message if actual PostgreSQL clusters were found and restarted
	if postgresqlClustersFound {
		if !hasErrors {
			p.log.Success("PostgreSQL clusters restart completed successfully")
		} else {
			p.log.Info("PostgreSQL clusters restart completed with some warnings")
		}
	}

	return nil
}

// restartPostgresqlClustersInNamespace restarts all PostgreSQL clusters in a single namespace
// Returns a boolean indicating if any PostgreSQL clusters were found and restarted, and an error if any
func (p *PostgresqlOperations) restartPostgresqlClustersInNamespace(ctx context.Context, namespace string, emptyNamespacesCh chan<- string) (bool, error) {
	// Only log the check message if not in dry-run mode
	if !p.dryRun {
		p.log.Info("Checking for PostgreSQL clusters in namespace %s", namespace)
	}

	// Check if PostgreSQL CRD exists
	_, err := p.clientset.RESTClient().Get().AbsPath("/apis/acid.zalan.do/v1").DoRaw(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			if !p.dryRun {
				p.log.Info("PostgreSQL CRD not found, skipping PostgreSQL operations")
			}
			return false, nil
		}
		return false, fmt.Errorf("failed to check for PostgreSQL CRD: %w", err)
	}

	// List PostgreSQL clusters
	postgresList, err := p.clientset.RESTClient().Get().
		AbsPath("/apis/acid.zalan.do/v1").
		Namespace(namespace).
		Resource("postgresqls").
		DoRaw(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			// Send the namespace name to the channel for counting
			emptyNamespacesCh <- namespace
			return false, nil
		}
		return false, fmt.Errorf("failed to list PostgreSQL clusters in namespace %s: %w", namespace, err)
	}

	// Parse PostgreSQL clusters from the response to check if any exist
	var postgresClusters struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal(postgresList, &postgresClusters); err != nil {
		return false, fmt.Errorf("failed to parse PostgreSQL clusters: %w", err)
	}

	if len(postgresClusters.Items) == 0 {
		// Send the namespace name to the channel for counting
		emptyNamespacesCh <- namespace
		return false, nil
	}

	// Filter clusters by age if minAge is set
	postgresqlClustersToRestart := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	tooYoungClusters := make([]struct {
		Name              string
		CreationTimestamp time.Time
	}, 0)

	for _, postgres := range postgresClusters.Items {
		if p.minAge != nil {
			age := time.Since(postgres.Metadata.CreationTimestamp)
			if age < *p.minAge {
				tooYoungClusters = append(tooYoungClusters, struct {
					Name              string
					CreationTimestamp time.Time
				}{
					Name:              postgres.Metadata.Name,
					CreationTimestamp: postgres.Metadata.CreationTimestamp,
				})
				continue
			}
		}

		postgresqlClustersToRestart = append(postgresqlClustersToRestart, struct {
			Name              string
			CreationTimestamp time.Time
		}{
			Name:              postgres.Metadata.Name,
			CreationTimestamp: postgres.Metadata.CreationTimestamp,
		})
	}

	// Log the clusters we're about to restart
	p.log.Info("Found %d PostgreSQL cluster(s) in namespace %s, will restart %d cluster(s)",
		len(postgresClusters.Items), namespace, len(postgresqlClustersToRestart))

	if p.dryRun {
		if len(postgresqlClustersToRestart) > 0 {
			p.log.Info("PostgreSQL clusters that would be restarted in namespace %s:", namespace)
			for _, postgres := range postgresqlClustersToRestart {
				age := time.Since(postgres.CreationTimestamp).Round(time.Second)
				p.log.Info("- %s/%s (age: %s)", namespace, postgres.Name, age)
			}
		}

		if len(tooYoungClusters) > 0 {
			p.log.Info("\nPostgreSQL clusters that would be skipped because they are too new in namespace %s:", namespace)
			for _, postgres := range tooYoungClusters {
				age := time.Since(postgres.CreationTimestamp).Round(time.Second)
				p.log.Info("- %s/%s (age: %s, required: %s)", namespace, postgres.Name,
					age, *p.minAge)
			}
		}

		return len(postgresqlClustersToRestart) > 0, nil
	}

	if len(postgresqlClustersToRestart) == 0 {
		p.log.Info("No PostgreSQL clusters to restart in namespace %s", namespace)
		return false, nil
	}

	// For each PostgreSQL cluster, add restart annotation
	for _, postgres := range postgresqlClustersToRestart {
		postgresName := postgres.Name
		p.log.Info("Processing PostgreSQL cluster: %s/%s", namespace, postgresName)

		// Apply restart annotation to PostgreSQL cluster
		if err := p.applyRestartAnnotation(ctx, namespace, postgresName); err != nil {
			p.log.Warning("Failed to restart PostgreSQL cluster %s/%s: %v", namespace, postgresName, err)
			continue
		}

		p.log.Success("Successfully triggered restart for PostgreSQL cluster %s/%s", namespace, postgresName)
	}

	// Wait for PostgreSQL pods to be fully ready
	p.log.Info("Waiting for PostgreSQL pods to complete restart in namespace %s", namespace)
	if err := p.waitForPostgresqlRestart(ctx, namespace); err != nil {
		return false, fmt.Errorf("failed to wait for PostgreSQL pods to restart: %w", err)
	}

	return true, nil
}

// applyRestartAnnotation adds the restart annotation to a PostgreSQL cluster
func (p *PostgresqlOperations) applyRestartAnnotation(ctx context.Context, namespace, name string) error {
	// Define the annotation to trigger a rolling update
	// For Zalando Postgres Operator, we'll use the standard timestamp annotation pattern
	restartTimestamp := time.Now().Format(time.RFC3339)

	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"zalando.org/postgres-operator-restart": restartTimestamp,
			},
		},
	}

	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return err
	}

	_, err = p.clientset.RESTClient().
		Patch(types.MergePatchType).
		AbsPath("/apis/acid.zalan.do/v1").
		Namespace(namespace).
		Resource("postgresqls").
		Name(name).
		Body(patchBytes).
		Do(ctx).
		Raw()

	return err
}

// waitForPostgresqlRestart waits for all PostgreSQL clusters to complete their restart
func (p *PostgresqlOperations) waitForPostgresqlRestart(ctx context.Context, namespace string) error {
	p.log.Info("Waiting for PostgreSQL clusters in namespace %s to complete restart...", namespace)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(p.timeout)*time.Second)
	defer cancel()

	// Check PostgreSQL status by checking pod status
	checkTicker := time.NewTicker(5 * time.Second)
	defer checkTicker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for PostgreSQL clusters restart in namespace %s", namespace)
		case <-checkTicker.C:
			// Check if all pods are in Running state
			pods, err := p.clientset.CoreV1().Pods(namespace).List(timeoutCtx, metav1.ListOptions{
				LabelSelector: "application=spilo",
			})
			if err != nil {
				p.log.Warning("Failed to list PostgreSQL pods: %v", err)
				continue
			}

			if len(pods.Items) == 0 {
				p.log.Info("No PostgreSQL pods found in namespace %s, continuing to wait...", namespace)
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
				p.log.Success("All PostgreSQL pods are now running and ready in namespace %s", namespace)
				return nil
			}

			p.log.Info("Waiting for PostgreSQL pods in namespace %s... (%d pods, %d not running, %d running but not ready)",
				namespace, len(pods.Items), pendingPods, notReadyPods)
		}
	}
}

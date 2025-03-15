package cmd

import (
	"context"
	stdcontext "context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/uderik/k8s-rollout-restart/pkg/k8s"
	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"github.com/uderik/k8s-rollout-restart/pkg/operations"
	"github.com/uderik/k8s-rollout-restart/pkg/reporter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	cfgFile       string
	dryRun        bool
	execute       bool
	ctxName       string
	namespaces    []string
	allNamespaces bool
	parallel      int
	timeout       int
	output        string
	noFlagger     bool
	doCordon      bool
	resourceTypes []string
	olderThan     string
	kubeAPIQPS    float32
	kubeAPIBurst  int
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "k8s-rollout-restart",
	Short: "Kubernetes cluster maintenance automation utility",
	Long: `A utility for automating Kubernetes cluster maintenance process, including:
- Temporary marking nodes as unschedulable (cordon)
- Restarting all components (Deployments, StatefulSets)
- Restarting Kafka clusters managed by Strimzi operator
- Verification of successful restart of all services
- Generating cluster state report`,
	RunE: runRoot,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.k8s-rollout-restart.yaml)")
	rootCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "Preview operations without execution")
	rootCmd.Flags().BoolVarP(&execute, "execute", "e", false, "Execute operations")
	rootCmd.Flags().StringVarP(&ctxName, "context", "c", "", "Kubernetes context")
	rootCmd.Flags().StringSliceVarP(&namespaces, "namespace", "n", []string{}, "Kubernetes namespace(s). Multiple namespaces can be specified comma-separated.")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Process resources across all namespaces")
	rootCmd.Flags().IntVarP(&parallel, "parallel", "p", 5, "Parallelism degree")
	rootCmd.Flags().IntVarP(&timeout, "timeout", "t", 300, "Timeout in seconds")
	rootCmd.Flags().StringVarP(&output, "output", "o", "text", "Output format (text|json)")
	rootCmd.Flags().BoolVar(&noFlagger, "no-flagger-filter", false, "Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)")
	rootCmd.Flags().BoolVar(&doCordon, "cordon", false, "Whether to cordon nodes before restart (if not set, nodes will not be cordoned)")
	rootCmd.Flags().StringSliceVar(&resourceTypes, "resources", []string{"deployments"}, "Resource types to restart (deployments, statefulsets, strimzi-kafka, zalando-postgresql, all)")
	rootCmd.Flags().StringVar(&olderThan, "older-than", "", "Restart only resources older than specified duration (e.g. 24h, 30m, 7d)")
	rootCmd.Flags().Float32Var(&kubeAPIQPS, "kube-api-qps", 50, "The maximum queries-per-second of requests sent to the Kubernetes API")
	rootCmd.Flags().IntVar(&kubeAPIBurst, "kube-api-burst", 300, "The maximum burst queries-per-second of requests sent to the Kubernetes API")

	// Mark execute and dry-run as mutually exclusive
	rootCmd.MarkFlagsMutuallyExclusive("dry-run", "execute")

	// Mark namespace and all-namespaces as mutually exclusive
	rootCmd.MarkFlagsMutuallyExclusive("namespace", "all-namespaces")
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.AddConfigPath("$HOME")
		viper.SetConfigName(".k8s-rollout-restart")
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		// Config file found and successfully parsed
	}
}

func runRoot(cmd *cobra.Command, args []string) error {
	// Create logger first, to enable logging as early as possible
	log := logger.NewLogger(dryRun)

	// Set log format if JSON output is requested
	if output == "json" {
		log.SetFormat(logger.JSONFormat)
	}

	// Immediately log the start of execution
	mode := "DRY-RUN"
	if execute {
		mode = "EXECUTE"
	}
	log.Info("Starting k8s-rollout-restart in %s mode", mode)

	// Verify flags
	if !dryRun && !execute {
		return fmt.Errorf("either --dry-run or --execute flag must be specified")
	}

	// Validate resource types first, before any expensive operations
	log.Info("Validating resource types")
	var restartDeployments, restartStatefulSets, restartKafka, restartPostgresql bool
	for _, resourceType := range resourceTypes {
		switch resourceType {
		case "deployments":
			restartDeployments = true
		case "statefulsets":
			restartStatefulSets = true
		case "strimzi-kafka":
			restartKafka = true
		case "zalando-postgresql":
			restartPostgresql = true
		case "all":
			restartDeployments = true
			restartStatefulSets = true
			restartKafka = true
			restartPostgresql = true
		default:
			return fmt.Errorf("invalid resource type: %s", resourceType)
		}
	}

	// Initialize rollback manager
	rollbackMgr := operations.NewRollbackManager(log)

	// Initialize Kubernetes client
	log.Info("Initializing Kubernetes client")
	client, err := k8s.NewClient(ctxName, kubeAPIQPS, kubeAPIBurst)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	k8sClient := client.AsK8sClient()

	// If all-namespaces flag is set, get all namespaces
	if allNamespaces {
		log.Info("Getting all namespaces")
		nsList, err := k8sClient.CoreV1().Namespaces().List(stdcontext.Background(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list namespaces: %w", err)
		}

		namespaces = []string{}
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
		log.Info("Found %d namespaces", len(namespaces))
	}

	// Parse olderThan parameter
	var minAge *time.Duration
	if olderThan != "" {
		duration, err := parseDuration(olderThan)
		if err != nil {
			return fmt.Errorf("invalid older-than value: %v", err)
		}
		minAge = &duration
	}

	// Create operation handlers
	clusterOps := operations.NewClusterOperations(k8sClient, parallel, timeout, noFlagger, dryRun)
	deployOps := operations.NewDeploymentOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge)
	statefulSetOps := operations.NewStatefulSetOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge)
	kafkaOps := operations.NewKafkaOperations(k8sClient, parallel, timeout, dryRun, minAge)
	postgresqlOps := operations.NewPostgresqlOperations(k8sClient, parallel, timeout, dryRun, minAge)
	rep := reporter.NewReporter(client.Clientset())

	// Setup signal handling with rollback
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Warning("Received interrupt signal, initiating graceful shutdown and rollback")

		// Rollback on interrupt if not in dry-run mode
		if !dryRun {
			shutdownCtx, shutdownCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
			defer shutdownCancel()

			if err := rollbackMgr.Rollback(shutdownCtx); err != nil {
				log.Error("Rollback failed: %v", err)
			}
		}

		cancel()
	}()

	// Generate initial report
	log.Info("Generating initial cluster state report")
	initialState, err := rep.GenerateReport(ctx, namespaces)
	if err != nil {
		return fmt.Errorf("failed to generate initial report: %w", err)
	}

	if dryRun {
		log.Info("=== DRY RUN MODE - No changes will be made ===")
		log.Info("\nInitial cluster state:")
		log.Info("Nodes: %d (%d unschedulable)", len(initialState.Nodes), countUnschedulableNodes(initialState))

		totalPods := 0
		if len(namespaces) > 0 {
			for _, ns := range namespaces {
				if pods, exists := initialState.Pods[ns]; exists {
					totalPods += len(pods)
					log.Info("Namespace %s: %d pods", ns, len(pods))
				} else {
					log.Info("Namespace %s: not found or contains no pods", ns)
				}
			}
		} else {
			for ns, pods := range initialState.Pods {
				totalPods += len(pods)
				log.Info("Namespace %s: %d pods", ns, len(pods))
			}
		}
		log.Info("Total pods: %d", totalPods)

		log.Info("\nComponents to be restarted:")
		if len(namespaces) > 0 {
			for _, ns := range namespaces {
				if restartDeployments {
					log.Info("- Deployments in namespace %s will be restarted", ns)
				}
				if restartStatefulSets {
					log.Info("- StatefulSets in namespace %s will be restarted", ns)
				}
				if restartKafka {
					log.Info("- Kafka clusters in namespace %s will be restarted", ns)
				}
				if restartPostgresql {
					log.Info("- PostgreSQL clusters in namespace %s will be restarted", ns)
				}
			}
		} else {
			if restartDeployments {
				log.Info("- Deployments: %d", initialState.Components.Deployments)
			}
			if restartStatefulSets {
				log.Info("- StatefulSets: %d", initialState.Components.StatefulSets)
			}
			if restartKafka {
				log.Info("- Kafka clusters: %d", initialState.Components.Kafka)
			}
			if restartPostgresql {
				log.Info("- PostgreSQL clusters: %d", initialState.Components.Postgresql)
			}
		}

		log.Info("\nOperations that would be performed:")
		if len(namespaces) > 0 {
			if doCordon {
				log.Info("1. Cordon nodes with pods from namespace: %s", namespaces[0])
				operationNumber := 2
				if restartDeployments {
					log.Info("%d. Restart all deployments in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartStatefulSets {
					log.Info("%d. Restart all statefulsets in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartKafka {
					log.Info("%d. Restart Kafka clusters in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartPostgresql {
					log.Info("%d. Restart PostgreSQL clusters in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				log.Info("%d. Wait for all components to be ready", operationNumber)
				operationNumber++
				log.Info("%d. Uncordon nodes with pods from namespace: %s", operationNumber, namespaces[0])
			} else {
				operationNumber := 1
				if restartDeployments {
					log.Info("%d. Restart all deployments in namespace: %s (without cordoning nodes)", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartStatefulSets {
					log.Info("%d. Restart all statefulsets in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartKafka {
					log.Info("%d. Restart Kafka clusters in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				if restartPostgresql {
					log.Info("%d. Restart PostgreSQL clusters in namespace: %s", operationNumber, namespaces[0])
					operationNumber++
				}
				log.Info("%d. Wait for all components to be ready", operationNumber)
			}
		} else {
			if doCordon {
				log.Info("1. Cordon all nodes")
				operationNumber := 2
				if restartDeployments {
					log.Info("%d. Restart all deployments in all namespaces", operationNumber)
					operationNumber++
				}
				if restartStatefulSets {
					log.Info("%d. Restart all statefulsets in all namespaces", operationNumber)
					operationNumber++
				}
				if restartKafka {
					log.Info("%d. Restart Kafka clusters in all namespaces", operationNumber)
					operationNumber++
				}
				if restartPostgresql {
					log.Info("%d. Restart PostgreSQL clusters in all namespaces", operationNumber)
					operationNumber++
				}
				log.Info("%d. Wait for all components to be ready", operationNumber)
				operationNumber++
				log.Info("%d. Uncordon all nodes", operationNumber)
			} else {
				operationNumber := 1
				if restartDeployments {
					log.Info("%d. Restart all deployments in all namespaces (without cordoning nodes)", operationNumber)
					operationNumber++
				}
				if restartStatefulSets {
					log.Info("%d. Restart all statefulsets in all namespaces", operationNumber)
					operationNumber++
				}
				if restartKafka {
					log.Info("%d. Restart Kafka clusters in all namespaces", operationNumber)
					operationNumber++
				}
				if restartPostgresql {
					log.Info("%d. Restart PostgreSQL clusters in all namespaces", operationNumber)
					operationNumber++
				}
				log.Info("%d. Wait for all components to be ready", operationNumber)
			}
		}

		if !noFlagger {
			log.Info("\nNote: For Flagger-managed deployments, only primary deployments will be restarted. Deployments without a primary counterpart will also be restarted.")
		}

		log.Info("\nParallel operations: %d", parallel)
		log.Info("Operation timeout: %d seconds", timeout)

		if doCordon {
			clusterOps.CordonNodes(ctx, namespaces)
		}
		if restartDeployments {
			deployOps.RestartDeployments(ctx, namespaces)
		}
		if restartStatefulSets {
			statefulSetOps.RestartStatefulSets(ctx, namespaces)
		}
		if restartKafka {
			kafkaOps.RestartKafkaClusters(ctx, namespaces)
		}
		if restartPostgresql {
			postgresqlOps.RestartPostgresqlClusters(ctx, namespaces)
		}

		return nil
	}

	if output == "json" {
		jsonData, err := initialState.ToJSON()
		if err != nil {
			return fmt.Errorf("failed to marshal initial state: %w", err)
		}
		fmt.Printf("Initial state:\n%s\n", string(jsonData))
	}

	// Cordon nodes - only if doCordon flag is set
	if doCordon {
		log.Info("Cordoning nodes")
		if err := clusterOps.CordonNodes(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to cordon nodes: %w", err)
		}

		// Register rollback for cordon operation
		if !dryRun {
			rollbackMgr.RegisterRollback("Uncordon nodes", func(rollbackCtx context.Context) error {
				return clusterOps.UncordonNodes(rollbackCtx, namespaces)
			})
		}
	}

	// Restart deployments if selected
	if restartDeployments {
		log.Info("Restarting deployments")
		if err := deployOps.RestartDeployments(ctx, namespaces); err != nil {
			if doCordon && !dryRun {
				// Rollback nodes that were cordoned if deployment restart fails
				rollbackCtx, rollbackCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
				defer rollbackCancel()

				if rollbackErr := rollbackMgr.Rollback(rollbackCtx); rollbackErr != nil {
					log.Error("Rollback failed: %v", rollbackErr)
				}
			}
			return fmt.Errorf("failed to restart deployments: %w", err)
		}
	}

	// Restart statefulsets if selected
	if restartStatefulSets {
		log.Info("Restarting statefulsets")
		if err := statefulSetOps.RestartStatefulSets(ctx, namespaces); err != nil {
			if doCordon && !dryRun {
				// Rollback nodes that were cordoned if statefulset restart fails
				rollbackCtx, rollbackCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
				defer rollbackCancel()

				if rollbackErr := rollbackMgr.Rollback(rollbackCtx); rollbackErr != nil {
					log.Error("Rollback failed: %v", rollbackErr)
				}
			}
			return fmt.Errorf("failed to restart statefulsets: %w", err)
		}
	}

	// Restart Kafka clusters
	if restartKafka {
		log.Info("Restarting Kafka clusters")
		if err := kafkaOps.RestartKafkaClusters(ctx, namespaces); err != nil {
			log.Warning("Failed to restart Kafka clusters: %v", err)
		}
	}

	// Restart PostgreSQL clusters
	if restartPostgresql {
		log.Info("Restarting PostgreSQL clusters")
		if err := postgresqlOps.RestartPostgresqlClusters(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart PostgreSQL clusters: %w", err)
		}
	}

	// Generate final report
	log.Info("Generating final cluster state report")
	finalState, err := rep.GenerateReport(ctx, namespaces)
	if err != nil {
		return fmt.Errorf("failed to generate final report: %w", err)
	}

	if output == "json" {
		jsonData, err := finalState.ToJSON()
		if err != nil {
			return fmt.Errorf("failed to marshal final state: %w", err)
		}
		fmt.Printf("Final state:\n%s\n", string(jsonData))
	}

	// Execute rollback to uncordon nodes if needed
	if doCordon && !dryRun {
		log.Info("Uncordoning nodes")
		if err := clusterOps.UncordonNodes(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to uncordon nodes: %w", err)
		}
		// Clear rollback operations since we manually performed the uncordon
		rollbackMgr.Clear()
	}

	log.Success("Cluster maintenance completed successfully")
	return nil
}

// Helper function to count unschedulable nodes
func countUnschedulableNodes(state *reporter.ClusterState) int {
	count := 0
	for _, node := range state.Nodes {
		if node.Unschedulable {
			count++
		}
	}
	return count
}

// parseDuration extends the standard time.ParseDuration to support days
func parseDuration(durationStr string) (time.Duration, error) {
	// Check for day format (e.g., "7d")
	if strings.Contains(durationStr, "d") {
		parts := strings.Split(durationStr, "d")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid day format in duration: %s", durationStr)
		}

		days, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, fmt.Errorf("invalid number of days: %s", parts[0])
		}

		// Convert days to hours and parse the rest normally
		hoursStr := fmt.Sprintf("%dh%s", days*24, parts[1])
		return time.ParseDuration(hoursStr)
	}

	// Standard duration parsing
	return time.ParseDuration(durationStr)
}

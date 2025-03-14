package cmd

import (
	"context"
	stdcontext "context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k8s-rollout-restart/pkg/k8s"
	"github.com/k8s-rollout-restart/pkg/logger"
	"github.com/k8s-rollout-restart/pkg/operations"
	"github.com/k8s-rollout-restart/pkg/reporter"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile   string
	dryRun    bool
	execute   bool
	ctxName   string
	namespace string
	parallel  int
	timeout   int
	output    string
	noFlagger bool
	doCordon  bool
)

// rootCmd represents the base command
var rootCmd = &cobra.Command{
	Use:   "k8s-rollout-restart",
	Short: "Kubernetes cluster maintenance automation utility",
	Long: `Kubernetes cluster maintenance automation utility.
Performs component restart with dependency handling and state verification.

Kubernetes cluster maintenance automation utility.
Performs component restart with dependency handling and state verification.`,
	RunE: runRoot,
}

// Execute adds all child commands to the root command and sets flags appropriately
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.k8s-rollout-restart.yaml)")
	rootCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "Preview operations without execution")
	rootCmd.Flags().BoolVarP(&execute, "execute", "e", false, "Execute operations")
	rootCmd.Flags().StringVarP(&ctxName, "context", "c", "", "Kubernetes context")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace")
	rootCmd.Flags().IntVarP(&parallel, "parallel", "p", 5, "Parallelism degree")
	rootCmd.Flags().IntVarP(&timeout, "timeout", "t", 300, "Timeout in seconds")
	rootCmd.Flags().StringVarP(&output, "output", "o", "text", "Output format (text|json)")
	rootCmd.Flags().BoolVar(&noFlagger, "no-flagger-filter", false, "Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)")
	rootCmd.Flags().BoolVar(&doCordon, "cordon", false, "Whether to cordon nodes before restart (if not set, nodes will not be cordoned)")

	// Mark execute and dry-run as mutually exclusive
	rootCmd.MarkFlagsMutuallyExclusive("dry-run", "execute")
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
	// Verify flags
	if !dryRun && !execute {
		return fmt.Errorf("either --dry-run or --execute flag must be specified")
	}

	// Create logger
	log := logger.NewLogger(dryRun)

	// Set log format if JSON output is requested
	if output == "json" {
		log.SetFormat(logger.JSONFormat)
	}

	// Initialize rollback manager
	rollbackMgr := operations.NewRollbackManager(log)

	// Initialize Kubernetes client
	client, err := k8s.NewClient(ctxName)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Create operation handlers
	clusterOps := operations.NewClusterOperations(client.Clientset(), parallel, timeout, noFlagger, dryRun)
	deployOps := operations.NewDeploymentOperations(client.Clientset(), parallel, timeout, noFlagger, dryRun)
	kafkaOps := operations.NewKafkaOperations(client.Clientset(), parallel, timeout, dryRun)
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
	initialState, err := rep.GenerateReport(ctx, namespace)
	if err != nil {
		return fmt.Errorf("failed to generate initial report: %w", err)
	}

	if dryRun {
		log.Info("=== DRY RUN MODE - No changes will be made ===")
		log.Info("\nInitial cluster state:")
		log.Info("Nodes: %d (%d unschedulable)", len(initialState.Nodes), countUnschedulableNodes(initialState))

		totalPods := 0
		if namespace != "" {
			if pods, exists := initialState.Pods[namespace]; exists {
				totalPods = len(pods)
				log.Info("Namespace %s: %d pods", namespace, totalPods)
			} else {
				log.Info("Namespace %s: not found or contains no pods", namespace)
			}
		} else {
			for ns, pods := range initialState.Pods {
				totalPods += len(pods)
				log.Info("Namespace %s: %d pods", ns, len(pods))
			}
		}
		log.Info("Total pods: %d", totalPods)

		log.Info("\nComponents to be restarted:")
		if namespace != "" {
			log.Info("- Deployments in namespace %s will be restarted", namespace)
			log.Info("- Kafka clusters in namespace %s will be restarted", namespace)
		} else {
			log.Info("- Deployments: %d", initialState.Components.Deployments)
			log.Info("- StatefulSets: %d", initialState.Components.StatefulSets)
			log.Info("- Kafka clusters: %d", initialState.Components.Kafka)
		}

		log.Info("\nOperations that would be performed:")
		if namespace != "" {
			if doCordon {
				log.Info("1. Cordon nodes with pods from namespace: %s", namespace)
				operationNumber := 2
				log.Info("%d. Restart all deployments in namespace: %s", operationNumber, namespace)
				operationNumber++
				log.Info("%d. Restart Kafka clusters in namespace: %s", operationNumber, namespace)
				operationNumber++
				log.Info("%d. Wait for all components to be ready", operationNumber)
				operationNumber++
				log.Info("%d. Uncordon nodes with pods from namespace: %s", operationNumber, namespace)
			} else {
				log.Info("1. Restart all deployments in namespace: %s (without cordoning nodes)", namespace)
				log.Info("2. Restart Kafka clusters in namespace: %s", namespace)
				log.Info("3. Wait for all components to be ready")
			}
		} else {
			if doCordon {
				log.Info("1. Cordon all nodes")
				operationNumber := 2
				log.Info("%d. Restart all deployments in all namespaces", operationNumber)
				operationNumber++
				log.Info("%d. Restart Kafka clusters in all namespaces", operationNumber)
				operationNumber++
				log.Info("%d. Wait for all components to be ready", operationNumber)
				operationNumber++
				log.Info("%d. Uncordon all nodes", operationNumber)
			} else {
				log.Info("1. Restart all deployments in all namespaces (without cordoning nodes)")
				log.Info("2. Restart Kafka clusters in all namespaces")
				log.Info("3. Wait for all components to be ready")
			}
		}

		if !noFlagger {
			log.Info("\nNote: Only Flagger primary deployments with Canary owner reference will be restarted")
		}

		log.Info("\nParallel operations: %d", parallel)
		log.Info("Operation timeout: %d seconds", timeout)

		if doCordon {
			clusterOps.CordonNodes(ctx, namespace)
		}
		deployOps.RestartDeployments(ctx, namespace)
		kafkaOps.RestartKafkaClusters(ctx, namespace)

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
		if err := clusterOps.CordonNodes(ctx, namespace); err != nil {
			return fmt.Errorf("failed to cordon nodes: %w", err)
		}

		// Register rollback for cordon operation
		if !dryRun {
			rollbackMgr.AddRollbackOperation("Uncordon nodes", func(rollbackCtx context.Context) error {
				return clusterOps.UncordonNodes(rollbackCtx, namespace)
			})
		}
	}

	// Restart deployments
	log.Info("Restarting deployments")
	if err := deployOps.RestartDeployments(ctx, namespace); err != nil {
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

	// Restart Kafka clusters
	log.Info("Restarting Kafka clusters")
	if err := kafkaOps.RestartKafkaClusters(ctx, namespace); err != nil {
		log.Warning("Failed to restart Kafka clusters: %v", err)
	}

	// Generate final report
	log.Info("Generating final cluster state report")
	finalState, err := rep.GenerateReport(ctx, namespace)
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
		if err := clusterOps.UncordonNodes(ctx, namespace); err != nil {
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

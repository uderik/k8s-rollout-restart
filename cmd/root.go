// Package cmd contains CLI commands for k8s-rollout-restart.
package cmd

import (
	stdcontext "context"
	"encoding/json"
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

// Version information (set via ldflags)
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	cfgFile        string
	dryRun         bool
	execute        bool
	ctxName        string
	namespaces     []string
	allNamespaces  bool
	ignoreNS       []string
	parallel       int
	timeout        int
	output         string
	noFlagger      bool
	doCordon       bool
	cordonAllNodes bool
	resourceTypes  []string
	olderThan      string
	kubeAPIQPS     float32
	kubeAPIBurst   int
	nodeLabels     []string
	excludeLabels  []string
	clearCache     bool
	podLabels      []string
	podAnnotations []string
	skipWait       bool
	showVersion    bool
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
- Generating cluster state report

This utility requires a Kubernetes context to be specified using the --context flag.`,
	RunE: runRoot,
	// Allow --version to work without required flags
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Skip validation if version flag is set
		if showVersion {
			return nil
		}
		return nil
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() error {
	// Disable automatic help output from Cobra
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	err := rootCmd.Execute()
	if err != nil {
		// Check if the error is related to flags
		if strings.Contains(err.Error(), "flag") || strings.Contains(err.Error(), "Usage:") {
			// For flag-related errors, show help
			fmt.Fprintf(os.Stderr, "\n")
			_ = rootCmd.Help()
		}
	}
	return err
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.k8s-rollout-restart.yaml)")
	rootCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "Preview operations without execution")
	rootCmd.Flags().BoolVarP(&execute, "execute", "e", false, "Execute operations")
	rootCmd.Flags().StringVarP(&ctxName, "context", "c", "", "Kubernetes context (required unless --version is used)")
	rootCmd.Flags().StringSliceVarP(&namespaces, "namespace", "n", []string{}, "Kubernetes namespace(s). Multiple namespaces can be specified comma-separated.")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Process resources across all namespaces")
	rootCmd.Flags().StringSliceVar(&ignoreNS, "ignore-namespaces", []string{"karpenter"}, "Namespaces to ignore. Multiple namespaces can be specified comma-separated.")
	rootCmd.Flags().IntVarP(&parallel, "parallel", "p", 5, "Parallelism degree")
	rootCmd.Flags().IntVarP(&timeout, "timeout", "t", 300, "Timeout in seconds")
	rootCmd.Flags().StringVarP(&output, "output", "o", "text", "Output format (text|json)")
	rootCmd.Flags().BoolVar(&noFlagger, "no-flagger-filter", false, "Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)")
	rootCmd.Flags().BoolVar(&doCordon, "cordon", false, "Whether to cordon nodes before restart (if not set, nodes will not be cordoned)")
	rootCmd.Flags().BoolVar(&cordonAllNodes, "cordon-all-nodes", false, "Cordon all nodes in the cluster, not just those with pods from specified namespaces")
	rootCmd.Flags().StringSliceVar(&resourceTypes, "resources", []string{"deployments"}, "Resource types to restart (deployments, statefulsets, strimzi-kafka, zalando-postgresql, elasticsearch, all)")
	rootCmd.Flags().StringVar(&olderThan, "older-than", "", "Restart only resources older than specified duration (e.g. 24h, 30m, 7d)")
	rootCmd.Flags().Float32Var(&kubeAPIQPS, "kube-api-qps", 20, "QPS for Kubernetes API client")
	rootCmd.Flags().IntVar(&kubeAPIBurst, "kube-api-burst", 40, "Burst for Kubernetes API client")
	rootCmd.Flags().StringSliceVar(&nodeLabels, "node-labels", []string{}, "Only cordon nodes with these labels (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&excludeLabels, "exclude-node-labels", []string{"eks.amazonaws.com/compute-type=fargate"}, "Exclude nodes with these labels from cordon (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&podLabels, "pod-labels", []string{}, "Only restart resources that have pods with these labels (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&podAnnotations, "pod-annotations", []string{}, "Only restart resources that have pods with these annotations (format: key=value). Multiple annotations can be specified comma-separated.")
	rootCmd.Flags().BoolVar(&clearCache, "clear-cache", false, "Clear Kubernetes client cache before execution")
	rootCmd.Flags().BoolVar(&skipWait, "skip-wait", false, "Skip waiting for pods to become ready after restart")
	rootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "Show version information")

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

	_ = viper.ReadInConfig() // Config file is optional
}

func runRoot(_ *cobra.Command, _ []string) error {
	// Handle version flag
	if showVersion {
		fmt.Printf("k8s-rollout-restart %s (commit: %s, built: %s)\n", version, commit, date)
		return nil
	}

	// Create logger first, to enable logging as early as possible
	log := logger.NewLogger(dryRun)

	// Set log format if JSON output is requested
	if output == "json" {
		log.SetFormat(logger.JSONFormat)
	}

	// Verify context is specified
	if ctxName == "" {
		return fmt.Errorf("kubernetes context must be specified using --context flag")
	}

	// Immediately log the start of execution
	mode := "DRY-RUN"
	if execute {
		mode = "EXECUTE"
	}
	log.Info("Starting k8s-rollout-restart in %s mode with context: %s", mode, ctxName)

	// Verify flags
	if !dryRun && !execute {
		return fmt.Errorf("either --dry-run or --execute flag must be specified")
	}

	// Validate resource types first, before any expensive operations
	log.Info("Validating resource types")
	var restartDeployments, restartStatefulSets, restartKafka, restartPostgresql, restartElasticsearch bool
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
		case "elasticsearch":
			restartElasticsearch = true
		case "all":
			restartDeployments = true
			restartStatefulSets = true
			restartKafka = true
			restartPostgresql = true
			restartElasticsearch = true
		default:
			return fmt.Errorf("invalid resource type: %s", resourceType)
		}
	}

	// Initialize Kubernetes client
	log.Info("Initializing Kubernetes client")
	client, err := k8s.NewClient(ctxName, kubeAPIQPS, kubeAPIBurst)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Clear cache if requested
	if clearCache {
		log.Info("Clearing Kubernetes client cache")
		client.ClearCache()
	}

	k8sClient := client.AsK8sClient()

	// If all-namespaces flag is set, get all namespaces
	if allNamespaces {
		log.Info("Getting all namespaces")
		namespacesList, err := k8sClient.CoreV1().Namespaces().List(stdcontext.Background(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list namespaces: %w", err)
		}

		// Build a set of namespaces to ignore
		ignoreSet := make(map[string]bool)
		for _, ns := range ignoreNS {
			ignoreSet[ns] = true
		}
		// Always ignore kube-system and kube-public
		ignoreSet["kube-system"] = true
		ignoreSet["kube-public"] = true

		for _, ns := range namespacesList.Items {
			// Skip ignored namespaces
			if ignoreSet[ns.Name] {
				continue
			}
			namespaces = append(namespaces, ns.Name)
		}
	}

	// Validate namespaces
	if len(namespaces) == 0 {
		return fmt.Errorf("no namespaces specified")
	}

	// Parse olderThan parameter
	var minAge *time.Duration
	if olderThan != "" {
		duration, err := parseDuration(olderThan)
		if err != nil {
			return fmt.Errorf("invalid older-than value: %w", err)
		}
		minAge = &duration
	}

	// Create operations
	clusterOps := operations.NewClusterOperations(k8sClient, parallel, timeout, noFlagger, dryRun)
	deploymentOps := operations.NewDeploymentOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge, podLabels, podAnnotations, skipWait)
	statefulSetOps := operations.NewStatefulSetOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge, podLabels, podAnnotations, skipWait)
	kafkaOps := operations.NewKafkaOperations(k8sClient, parallel, timeout, dryRun, minAge, skipWait)
	postgresqlOps := operations.NewPostgresqlOperations(k8sClient, parallel, timeout, dryRun, minAge, skipWait)
	elasticsearchOps := operations.NewElasticsearchOperations(k8sClient, parallel, timeout, dryRun, minAge, skipWait)

	// Initialize reporter
	log.Info("Initializing reporter")
	reporter := reporter.NewReporter(k8sClient)

	// Generate initial report
	log.Info("Generating initial report")
	initialReport, err := reporter.GenerateReport(stdcontext.Background(), namespaces)
	if err != nil {
		return fmt.Errorf("failed to generate initial report: %w", err)
	}

	// If dry-run, just print the report and exit
	if dryRun {
		log.Info("Dry-run mode: would perform the following operations:")

		// Create context for dry-run operations
		ctx := stdcontext.Background()

		// Get specific resources that would be restarted
		if restartDeployments {
			deploymentsToRestart, err := deploymentOps.GetDeploymentsToRestart(ctx, namespaces)
			if err != nil {
				log.Warning("Failed to get deployments to restart: %v", err)
			} else {
				if len(deploymentsToRestart) > 0 {
					log.Info("  - Restart %d deployment(s):", len(deploymentsToRestart))
					for _, deployment := range deploymentsToRestart {
						log.Info("    * %s", deployment)
					}
				} else {
					log.Info("  - No deployments to restart")
				}
			}
		}

		if restartStatefulSets {
			statefulSetsToRestart, err := statefulSetOps.GetStatefulSetsToRestart(ctx, namespaces)
			if err != nil {
				log.Warning("Failed to get statefulsets to restart: %v", err)
			} else {
				if len(statefulSetsToRestart) > 0 {
					log.Info("  - Restart %d statefulset(s):", len(statefulSetsToRestart))
					for _, statefulset := range statefulSetsToRestart {
						log.Info("    * %s", statefulset)
					}
				} else {
					log.Info("  - No statefulsets to restart")
				}
			}
		}

		if restartKafka {
			log.Info("  - Restart Kafka clusters in namespaces: %v", namespaces)
		}
		if restartPostgresql {
			log.Info("  - Restart PostgreSQL clusters in namespaces: %v", namespaces)
		}
		if restartElasticsearch {
			log.Info("  - Restart Elasticsearch clusters in namespaces: %v", namespaces)
		}
		if doCordon {
			log.Info("  - Cordon nodes with pods from namespaces: %v", namespaces)
		}

		// Print initial report
		if output == "json" {
			jsonData, err := json.Marshal(initialReport)
			if err != nil {
				return fmt.Errorf("failed to marshal report to JSON: %w", err)
			}
			fmt.Println(string(jsonData))
		} else {
			log.Info("Initial cluster state:")
			components := make([]string, 0)
			if restartDeployments {
				components = append(components, fmt.Sprintf("Deployments: %d", initialReport.Components.Deployments))
			}
			if restartStatefulSets {
				components = append(components, fmt.Sprintf("StatefulSets: %d", initialReport.Components.StatefulSets))
			}
			if restartKafka {
				components = append(components, fmt.Sprintf("Kafka: %d", initialReport.Components.Kafka))
			}
			if restartPostgresql {
				components = append(components, fmt.Sprintf("PostgreSQL: %d", initialReport.Components.Postgresql))
			}
			if restartElasticsearch {
				components = append(components, fmt.Sprintf("Elasticsearch: %d", initialReport.Components.Elasticsearch))
			}
			log.Info("Nodes: %d, %s, Unschedulable: %d",
				len(initialReport.Nodes),
				strings.Join(components, ", "),
				countUnschedulableNodes(initialReport))
		}
		return nil
	}

	// Setup graceful shutdown handler for uncordoning nodes
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()

	if doCordon {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			sig := <-sigCh
			log.Warning("Received signal %v, uncordoning nodes before exit...", sig)
			if err := clusterOps.UncordonNodes(stdcontext.Background(), namespaces); err != nil {
				log.Error("Failed to uncordon nodes during shutdown: %v", err)
			}
			cancel()
			os.Exit(1)
		}()
	}

	// Execute operations
	if doCordon {
		log.Info("Cordoning nodes")
		if err := clusterOps.CordonNodes(ctx, namespaces, cordonAllNodes, nodeLabels, excludeLabels); err != nil {
			return fmt.Errorf("failed to cordon nodes: %w", err)
		}
	}

	if restartDeployments {
		log.Info("Restarting deployments")
		if err := deploymentOps.RestartDeployments(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart deployments: %w", err)
		}
	}

	if restartStatefulSets {
		log.Info("Restarting statefulsets")
		if err := statefulSetOps.RestartStatefulSets(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart statefulsets: %w", err)
		}
	}

	if restartKafka {
		log.Info("Restarting Kafka clusters")
		if err := kafkaOps.RestartKafkaClusters(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart Kafka clusters: %w", err)
		}
	}

	if restartPostgresql {
		log.Info("Restarting PostgreSQL clusters")
		if err := postgresqlOps.RestartPostgresqlClusters(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart PostgreSQL clusters: %w", err)
		}
	}

	if restartElasticsearch {
		log.Info("Restarting Elasticsearch clusters")
		if err := elasticsearchOps.RestartElasticsearchClusters(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to restart Elasticsearch clusters: %w", err)
		}
	}

	// Generate final report
	log.Info("Generating final report")
	finalReport, err := reporter.GenerateReport(ctx, namespaces)
	if err != nil {
		return fmt.Errorf("failed to generate final report: %w", err)
	}

	// Print final report
	if output == "json" {
		jsonData, err := json.Marshal(finalReport)
		if err != nil {
			return fmt.Errorf("failed to marshal report to JSON: %w", err)
		}
		fmt.Println(string(jsonData))
	} else {
		log.Info("Final cluster state:")
		components := make([]string, 0)
		if restartDeployments {
			components = append(components, fmt.Sprintf("Deployments: %d", finalReport.Components.Deployments))
		}
		if restartStatefulSets {
			components = append(components, fmt.Sprintf("StatefulSets: %d", finalReport.Components.StatefulSets))
		}
		if restartKafka {
			components = append(components, fmt.Sprintf("Kafka: %d", finalReport.Components.Kafka))
		}
		if restartPostgresql {
			components = append(components, fmt.Sprintf("PostgreSQL: %d", finalReport.Components.Postgresql))
		}
		if restartElasticsearch {
			components = append(components, fmt.Sprintf("Elasticsearch: %d", finalReport.Components.Elasticsearch))
		}
		log.Info("Nodes: %d, %s, Unschedulable: %d",
			len(finalReport.Nodes),
			strings.Join(components, ", "),
			countUnschedulableNodes(finalReport))
	}

	// Uncordon nodes if they were cordoned
	if doCordon {
		log.Info("Uncordoning nodes")
		if err := clusterOps.UncordonNodes(ctx, namespaces); err != nil {
			return fmt.Errorf("failed to uncordon nodes: %w", err)
		}
	}

	// Stop signal handler since we're done
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)

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

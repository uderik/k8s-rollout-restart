// Package cmd contains CLI commands for k8s-rollout-restart.
package cmd

import (
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
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
	dryRun          bool
	execute         bool
	ctxName         string
	namespaces      []string
	allNamespaces   bool
	ignoreNS        []string
	parallel        int
	timeout         int
	output          string
	noFlagger       bool
	doCordon        bool
	cordonAllNodes  bool
	resourceTypes   []string
	olderThan       string
	kubeAPIQPS      float32
	kubeAPIBurst    int
	nodeLabels      []string
	excludeLabels   []string
	podLabels       []string
	podAnnotations  []string
	containerImages []string
	skipWait        bool
	showVersion     bool
	kafkaAPIVer     string
	podSetAPIVer    string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "k8s-rollout-restart",
	Short: "Kubernetes cluster maintenance automation utility",
	Long: `A utility for automating Kubernetes cluster maintenance process, including:
- Marking nodes as unschedulable (cordon); nodes are intentionally left cordoned
- Restarting all components (Deployments, StatefulSets)
- Restarting Kafka clusters managed by Strimzi operator
- Verification of successful restart of all services
- Generating cluster state report

This utility requires a Kubernetes context to be specified using the --context flag.`,
	RunE: runRoot,
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
	rootCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "Preview operations without execution")
	rootCmd.Flags().BoolVarP(&execute, "execute", "e", false, "Execute operations")
	rootCmd.Flags().StringVarP(&ctxName, "context", "c", "", "Kubernetes context (required unless --version is used)")
	rootCmd.Flags().StringSliceVarP(&namespaces, "namespace", "n", []string{}, "Kubernetes namespace(s). Multiple namespaces can be specified comma-separated.")
	rootCmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Process resources across all namespaces")
	rootCmd.Flags().StringSliceVar(&ignoreNS, "ignore-namespaces", []string{"karpenter"}, "Namespaces to ignore. Multiple namespaces can be specified comma-separated.")
	rootCmd.Flags().IntVarP(&parallel, "parallel", "p", 5, "Maximum number of restarts in progress at the same time across all namespaces and resource types "+
		"(Deployments, StatefulSets, Kafka and PostgreSQL clusters; each resource is patched and waited on before the next; "+
		"with --skip-wait it limits concurrent patches). "+
		"All targets are planned before anything is changed; a planning error (discovery or selection) restarts nothing. "+
		"Any failure (PATCH rejected or ambiguous, rollout timed out or failed, API error while waiting) stops new launches "+
		"of every resource type; "+
		"with --skip-wait only PATCH results count, but any failed PATCH stops further launches")
	rootCmd.Flags().IntVarP(&timeout, "timeout", "t", 300, "Timeout in seconds, applied per resource (each rollout/cluster restart wait)")
	rootCmd.Flags().StringVarP(&output, "output", "o", "text", "Output format (text|json)")
	rootCmd.Flags().BoolVar(&noFlagger, "no-flagger-filter", false, "Disable Flagger Canary filter (restart all deployments, not just Flagger primary ones)")
	rootCmd.Flags().BoolVar(&doCordon, "cordon", false, "Cordon nodes before restart so pods move to other nodes. Nodes are intentionally LEFT CORDONED afterwards "+
		"(e.g. for Karpenter to remove them) and marked with the "+operations.CordonedByAnnotation+"=<run-id> annotation; "+
		"a 'kubectl uncordon' command for manual rollback is printed at the end")
	rootCmd.Flags().BoolVar(&cordonAllNodes, "cordon-all-nodes", false, "Cordon all nodes in the cluster, not just those with pods from specified namespaces")
	rootCmd.Flags().StringSliceVar(&resourceTypes, "resources", []string{"deployments"}, "Resource types to restart (deployments, statefulsets, strimzi-kafka, zalando-postgresql, all)")
	rootCmd.Flags().StringVar(&olderThan, "older-than", "", "Restart only resources older than specified duration (e.g. 24h, 30m, 7d)")
	rootCmd.Flags().Float32Var(&kubeAPIQPS, "kube-api-qps", 20, "QPS for Kubernetes API client")
	rootCmd.Flags().IntVar(&kubeAPIBurst, "kube-api-burst", 40, "Burst for Kubernetes API client")
	rootCmd.Flags().StringSliceVar(&nodeLabels, "node-labels", []string{}, "Only cordon nodes with these labels (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&excludeLabels, "exclude-node-labels", []string{"eks.amazonaws.com/compute-type=fargate"}, "Exclude nodes with these labels from cordon (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&podLabels, "pod-labels", []string{}, "Only restart resources that have pods with these labels (format: key=value). Multiple labels can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&podAnnotations, "pod-annotations", []string{}, "Only restart resources that have pods with these annotations (format: key=value). Multiple annotations can be specified comma-separated.")
	rootCmd.Flags().StringSliceVar(&containerImages, "container-image", []string{}, "Only restart resources that have pods running a container whose image contains one of these substrings (e.g. proxyv2:1.27.1). Multiple values comma-separated (OR).")
	rootCmd.Flags().BoolVar(&skipWait, "skip-wait", false, "Skip waiting for pods to become ready after restart")
	rootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "Show version information")
	rootCmd.Flags().StringVar(&kafkaAPIVer, "strimzi-kafka-api", operations.DefaultKafkaAPIVersion, "Strimzi Kafka CR API group/version")
	rootCmd.Flags().StringVar(&podSetAPIVer, "strimzi-podset-api", operations.DefaultStrimziPodSetAPIVersion, "Strimzi StrimziPodSet API group/version")

	// Mark execute and dry-run as mutually exclusive
	rootCmd.MarkFlagsMutuallyExclusive("dry-run", "execute")

	// Mark namespace and all-namespaces as mutually exclusive
	rootCmd.MarkFlagsMutuallyExclusive("namespace", "all-namespaces")
}

func runRoot(cmd *cobra.Command, _ []string) error {
	// Handle version flag
	if showVersion {
		fmt.Printf("k8s-rollout-restart %s (commit: %s, built: %s)\n", version, commit, date)
		return nil
	}

	// Validate output format first: it decides how everything else is logged.
	if err := validateOutputFormat(output); err != nil {
		return err
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

	// Validate numeric flags before any expensive work. Non-positive values
	// lead to deadlocks (parallel), instantly-expiring contexts (timeout) or a
	// client rate limiter that rejects every request (qps/burst).
	if parallel < 1 {
		return fmt.Errorf("--parallel must be at least 1 (got %d)", parallel)
	}
	if timeout < 1 {
		return fmt.Errorf("--timeout must be at least 1 second (got %d)", timeout)
	}
	if kubeAPIQPS <= 0 {
		return fmt.Errorf("--kube-api-qps must be greater than 0 (got %g)", kubeAPIQPS)
	}
	if kubeAPIBurst < 1 {
		return fmt.Errorf("--kube-api-burst must be at least 1 (got %d)", kubeAPIBurst)
	}

	// Validate resource types and filters first, before any expensive operations
	log.Info("Validating resource types")
	selection, err := parseResourceTypes(resourceTypes)
	if err != nil {
		return err
	}
	if err := validateExplicitFilterFlags(cmd.Flags().Changed, map[string][]string{
		"pod-labels":          podLabels,
		"pod-annotations":     podAnnotations,
		"container-image":     containerImages,
		"node-labels":         nodeLabels,
		"exclude-node-labels": excludeLabels,
	}); err != nil {
		return err
	}
	if err := validateFilterFlags(podLabels, podAnnotations, containerImages, nodeLabels, excludeLabels); err != nil {
		return err
	}
	if err := validateFilterScope(selection, podLabels, podAnnotations, containerImages); err != nil {
		return err
	}
	restartDeployments := selection.deployments
	restartStatefulSets := selection.statefulSets
	restartKafka := selection.kafka
	restartPostgresql := selection.postgresql

	// Parse olderThan parameter
	minAge, err := parseOlderThan(olderThan)
	if err != nil {
		return err
	}

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
		listCtx, listCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
		namespacesList, err := k8sClient.CoreV1().Namespaces().List(listCtx, metav1.ListOptions{})
		listCancel()
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

	// Create operations. Every handler logs through the configured root
	// logger, so --output=json applies to every line.
	ops := newMaintenanceOps(k8sClient, log, minAge)
	clusterOps := ops.cluster
	reporter := ops.reporter

	// Generate initial report. It is informational only: its lookup errors
	// are warnings. Failures that matter are detected by the restart plan.
	log.Info("Generating initial report")
	initialReportCtx, initialReportCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
	initialReport, err := reporter.GenerateReport(initialReportCtx, namespaces)
	initialReportCancel()
	if err != nil {
		return fmt.Errorf("failed to generate initial report: %w", err)
	}

	flags := restartFlags{
		deployments:   restartDeployments,
		statefulSets:  restartStatefulSets,
		kafka:         restartKafka,
		postgresql:    restartPostgresql,
		cordon:        doCordon,
		cordonAll:     cordonAllNodes,
		outputJSON:    output == "json",
		reportTimeout: time.Duration(timeout) * time.Second,
	}

	// If dry-run, just print the plan and the report and exit
	if dryRun {
		log.Info("Dry-run mode: would perform the following operations:")

		// Bound the dry-run lookups by --timeout so a stuck API server cannot
		// make the preview hang forever.
		ctx, dryRunCancel := stdcontext.WithTimeout(stdcontext.Background(), time.Duration(timeout)*time.Second)
		defer dryRunCancel()

		// The same planning function as execute, so the preview always
		// matches what an execute run would restart.
		phases, planErr := planPhases(ctx, log, namespaces, flags, ops)
		logPlan(log, phases)
		if planErr != nil {
			log.Error("Planning failed: an execute run would neither cordon nor restart anything")
			return fmt.Errorf("planning failed, nothing would be cordoned or restarted: %w", planErr)
		}
		if doCordon {
			// Same selection (and local-PV check) as execute.
			if cordonAllNodes {
				log.Info("  - Cordon all nodes (left cordoned afterwards):")
			} else {
				log.Info("  - Cordon nodes with pods from namespaces %v (left cordoned afterwards):", namespaces)
			}
			if err := clusterOps.CordonNodes(ctx, namespaces, cordonAllNodes, nodeLabels, excludeLabels); err != nil {
				return fmt.Errorf("failed to select nodes to cordon: %w", err)
			}
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
			log.Info("Nodes: %d, %s, Unschedulable: %d",
				len(initialReport.Nodes),
				strings.Join(components, ", "),
				countUnschedulableNodes(initialReport))
		}
		return nil
	}

	// Catch SIGINT/SIGTERM before any cluster mutation starts: the first
	// signal cancels in-flight operations gracefully, a second one forces exit.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Deadlines: every single API request is bounded by k8s.RequestTimeout
	// (rest.Config.Timeout), so pre-wait List/Patch calls cannot hang forever.
	// There is deliberately no overall deadline on the maintenance context:
	// waits legitimately take up to --timeout per phase/target, and each
	// waiter enforces --timeout itself.
	summary := func() {
		if doCordon {
			logCordonedNodes(log, clusterOps.CordonedNodes(), clusterOps.RunID(), ctxName)
		}
	}

	runErr := runInterruptible(stdcontext.Background(), log, sigCh, func(ctx stdcontext.Context) error {
		return runMaintenance(ctx, log, namespaces, flags, ops, nodeLabels, excludeLabels)
	}, func() {
		summary()
		os.Exit(130)
	})
	summary()
	if runErr != nil {
		return runErr
	}

	log.Success("Cluster maintenance completed successfully")
	return nil
}

// runInterruptible runs work and handles termination signals from sigCh. The
// first signal cancels work's context and waits for it to return; a second
// signal calls forceExit (os.Exit in production). Nodes are never uncordoned,
// so there is no cleanup phase.
func runInterruptible(parent stdcontext.Context, log *logger.Logger, sigCh <-chan os.Signal, work func(stdcontext.Context) error, forceExit func()) error {
	ctx, cancel := stdcontext.WithCancel(parent)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- work(ctx) }()

	var firstSig os.Signal
	for {
		select {
		case err := <-done:
			if firstSig == nil {
				return err
			}
			interrupted := fmt.Errorf("interrupted by signal %v", firstSig)
			if err != nil && !errors.Is(err, stdcontext.Canceled) {
				return errors.Join(interrupted, err)
			}
			return interrupted
		case sig := <-sigCh:
			if firstSig == nil {
				firstSig = sig
				log.Warning("Received signal %v, stopping in-flight operations (send it again to force exit)...", sig)
				cancel()
				continue
			}
			log.Error("Received second signal %v, forcing exit", sig)
			forceExit()
			return fmt.Errorf("forced exit after second signal %v", sig)
		}
	}
}

// newMaintenanceOps creates every operation handler and the reporter from the
// global flags. All of them log through log (the configured root logger), so
// that --output=json yields JSON on every stdout line.
func newMaintenanceOps(k8sClient operations.K8sClient, log *logger.Logger, minAge *time.Duration) maintenanceOps {
	clusterOps := operations.NewClusterOperations(k8sClient, parallel, timeout, noFlagger, dryRun)
	clusterOps.SetLogger(log)
	deploymentOps := operations.NewDeploymentOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge, podLabels, podAnnotations, containerImages, skipWait)
	deploymentOps.SetLogger(log)
	statefulSetOps := operations.NewStatefulSetOperations(k8sClient, parallel, timeout, noFlagger, dryRun, minAge, podLabels, podAnnotations, containerImages, skipWait)
	statefulSetOps.SetLogger(log)
	kafkaOps := operations.NewKafkaOperations(k8sClient, parallel, timeout, dryRun, minAge, skipWait, kafkaAPIVer, podSetAPIVer)
	kafkaOps.SetLogger(log)
	postgresqlOps := operations.NewPostgresqlOperations(k8sClient, parallel, timeout, dryRun, minAge, skipWait)
	postgresqlOps.SetLogger(log)
	// One run ID for node cordon markers and PostgreSQL pod markers.
	postgresqlOps.SetRunID(clusterOps.RunID())
	postgresqlOps.SetKubeContext(ctxName)
	// One run-wide stop signal: after any restart failure no new restart
	// starts for any resource type.
	runStop := operations.NewRunStop()
	deploymentOps.SetRunStop(runStop)
	statefulSetOps.SetRunStop(runStop)
	kafkaOps.SetRunStop(runStop)
	postgresqlOps.SetRunStop(runStop)

	log.Info("Initializing reporter")
	rep := reporter.NewReporter(k8sClient,
		reporter.WithLogger(log),
		reporter.WithParallelism(parallel),
		reporter.WithKafkaAPIVersion(kafkaAPIVer),
		// Nodes cordoned by this run stay in the final report even after
		// the restart moved all pods away from them.
		reporter.WithCordonedNodes(clusterOps.CordonedNodes),
	)
	return maintenanceOps{
		cluster:     clusterOps,
		deployment:  deploymentOps,
		statefulSet: statefulSetOps,
		kafka:       kafkaOps,
		postgresql:  postgresqlOps,
		reporter:    rep,
	}
}

// logPlan prints the restart plan. Dry-run and execute print it the same
// way, from the same planning function.
func logPlan(log *logger.Logger, phases []restartPhase) {
	log.Info("Restart plan:")
	for _, ph := range phases {
		if len(ph.targets) == 0 {
			log.Info("  - %s: none to restart", ph.label)
			continue
		}
		log.Info("  - %s: %d to restart", ph.label, len(ph.targets))
		for _, t := range ph.targets {
			log.Info("    * %s", t)
		}
	}
}

// logCordonedNodes prints the nodes cordoned by this run and a ready-to-copy
// command for manual rollback. Nodes are intentionally left cordoned.
func logCordonedNodes(log *logger.Logger, nodes []string, runID, kubeContext string) {
	if len(nodes) == 0 {
		log.Info("No nodes were cordoned by this run")
		return
	}
	log.Info("%d node(s) cordoned by this run (%s=%s) are intentionally left cordoned: %s",
		len(nodes), operations.CordonedByAnnotation, runID, strings.Join(nodes, ", "))
	log.Info("To roll back manually: %s", uncordonCommand(nodes, kubeContext))
}

// uncordonCommand returns a kubectl command that uncordons nodes and removes
// this tool's marker annotation. Both commands target the same kube context
// as the run (--context), so copying the hint can never act on the
// current-context of another cluster. All values are shell-quoted. (The tool
// has no --kubeconfig flag: the kubeconfig comes from $KUBECONFIG or
// ~/.kube/config, which kubectl resolves the same way.)
func uncordonCommand(nodes []string, kubeContext string) string {
	quoted := make([]string, len(nodes))
	for i, n := range nodes {
		quoted[i] = operations.ShellQuote(n)
	}
	names := strings.Join(quoted, " ")
	ctxFlag := ""
	if kubeContext != "" {
		ctxFlag = " --context " + operations.ShellQuote(kubeContext)
	}
	return fmt.Sprintf("kubectl%s uncordon %s && kubectl%s annotate node %s %s-",
		ctxFlag, names, ctxFlag, names, operations.CordonedByAnnotation)
}

// restartFlags captures which resource types and behaviors are enabled.
type restartFlags struct {
	deployments  bool
	statefulSets bool
	kafka        bool
	postgresql   bool
	cordon       bool
	cordonAll    bool
	outputJSON   bool
	// reportTimeout bounds the final report generation.
	reportTimeout time.Duration
}

// maintenanceOps bundles the operation handlers used by runMaintenance.
type maintenanceOps struct {
	cluster     *operations.ClusterOperations
	deployment  *operations.DeploymentOperations
	statefulSet *operations.StatefulSetOperations
	kafka       *operations.KafkaOperations
	postgresql  *operations.PostgresqlOperations
	reporter    *reporter.Reporter
}

// restartPhase is one selected resource type of the restart sequence with
// its precomputed plan.
type restartPhase struct {
	label   string   // plural, e.g. "statefulsets"
	kind    string   // singular prefix of not-started targets, e.g. "statefulset"
	targets []string // planned "namespace/name" targets
	// run restarts exactly the planned targets.
	run func(stdcontext.Context) error
}

// planPhases builds the restart plan of every selected resource type, in
// execution order: API discovery and target selection, read-only, with the
// same code the restart uses. Dry-run and execute both call it, so the
// preview always equals what an execute run restarts. Every resource type is
// planned even after an error, so that all planned targets can be reported;
// the returned error joins every planning error.
func planPhases(ctx stdcontext.Context, log *logger.Logger, namespaces []string, flags restartFlags, ops maintenanceOps) ([]restartPhase, error) {
	var (
		phases []restartPhase
		errs   []error
	)
	add := func(label, kind string, targets []string, err error, run func(stdcontext.Context) error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to plan %s: %w", label, err))
		}
		phases = append(phases, restartPhase{label: label, kind: kind, targets: targets, run: run})
	}
	log.Info("Planning restarts (read-only)")
	if flags.deployments {
		plan, err := ops.deployment.PlanDeployments(ctx, namespaces)
		add("deployments", "deployment", plan.Targets(), err, func(ctx stdcontext.Context) error {
			return ops.deployment.RestartPlannedDeployments(ctx, plan)
		})
	}
	if flags.statefulSets {
		plan, err := ops.statefulSet.PlanStatefulSets(ctx, namespaces)
		add("statefulsets", "statefulset", plan.Targets(), err, func(ctx stdcontext.Context) error {
			return ops.statefulSet.RestartPlannedStatefulSets(ctx, plan)
		})
	}
	if flags.kafka {
		plan, err := ops.kafka.PlanKafkaClusters(ctx, namespaces)
		add("Kafka clusters", "kafka", plan.Targets(), err, func(ctx stdcontext.Context) error {
			return ops.kafka.RestartPlannedKafkaClusters(ctx, plan)
		})
	}
	if flags.postgresql {
		plan, err := ops.postgresql.PlanPostgresqlClusters(ctx, namespaces)
		add("PostgreSQL clusters", "postgresql", plan.Targets(), err, func(ctx stdcontext.Context) error {
			return ops.postgresql.RestartPlannedPostgresqlClusters(ctx, plan)
		})
	}
	return phases, errors.Join(errs...)
}

// notStartedPhaseErr reports every planned target of a resource type that is
// not started because of an earlier failure (or an interruption).
func notStartedPhaseErr(ctx stdcontext.Context, log *logger.Logger, ph restartPhase) error {
	if err := ctx.Err(); err != nil {
		log.Error("%s not started: interrupted", ph.label)
		return fmt.Errorf("%s not started: %w", ph.label, err)
	}
	if len(ph.targets) == 0 {
		return nil
	}
	errs := make([]error, 0, len(ph.targets))
	for _, t := range ph.targets {
		errs = append(errs, fmt.Errorf("%s %s: %w", ph.kind, t, operations.ErrNotStartedEarlierFailure))
	}
	log.Error("%d %s not started due to earlier failure: %s", len(ph.targets), ph.label, strings.Join(ph.targets, ", "))
	return fmt.Errorf("%s: %w", ph.label, errors.Join(errs...))
}

// runMaintenance plans, cordons, restarts and prints the final report. Nodes
// cordoned here are intentionally never uncordoned.
//
// Plan before mutate: the targets of every selected resource type are
// planned first (planPhases). If planning fails anywhere (API discovery,
// a forbidden List, ...), nothing is cordoned or patched: every planned
// target is reported as "not started due to earlier failure" and an error is
// returned. Otherwise the phases restart exactly the planned targets. After
// any later failure (cordon, or any restart failure of a resource type) no
// further resource type is started: every target of the remaining resource
// types is reported as "not started due to earlier failure" and an error is
// returned.
func runMaintenance(ctx stdcontext.Context, log *logger.Logger, namespaces []string, flags restartFlags, ops maintenanceOps, nodeLabels, excludeLabels []string) error {
	// Like every other pre-wait request, each planning request is bounded by
	// the client request timeout; there is no overall deadline.
	phases, planErr := planPhases(ctx, log, namespaces, flags, ops)
	logPlan(log, phases)
	if planErr != nil {
		log.Error("Planning failed: nothing is cordoned or restarted")
		errs := []error{planErr}
		for _, ph := range phases {
			if err := notStartedPhaseErr(ctx, log, ph); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	var errs []error
	if flags.cordon {
		log.Info("Cordoning nodes")
		if err := ops.cluster.CordonNodes(ctx, namespaces, flags.cordonAll, nodeLabels, excludeLabels); err != nil {
			errs = append(errs, fmt.Errorf("failed to cordon nodes: %w", err))
		}
	}

	for _, ph := range phases {
		if len(errs) > 0 || ctx.Err() != nil {
			if err := notStartedPhaseErr(ctx, log, ph); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		log.Info("Restarting %s", ph.label)
		if err := ph.run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to restart %s: %w", ph.label, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// Generate final report
	log.Info("Generating final report")
	reportCtx, reportCancel := stdcontext.WithTimeout(ctx, flags.reportTimeout)
	finalReport, err := ops.reporter.GenerateReport(reportCtx, namespaces)
	reportCancel()
	if err != nil {
		return fmt.Errorf("failed to generate final report: %w", err)
	}

	// Print final report
	if flags.outputJSON {
		jsonData, err := json.Marshal(finalReport)
		if err != nil {
			return fmt.Errorf("failed to marshal report to JSON: %w", err)
		}
		fmt.Println(string(jsonData))
	} else {
		log.Info("Final cluster state:")
		components := make([]string, 0)
		if flags.deployments {
			components = append(components, fmt.Sprintf("Deployments: %d", finalReport.Components.Deployments))
		}
		if flags.statefulSets {
			components = append(components, fmt.Sprintf("StatefulSets: %d", finalReport.Components.StatefulSets))
		}
		if flags.kafka {
			components = append(components, fmt.Sprintf("Kafka: %d", finalReport.Components.Kafka))
		}
		if flags.postgresql {
			components = append(components, fmt.Sprintf("PostgreSQL: %d", finalReport.Components.Postgresql))
		}
		log.Info("Nodes: %d, %s, Unschedulable: %d",
			len(finalReport.Nodes),
			strings.Join(components, ", "),
			countUnschedulableNodes(finalReport))
	}

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

// resourceSelection records which resource types were requested via --resources.
type resourceSelection struct {
	deployments  bool
	statefulSets bool
	kafka        bool
	postgresql   bool
}

// operatorResources reports whether any operator-managed custom resource
// (Kafka, PostgreSQL) is selected.
func (r resourceSelection) operatorResources() bool {
	return r.kafka || r.postgresql
}

// parseResourceTypes validates --resources and converts it into a selection.
func parseResourceTypes(types []string) (resourceSelection, error) {
	var sel resourceSelection
	if len(types) == 0 {
		return sel, fmt.Errorf("--resources must not be empty (valid: deployments, statefulsets, strimzi-kafka, zalando-postgresql, all)")
	}
	for _, resourceType := range types {
		switch resourceType {
		case "deployments":
			sel.deployments = true
		case "statefulsets":
			sel.statefulSets = true
		case "strimzi-kafka":
			sel.kafka = true
		case "zalando-postgresql":
			sel.postgresql = true
		case "elasticsearch":
			return sel, fmt.Errorf("elasticsearch support was removed; restart ECK-managed Elasticsearch clusters through ECK itself")
		case "all":
			sel = resourceSelection{
				deployments:  true,
				statefulSets: true,
				kafka:        true,
				postgresql:   true,
			}
		default:
			return sel, fmt.Errorf("invalid resource type: %q", resourceType)
		}
	}
	return sel, nil
}

// validateOutputFormat checks the -o/--output value.
func validateOutputFormat(format string) error {
	switch format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("invalid --output value %q (valid: text, json)", format)
	}
}

// validateKeyValueList ensures every item has the form key=value with a
// non-empty key. Malformed items would otherwise be silently dropped, turning
// a filter into "match everything".
func validateKeyValueList(flagName string, items []string) error {
	for _, item := range items {
		key, _, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("invalid --%s value %q: expected format key=value", flagName, item)
		}
	}
	return nil
}

// validateExplicitFilterFlags rejects filter flags that were explicitly set to
// an empty value (e.g. --pod-labels=""): pflag turns that into an empty slice,
// which would silently disable the filter instead of narrowing the selection.
func validateExplicitFilterFlags(changed func(string) bool, values map[string][]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !changed(name) {
			continue
		}
		empty := true
		for _, v := range values[name] {
			if strings.TrimSpace(v) != "" {
				empty = false
				break
			}
		}
		if empty {
			return fmt.Errorf("--%s was set to an empty value; omit the flag instead", name)
		}
	}
	return nil
}

// validateFilterFlags validates the pod/node selector flags.
func validateFilterFlags(podLabels, podAnnotations, containerImages, nodeLabels, excludeLabels []string) error {
	for _, kv := range []struct {
		name  string
		items []string
	}{
		{"pod-labels", podLabels},
		{"pod-annotations", podAnnotations},
		{"node-labels", nodeLabels},
		{"exclude-node-labels", excludeLabels},
	} {
		if err := validateKeyValueList(kv.name, kv.items); err != nil {
			return err
		}
	}
	for _, image := range containerImages {
		// An empty substring matches every image, i.e. silently disables the filter.
		if strings.TrimSpace(image) == "" {
			return fmt.Errorf("invalid --container-image value: entries must not be empty")
		}
	}
	return nil
}

// validateFilterScope rejects pod filters combined with operator-managed
// resources. Pod filters are only applied to Deployments and StatefulSets;
// operator CRs would otherwise be restarted regardless of the filter.
func validateFilterScope(sel resourceSelection, podLabels, podAnnotations, containerImages []string) error {
	if len(podLabels) == 0 && len(podAnnotations) == 0 && len(containerImages) == 0 {
		return nil
	}
	if sel.operatorResources() {
		return fmt.Errorf("--pod-labels, --pod-annotations and --container-image only apply to deployments and statefulsets; " +
			"they cannot be combined with strimzi-kafka, zalando-postgresql or all in --resources")
	}
	return nil
}

// parseOlderThan parses --older-than. An empty value disables the filter.
func parseOlderThan(value string) (*time.Duration, error) {
	if value == "" {
		return nil, nil
	}
	duration, err := parseDuration(value)
	if err != nil {
		return nil, fmt.Errorf("invalid older-than value: %w", err)
	}
	if duration <= 0 {
		return nil, fmt.Errorf("invalid older-than value %q: must be greater than 0", value)
	}
	return &duration, nil
}

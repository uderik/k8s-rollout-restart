package cmd

import (
	stdcontext "context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"github.com/uderik/k8s-rollout-restart/pkg/operations"
)

func TestParseDuration(t *testing.T) {
	// Test regular time.Duration formats
	duration, err := parseDuration("1h")
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, duration)

	duration, err = parseDuration("30m")
	assert.NoError(t, err)
	assert.Equal(t, 30*time.Minute, duration)

	duration, err = parseDuration("45s")
	assert.NoError(t, err)
	assert.Equal(t, 45*time.Second, duration)

	// Test day format
	duration, err = parseDuration("1d")
	assert.NoError(t, err)
	assert.Equal(t, 24*time.Hour, duration)

	duration, err = parseDuration("7d")
	assert.NoError(t, err)
	assert.Equal(t, 7*24*time.Hour, duration)

	// Test combined formats
	duration, err = parseDuration("1d12h")
	assert.NoError(t, err)
	assert.Equal(t, 36*time.Hour, duration)

	duration, err = parseDuration("2d30m")
	assert.NoError(t, err)
	assert.Equal(t, 48*time.Hour+30*time.Minute, duration)

	// Test invalid formats
	_, err = parseDuration("")
	assert.Error(t, err)

	_, err = parseDuration("d")
	assert.Error(t, err)

	_, err = parseDuration("1dd")
	assert.Error(t, err)

	_, err = parseDuration("invalid")
	assert.Error(t, err)

	// Note: parseDuration does not currently validate for negative values,
	// as time.ParseDuration accepts them. If this changes in the future,
	// add a test for that here.
}

func TestResourcesFlag_Validation(t *testing.T) {
	sel, err := parseResourceTypes([]string{"deployments", "statefulsets"})
	assert.NoError(t, err)
	assert.Equal(t, resourceSelection{deployments: true, statefulSets: true}, sel)
	assert.False(t, sel.operatorResources())

	sel, err = parseResourceTypes([]string{"all"})
	assert.NoError(t, err)
	assert.True(t, sel.deployments && sel.statefulSets && sel.kafka && sel.postgresql)

	_, err = parseResourceTypes([]string{"deployments", "elasticsearch"})
	if assert.Error(t, err, "elasticsearch must be rejected") {
		assert.Contains(t, err.Error(), "elasticsearch support was removed")
	}

	_, err = parseResourceTypes([]string{})
	assert.Error(t, err, "empty --resources must be rejected")

	_, err = parseResourceTypes(nil)
	assert.Error(t, err)

	_, err = parseResourceTypes([]string{"deployments", ""})
	assert.Error(t, err)

	_, err = parseResourceTypes([]string{"pods"})
	assert.Error(t, err)
}

func TestOlderThanFlag_Parsing(t *testing.T) {
	minAge, err := parseOlderThan("")
	assert.NoError(t, err)
	assert.Nil(t, minAge)

	minAge, err = parseOlderThan("7d")
	assert.NoError(t, err)
	if assert.NotNil(t, minAge) {
		assert.Equal(t, 7*24*time.Hour, *minAge)
	}

	for _, bad := range []string{"0s", "0d", "-1h", "-2d", "abc"} {
		_, err = parseOlderThan(bad)
		assert.Error(t, err, "--older-than %q must be rejected", bad)
	}
}

func TestOutputFormat_Validation(t *testing.T) {
	assert.NoError(t, validateOutputFormat("text"))
	assert.NoError(t, validateOutputFormat("json"))
	assert.Error(t, validateOutputFormat("yaml"))
	assert.Error(t, validateOutputFormat(""))
	assert.Error(t, validateOutputFormat("JSON"))
}

func TestFilterFlags_Validation(t *testing.T) {
	// Valid values, including an empty label value.
	assert.NoError(t, validateFilterFlags(
		[]string{"app=web", "tier="},
		[]string{"sidecar.istio.io/inject=true"},
		[]string{"proxyv2:1.27.1"},
		[]string{"role=worker"},
		[]string{"eks.amazonaws.com/compute-type=fargate"},
	))
	assert.NoError(t, validateFilterFlags(nil, nil, nil, nil, nil))

	tests := []struct {
		name                                           string
		podLabels, podAnnotations, images, nodes, excl []string
		wantFlag                                       string
	}{
		{name: "pod label without =", podLabels: []string{"app"}, wantFlag: "--pod-labels"},
		{name: "pod label empty key", podLabels: []string{"=web"}, wantFlag: "--pod-labels"},
		{name: "pod annotation without =", podAnnotations: []string{"foo"}, wantFlag: "--pod-annotations"},
		{name: "node label without =", nodes: []string{"worker"}, wantFlag: "--node-labels"},
		{name: "exclude label without =", excl: []string{"fargate"}, wantFlag: "--exclude-node-labels"},
		{name: "empty container image", images: []string{""}, wantFlag: "--container-image"},
		{name: "blank container image", images: []string{"proxyv2", "  "}, wantFlag: "--container-image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFilterFlags(tt.podLabels, tt.podAnnotations, tt.images, tt.nodes, tt.excl)
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tt.wantFlag)
			}
		})
	}
}

func TestFilterScope_Validation(t *testing.T) {
	workloads := resourceSelection{deployments: true, statefulSets: true}
	withKafka := resourceSelection{deployments: true, kafka: true}
	all, err := parseResourceTypes([]string{"all"})
	assert.NoError(t, err)

	// Pod filters with workloads only are fine.
	assert.NoError(t, validateFilterScope(workloads, []string{"app=web"}, nil, nil))
	assert.NoError(t, validateFilterScope(workloads, nil, []string{"a=b"}, []string{"img"}))

	// Operator resources without pod filters are fine.
	assert.NoError(t, validateFilterScope(all, nil, nil, nil))

	// Pod filters combined with operator resources are rejected.
	assert.Error(t, validateFilterScope(withKafka, []string{"app=web"}, nil, nil))
	assert.Error(t, validateFilterScope(all, nil, []string{"a=b"}, nil))
	assert.Error(t, validateFilterScope(resourceSelection{kafka: true}, nil, nil, []string{"img"}))
	assert.Error(t, validateFilterScope(resourceSelection{postgresql: true}, []string{"app=db"}, nil, nil))
}

func TestRootCommand_Flags(t *testing.T) {
	for _, name := range []string{
		"dry-run", "execute", "context", "namespace", "all-namespaces", "ignore-namespaces",
		"parallel", "timeout", "output", "cordon", "cordon-all-nodes", "resources", "older-than",
		"node-labels", "exclude-node-labels", "pod-labels", "pod-annotations", "container-image",
		"skip-wait", "version",
	} {
		assert.NotNil(t, rootCmd.Flags().Lookup(name), "flag --%s must exist", name)
	}
	cordon := rootCmd.Flags().Lookup("cordon")
	assert.Contains(t, cordon.Usage, "LEFT CORDONED")
	assert.Contains(t, cordon.Usage, operations.CordonedByAnnotation)
	assert.Contains(t, rootCmd.Flags().Lookup("parallel").Usage, "across all namespaces")
	assert.Contains(t, rootCmd.Flags().Lookup("timeout").Usage, "per resource")
}

// An explicitly empty slice flag parses to an empty slice; only Changed tells
// it apart from "not set".
func TestExplicitEmptyFilterFlags(t *testing.T) {
	for _, name := range []string{"pod-labels", "pod-annotations", "container-image", "node-labels", "exclude-node-labels"} {
		t.Run(name, func(t *testing.T) {
			var values []string
			cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
			cmd.Flags().StringSliceVar(&values, name, []string{"default=x"}, "")
			assert.NoError(t, cmd.ParseFlags([]string{"--" + name + "="}))
			assert.Empty(t, values)
			err := validateExplicitFilterFlags(cmd.Flags().Changed, map[string][]string{name: values})
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), "--"+name)
			}
		})
	}

	notChanged := func(string) bool { return false }
	assert.NoError(t, validateExplicitFilterFlags(notChanged, map[string][]string{"pod-labels": nil}))
	changed := func(string) bool { return true }
	assert.NoError(t, validateExplicitFilterFlags(changed, map[string][]string{"pod-labels": {"app=web"}}))
	assert.Error(t, validateExplicitFilterFlags(changed, map[string][]string{"container-image": {" "}}))
}

// End-to-end through cobra: validation errors happen before any cluster access.
func TestRootCommand_Integration(t *testing.T) {
	saved := struct {
		ctx             string
		dry             bool
		podLabels       []string
		olderThan       string
		containerImages []string
	}{ctxName, dryRun, podLabels, olderThan, containerImages}
	t.Cleanup(func() {
		ctxName, dryRun, podLabels, olderThan, containerImages = saved.ctx, saved.dry, saved.podLabels, saved.olderThan, saved.containerImages
		rootCmd.SetArgs(nil)
	})

	run := func(args ...string) error {
		rootCmd.SetArgs(args)
		return rootCmd.Execute()
	}

	err := run("--context", "test-ctx")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "either --dry-run or --execute")
	}

	err = run("--context", "test-ctx", "--dry-run", "--older-than", "bogus")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "older-than")
	}

	err = run("--context", "test-ctx", "--dry-run", "--older-than", "1h", "--pod-labels=")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "--pod-labels")
	}
	podLabels = nil

	err = run("--context", "test-ctx", "--dry-run", "--container-image=")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "--container-image")
	}
}

func TestRunInterruptible_Success(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	err := runInterruptible(stdcontext.Background(), logger.NewLogger(false), sigCh,
		func(stdcontext.Context) error { return nil },
		func() { t.Fatal("forceExit must not be called") })
	assert.NoError(t, err)

	errBoom := errors.New("boom")
	err = runInterruptible(stdcontext.Background(), logger.NewLogger(false), sigCh,
		func(stdcontext.Context) error { return errBoom },
		func() { t.Fatal("forceExit must not be called") })
	assert.ErrorIs(t, err, errBoom)
}

func TestRunInterruptible_FirstSignalCancelsGracefully(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	started := make(chan struct{})
	go func() {
		<-started
		sigCh <- syscall.SIGINT
	}()
	err := runInterruptible(stdcontext.Background(), logger.NewLogger(false), sigCh,
		func(ctx stdcontext.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		func() { t.Fatal("forceExit must not be called on the first signal") })
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "interrupted by signal")
		assert.False(t, errors.Is(err, stdcontext.Canceled), "plain cancellation should not be reported as an extra error")
	}
}

func TestRunInterruptible_SecondSignalForcesExit(t *testing.T) {
	sigCh := make(chan os.Signal, 2)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	forced := false

	go func() {
		<-started
		sigCh <- syscall.SIGTERM
		sigCh <- syscall.SIGINT
	}()
	done := make(chan error, 1)
	go func() {
		done <- runInterruptible(stdcontext.Background(), logger.NewLogger(false), sigCh,
			func(stdcontext.Context) error {
				close(started)
				<-release // ignores cancellation: graceful shutdown is stuck
				return nil
			},
			func() { forced = true })
	}()
	select {
	case err := <-done:
		assert.True(t, forced, "second signal must force exit")
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("runInterruptible did not return after the second signal")
	}
}

func TestUncordonCommandHint(t *testing.T) {
	cmd := uncordonCommand([]string{"n1", "n2"}, "prod")
	assert.Equal(t, "kubectl --context prod uncordon n1 n2 && kubectl --context prod annotate node n1 n2 "+operations.CordonedByAnnotation+"-", cmd)

	// Context names with shell metacharacters are quoted.
	cmd = uncordonCommand([]string{"ip-10-0-0-1.ec2.internal"}, "arn:aws:eks:eu-west-1:1:cluster/prod it's")
	assert.Equal(t, `kubectl --context 'arn:aws:eks:eu-west-1:1:cluster/prod it'"'"'s' uncordon ip-10-0-0-1.ec2.internal && `+
		`kubectl --context 'arn:aws:eks:eu-west-1:1:cluster/prod it'"'"'s' annotate node ip-10-0-0-1.ec2.internal `+operations.CordonedByAnnotation+"-", cmd)

	assert.True(t, strings.HasPrefix(uncordonCommand([]string{"n1"}, ""), "kubectl uncordon n1"))
	assert.Equal(t, "''", operations.ShellQuote(""))
	assert.Equal(t, "'$(rm -rf /)'", operations.ShellQuote("$(rm -rf /)"))
}

package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	postgresqlAPIGroup   = "acid.zalan.do"
	postgresqlAPIVersion = "v1"
	// postgresqlPodSelector selects Spilo pods of a cluster; the cluster name
	// is appended as the value of the (default) cluster-name label.
	postgresqlPodSelector = "application=spilo,cluster-name="
	// postgresqlRollingUpdateAnnotation is the operator's own pod marker
	// (rollingUpdatePodAnnotationKey in postgres-operator). Pods carrying it
	// are recreated by the next sync of the cluster, replicas first and the
	// primary last after a switchover.
	postgresqlRollingUpdateAnnotation = "zalando-postgres-operator-rolling-update-required"
	// postgresqlRestartRequestAnnotation is set on the postgresql CR to
	// trigger an update event (and therefore a sync) in the operator.
	postgresqlRestartRequestAnnotation = "k8s-rollout-restart/restartedAt"
	// postgresqlRunningStatus is status.PostgresClusterStatus of a healthy cluster.
	postgresqlRunningStatus = "Running"
	// Statuses the operator sets when a sync/update failed. It only retries
	// on its next resync (resync_period, 30m by default).
	postgresqlSyncFailedStatus   = "SyncFailed"
	postgresqlUpdateFailedStatus = "UpdateFailed"
	// postgresqlMarkerPath is the JSON pointer of the rolling-update marker
	// (the key contains neither "/" nor "~", so no escaping is needed).
	postgresqlMarkerPath = "/metadata/annotations/" + postgresqlRollingUpdateAnnotation
	// PostgresqlMarkedByAnnotation is set (value: run ID) together with the
	// rolling-update marker on every pod this tool marks. Cleanup only ever
	// removes markers from pods carrying this run's ID, so a marker set by
	// the operator or another run is never removed.
	PostgresqlMarkedByAnnotation = "k8s-rollout-restart/marked-by"
	// postgresqlMarkedByPath is the JSON pointer of PostgresqlMarkedByAnnotation
	// ("/" escaped as "~1", RFC 6901).
	postgresqlMarkedByPath = "/metadata/annotations/k8s-rollout-restart~1marked-by"
	// postgresqlUnmarkAttempts bounds the read/remove attempts per pod during
	// cleanup.
	postgresqlUnmarkAttempts = 3
	// postgresqlUnmarkRetryDelay is the pause between cleanup attempts.
	postgresqlUnmarkRetryDelay = time.Second
	// postgresqlMarkAttempts bounds how often the marking plan is refreshed
	// when pods change concurrently.
	postgresqlMarkAttempts = 3
	// postgresqlMaxRetriggers caps the re-triggered syncs per cluster.
	postgresqlMaxRetriggers = 3
	// postgresqlMaxStallInterval caps the no-progress interval after which a
	// sync is re-triggered (min(60s, timeout/3)).
	postgresqlMaxStallInterval = 60 * time.Second
	// postgresqlCleanupTimeout bounds marker removal after a failed restart.
	postgresqlCleanupTimeout = 30 * time.Second
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
	skipWait  bool

	// stallInterval overrides min(60s, timeout/3) when > 0 (tests).
	stallInterval    time.Duration
	maxRetriggers    int
	cleanupTimeout   time.Duration
	unmarkRetryDelay time.Duration

	// runID is written to PostgresqlMarkedByAnnotation on every pod marked by
	// this run.
	runID string
	// kubeContext is the --context of the run; it is added to the manual
	// cleanup hint so the command targets the same cluster.
	kubeContext string

	runStop *RunStop // run-wide stop signal shared with the other resource types
}

// NewPostgresqlOperations creates a new PostgresqlOperations instance
func NewPostgresqlOperations(clientset K8sClient, parallel, timeout int, dryRun bool, minAge *time.Duration, skipWait bool) *PostgresqlOperations {
	return &PostgresqlOperations{
		clientset: clientset,
		parallel:  parallel,
		timeout:   timeout,
		dryRun:    dryRun,
		log:       logger.NewLogger(dryRun),
		minAge:    minAge,
		skipWait:  skipWait,

		maxRetriggers:    postgresqlMaxRetriggers,
		cleanupTimeout:   postgresqlCleanupTimeout,
		unmarkRetryDelay: postgresqlUnmarkRetryDelay,
		runID:            newRunID(),
	}
}

// SetLogger replaces the logger (the configured root logger, so that
// --output=json applies to every line).
func (p *PostgresqlOperations) SetLogger(l *logger.Logger) {
	if l != nil {
		p.log = l
	}
}

// SetRunID sets the run identifier written to PostgresqlMarkedByAnnotation
// (normally the same ID the cordon uses).
func (p *PostgresqlOperations) SetRunID(id string) {
	if id != "" {
		p.runID = id
	}
}

// SetKubeContext sets the kube context of the run (--context), used in the
// manual marker cleanup hint.
func (p *PostgresqlOperations) SetKubeContext(kubeContext string) {
	p.kubeContext = kubeContext
}

// SetRunStop sets the run-wide stop signal shared by all resource types.
func (p *PostgresqlOperations) SetRunStop(rs *RunStop) {
	p.runStop = rs
}

// PostgresqlPlan is the precomputed restart plan of the PostgreSQL clusters
// of a run: the discovered API path, the selected clusters and the planning
// errors (API discovery, selection). The dry run previews it and the execute
// path restarts exactly these targets (RestartPlannedPostgresqlClusters).
type PostgresqlPlan struct {
	apiPath string
	targets []crTarget
	errs    []error
}

// Targets returns "namespace/name" of every planned PostgreSQL cluster.
func (p *PostgresqlPlan) Targets() []string {
	if p == nil {
		return nil
	}
	return crTargetNames(p.targets)
}

// Err returns the planning errors (nil when the plan is complete).
func (p *PostgresqlPlan) Err() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.errs...)
}

// PlanPostgresqlClusters discovers the postgresql API and selects the
// clusters to restart in every namespace (read-only), logging the selection.
// A missing API group yields an empty plan. The returned plan is never nil.
func (p *PostgresqlOperations) PlanPostgresqlClusters(ctx context.Context, namespaces []string) (*PostgresqlPlan, error) {
	plan := &PostgresqlPlan{}
	apiPath, present, err := ResolveAPIGroupVersion(ctx, p.clientset.RESTClient(), postgresqlAPIGroup, postgresqlAPIVersion)
	if err != nil {
		err = fmt.Errorf("postgresql API: %w", err)
		p.log.Error("Failed to plan PostgreSQL clusters: %v", err)
		plan.errs = append(plan.errs, err)
		return plan, plan.Err()
	}
	if !present {
		p.log.Info("PostgreSQL API group %s not found, skipping PostgreSQL operations", postgresqlAPIGroup)
		return plan, nil
	}
	plan.apiPath = apiPath
	plan.targets, plan.errs = selectCRTargets(ctx, p.log, "PostgreSQL", p.parallel, p.dryRun, p.minAge, namespaces,
		func(ctx context.Context, ns string) ([]namedCR, []namedCR, int, error) {
			return p.selectPostgresqls(ctx, ns, apiPath)
		})
	return plan, plan.Err()
}

// RestartPostgresqlClusters plans and restarts the PostgreSQL clusters of the
// given namespaces (PlanPostgresqlClusters + RestartPlannedPostgresqlClusters).
func (p *PostgresqlOperations) RestartPostgresqlClusters(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		p.log.Info("No namespaces specified, skipping PostgreSQL clusters restart")
		return nil
	}
	plan, _ := p.PlanPostgresqlClusters(ctx, namespaces)
	return p.RestartPlannedPostgresqlClusters(ctx, plan)
}

// RestartPlannedPostgresqlClusters restarts the PostgreSQL clusters of a plan
// with at most --parallel restarts in progress at any time. The first failure
// (with or without --skip-wait, including an API error while waiting) stops
// new launches here and, through the shared RunStop, for every other resource
// type; the stop is tripped as soon as the failure is detected, before any
// further waiting or marker cleanup. Clusters in flight are still waited for
// and the remaining ones are reported as "not started due to earlier
// failure". A plan with errors (discovery or selection) trips the stop before
// anything is launched, so no cluster is restarted at all.
func (p *PostgresqlOperations) RestartPlannedPostgresqlClusters(ctx context.Context, plan *PostgresqlPlan) error {
	if plan == nil {
		return nil
	}
	errs := append([]error(nil), plan.errs...)
	// A planning error stops the run before anything is launched.
	rs := tripOnSelectionErrors(p.runStop, errs)
	if !p.dryRun && len(plan.targets) > 0 {
		p.log.Info("Starting restart of %d PostgreSQL cluster(s)", len(plan.targets))
		apiPath := plan.apiPath
		errs = append(errs, restartCRTargets(ctx, p.log, "PostgreSQL", p.parallel, p.skipWait, rs, plan.targets,
			func(ctx context.Context, t crTarget, rs *RunStop) error {
				return p.restartPostgresqlCluster(ctx, t.namespace, t.name, apiPath, rs)
			}))
	}
	if err := errors.Join(errs...); err != nil {
		p.log.Warning("PostgreSQL clusters restart finished with errors")
		return fmt.Errorf("postgresql restart failed: %w", err)
	}
	if len(plan.targets) > 0 && !p.dryRun {
		p.log.Success("PostgreSQL clusters restart completed successfully")
	}
	return nil
}

// selectPostgresqls lists the postgresql CRs of a namespace and splits them
// by --older-than. It is shared by execute and the dry-run preview.
func (p *PostgresqlOperations) selectPostgresqls(ctx context.Context, namespace, apiPath string) (toRestart, tooYoung []namedCR, total int, err error) {
	postgresList, err := p.clientset.RESTClient().Get().
		AbsPath(apiPath).
		Namespace(namespace).
		Resource("postgresqls").
		DoRaw(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to list PostgreSQL clusters in namespace %s: %w", namespace, err)
	}
	toRestart, tooYoung, total, err = filterCRsByAge(postgresList, p.minAge)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to parse PostgreSQL clusters in namespace %s: %w", namespace, err)
	}
	return toRestart, tooYoung, total, nil
}

// GetPostgresqlClustersToRestart returns "namespace/name" of every
// PostgreSQL cluster that would be restarted (PlanPostgresqlClusters).
func (p *PostgresqlOperations) GetPostgresqlClustersToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	plan, err := p.PlanPostgresqlClusters(ctx, namespaces)
	return plan.Targets(), err
}

// restartPostgresqlCluster performs a rolling restart of one Zalando cluster.
//
// Changing spec.podAnnotations does NOT make postgres-operator roll the pods
// (it only syncs annotations onto the existing pods). Instead we use the
// operator's own mechanism: every pod of the cluster is marked with
// zalando-postgres-operator-rolling-update-required=true and the CR is touched
// so that the operator's update handler runs a sync. During syncStatefulSet
// the operator collects flagged pods and recreates them (replicas first, then
// a switchover and the former primary).
//
// The operator only retries a failed sync on its next resync (resync_period,
// 30m by default), and markers left on pods make it rotate them whenever that
// next sync happens. Therefore:
//   - the cluster must be healthy before anything is marked (preflight);
//   - every marker is bound to the pod UID it was planned for;
//   - a stalled or failed rotation is re-triggered by touching the CR again;
//   - if the restart does not complete, the markers we set are removed again
//     so the operator does not rotate pods unexpectedly after this run.
//
// Requires RBAC "patch" on pods in addition to "patch" on postgresqls.
//
// Every failure (preflight, marking, rejected or ambiguous trigger or
// re-trigger PATCH, API error while waiting, rotation timeout) trips rs as
// soon as it is detected, before any further waiting or marker cleanup, so no
// new restart starts anywhere from then on. After an ambiguous trigger, a
// failed re-trigger or an API error while waiting the rotation is still
// waited for (unless --skip-wait), and the error is returned afterwards, also
// when the rotation completed.
func (p *PostgresqlOperations) restartPostgresqlCluster(ctx context.Context, namespace, name, apiPath string, rs *RunStop) error {
	p.log.Info("Processing PostgreSQL cluster: %s/%s", namespace, name)
	selector := postgresqlPodSelector + name
	// trip stops new launches run-wide; the cause names the cluster.
	trip := func(err error) {
		rs.Trip(fmt.Errorf("postgresql %s/%s: %w", namespace, name, err))
	}

	pods, err := p.preflight(ctx, namespace, name, apiPath, selector)
	if err != nil {
		err = fmt.Errorf("skipped, no pods were marked: preflight failed: %w", err)
		trip(err)
		return err
	}

	// markPods trips rs itself as soon as marking fails.
	marked, err := p.markPods(ctx, namespace, name, apiPath, selector, pods, trip)
	if err != nil {
		summary := p.removeMarkers(namespace, selector, marked)
		return fmt.Errorf("failed to mark pods for rolling update, CR not touched (%s): %w", summary, err)
	}

	triggerErr := p.triggerSync(ctx, namespace, name, apiPath)
	switch {
	case triggerErr == nil:
		p.log.Success("Successfully triggered rolling update for PostgreSQL cluster %s/%s (%d pod(s) marked)",
			namespace, name, len(marked))
	case mutationDefinitelyFailed(triggerErr):
		// Rejected: the operator was not triggered; never leave markers
		// behind for an unobserved later sync.
		triggerErr = fmt.Errorf("failed to trigger operator sync: %w", triggerErr)
		trip(triggerErr)
		summary := p.removeMarkers(namespace, selector, marked)
		return fmt.Errorf("%w (%s)", triggerErr, summary)
	default:
		// The touch may have been applied: the operator may be rotating.
		// No new restart may start anywhere from now on.
		triggerErr = &rolloutJobError{
			err:       fmt.Errorf("failed to trigger operator sync, outcome unknown: %w", triggerErr),
			ambiguous: true,
		}
		trip(triggerErr)
		if p.skipWait {
			// Nobody waits for the outcome: remove the markers.
			summary := p.removeMarkers(namespace, selector, marked)
			return fmt.Errorf("%w (%s)", triggerErr, summary)
		}
		p.log.Warning("Triggering operator sync for PostgreSQL cluster %s/%s may have failed (%v); "+
			"waiting for the rotation, markers are removed if it does not complete", namespace, name, triggerErr)
	}

	if p.skipWait {
		// The only exit that intentionally leaves markers behind.
		p.log.Warning("Skipping wait for PostgreSQL cluster %s/%s (--skip-wait flag is set): pods %v stay marked "+
			"(%s=true, %s=%s) until the operator rotates them; if its sync fails, it rotates them on a later "+
			"resync (resync_period, 30m by default)",
			namespace, name, sortedNames(marked), postgresqlRollingUpdateAnnotation, PostgresqlMarkedByAnnotation, p.runID)
		return nil
	}

	// waitForRotation trips rs itself as soon as a re-trigger PATCH or an
	// API request fails.
	duringWaitErr, waitErr := p.waitForRotation(ctx, namespace, name, apiPath, selector, marked, trip)
	failErr := joinErrs(triggerErr, duringWaitErr)
	if waitErr != nil {
		trip(waitErr)
		summary := p.removeMarkers(namespace, selector, marked)
		return fmt.Errorf("%w (%s)", joinErrs(failErr, waitErr), summary)
	}
	if failErr != nil {
		// The rotation completed, but a failed or ambiguous trigger PATCH or
		// an API error while waiting stays a failure: the run must not
		// report success after it.
		return fmt.Errorf("%w (the rotation completed nevertheless)", failErr)
	}
	return nil
}

// joinErrs joins the non-nil errors into one error with a single-line
// message ("a; b"); every error stays reachable through errors.Is/As. It
// returns nil if all are nil.
func joinErrs(errs ...error) error {
	var (
		kept []error
		msgs []string
	)
	for _, err := range errs {
		if err != nil {
			kept = append(kept, err)
			msgs = append(msgs, err.Error())
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return &joinedErr{errs: kept, msg: strings.Join(msgs, "; ")}
}

// joinedErr is errors.Join with a single-line message.
type joinedErr struct {
	errs []error
	msg  string
}

func (e *joinedErr) Error() string   { return e.msg }
func (e *joinedErr) Unwrap() []error { return e.errs }

// postgresqlState is the part of the postgresql CR the restart relies on.
type postgresqlState struct {
	status    string
	instances int
}

// getPostgresql reads status.PostgresClusterStatus and spec.numberOfInstances.
func (p *PostgresqlOperations) getPostgresql(ctx context.Context, namespace, name, apiPath string) (postgresqlState, error) {
	raw, err := p.clientset.RESTClient().Get().
		AbsPath(apiPath).
		Namespace(namespace).
		Resource("postgresqls").
		Name(name).
		DoRaw(ctx)
	if err != nil {
		return postgresqlState{}, fmt.Errorf("failed to get PostgreSQL %s/%s: %w", namespace, name, err)
	}
	var cr struct {
		Spec struct {
			NumberOfInstances int `json:"numberOfInstances"`
		} `json:"spec"`
		Status struct {
			PostgresClusterStatus string `json:"PostgresClusterStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return postgresqlState{}, fmt.Errorf("failed to parse PostgreSQL %s/%s: %w", namespace, name, err)
	}
	return postgresqlState{status: cr.Status.PostgresClusterStatus, instances: cr.Spec.NumberOfInstances}, nil
}

// listPostgresqlPods lists all pods of the cluster.
func (p *PostgresqlOperations) listPostgresqlPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	list, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list PostgreSQL pods in %s (selector %q): %w", namespace, selector, err)
	}
	return list.Items, nil
}

// preflight verifies that the cluster is healthy before any marker is set:
// the operator reports Running and exactly numberOfInstances pods exist, all
// Running and Ready, and no pod carries a rolling-update marker yet (a
// pending rotation requested by the operator or someone else is neither
// adopted nor removed). It returns those pods (the restart plan).
func (p *PostgresqlOperations) preflight(ctx context.Context, namespace, name, apiPath, selector string) ([]corev1.Pod, error) {
	pods, err := p.healthyPods(ctx, namespace, name, apiPath, selector)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		if err := foreignMarkerErr(&pods[i]); err != nil {
			return nil, err
		}
	}
	return pods, nil
}

// healthyPods re-reads the CR and the pods and checks them: the operator
// reports Running, spec.numberOfInstances is at least 1, and exactly that many
// pods (the current spec, not an earlier read) exist, all Running and Ready
// and none being deleted. It returns the pods.
func (p *PostgresqlOperations) healthyPods(ctx context.Context, namespace, name, apiPath, selector string) ([]corev1.Pod, error) {
	state, err := p.getPostgresql(ctx, namespace, name, apiPath)
	if err != nil {
		return nil, err
	}
	if state.status != postgresqlRunningStatus {
		return nil, fmt.Errorf("cluster status is %q, expected %q", state.status, postgresqlRunningStatus)
	}
	if state.instances < 1 {
		return nil, fmt.Errorf("spec.numberOfInstances is %d", state.instances)
	}
	pods, err := p.listPostgresqlPods(ctx, namespace, selector)
	if err != nil {
		return nil, err
	}
	if err := checkPostgresqlPodsHealthy(pods, state.instances); err != nil {
		return nil, err
	}
	return pods, nil
}

// checkPostgresqlPodsHealthy requires exactly instances pods, none being
// deleted, all Running and Ready.
func checkPostgresqlPodsHealthy(pods []corev1.Pod, instances int) error {
	if len(pods) != instances {
		return fmt.Errorf("found %d pod(s), spec.numberOfInstances is %d", len(pods), instances)
	}
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !podReadyTrue(pod) {
			return fmt.Errorf("pod %s is not Running and Ready", pod.Name)
		}
	}
	return nil
}

// foreignMarkerErr reports a pod that already carries a rolling-update marker
// (or another run's ownership annotation) before this run marked it.
func foreignMarkerErr(pod *corev1.Pod) error {
	if v, ok := pod.Annotations[postgresqlRollingUpdateAnnotation]; ok {
		return fmt.Errorf("pod %s already carries %s=%q (pending rotation of the operator or another run, marked-by %q); "+
			"refusing to adopt or remove it", pod.Name, postgresqlRollingUpdateAnnotation, v, pod.Annotations[PostgresqlMarkedByAnnotation])
	}
	if v, ok := pod.Annotations[PostgresqlMarkedByAnnotation]; ok {
		return fmt.Errorf("pod %s already carries %s=%q from another run", pod.Name, PostgresqlMarkedByAnnotation, v)
	}
	return nil
}

// markPods sets the rolling-update marker on every planned pod, bound to the
// pod's UID. When a pod changed since planning (replaced, deleted, updated)
// the plan is refreshed and the current instances are marked. A refresh
// re-reads the CR as well and must pass the preflight checks again against
// the current CR (status Running, exactly the current spec.numberOfInstances
// pods, all Running and Ready, none being deleted); otherwise marking fails.
//
// On the first failure (a rejected or ambiguous marker PATCH, a failed
// refresh) trip is called immediately, before any further API call, and no
// further pod is marked, so no new restart starts anywhere while this
// cluster is being cleaned up. It returns every pod that was, or may have
// been, marked (UID -> name), also on error, so that the caller can remove
// the markers again.
func (p *PostgresqlOperations) markPods(ctx context.Context, namespace, name, apiPath, selector string, plan []corev1.Pod, trip func(error)) (map[types.UID]string, error) {
	marked := make(map[types.UID]string, len(plan))
	fail := func(err error) (map[types.UID]string, error) {
		trip(err)
		return marked, err
	}
	pending := plan
	for attempt := 1; ; attempt++ {
		var stale []string
		for i := range pending {
			pod := &pending[i]
			err := p.markPodForRollingUpdate(ctx, namespace, pod)
			switch {
			case err == nil:
				marked[pod.UID] = pod.Name
			case apierrors.IsNotFound(err) || isRetryablePatchError(err):
				stale = append(stale, pod.Name)
			default:
				if !mutationDefinitelyFailed(err) {
					marked[pod.UID] = pod.Name
				}
				return fail(fmt.Errorf("failed to mark pod %s/%s for rolling update: %w", namespace, pod.Name, err))
			}
		}
		if len(stale) == 0 {
			return marked, nil
		}
		if attempt >= postgresqlMarkAttempts {
			return fail(fmt.Errorf("pods %v keep changing while being marked, giving up after %d attempts", stale, attempt))
		}
		p.log.Info("PostgreSQL pods %v in %s changed since planning, refreshing the plan", stale, namespace)
		pods, err := p.healthyPods(ctx, namespace, name, apiPath, selector)
		if err != nil {
			return fail(fmt.Errorf("cluster changed while being marked: %w", err))
		}
		pending = pending[:0:0]
		for i := range pods {
			pod := pods[i]
			if _, done := marked[pod.UID]; done {
				continue
			}
			if err := foreignMarkerErr(&pod); err != nil {
				return fail(err)
			}
			pending = append(pending, pod)
		}
	}
}

// markPodForRollingUpdate sets the operator's rolling-update marker and this
// run's ownership annotation on a pod in one JSON patch. The patch first
// tests the pod UID (a pod replaced under the same name is never marked) and
// the resourceVersion (the pod is unchanged since it was checked to carry no
// marker, so a concurrently set marker is never overwritten and adopted).
func (p *PostgresqlOperations) markPodForRollingUpdate(ctx context.Context, namespace string, pod *corev1.Pod) error {
	ops := []map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(pod.UID)},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
	}
	if pod.Annotations == nil {
		ops = append(ops, map[string]interface{}{"op": "add", "path": "/metadata/annotations", "value": map[string]string{
			postgresqlRollingUpdateAnnotation: "true",
			PostgresqlMarkedByAnnotation:      p.runID,
		}})
	} else {
		ops = append(ops,
			map[string]interface{}{"op": "add", "path": postgresqlMarkerPath, "value": "true"},
			map[string]interface{}{"op": "add", "path": postgresqlMarkedByPath, "value": p.runID},
		)
	}
	return p.patchPod(ctx, namespace, pod.Name, ops)
}

// unmarkPod removes this run's marker and ownership annotation from the given
// pod instance. The patch tests the UID and the ownership annotation, so it
// never touches a replaced pod or another run's marker.
func (p *PostgresqlOperations) unmarkPod(ctx context.Context, namespace string, pod *corev1.Pod) error {
	ops := []map[string]interface{}{
		{"op": "test", "path": "/metadata/uid", "value": string(pod.UID)},
		{"op": "test", "path": postgresqlMarkedByPath, "value": p.runID},
	}
	if _, ok := pod.Annotations[postgresqlRollingUpdateAnnotation]; ok {
		ops = append(ops, map[string]interface{}{"op": "remove", "path": postgresqlMarkerPath})
	}
	ops = append(ops, map[string]interface{}{"op": "remove", "path": postgresqlMarkedByPath})
	return p.patchPod(ctx, namespace, pod.Name, ops)
}

func (p *PostgresqlOperations) patchPod(ctx context.Context, namespace, podName string, ops []map[string]interface{}) error {
	patchBytes, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return p.clientset.RESTClient().
		Patch(types.JSONPatchType).
		AbsPath("/api/v1").
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		Body(patchBytes).
		Do(ctx).
		Error()
}

// removeMarkers removes this run's rolling-update markers from the marked
// pod instances that still exist, so that the operator does not rotate them on
// its own later (the next resync can be up to resync_period, 30m by default,
// after this run). Only pods carrying this run's PostgresqlMarkedByAnnotation
// are touched. A pod is only reported clean when that is confirmed: the
// remove PATCH succeeded, or a re-read shows the instance is gone (rotated)
// or carries no marker of this run. A failed removal (409/422/any error) is
// followed by a re-read and a retry, a bounded number of times; pods that are
// still marked afterwards are reported as such. It uses a fresh bounded
// context because it also runs after the run's context was cancelled, and
// returns a summary.
func (p *PostgresqlOperations) removeMarkers(namespace, selector string, marked map[types.UID]string) string {
	if len(marked) == 0 {
		return "no pods were marked"
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cleanupTimeout)
	defer cancel()

	pending := make(map[types.UID]string, len(marked))
	for uid, name := range marked {
		pending[uid] = name
	}
	var rotated, removed, notMarked, foreign []string
	lastErr := make(map[types.UID]error)
	for attempt := 1; len(pending) > 0 && attempt <= postgresqlUnmarkAttempts && ctx.Err() == nil; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				continue
			case <-time.After(p.unmarkRetryDelay):
			}
		}
		pods, err := p.listPostgresqlPods(ctx, namespace, selector)
		if err != nil {
			for uid := range pending {
				lastErr[uid] = err
			}
			p.log.Warning("Could not list PostgreSQL pods in %s to remove rolling-update markers (attempt %d/%d): %v",
				namespace, attempt, postgresqlUnmarkAttempts, err)
			continue
		}
		current := make(map[types.UID]*corev1.Pod, len(pods))
		for i := range pods {
			current[pods[i].UID] = &pods[i]
		}
		for _, uid := range sortedUIDs(pending) {
			name := pending[uid]
			pod, exists := current[uid]
			if !exists {
				rotated = append(rotated, name)
				delete(pending, uid)
				continue
			}
			_, hasMarker := pod.Annotations[postgresqlRollingUpdateAnnotation]
			owner, hasOwner := pod.Annotations[PostgresqlMarkedByAnnotation]
			switch {
			case hasOwner && owner == p.runID:
				if err := p.unmarkPod(ctx, namespace, pod); err != nil {
					// Not confirmed: re-read and retry on the next attempt.
					lastErr[uid] = err
					p.log.Warning("Failed to remove rolling-update marker from pod %s/%s (attempt %d/%d): %v",
						namespace, name, attempt, postgresqlUnmarkAttempts, err)
					continue
				}
				removed = append(removed, name)
			case !hasMarker && !hasOwner:
				// The marking PATCH was never applied (ambiguous result).
				notMarked = append(notMarked, name)
			default:
				p.log.Warning("Pod %s/%s carries a rolling-update marker not owned by this run (%s=%q); leaving it as is",
					namespace, name, PostgresqlMarkedByAnnotation, owner)
				foreign = append(foreign, name)
			}
			delete(pending, uid)
		}
	}

	summary := fmt.Sprintf("rotated: %v, left untouched (marker removed): %v", rotated, removed)
	if len(notMarked) > 0 {
		summary += fmt.Sprintf(", never marked: %v", notMarked)
	}
	if len(foreign) > 0 {
		summary += fmt.Sprintf(", marked by someone else (left as is): %v", foreign)
	}
	p.log.Info("PostgreSQL pods in %s %s", namespace, summary)
	if len(pending) > 0 {
		failed := make([]string, 0, len(pending))
		names := make([]string, 0, len(pending))
		for _, uid := range sortedUIDs(pending) {
			names = append(names, pending[uid])
			if err := lastErr[uid]; err != nil {
				failed = append(failed, fmt.Sprintf("%s (%v)", pending[uid], err))
			} else {
				failed = append(failed, pending[uid])
			}
		}
		p.log.Warning("Rolling-update marker could NOT be removed from pods %v in %s: the operator rotates them on its next sync "+
			"(resync_period, 30m by default); remove manually with: %s",
			failed, namespace, p.unmarkCommand(namespace, names))
		summary += fmt.Sprintf(", STILL MARKED (the operator rotates them on its next sync): %v", failed)
	}
	return summary
}

// unmarkCommand returns a kubectl command that removes this tool's markers
// from the given pods. It targets the run's --context (so it never acts on
// the current-context of another cluster); all values are shell-quoted.
func (p *PostgresqlOperations) unmarkCommand(namespace string, pods []string) string {
	ctxFlag := ""
	if p.kubeContext != "" {
		ctxFlag = " --context " + ShellQuote(p.kubeContext)
	}
	quoted := make([]string, len(pods))
	for i, name := range pods {
		quoted[i] = ShellQuote(name)
	}
	return fmt.Sprintf("kubectl%s annotate pod -n %s %s %s- %s-", ctxFlag, ShellQuote(namespace),
		strings.Join(quoted, " "), postgresqlRollingUpdateAnnotation, PostgresqlMarkedByAnnotation)
}

func sortedUIDs(m map[types.UID]string) []types.UID {
	uids := make([]types.UID, 0, len(m))
	for uid := range m {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool {
		if m[uids[i]] != m[uids[j]] {
			return m[uids[i]] < m[uids[j]]
		}
		return uids[i] < uids[j]
	})
	return uids
}

func sortedNames(m map[types.UID]string) []string {
	names := make([]string, 0, len(m))
	for _, uid := range sortedUIDs(m) {
		names = append(names, m[uid])
	}
	return names
}

// rotationProgress is one observation of a PostgreSQL rotation.
type rotationProgress struct {
	state       postgresqlState
	replaced    int // marked instances that no longer exist
	total       int // marked instances
	stillMarked int // pods of the cluster carrying the marker
	active      int // pods not being deleted
	ready       int // active pods Running and Ready
}

func (r rotationProgress) done() bool {
	return r.replaced == r.total && r.stillMarked == 0 &&
		r.active == r.state.instances && r.ready == r.active &&
		r.state.status == postgresqlRunningStatus
}

func (r rotationProgress) String() string {
	return fmt.Sprintf("%d/%d marked pod(s) replaced, %d pod(s) still marked, %d/%d pod(s) ready (numberOfInstances %d), status %q",
		r.replaced, r.total, r.stillMarked, r.ready, r.active, r.state.instances, r.state.status)
}

// observeRotation reads the CR and pods once.
func (p *PostgresqlOperations) observeRotation(ctx context.Context, namespace, name, apiPath, selector string, marked map[types.UID]string) (rotationProgress, error) {
	state, err := p.getPostgresql(ctx, namespace, name, apiPath)
	if err != nil {
		return rotationProgress{}, err
	}
	pods, err := p.listPostgresqlPods(ctx, namespace, selector)
	if err != nil {
		return rotationProgress{}, err
	}
	progress := rotationProgress{state: state, total: len(marked)}
	present := make(map[types.UID]bool, len(pods))
	for i := range pods {
		pod := &pods[i]
		present[pod.UID] = true
		if pod.Annotations[postgresqlRollingUpdateAnnotation] != "" {
			progress.stillMarked++
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		progress.active++
		if pod.Status.Phase == corev1.PodRunning && podReadyTrue(pod) {
			progress.ready++
		}
	}
	for uid := range marked {
		if !present[uid] {
			progress.replaced++
		}
	}
	return progress, nil
}

// waitForRotation waits until every marked pod instance has been replaced, no
// pod carries the marker any more, numberOfInstances pods are Running and
// Ready and the operator reports Running. A failed sync (SyncFailed /
// UpdateFailed) or no progress for the stall interval re-triggers a sync by
// touching the CR again, at most maxRetriggers times: otherwise the operator
// would only retry on its next resync, long after our timeout.
//
// A failed re-trigger PATCH (rejected or ambiguous) and the first API error
// while polling (e.g. a forbidden pod List) are passed to trip at once, so no
// new restart starts anywhere, and waiting goes on. waitErr is non-nil when
// the rotation did not complete (timeout, interruption); duringWaitErr holds
// the failed re-trigger PATCHes and the first polling API error and is
// returned also when the rotation completed.
func (p *PostgresqlOperations) waitForRotation(ctx context.Context, namespace, name, apiPath, selector string,
	marked map[types.UID]string, trip func(error)) (duringWaitErr, waitErr error) {
	p.log.Info("Waiting for PostgreSQL cluster %s/%s to complete restart...", namespace, name)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(p.timeout)*time.Second)
	defer cancel()

	ticker := time.NewTicker(crPollInterval)
	defer ticker.Stop()

	stall := p.effectiveStallInterval()
	lastProgress := time.Now()
	lastTrigger := lastProgress
	replaced, retriggers := 0, 0
	lastStatus := "no status yet"
	var retriggerErr, apiErr error
	for {
		progress, err := p.observeRotation(timeoutCtx, namespace, name, apiPath, selector, marked)
		switch {
		case err != nil:
			if timeoutCtx.Err() == nil {
				if apiErr == nil {
					apiErr = fmt.Errorf("API error while waiting for the rotation: %w", err)
					// Stop new launches before waiting any longer.
					trip(apiErr)
				}
				p.log.Warning("Error while waiting for PostgreSQL cluster %s/%s: %v; no new restart is started, still waiting for the rotation",
					namespace, name, err)
				lastStatus = err.Error()
			}
		case progress.done():
			p.log.Success("PostgreSQL cluster %s/%s restarted: all %d marked pod(s) replaced and ready", namespace, name, progress.total)
			return joinErrs(retriggerErr, apiErr), nil
		default:
			lastStatus = progress.String()
			p.log.Info("Waiting for PostgreSQL cluster %s/%s: %s", namespace, name, lastStatus)
			now := time.Now()
			if progress.replaced > replaced {
				replaced = progress.replaced
				lastProgress = now
			}
			reason := ""
			switch {
			case progress.state.status == postgresqlSyncFailedStatus || progress.state.status == postgresqlUpdateFailedStatus:
				if now.Sub(lastTrigger) >= stall/4 {
					reason = fmt.Sprintf("operator reports %q", progress.state.status)
				}
			case progress.replaced < progress.total && now.Sub(later(lastProgress, lastTrigger)) >= stall:
				reason = fmt.Sprintf("no marked pod replaced for %s", stall)
			}
			if reason != "" && retriggers < p.maxRetriggers {
				retriggers++
				p.log.Warning("PostgreSQL cluster %s/%s: %s, re-triggering operator sync (%d/%d)",
					namespace, name, reason, retriggers, p.maxRetriggers)
				if err := p.triggerSync(timeoutCtx, namespace, name, apiPath); err != nil {
					err = fmt.Errorf("failed to re-trigger operator sync (%d/%d): %w", retriggers, p.maxRetriggers, err)
					// Stop new launches before waiting any longer.
					trip(err)
					retriggerErr = joinErrs(retriggerErr, err)
					p.log.Warning("PostgreSQL cluster %s/%s: %v; no new restart is started, still waiting for the rotation",
						namespace, name, err)
				}
				lastTrigger = now
			}
		}

		select {
		case <-timeoutCtx.Done():
			if ctx.Err() != nil {
				return joinErrs(retriggerErr, apiErr), fmt.Errorf("interrupted while waiting for PostgreSQL cluster %s/%s restart (last status: %s): %w",
					namespace, name, lastStatus, ctx.Err())
			}
			return joinErrs(retriggerErr, apiErr), fmt.Errorf("timeout waiting for PostgreSQL cluster %s/%s restart (last status: %s)",
				namespace, name, lastStatus)
		case <-ticker.C:
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// effectiveStallInterval is min(60s, timeout/3) unless set explicitly.
func (p *PostgresqlOperations) effectiveStallInterval() time.Duration {
	if p.stallInterval > 0 {
		return p.stallInterval
	}
	stall := time.Duration(p.timeout) * time.Second / 3
	if stall > postgresqlMaxStallInterval {
		stall = postgresqlMaxStallInterval
	}
	return stall
}

// triggerSync touches a metadata annotation of the postgresql CR. The
// operator queues an update event for annotation changes, which syncs the
// cluster and recreates the marked pods.
func (p *PostgresqlOperations) triggerSync(ctx context.Context, namespace, name, apiPath string) error {
	patchBytes, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				postgresqlRestartRequestAnnotation: time.Now().Format(time.RFC3339Nano),
			},
		},
	})
	if err != nil {
		return err
	}

	return p.clientset.RESTClient().
		Patch(types.MergePatchType).
		AbsPath(apiPath).
		Namespace(namespace).
		Resource("postgresqls").
		Name(name).
		Body(patchBytes).
		Do(ctx).
		Error()
}

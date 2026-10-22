package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// StatefulSetOperations implements StatefulSetOperator interface.
//
// --parallel has the same semantics as for Deployments: at most --parallel
// StatefulSet rollouts are in progress at any time, globally across
// namespaces. A worker patches one StatefulSet, waits for it to become ready
// (bounded per resource by --timeout) and then takes the next one. Any
// failure stops new launches, with or without --skip-wait.
type StatefulSetOperations struct {
	clientset       K8sClient
	parallel        int
	timeout         int
	dryRun          bool
	log             *logger.Logger
	minAge          *time.Duration
	podLabels       []string
	podAnnotations  []string
	containerImages []string
	skipWait        bool
	runStop         *RunStop      // run-wide stop signal shared with the other resource types
	pollInterval    time.Duration // readiness poll interval (defaultPollInterval when zero)
}

// SetLogger replaces the logger (the configured root logger, so that
// --output=json applies to every line).
func (s *StatefulSetOperations) SetLogger(l *logger.Logger) {
	if l != nil {
		s.log = l
	}
}

// SetRunStop sets the run-wide stop signal shared by all resource types: a
// failure here stops launches of the other types and vice versa.
func (s *StatefulSetOperations) SetRunStop(rs *RunStop) {
	s.runStop = rs
}

// NewStatefulSetOperations creates a new StatefulSetOperations instance
func NewStatefulSetOperations(clientset K8sClient, parallel, timeout int, _, dryRun bool, minAge *time.Duration, podLabels []string, podAnnotations []string, containerImages []string, skipWait bool) *StatefulSetOperations {
	return &StatefulSetOperations{
		clientset:       clientset,
		parallel:        parallel,
		timeout:         timeout,
		dryRun:          dryRun,
		log:             logger.NewLogger(dryRun),
		minAge:          minAge,
		podLabels:       podLabels,
		podAnnotations:  podAnnotations,
		containerImages: containerImages,
		skipWait:        skipWait,
	}
}

// hasPodFilters reports whether any pod-level filter (labels, annotations, or
// container image) is active, based on the parsed (valid) filter values.
func (s *StatefulSetOperations) hasPodFilters() bool {
	return len(ParseLabelsOrAnnotations(s.podLabels)) > 0 ||
		len(ParseLabelsOrAnnotations(s.podAnnotations)) > 0 ||
		len(nonEmptyStrings(s.containerImages)) > 0
}

// podMatchesFilters reports whether a pod satisfies all active pod-level filters.
func (s *StatefulSetOperations) podMatchesFilters(pod *corev1.Pod) bool {
	return s.podHasRequiredLabels(pod.Labels) &&
		s.podHasRequiredAnnotations(pod.Annotations) &&
		PodMatchesAnyImage(pod, s.containerImages)
}

// podHasRequiredLabels checks if a pod has all the required labels
func (s *StatefulSetOperations) podHasRequiredLabels(podLabels map[string]string) bool {
	return MatchesRequirements(podLabels, ParseLabelsOrAnnotations(s.podLabels))
}

// podHasRequiredAnnotations checks if a pod has all the required annotations
func (s *StatefulSetOperations) podHasRequiredAnnotations(podAnnotations map[string]string) bool {
	return MatchesRequirements(podAnnotations, ParseLabelsOrAnnotations(s.podAnnotations))
}

// statefulSetSelection is the result of selecting StatefulSets in one
// namespace. It is shared by the dry-run preview and the execute path.
type statefulSetSelection struct {
	namespace      string
	total          int
	toRestart      []appsv1.StatefulSet
	postgres       []string
	elasticsearch  []string
	tooYoung       []appsv1.StatefulSet
	noMatchingPods []string
	unsupported    []string // "name: reason" for update strategies a restart cannot complete under
}

// selectStatefulSets decides which StatefulSets in a namespace will be restarted.
func (s *StatefulSetOperations) selectStatefulSets(ctx context.Context, namespace string) (*statefulSetSelection, error) {
	statefulsets, err := s.clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list StatefulSets in namespace %s: %w", namespace, err)
	}

	sel := &statefulSetSelection{namespace: namespace, total: len(statefulsets.Items)}

	var candidates []appsv1.StatefulSet
	for _, statefulset := range statefulsets.Items {
		if isPostgresOperatorStatefulSet(&statefulset) {
			sel.postgres = append(sel.postgres, statefulset.Name)
			continue
		}
		if isElasticsearchStatefulSet(&statefulset) {
			sel.elasticsearch = append(sel.elasticsearch, statefulset.Name)
			continue
		}
		if s.minAge != nil && time.Since(statefulset.CreationTimestamp.Time) < *s.minAge {
			sel.tooYoung = append(sel.tooYoung, statefulset)
			continue
		}
		candidates = append(candidates, statefulset)
	}

	if s.hasPodFilters() && len(candidates) > 0 {
		matching, err := s.statefulSetsWithMatchingPods(ctx, namespace)
		if err != nil {
			return nil, err
		}
		filtered := candidates[:0]
		for _, sts := range candidates {
			if !matching[sts.UID] {
				sel.noMatchingPods = append(sel.noMatchingPods, sts.Name)
				continue
			}
			filtered = append(filtered, sts)
		}
		candidates = filtered
	}

	for _, sts := range candidates {
		if reason, unsupported := unsupportedUpdateStrategy(&sts); unsupported {
			sel.unsupported = append(sel.unsupported, fmt.Sprintf("%s: %s", sts.Name, reason))
			continue
		}
		sel.toRestart = append(sel.toRestart, sts)
	}

	return sel, nil
}

// unsupportedUpdateStrategy reports whether a template-annotation restart
// cannot complete under the StatefulSet's update strategy: with OnDelete the
// controller never replaces pods, and with a RollingUpdate partition > 0 pods
// with ordinals below the partition keep the old revision.
func unsupportedUpdateStrategy(sts *appsv1.StatefulSet) (string, bool) {
	strategy := sts.Spec.UpdateStrategy
	if strategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return "updateStrategy is OnDelete, pods would not be replaced automatically", true
	}
	if strategy.RollingUpdate != nil && strategy.RollingUpdate.Partition != nil && *strategy.RollingUpdate.Partition > 0 {
		return fmt.Sprintf("rollingUpdate.partition=%d, pods with lower ordinals would not be restarted",
			*strategy.RollingUpdate.Partition), true
	}
	return "", false
}

// statefulSetsWithMatchingPods returns the UIDs of StatefulSets that control
// at least one pod satisfying the active pod filters. Ownership is verified via
// the pod's controller ownerReference UID, not via label selectors.
func (s *StatefulSetOperations) statefulSetsWithMatchingPods(ctx context.Context, namespace string) (map[types.UID]bool, error) {
	pods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}
	result := make(map[types.UID]bool)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !s.podMatchesFilters(pod) {
			continue
		}
		if ref := metav1.GetControllerOf(pod); ref != nil && ref.Kind == "StatefulSet" {
			result[ref.UID] = true
		}
	}
	return result, nil
}

// selectAll runs selectStatefulSets for every namespace, with at most
// s.parallel namespaces in flight. Errors are collected per namespace.
func (s *StatefulSetOperations) selectAll(ctx context.Context, namespaces []string) ([]*statefulSetSelection, []error) {
	sels := make([]*statefulSetSelection, len(namespaces))
	errs := make([]error, len(namespaces))
	forEachBounded(s.parallel, len(namespaces), func(i int) {
		if err := ctx.Err(); err != nil {
			errs[i] = fmt.Errorf("namespace %s: statefulsets not selected: %w", namespaces[i], err)
			return
		}
		sels[i], errs[i] = s.selectStatefulSets(ctx, namespaces[i])
	})

	var outSels []*statefulSetSelection
	var outErrs []error
	for i := range namespaces {
		if sels[i] != nil {
			outSels = append(outSels, sels[i])
		}
		if errs[i] != nil {
			outErrs = append(outErrs, errs[i])
		}
	}
	return outSels, outErrs
}

// logSelection reports what was selected and skipped in a namespace.
func (s *StatefulSetOperations) logSelection(sel *statefulSetSelection) {
	ns := sel.namespace
	s.log.Info("Restarting StatefulSets in namespace: %s", ns)
	if sel.total == 0 {
		s.log.Info("No StatefulSets found in namespace: %s", ns)
		return
	}

	if len(sel.postgres) > 0 {
		s.log.Info("Skipping the following StatefulSets in namespace %s (will be restarted via PostgreSQL Operator):", ns)
		for _, name := range sel.postgres {
			s.log.Info("- %s (controlled by Zalando PostgreSQL Operator)", name)
		}
	}
	if len(sel.elasticsearch) > 0 {
		s.log.Info("Skipping the following StatefulSets in namespace %s (managed by the ECK operator, not restarted by this tool):", ns)
		for _, name := range sel.elasticsearch {
			s.log.Info("- %s (managed by ECK, skipped)", name)
		}
	}
	for _, sts := range sel.tooYoung {
		s.log.Info("Skipping StatefulSet %s/%s: too new (age: %s, required: %s)",
			ns, sts.Name, time.Since(sts.CreationTimestamp.Time).Round(time.Second), *s.minAge)
	}
	for _, name := range sel.noMatchingPods {
		s.log.Info("Skipping StatefulSet %s/%s: no pods matching the requested labels/annotations/image", ns, name)
	}
	for _, item := range sel.unsupported {
		s.log.Warning("Skipping StatefulSet %s/%s: unsupported for restart", ns, item)
	}

	if len(sel.toRestart) == 0 {
		s.log.Info("No eligible StatefulSets to restart in namespace: %s", ns)
		return
	}
	s.log.Info("Found %d StatefulSet(s) to restart in namespace: %s", len(sel.toRestart), ns)
	if s.dryRun {
		for _, sts := range sel.toRestart {
			s.log.Info("Would restart StatefulSet: %s/%s", ns, sts.Name)
		}
	}
}

// StatefulSetPlan is the precomputed restart plan of the StatefulSets of a
// run: the StatefulSets selected in every namespace plus the selection
// errors. The dry run previews it and the execute path restarts exactly these
// targets (RestartPlannedStatefulSets).
type StatefulSetPlan struct {
	jobs []statefulSetJob
	errs []error
}

// statefulSetJob is one planned StatefulSet restart.
type statefulSetJob struct {
	namespace string
	name      string
}

// Targets returns "namespace/name" of every planned StatefulSet.
func (p *StatefulSetPlan) Targets() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.jobs))
	for _, j := range p.jobs {
		out = append(out, j.namespace+"/"+j.name)
	}
	return out
}

// Err returns the selection errors of the plan (nil when it is complete).
func (p *StatefulSetPlan) Err() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.errs...)
}

// PlanStatefulSets selects the StatefulSets to restart in every namespace
// (read-only) and logs the selection. The returned plan is never nil; the
// error reports selection failures, in which case the plan holds the targets
// of the other namespaces.
func (s *StatefulSetOperations) PlanStatefulSets(ctx context.Context, namespaces []string) (*StatefulSetPlan, error) {
	sels, errs := s.selectAll(ctx, namespaces)
	for _, err := range errs {
		s.log.Error("Failed to select StatefulSets: %v", err)
	}
	plan := &StatefulSetPlan{errs: errs}
	for _, sel := range sels {
		s.logSelection(sel)
		for _, sts := range sel.toRestart {
			plan.jobs = append(plan.jobs, statefulSetJob{namespace: sel.namespace, name: sts.Name})
		}
	}
	return plan, plan.Err()
}

// RestartStatefulSets plans and restarts the StatefulSets of the given
// namespaces (PlanStatefulSets + RestartPlannedStatefulSets).
func (s *StatefulSetOperations) RestartStatefulSets(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		s.log.Info("No namespaces specified, skipping statefulsets restart")
		return nil
	}
	plan, _ := s.PlanStatefulSets(ctx, namespaces)
	return s.RestartPlannedStatefulSets(ctx, plan)
}

// RestartPlannedStatefulSets restarts the StatefulSets of a plan. The first
// restart failure (PATCH rejected or ambiguous, rollout timed out, API error
// while waiting; with --skip-wait: any failed PATCH) stops launching further
// rollouts; rollouts in progress are still waited for and the remaining
// StatefulSets are reported as "not started due to earlier failure". A plan
// with selection errors trips the run-wide stop before anything is launched,
// so no StatefulSet is restarted at all. All errors are returned joined.
func (s *StatefulSetOperations) RestartPlannedStatefulSets(ctx context.Context, plan *StatefulSetPlan) error {
	if plan == nil {
		return nil
	}
	errs := append([]error(nil), plan.errs...)
	// A selection error stops the run before anything is launched.
	rs := tripOnSelectionErrors(s.runStop, errs)
	jobs := plan.jobs

	if s.dryRun || len(jobs) == 0 {
		return errors.Join(errs...)
	}

	if s.skipWait {
		s.log.Info("Skipping wait for StatefulSets readiness (--skip-wait flag is set)")
	}

	// Each worker patches one StatefulSet and waits for its rollout before
	// taking the next one, so at most s.parallel rollouts are in progress at
	// any time across all namespaces. After a failure no new rollout is
	// started (see stopsLaunches).
	jobErrs, notStarted := runRolloutJobs(s.parallel, len(jobs), rs, func(i int) error {
		return s.restartOne(ctx, rs, jobs[i].namespace, jobs[i].name)
	}, func(err error) bool { return stopsLaunches(err) })

	failed := 0
	for i, err := range jobErrs {
		switch {
		case err == nil:
		case errors.Is(err, ErrNotStartedEarlierFailure):
			errs = append(errs, fmt.Errorf("StatefulSet %s/%s: %w", jobs[i].namespace, jobs[i].name, err))
		default:
			failed++
			errs = append(errs, err)
		}
	}
	if notStarted > 0 {
		s.log.Error("%d StatefulSet(s) not started due to earlier failure", notStarted)
	}
	switch {
	case failed == 0 && notStarted == 0 && s.skipWait:
		s.log.Success("Triggered restart of %d StatefulSet(s)", len(jobs))
	case failed == 0 && notStarted == 0:
		s.log.Success("All %d restarted StatefulSet(s) are ready", len(jobs))
	default:
		s.log.Error("%d of %d StatefulSet restart(s) failed, %d not started", failed, len(jobs), notStarted)
	}

	return errors.Join(errs...)
}

// restartOne patches a single StatefulSet and, unless --skip-wait is set,
// waits (bounded by --timeout) for its rollout to complete. An API error
// while waiting trips rs at once.
func (s *StatefulSetOperations) restartOne(ctx context.Context, rs *RunStop, namespace, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("StatefulSet %s/%s not restarted: %w", namespace, name, err)
	}
	s.log.Info("Restarting StatefulSet: %s/%s", namespace, name)
	patched, err := s.clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, restartPatch(), metav1.PatchOptions{})
	if err != nil {
		err = &rolloutJobError{
			err:       fmt.Errorf("failed to trigger rollout for StatefulSet %s/%s: %w", namespace, name, err),
			ambiguous: patchOutcomeAmbiguous(err),
		}
		s.log.Error("%v", err)
		return err
	}
	s.log.Success("Successfully triggered rollout for StatefulSet: %s/%s", namespace, name)
	if s.skipWait {
		return nil
	}
	target := rolloutTarget{namespace: namespace, name: name, minGeneration: patched.Generation}
	if err := s.waitForStatefulSetsReady(ctx, []rolloutTarget{target}, rs.Trip); err != nil {
		return fmt.Errorf("StatefulSet %s/%s did not become ready: %w", namespace, name, err)
	}
	return nil
}

// waitForStatefulSetsReady waits for the restarted statefulsets to finish
// their rollout. Every API call uses the bounded timeout context.
func (s *StatefulSetOperations) waitForStatefulSetsReady(ctx context.Context, targets []rolloutTarget, trip func(error)) error {
	check := func(ctx context.Context, t rolloutTarget) (bool, string, error, error) {
		sts, err := s.clientset.AppsV1().StatefulSets(t.namespace).Get(ctx, t.name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) && ctx.Err() == nil {
				return false, "", fmt.Errorf("StatefulSet no longer exists: %w", err), nil
			}
			return false, "", nil, err
		}
		done, reason := statefulSetRolloutStatus(sts, t.minGeneration)
		return done, reason, nil, nil
	}
	return waitForRollouts(ctx, s.log, "StatefulSet", time.Duration(s.timeout)*time.Second, s.pollInterval, targets, check, trip)
}

// statefulSetRolloutStatus reports whether a StatefulSet has completed its
// rollout. minGeneration is the generation returned by the restart PATCH.
func statefulSetRolloutStatus(sts *appsv1.StatefulSet, minGeneration int64) (bool, string) {
	generation := sts.Generation
	if minGeneration > generation {
		generation = minGeneration
	}
	st := sts.Status
	if st.ObservedGeneration < generation {
		return false, fmt.Sprintf("generation not observed yet (observed: %d, expected: %d)", st.ObservedGeneration, generation)
	}

	// Status.Replicas transiently drops below the desired count mid-rollout
	// as the controller recreates pods, so compare against the spec.
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}

	switch {
	case st.UpdatedReplicas != desired:
		return false, fmt.Sprintf("pods updating (%d/%d updated)", st.UpdatedReplicas, desired)
	case st.Replicas != desired:
		return false, fmt.Sprintf("replica count converging (%d/%d)", st.Replicas, desired)
	case st.ReadyReplicas != desired:
		return false, fmt.Sprintf("pods starting (%d/%d ready)", st.ReadyReplicas, desired)
	case st.AvailableReplicas != desired:
		return false, fmt.Sprintf("pods becoming available (%d/%d available)", st.AvailableReplicas, desired)
	case st.CurrentRevision != st.UpdateRevision:
		return false, fmt.Sprintf("revision not converged (current: %s, update: %s)", st.CurrentRevision, st.UpdateRevision)
	}
	return true, ""
}

// isPostgresOperatorStatefulSet reports whether the statefulset is managed by
// the Zalando PostgreSQL Operator: it is owned by a "postgresql" resource or
// carries the Spilo application label set by the operator.
func isPostgresOperatorStatefulSet(statefulset *appsv1.StatefulSet) bool {
	for _, ref := range statefulset.OwnerReferences {
		if ref.Kind == "postgresql" {
			return true
		}
	}
	return statefulset.Labels["application"] == "spilo"
}

// isElasticsearchStatefulSet reports whether the statefulset belongs to an
// ECK-managed Elasticsearch cluster: it carries the ECK type label or is owned
// by an Elasticsearch resource.
func isElasticsearchStatefulSet(statefulset *appsv1.StatefulSet) bool {
	if statefulset.Labels["common.k8s.elastic.co/type"] == "elasticsearch" {
		return true
	}
	for _, ref := range statefulset.OwnerReferences {
		if ref.Kind == "Elasticsearch" {
			return true
		}
	}
	return false
}

// GetStatefulSetsToRestart returns "namespace/name" of every StatefulSet
// that would be restarted (PlanStatefulSets).
func (s *StatefulSetOperations) GetStatefulSetsToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	plan, err := s.PlanStatefulSets(ctx, namespaces)
	return plan.Targets(), err
}

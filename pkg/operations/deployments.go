package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DeploymentOperations implements DeploymentOperator interface.
//
// Concurrency semantics of --parallel: targets are first selected in every
// namespace (at most --parallel namespaces at once), then a pool of --parallel
// workers processes them globally across namespaces. A worker patches one
// Deployment, waits for its rollout to complete (bounded per resource by
// --timeout) and only then takes the next one, so at most --parallel rollouts
// are in progress at any time. With --skip-wait the worker does not wait, so
// --parallel only bounds concurrent PATCH requests. Any failure stops new
// launches, with or without --skip-wait.
type DeploymentOperations struct {
	clientset            K8sClient
	parallel             int
	timeout              int
	dryRun               bool
	noFlagger            bool
	log                  *logger.Logger
	minAge               *time.Duration
	podLabels            []string
	podAnnotations       []string
	containerImages      []string          // restart only resources whose pods run a matching container image
	parsedPodLabels      map[string]string // cached parsed pod labels
	parsedPodAnnotations map[string]string // cached parsed pod annotations
	skipWait             bool
	runStop              *RunStop      // run-wide stop signal shared with the other resource types
	pollInterval         time.Duration // readiness poll interval (defaultPollInterval when zero)
}

// SetLogger replaces the logger (the configured root logger, so that
// --output=json applies to every line).
func (d *DeploymentOperations) SetLogger(l *logger.Logger) {
	if l != nil {
		d.log = l
	}
}

// SetRunStop sets the run-wide stop signal shared by all resource types: a
// failure here stops launches of the other types and vice versa.
func (d *DeploymentOperations) SetRunStop(rs *RunStop) {
	d.runStop = rs
}

// NewDeploymentOperations creates a new DeploymentOperations instance
func NewDeploymentOperations(clientset K8sClient, parallel, timeout int, noFlagger, dryRun bool, minAge *time.Duration, podLabels []string, podAnnotations []string, containerImages []string, skipWait bool) *DeploymentOperations {
	log := logger.NewLogger(dryRun)
	parsedLabels, err := ParseLabelsOrAnnotationsStrict(podLabels)
	if err != nil {
		log.Warning("Ignoring malformed pod label filters: %v", err)
	}
	parsedAnnotations, err := ParseLabelsOrAnnotationsStrict(podAnnotations)
	if err != nil {
		log.Warning("Ignoring malformed pod annotation filters: %v", err)
	}
	return &DeploymentOperations{
		clientset:            clientset,
		parallel:             parallel,
		timeout:              timeout,
		dryRun:               dryRun,
		noFlagger:            noFlagger,
		log:                  log,
		minAge:               minAge,
		podLabels:            podLabels,
		podAnnotations:       podAnnotations,
		containerImages:      containerImages,
		parsedPodLabels:      parsedLabels,
		parsedPodAnnotations: parsedAnnotations,
		skipWait:             skipWait,
	}
}

// requiredPodLabels returns the parsed pod label requirements. Instances built
// without the constructor (tests) are parsed on the fly.
func (d *DeploymentOperations) requiredPodLabels() map[string]string {
	if d.parsedPodLabels == nil {
		return ParseLabelsOrAnnotations(d.podLabels)
	}
	return d.parsedPodLabels
}

// requiredPodAnnotations returns the parsed pod annotation requirements.
func (d *DeploymentOperations) requiredPodAnnotations() map[string]string {
	if d.parsedPodAnnotations == nil {
		return ParseLabelsOrAnnotations(d.podAnnotations)
	}
	return d.parsedPodAnnotations
}

// hasPodFilters reports whether any pod-level filter (labels, annotations, or
// container image) is active, based on the parsed (valid) filter values.
func (d *DeploymentOperations) hasPodFilters() bool {
	return len(d.requiredPodLabels()) > 0 ||
		len(d.requiredPodAnnotations()) > 0 ||
		len(nonEmptyStrings(d.containerImages)) > 0
}

// podMatchesFilters reports whether a pod satisfies all active pod-level filters
// (required labels AND required annotations AND container image).
func (d *DeploymentOperations) podMatchesFilters(pod *corev1.Pod) bool {
	return d.podHasRequiredLabels(pod.Labels) &&
		d.podHasRequiredAnnotations(pod.Annotations) &&
		PodMatchesAnyImage(pod, d.containerImages)
}

// podHasRequiredLabels checks if a pod has all the required labels
func (d *DeploymentOperations) podHasRequiredLabels(podLabels map[string]string) bool {
	return MatchesRequirements(podLabels, d.requiredPodLabels())
}

// podHasRequiredAnnotations checks if a pod has all the required annotations
func (d *DeploymentOperations) podHasRequiredAnnotations(podAnnotations map[string]string) bool {
	return MatchesRequirements(podAnnotations, d.requiredPodAnnotations())
}

// deploymentCandidate is a Deployment selected for restart.
type deploymentCandidate struct {
	deployment appsv1.Deployment
	reason     string
}

// deploymentSelection is the result of selecting Deployments in one namespace.
// It is shared by the dry-run preview and the execute path so both always
// agree on what will be restarted.
type deploymentSelection struct {
	namespace      string
	total          int
	toRestart      []deploymentCandidate
	hasPrimary     []string // Flagger targets skipped because their primary is managed by Flagger
	tooYoung       []appsv1.Deployment
	noMatchingPods []string
	paused         []string
}

// isFlaggerPrimary reports whether the deployment is a Flagger-managed primary.
func (d *DeploymentOperations) isFlaggerPrimary(deployment appsv1.Deployment) bool {
	return strings.HasSuffix(deployment.Name, "-primary") && d.hasFlaggerCanaryOwner(deployment)
}

// selectDeployments decides which Deployments in a namespace will be
// restarted. The Flagger primary/target relationship is built from the full
// Deployment list before any filter is applied, so filtering out a primary
// (e.g. by --min-age) never causes its canary target to be restarted instead.
func (d *DeploymentOperations) selectDeployments(ctx context.Context, namespace string) (*deploymentSelection, error) {
	deployments, err := d.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments in namespace %s: %w", namespace, err)
	}

	sel := &deploymentSelection{namespace: namespace, total: len(deployments.Items)}

	flaggerPrimaries := make(map[string]bool)
	if !d.noFlagger {
		for _, deployment := range deployments.Items {
			if d.isFlaggerPrimary(deployment) {
				flaggerPrimaries[deployment.Name] = true
			}
		}
	}

	// Flagger classification and cheap filters first.
	var candidates []deploymentCandidate
	for _, deployment := range deployments.Items {
		reason := "(no primary counterpart)"
		if d.noFlagger {
			reason = "(Flagger filter disabled)"
		} else if flaggerPrimaries[deployment.Name] {
			reason = "(Flagger primary deployment)"
		} else if flaggerPrimaries[deployment.Name+"-primary"] {
			sel.hasPrimary = append(sel.hasPrimary, deployment.Name)
			continue
		}

		if d.minAge != nil && time.Since(deployment.CreationTimestamp.Time) < *d.minAge {
			sel.tooYoung = append(sel.tooYoung, deployment)
			continue
		}

		candidates = append(candidates, deploymentCandidate{deployment: deployment, reason: reason})
	}

	// Pod-level filters, using controller ownership rather than selectors.
	if d.hasPodFilters() && len(candidates) > 0 {
		matching, err := d.deploymentsWithMatchingPods(ctx, namespace)
		if err != nil {
			return nil, err
		}
		filtered := candidates[:0]
		for _, c := range candidates {
			if !matching[c.deployment.UID] {
				sel.noMatchingPods = append(sel.noMatchingPods, c.deployment.Name)
				continue
			}
			filtered = append(filtered, c)
		}
		candidates = filtered
	}

	// A paused Deployment does not roll out template changes; patching it
	// would only leave a pending change behind and waiting would time out.
	for _, c := range candidates {
		if c.deployment.Spec.Paused {
			sel.paused = append(sel.paused, c.deployment.Name)
			continue
		}
		sel.toRestart = append(sel.toRestart, c)
	}

	return sel, nil
}

// deploymentsWithMatchingPods returns the UIDs of Deployments that control at
// least one pod satisfying the active pod filters. Ownership is verified via
// the controller chain Pod -> ReplicaSet -> Deployment, so pods of another
// workload with an overlapping selector are never attributed to a Deployment.
func (d *DeploymentOperations) deploymentsWithMatchingPods(ctx context.Context, namespace string) (map[types.UID]bool, error) {
	pods, err := d.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	matchingRS := make(map[types.UID]bool)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !d.podMatchesFilters(pod) {
			continue
		}
		if ref := metav1.GetControllerOf(pod); ref != nil && ref.Kind == "ReplicaSet" {
			matchingRS[ref.UID] = true
		}
	}

	result := make(map[types.UID]bool)
	if len(matchingRS) == 0 {
		return result, nil
	}

	replicaSets, err := d.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list replicasets in namespace %s: %w", namespace, err)
	}
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if !matchingRS[rs.UID] {
			continue
		}
		if ref := metav1.GetControllerOf(rs); ref != nil && ref.Kind == "Deployment" {
			result[ref.UID] = true
		}
	}
	return result, nil
}

// selectAll runs selectDeployments for every namespace, with at most
// d.parallel namespaces in flight. Errors are collected per namespace.
func (d *DeploymentOperations) selectAll(ctx context.Context, namespaces []string) ([]*deploymentSelection, []error) {
	sels := make([]*deploymentSelection, len(namespaces))
	errs := make([]error, len(namespaces))
	forEachBounded(d.parallel, len(namespaces), func(i int) {
		if err := ctx.Err(); err != nil {
			errs[i] = fmt.Errorf("namespace %s: deployments not selected: %w", namespaces[i], err)
			return
		}
		sels[i], errs[i] = d.selectDeployments(ctx, namespaces[i])
	})

	var outSels []*deploymentSelection
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
func (d *DeploymentOperations) logSelection(sel *deploymentSelection) {
	ns := sel.namespace
	if sel.total == 0 {
		d.log.Info("No deployments found in namespace %s", ns)
		return
	}
	d.log.Info("Found %d deployment(s) in namespace %s", sel.total, ns)

	for _, name := range sel.hasPrimary {
		d.log.Info("Skipping deployment %s/%s: has a Flagger primary counterpart", ns, name)
	}
	for _, deployment := range sel.tooYoung {
		d.log.Info("Skipping deployment %s/%s: too new (age: %s, required: %s)", ns, deployment.Name,
			time.Since(deployment.CreationTimestamp.Time).Round(time.Second), *d.minAge)
	}
	for _, name := range sel.noMatchingPods {
		d.log.Info("Skipping deployment %s/%s: no pods matching the requested labels/annotations/image", ns, name)
	}
	for _, name := range sel.paused {
		d.log.Warning("Skipping deployment %s/%s: deployment is paused (spec.paused=true); resume it to restart", ns, name)
	}

	if len(sel.toRestart) == 0 {
		d.log.Info("No deployments found to restart in namespace %s", ns)
		return
	}
	verb := "Will restart"
	if d.dryRun {
		verb = "Would restart"
	}
	d.log.Info("%s %d/%d deployment(s) in namespace %s", verb, len(sel.toRestart), sel.total, ns)
	if d.dryRun {
		for _, c := range sel.toRestart {
			d.log.Info("- %s %s", c.deployment.Name, c.reason)
		}
	}
}

// DeploymentPlan is the precomputed restart plan of the Deployments of a run:
// the Deployments selected in every namespace plus the selection errors. The
// dry run previews it and the execute path restarts exactly these targets
// (RestartPlannedDeployments), so both always agree.
type DeploymentPlan struct {
	jobs []deploymentJob
	errs []error
}

// deploymentJob is one planned Deployment restart.
type deploymentJob struct {
	namespace string
	candidate deploymentCandidate
}

// Targets returns "namespace/name" of every planned Deployment.
func (p *DeploymentPlan) Targets() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.jobs))
	for _, j := range p.jobs {
		out = append(out, j.namespace+"/"+j.candidate.deployment.Name)
	}
	return out
}

// Err returns the selection errors of the plan (nil when it is complete).
func (p *DeploymentPlan) Err() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.errs...)
}

// PlanDeployments selects the Deployments to restart in every namespace
// (read-only) and logs the selection. The returned plan is never nil; the
// error reports selection failures (e.g. a forbidden List in one namespace),
// in which case the plan holds the targets of the other namespaces.
func (d *DeploymentOperations) PlanDeployments(ctx context.Context, namespaces []string) (*DeploymentPlan, error) {
	sels, errs := d.selectAll(ctx, namespaces)
	for _, err := range errs {
		d.log.Error("Failed to select deployments: %v", err)
	}
	plan := &DeploymentPlan{errs: errs}
	for _, sel := range sels {
		d.logSelection(sel)
		for _, c := range sel.toRestart {
			plan.jobs = append(plan.jobs, deploymentJob{namespace: sel.namespace, candidate: c})
		}
	}
	return plan, plan.Err()
}

// RestartDeployments plans and restarts the Deployments of the given
// namespaces (PlanDeployments + RestartPlannedDeployments).
func (d *DeploymentOperations) RestartDeployments(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		d.log.Info("No namespaces specified, skipping deployments restart")
		return nil
	}
	plan, _ := d.PlanDeployments(ctx, namespaces)
	return d.RestartPlannedDeployments(ctx, plan)
}

// RestartPlannedDeployments restarts the Deployments of a plan. The first
// restart failure (PATCH rejected or ambiguous, rollout timed out or failed,
// API error while waiting) stops launching further rollouts: rollouts already
// in progress are still waited for, the remaining Deployments are reported as
// "not started due to earlier failure". With --skip-wait any failed PATCH
// stops further launches. A plan with selection errors trips the run-wide
// stop before anything is launched, so no Deployment is restarted at all.
// All errors are returned joined together.
func (d *DeploymentOperations) RestartPlannedDeployments(ctx context.Context, plan *DeploymentPlan) error {
	if plan == nil {
		return nil
	}
	errs := append([]error(nil), plan.errs...)
	// A selection error stops the run before anything is launched.
	rs := tripOnSelectionErrors(d.runStop, errs)
	jobs := plan.jobs

	if d.dryRun || len(jobs) == 0 {
		return errors.Join(errs...)
	}

	if d.skipWait {
		d.log.Info("Skipping wait for deployments readiness (--skip-wait flag is set)")
	}

	// Each worker patches one Deployment and waits for its rollout before
	// taking the next one, so at most d.parallel rollouts are in progress at
	// any time across all namespaces. After a failure no new rollout is
	// started (see stopsLaunches): a timed-out or ambiguous rollout may still
	// be running in the cluster and keeps occupying its slot.
	jobErrs, notStarted := runRolloutJobs(d.parallel, len(jobs), rs, func(i int) error {
		return d.restartOne(ctx, rs, jobs[i].namespace, jobs[i].candidate)
	}, func(err error) bool { return stopsLaunches(err) })

	failed := 0
	for i, err := range jobErrs {
		switch {
		case err == nil:
		case errors.Is(err, ErrNotStartedEarlierFailure):
			errs = append(errs, fmt.Errorf("deployment %s/%s: %w", jobs[i].namespace, jobs[i].candidate.deployment.Name, err))
		default:
			failed++
			errs = append(errs, err)
		}
	}
	if notStarted > 0 {
		d.log.Error("%d deployment(s) not started due to earlier failure", notStarted)
	}
	switch {
	case failed == 0 && notStarted == 0 && d.skipWait:
		d.log.Success("Triggered restart of %d deployment(s)", len(jobs))
	case failed == 0 && notStarted == 0:
		d.log.Success("Successfully restarted %d deployment(s)", len(jobs))
	default:
		d.log.Error("%d of %d deployment restart(s) failed, %d not started", failed, len(jobs), notStarted)
	}

	return errors.Join(errs...)
}

// restartOne patches a single Deployment and, unless --skip-wait is set,
// waits (bounded by --timeout) for its rollout to complete. An API error
// while waiting trips rs at once.
func (d *DeploymentOperations) restartOne(ctx context.Context, rs *RunStop, namespace string, c deploymentCandidate) error {
	name := c.deployment.Name
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("deployment %s/%s not restarted: %w", namespace, name, err)
	}
	d.log.Info("Restarting deployment %s/%s %s", namespace, name, c.reason)
	patched, err := d.clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, restartPatch(), metav1.PatchOptions{})
	if err != nil {
		err = &rolloutJobError{
			err:       fmt.Errorf("failed to patch deployment %s/%s: %w", namespace, name, err),
			ambiguous: patchOutcomeAmbiguous(err),
		}
		d.log.Error("%v", err)
		return err
	}
	d.log.Success("Successfully triggered restart of deployment %s/%s", namespace, name)
	if d.skipWait {
		return nil
	}
	target := rolloutTarget{namespace: namespace, name: name, minGeneration: patched.Generation}
	if err := d.waitForDeploymentsReady(ctx, []rolloutTarget{target}, rs.Trip); err != nil {
		return fmt.Errorf("deployment %s/%s did not become ready: %w", namespace, name, err)
	}
	return nil
}

// waitForDeploymentsReady waits for the restarted deployments to finish their
// rollout. Every API call uses the bounded timeout context.
func (d *DeploymentOperations) waitForDeploymentsReady(ctx context.Context, targets []rolloutTarget, trip func(error)) error {
	check := func(ctx context.Context, t rolloutTarget) (bool, string, error, error) {
		deployment, err := d.clientset.AppsV1().Deployments(t.namespace).Get(ctx, t.name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) && ctx.Err() == nil {
				return false, "", fmt.Errorf("deployment no longer exists: %w", err), nil
			}
			return false, "", nil, err
		}
		done, reason, terminal := deploymentRolloutStatus(deployment, t.minGeneration)
		return done, reason, terminal, nil
	}
	return waitForRollouts(ctx, d.log, "deployment", time.Duration(d.timeout)*time.Second, d.pollInterval, targets, check, trip)
}

// deploymentRolloutStatus reports whether a Deployment has completed its
// rollout. minGeneration is the generation returned by the restart PATCH.
//
// Completion is derived from generation and replica counts only. The
// Progressing condition is not required, because Kubernetes removes it when
// progressDeadlineSeconds is MaxInt32; it is only used to fail fast when the
// progress deadline has been exceeded.
func deploymentRolloutStatus(deployment *appsv1.Deployment, minGeneration int64) (done bool, reason string, terminal error) {
	generation := deployment.Generation
	if minGeneration > generation {
		generation = minGeneration
	}
	if deployment.Status.ObservedGeneration < generation {
		return false, fmt.Sprintf("generation not observed yet (observed: %d, expected: %d)",
			deployment.Status.ObservedGeneration, generation), nil
	}

	var notes []string
	for _, condition := range deployment.Status.Conditions {
		switch condition.Type {
		case appsv1.DeploymentProgressing:
			if condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded" {
				return false, "", fmt.Errorf("progress deadline exceeded: %s", condition.Message)
			}
		case appsv1.DeploymentReplicaFailure:
			if condition.Status == corev1.ConditionTrue {
				notes = append(notes, fmt.Sprintf("replica failure: %s", condition.Message))
			}
		}
	}

	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	st := deployment.Status

	switch {
	case st.UpdatedReplicas != desired:
		notes = append([]string{fmt.Sprintf("pods updating (%d/%d updated)", st.UpdatedReplicas, desired)}, notes...)
	case st.Replicas != st.UpdatedReplicas:
		notes = append([]string{fmt.Sprintf("old pods terminating (%d total, %d updated)", st.Replicas, st.UpdatedReplicas)}, notes...)
	case st.AvailableReplicas != desired:
		notes = append([]string{fmt.Sprintf("pods becoming available (%d/%d available)", st.AvailableReplicas, desired)}, notes...)
	case st.ReadyReplicas != desired:
		notes = append([]string{fmt.Sprintf("pods starting (%d/%d ready)", st.ReadyReplicas, desired)}, notes...)
	default:
		return true, "", nil
	}
	return false, strings.Join(notes, ", "), nil
}

// hasFlaggerCanaryOwner checks if the deployment has a Flagger Canary owner reference
func (d *DeploymentOperations) hasFlaggerCanaryOwner(deployment appsv1.Deployment) bool {
	for _, ownerRef := range deployment.OwnerReferences {
		if ownerRef.APIVersion == "flagger.app/v1beta1" &&
			ownerRef.Kind == "Canary" &&
			ownerRef.Controller != nil && *ownerRef.Controller {
			return true
		}
	}
	return false
}

// GetDeploymentsToRestart returns "namespace/name" of every Deployment that
// would be restarted (PlanDeployments).
func (d *DeploymentOperations) GetDeploymentsToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	plan, err := d.PlanDeployments(ctx, namespaces)
	return plan.Targets(), err
}

package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// Default Strimzi API group/versions. These have changed across Strimzi
// releases (Kafka and StrimziPodSet moved from v1beta2 to v1 in 0.49), so they
// are configurable via CLI flags. When the configured value is empty or equal
// to the default, the served version is discovered at runtime (v1 preferred,
// v1beta2 as a fallback).
const (
	// DefaultKafkaAPIVersion is the default group/version for the Kafka CR.
	DefaultKafkaAPIVersion = "kafka.strimzi.io/v1"
	// DefaultStrimziPodSetAPIVersion is the default group/version for StrimziPodSet.
	DefaultStrimziPodSetAPIVersion = "core.strimzi.io/v1"
)

const (
	strimziClusterLabel        = "strimzi.io/cluster"
	strimziKindLabel           = "strimzi.io/kind"
	strimziManualRollingUpdate = "strimzi.io/manual-rolling-update"
)

// strimziSupportedVersions lists the Strimzi API versions this tool can work
// with, in order of preference.
var strimziSupportedVersions = []string{"v1", "v1beta2"}

// KafkaOperations implements KafkaOperator interface
type KafkaOperations struct {
	clientset     K8sClient
	parallel      int
	timeout       int
	dryRun        bool
	log           *logger.Logger
	minAge        *time.Duration
	skipWait      bool
	kafkaAPIPath  string // e.g. /apis/kafka.strimzi.io/v1beta2 (configured value)
	podSetAPIPath string // e.g. /apis/core.strimzi.io/v1beta2 (configured value)

	// Group and explicitly requested version for each API. An empty version
	// means "discover the served version at runtime".
	kafkaGroup    string
	kafkaVersion  string
	podSetGroup   string
	podSetVersion string

	runStop *RunStop // run-wide stop signal shared with the other resource types
}

// NewKafkaOperations creates a new KafkaOperations instance.
// kafkaAPIVersion and podSetAPIVersion are group/version strings (e.g.
// "kafka.strimzi.io/v1beta2"); empty values (or the defaults) enable runtime
// discovery of the served version.
func NewKafkaOperations(clientset K8sClient, parallel, timeout int, dryRun bool, minAge *time.Duration, skipWait bool, kafkaAPIVersion, podSetAPIVersion string) *KafkaOperations {
	autoKafka := kafkaAPIVersion == "" || kafkaAPIVersion == DefaultKafkaAPIVersion
	autoPodSet := podSetAPIVersion == "" || podSetAPIVersion == DefaultStrimziPodSetAPIVersion
	if kafkaAPIVersion == "" {
		kafkaAPIVersion = DefaultKafkaAPIVersion
	}
	if podSetAPIVersion == "" {
		podSetAPIVersion = DefaultStrimziPodSetAPIVersion
	}
	kafkaGroup, kafkaVersion := splitGroupVersion(kafkaAPIVersion)
	podSetGroup, podSetVersion := splitGroupVersion(podSetAPIVersion)
	if autoKafka {
		kafkaVersion = ""
	}
	if autoPodSet {
		podSetVersion = ""
	}
	return &KafkaOperations{
		clientset:     clientset,
		parallel:      parallel,
		timeout:       timeout,
		dryRun:        dryRun,
		log:           logger.NewLogger(dryRun),
		minAge:        minAge,
		skipWait:      skipWait,
		kafkaAPIPath:  "/apis/" + trimAPIsPrefix(kafkaAPIVersion),
		podSetAPIPath: "/apis/" + trimAPIsPrefix(podSetAPIVersion),
		kafkaGroup:    kafkaGroup,
		kafkaVersion:  kafkaVersion,
		podSetGroup:   podSetGroup,
		podSetVersion: podSetVersion,
	}
}

// SetLogger replaces the logger (the configured root logger, so that
// --output=json applies to every line).
func (k *KafkaOperations) SetLogger(l *logger.Logger) {
	if l != nil {
		k.log = l
	}
}

// SetRunStop sets the run-wide stop signal shared by all resource types.
func (k *KafkaOperations) SetRunStop(rs *RunStop) {
	k.runStop = rs
}

// strimziAPIs holds the API paths resolved for a single run.
type strimziAPIs struct {
	kafkaPath  string
	podSetPath string
	// podSetErr is set when the StrimziPodSet API cannot be used.
	podSetErr error
	// podSetMissing is true when podSetErr only says that the StrimziPodSet
	// API group is not installed (no discovery failure): that is harmless
	// as long as there is no Kafka cluster to restart.
	podSetMissing bool
}

// KafkaPlan is the precomputed restart plan of the Kafka clusters of a run:
// the resolved Strimzi APIs, the selected clusters and the planning errors
// (API discovery, selection). The dry run previews it and the execute path
// restarts exactly these targets (RestartPlannedKafkaClusters).
type KafkaPlan struct {
	apis    strimziAPIs
	targets []crTarget
	errs    []error
}

// Targets returns "namespace/name" of every planned Kafka cluster.
func (p *KafkaPlan) Targets() []string {
	if p == nil {
		return nil
	}
	return crTargetNames(p.targets)
}

// Err returns the planning errors (nil when the plan is complete).
func (p *KafkaPlan) Err() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.errs...)
}

// PlanKafkaClusters discovers the Strimzi APIs and selects the Kafka
// clusters to restart in every namespace (read-only), logging the selection.
// A missing Kafka API group yields an empty plan. A discovery error of the
// Kafka or the StrimziPodSet API (e.g. 403/5xx) is a planning error even
// when no Kafka cluster is selected; a StrimziPodSet API group that is simply
// not installed is an error only when there is a cluster to restart. The
// returned plan is never nil.
func (k *KafkaOperations) PlanKafkaClusters(ctx context.Context, namespaces []string) (*KafkaPlan, error) {
	plan := &KafkaPlan{}
	apis, present, err := k.resolveAPIs(ctx)
	if err != nil {
		k.log.Error("Failed to plan Kafka clusters: %v", err)
		plan.errs = append(plan.errs, err)
		return plan, plan.Err()
	}
	if !present {
		k.log.Info("Kafka API group %s not found, skipping Kafka operations", k.kafkaGroup)
		return plan, nil
	}
	plan.apis = apis
	plan.targets, plan.errs = selectCRTargets(ctx, k.log, "Kafka", k.parallel, k.dryRun, k.minAge, namespaces,
		func(ctx context.Context, ns string) ([]namedCR, []namedCR, int, error) {
			return k.selectKafkas(ctx, ns, apis.kafkaPath)
		})
	if apis.podSetErr != nil && (!apis.podSetMissing || len(plan.targets) > 0) {
		k.log.Error("Failed to plan Kafka clusters: %v", apis.podSetErr)
		plan.errs = append(plan.errs, apis.podSetErr)
	}
	return plan, plan.Err()
}

// RestartKafkaClusters plans and restarts the Kafka clusters of the given
// namespaces (PlanKafkaClusters + RestartPlannedKafkaClusters).
func (k *KafkaOperations) RestartKafkaClusters(ctx context.Context, namespaces []string) error {
	if len(namespaces) == 0 {
		k.log.Info("No namespaces specified, skipping Kafka cluster restart")
		return nil
	}
	plan, _ := k.PlanKafkaClusters(ctx, namespaces)
	return k.RestartPlannedKafkaClusters(ctx, plan)
}

// RestartPlannedKafkaClusters restarts the Kafka clusters of a plan with at
// most --parallel restarts in progress at any time. Any failure (a rejected
// or ambiguous PodSet PATCH, a failed or timed-out wait, an API error while
// waiting), with or without --skip-wait, stops new launches here and, through
// the shared RunStop, for every other resource type; clusters in flight are
// still waited for and the remaining ones are reported as "not started due to
// earlier failure". A plan with errors (discovery or selection) trips the
// stop before anything is launched, so no cluster is restarted at all.
func (k *KafkaOperations) RestartPlannedKafkaClusters(ctx context.Context, plan *KafkaPlan) error {
	if plan == nil {
		return nil
	}
	errs := append([]error(nil), plan.errs...)
	// A planning error stops the run before anything is launched.
	rs := tripOnSelectionErrors(k.runStop, errs)
	if !k.dryRun && len(plan.targets) > 0 {
		k.log.Info("Starting restart of %d Kafka cluster(s)", len(plan.targets))
		apis := plan.apis
		errs = append(errs, restartCRTargets(ctx, k.log, "Kafka", k.parallel, k.skipWait, rs, plan.targets,
			func(ctx context.Context, t crTarget, rs *RunStop) error {
				return k.restartKafkaCluster(ctx, t.namespace, t.name, apis, rs)
			}))
	}
	if err := errors.Join(errs...); err != nil {
		k.log.Warning("Kafka clusters restart finished with errors")
		return fmt.Errorf("kafka restart failed: %w", err)
	}
	if len(plan.targets) > 0 && !k.dryRun {
		k.log.Success("Kafka clusters restart completed successfully")
	}
	return nil
}

// resolveAPIs discovers the served Kafka and StrimziPodSet API versions.
// present is false when the Kafka API group is not installed at all.
func (k *KafkaOperations) resolveAPIs(ctx context.Context) (strimziAPIs, bool, error) {
	rc := k.clientset.RESTClient()

	kafkaPath, present, err := ResolveAPIGroupVersion(ctx, rc, k.kafkaGroup, k.kafkaVersion, strimziSupportedVersions...)
	if err != nil {
		return strimziAPIs{}, false, fmt.Errorf("kafka API: %w", err)
	}
	if !present {
		return strimziAPIs{}, false, nil
	}

	apis := strimziAPIs{kafkaPath: kafkaPath}
	podSetPath, present, err := ResolveAPIGroupVersion(ctx, rc, k.podSetGroup, k.podSetVersion, strimziSupportedVersions...)
	switch {
	case err != nil:
		apis.podSetErr = fmt.Errorf("StrimziPodSet API: %w", err)
	case !present:
		apis.podSetErr = fmt.Errorf("StrimziPodSet API group %s is not installed, cannot restart Kafka clusters", k.podSetGroup)
		apis.podSetMissing = true
	default:
		apis.podSetPath = podSetPath
	}
	return apis, true, nil
}

// selectKafkas lists the Kafka CRs of a namespace and splits them by
// --older-than. It is shared by execute and the dry-run preview.
func (k *KafkaOperations) selectKafkas(ctx context.Context, namespace, kafkaPath string) (toRestart, tooYoung []namedCR, total int, err error) {
	kafkaList, err := k.clientset.RESTClient().Get().
		AbsPath(kafkaPath).
		Namespace(namespace).
		Resource("kafkas").
		DoRaw(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to list Kafka clusters in namespace %s: %w", namespace, err)
	}
	toRestart, tooYoung, total, err = filterCRsByAge(kafkaList, k.minAge)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to parse Kafka clusters in namespace %s: %w", namespace, err)
	}
	return toRestart, tooYoung, total, nil
}

// GetKafkaClustersToRestart returns "namespace/name" of every Kafka cluster
// that would be restarted (PlanKafkaClusters).
func (k *KafkaOperations) GetKafkaClustersToRestart(ctx context.Context, namespaces []string) ([]string, error) {
	plan, err := k.PlanKafkaClusters(ctx, namespaces)
	return plan.Targets(), err
}

// strimziPodSet holds the StrimziPodSet fields we care about.
type strimziPodSet struct {
	Metadata struct {
		Name            string                  `json:"name"`
		Labels          map[string]string       `json:"labels"`
		Annotations     map[string]string       `json:"annotations"`
		OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		// Pods is the desired set of pods managed by the PodSet.
		Pods []json.RawMessage `json:"pods"`
	} `json:"spec"`
}

// restartKafkaCluster triggers a manual rolling update of every StrimziPodSet
// that belongs to the Kafka cluster and waits for it to finish.
func (k *KafkaOperations) restartKafkaCluster(ctx context.Context, namespace, kafkaName string, apis strimziAPIs, rs *RunStop) error {
	k.log.Info("Processing Kafka cluster: %s/%s", namespace, kafkaName)

	podSets, err := k.listPodSetsForKafka(ctx, namespace, kafkaName, apis.podSetPath)
	if err != nil {
		return err
	}
	if len(podSets) == 0 {
		return fmt.Errorf("no StrimziPodSets found for Kafka cluster %s/%s (label %s=%s)",
			namespace, kafkaName, strimziClusterLabel, kafkaName)
	}

	spec := k.kafkaWaitSpec(namespace, kafkaName, podSets, apis)
	if !k.skipWait {
		if err := spec.recordPods(ctx, k.clientset); err != nil {
			return err
		}
	}

	// A failed PATCH of one PodSet must not abandon the PodSets that were
	// already annotated: their rolls are in progress and the caller must not
	// move on before they finish. Ambiguous failures (timeouts, connection
	// resets, 5xx) may have been applied, so they are waited for as well.
	// Any PATCH failure trips rs right away, so no new restart of any
	// resource type starts while this cluster is still being waited for.
	var (
		errs      []error
		mutated   []string
		ambiguous bool
	)
	for _, podSetName := range podSets {
		k.log.Info("Adding restart annotation to StrimziPodSet %s/%s", namespace, podSetName)
		if err := k.annotateStrimziPodSet(ctx, namespace, podSetName, apis.podSetPath); err != nil {
			wrapped := fmt.Errorf("failed to annotate StrimziPodSet %s/%s: %w", namespace, podSetName, err)
			errs = append(errs, wrapped)
			rs.Trip(wrapped)
			if !mutationDefinitelyFailed(err) {
				// May have been applied: wait for it as well.
				ambiguous = true
				k.log.Warning("Annotating StrimziPodSet %s/%s may have been applied (%v); waiting for it as well", namespace, podSetName, err)
				mutated = append(mutated, podSetName)
			}
			continue
		}
		mutated = append(mutated, podSetName)
		k.log.Success("Successfully added restart annotation to StrimziPodSet %s/%s", namespace, podSetName)
	}
	// result returns the collected errors, marked as ambiguous when a PodSet
	// PATCH may have been applied (see stopsLaunches).
	result := func() error {
		if err := errors.Join(errs...); err != nil {
			return &rolloutJobError{err: err, ambiguous: ambiguous}
		}
		return nil
	}
	if len(mutated) == 0 {
		return result()
	}
	if k.skipWait {
		k.log.Info("Skipping wait for Kafka cluster %s/%s (--skip-wait flag is set)", namespace, kafkaName)
		return result()
	}

	if len(errs) > 0 {
		k.log.Warning("Kafka cluster %s/%s: waiting for the %d of %d StrimziPodSet(s) that were (or may have been) annotated before reporting the errors",
			namespace, kafkaName, len(mutated), len(podSets))
	}
	waitSpec := k.kafkaWaitSpec(namespace, kafkaName, mutated, apis)
	waitSpec.oldUIDs = spec.oldUIDs
	waitSpec.expected = spec.expected
	// An API error while waiting trips rs at once; waiting goes on.
	if err := waitForClusterRestart(ctx, k.clientset, k.log, k.timeout, waitSpec, rs.Trip); err != nil {
		errs = append(errs, err)
	}
	return result()
}

// kafkaWaitSpec describes the pods of the given PodSets of a Kafka cluster.
// The expected pod count is read from the PodSets' desired state.
func (k *KafkaOperations) kafkaWaitSpec(namespace, kafkaName string, podSets []string, apis strimziAPIs) clusterWaitSpec {
	podSetNames := make(map[string]bool, len(podSets))
	for _, name := range podSets {
		podSetNames[name] = true
	}
	return clusterWaitSpec{
		kind:      "Kafka",
		namespace: namespace,
		name:      kafkaName,
		selector:  strimziClusterLabel + "=" + kafkaName,
		filter: func(pod *corev1.Pod) bool {
			return podOwnedByPodSet(pod, podSetNames)
		},
		desired: func(ctx context.Context) (int, error) {
			return k.desiredPodSetPods(ctx, namespace, podSets, apis)
		},
		extraCheck: func(ctx context.Context) (bool, string, error) {
			return k.kafkaRollCompleted(ctx, namespace, kafkaName, podSets, apis)
		},
	}
}

// getStrimziPodSet fetches a single StrimziPodSet.
func (k *KafkaOperations) getStrimziPodSet(ctx context.Context, namespace, name string, apis strimziAPIs) (strimziPodSet, error) {
	var ps strimziPodSet
	raw, err := k.clientset.RESTClient().Get().
		AbsPath(apis.podSetPath).
		Namespace(namespace).
		Resource("strimzipodsets").
		Name(name).
		DoRaw(ctx)
	if err != nil {
		return ps, fmt.Errorf("failed to get StrimziPodSet %s/%s: %w", namespace, name, err)
	}
	if err := json.Unmarshal(raw, &ps); err != nil {
		return ps, fmt.Errorf("failed to parse StrimziPodSet %s/%s: %w", namespace, name, err)
	}
	return ps, nil
}

// desiredPodSetPods sums spec.pods of the given PodSets: the number of pods
// the operator wants to run, independent of what currently exists.
func (k *KafkaOperations) desiredPodSetPods(ctx context.Context, namespace string, podSets []string, apis strimziAPIs) (int, error) {
	total := 0
	for _, name := range podSets {
		ps, err := k.getStrimziPodSet(ctx, namespace, name, apis)
		if err != nil {
			return 0, err
		}
		total += len(ps.Spec.Pods)
	}
	return total, nil
}

// mutationDefinitelyFailed reports whether a failed mutating request was
// certainly not applied: the API server answered with a 4xx status (other
// than 408). Transport errors, timeouts and 5xx answers are ambiguous.
func mutationDefinitelyFailed(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= 400 && code < 500 && code != http.StatusRequestTimeout
}

// listPodSetsForKafka lists StrimziPodSets labelled with the Kafka cluster
// name. This covers both the classic "<cluster>-kafka"/"<cluster>-zookeeper"
// PodSets and KafkaNodePool PodSets ("<cluster>-<pool>").
func (k *KafkaOperations) listPodSetsForKafka(ctx context.Context, namespace, kafkaName, podSetPath string) ([]string, error) {
	raw, err := k.clientset.RESTClient().Get().
		AbsPath(podSetPath).
		Namespace(namespace).
		Resource("strimzipodsets").
		Param("labelSelector", strimziClusterLabel+"="+kafkaName).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list StrimziPodSets in namespace %s: %w", namespace, err)
	}

	var list struct {
		Items []strimziPodSet `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("failed to parse StrimziPodSets: %w", err)
	}
	return selectPodSetsForKafka(list.Items, kafkaName), nil
}

// selectPodSetsForKafka returns the names of the PodSets that belong to the
// given Kafka cluster (by label, and by owner reference when present).
func selectPodSetsForKafka(items []strimziPodSet, kafkaName string) []string {
	var names []string
	for _, ps := range items {
		if ps.Metadata.Labels[strimziClusterLabel] != kafkaName {
			continue
		}
		if kind := ps.Metadata.Labels[strimziKindLabel]; kind != "" && kind != "Kafka" {
			continue
		}
		if len(ps.Metadata.OwnerReferences) > 0 {
			owned := false
			for _, ref := range ps.Metadata.OwnerReferences {
				if ref.Kind == "Kafka" && ref.Name == kafkaName {
					owned = true
					break
				}
				if ref.Kind == "KafkaNodePool" {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
		}
		names = append(names, ps.Metadata.Name)
	}
	sort.Strings(names)
	return names
}

// podOwnedByPodSet reports whether the pod is controlled by one of the PodSets.
func podOwnedByPodSet(pod *corev1.Pod, podSets map[string]bool) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "StrimziPodSet" && podSets[ref.Name] {
			return true
		}
	}
	return false
}

// annotateStrimziPodSet adds the manual rolling update annotation to a StrimziPodSet
func (k *KafkaOperations) annotateStrimziPodSet(ctx context.Context, namespace, name, podSetPath string) error {
	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				strimziManualRollingUpdate: "true",
			},
		},
	}

	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return err
	}

	err = k.clientset.RESTClient().
		Patch(types.MergePatchType).
		AbsPath(podSetPath).
		Namespace(namespace).
		Resource("strimzipodsets").
		Name(name).
		Body(patchBytes).
		Do(ctx).
		Error()

	return err
}

// kafkaRollCompleted checks the operator-side signals of a finished manual
// rolling update: the annotation was removed from every PodSet and the Kafka
// CR reports Ready=True.
func (k *KafkaOperations) kafkaRollCompleted(ctx context.Context, namespace, kafkaName string, podSets []string, apis strimziAPIs) (bool, string, error) {
	for _, name := range podSets {
		ps, err := k.getStrimziPodSet(ctx, namespace, name, apis)
		if err != nil {
			return false, "", err
		}
		if _, ok := ps.Metadata.Annotations[strimziManualRollingUpdate]; ok {
			return false, fmt.Sprintf("StrimziPodSet %s still has %s annotation", name, strimziManualRollingUpdate), nil
		}
	}

	raw, err := k.clientset.RESTClient().Get().
		AbsPath(apis.kafkaPath).
		Namespace(namespace).
		Resource("kafkas").
		Name(kafkaName).
		DoRaw(ctx)
	if err != nil {
		return false, "", fmt.Errorf("failed to get Kafka %s/%s: %w", namespace, kafkaName, err)
	}
	ready, err := crConditionTrue(raw, "Ready")
	if err != nil {
		return false, "", fmt.Errorf("failed to parse Kafka %s/%s: %w", namespace, kafkaName, err)
	}
	if !ready {
		return false, "Kafka CR is not Ready yet", nil
	}
	return true, "", nil
}

// crConditionTrue reports whether status.conditions contains condType=True.
func crConditionTrue(raw []byte, condType string) (bool, error) {
	var cr struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return false, err
	}
	for _, c := range cr.Status.Conditions {
		if c.Type == condType {
			return c.Status == "True", nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Helpers shared by the operator-managed (CR based) restarts: Kafka and
// PostgreSQL.
// ---------------------------------------------------------------------------

// crPollInterval is how often waiters poll the cluster. A variable so tests
// can shorten it.
var crPollInterval = 5 * time.Second

// trimAPIsPrefix strips an optional leading "/apis/" from a group/version.
func trimAPIsPrefix(s string) string {
	s = strings.TrimPrefix(s, "/")
	return strings.TrimPrefix(s, "apis/")
}

// splitGroupVersion splits "group/version" (optionally prefixed with /apis/).
func splitGroupVersion(gv string) (group, version string) {
	gv = trimAPIsPrefix(gv)
	if i := strings.Index(gv, "/"); i >= 0 {
		return gv[:i], gv[i+1:]
	}
	return gv, ""
}

// ResolveAPIGroupVersion discovers which versions of an API group are served
// by querying /apis/<group>. It returns the "/apis/<group>/<version>" path to
// use and whether the group is installed at all.
//
//   - group not installed (404)           -> ("", false, nil)
//   - discovery failed (403, 5xx, ...)    -> error
//   - version != "": that version must be served, otherwise an error
//   - version == "": the first served version from preferred is used; if none
//     is served this is an error (incompatible operator version), not
//     "not installed".
func ResolveAPIGroupVersion(ctx context.Context, rc rest.Interface, group, version string, preferred ...string) (string, bool, error) {
	raw, err := rc.Get().AbsPath("/apis", group).DoRaw(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to discover API group %s: %w", group, err)
	}

	var apiGroup struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(raw, &apiGroup); err != nil {
		return "", false, fmt.Errorf("failed to parse API group %s discovery: %w", group, err)
	}

	served := make(map[string]bool, len(apiGroup.Versions))
	servedList := make([]string, 0, len(apiGroup.Versions))
	for _, v := range apiGroup.Versions {
		served[v.Version] = true
		servedList = append(servedList, v.Version)
	}

	if version != "" {
		if served[version] {
			return "/apis/" + group + "/" + version, true, nil
		}
		return "", true, fmt.Errorf("API group %s is installed but version %s is not served (served: %s)",
			group, version, strings.Join(servedList, ", "))
	}

	for _, v := range preferred {
		if served[v] {
			return "/apis/" + group + "/" + v, true, nil
		}
	}
	return "", true, fmt.Errorf("API group %s is installed but none of the supported versions (%s) is served (served: %s)",
		group, strings.Join(preferred, ", "), strings.Join(servedList, ", "))
}

// crTarget is one operator-managed custom resource (Kafka, postgresql)
// selected for restart.
type crTarget struct {
	namespace string
	name      string
}

// crTargetNames returns "namespace/name" of every target.
func crTargetNames(targets []crTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.namespace+"/"+t.name)
	}
	return out
}

// crSelector lists the CRs of one namespace and splits them by --older-than.
type crSelector func(ctx context.Context, namespace string) (toRestart, tooYoung []namedCR, total int, err error)

// selectCRTargets runs sel for every namespace (at most parallel at once),
// logs the selection per namespace in namespace order and returns the
// targets (in namespace order) and the selection errors. Selection is
// read-only, so it also runs when launches are already stopped: the targets
// are then reported as not started.
func selectCRTargets(ctx context.Context, log *logger.Logger, kind string, parallel int, dryRun bool, minAge *time.Duration, namespaces []string, sel crSelector) ([]crTarget, []error) {
	type result struct {
		toRestart, tooYoung []namedCR
		total               int
		err                 error
	}
	results := make([]result, len(namespaces))
	forEachBounded(parallel, len(namespaces), func(i int) {
		r := &results[i]
		if err := ctx.Err(); err != nil {
			r.err = fmt.Errorf("namespace %s: %s clusters not selected: %w", namespaces[i], kind, err)
			return
		}
		r.toRestart, r.tooYoung, r.total, r.err = sel(ctx, namespaces[i])
	})

	var (
		targets []crTarget
		errs    []error
	)
	for i, ns := range namespaces {
		r := results[i]
		switch {
		case r.err != nil:
			log.Error("Failed to select %s clusters: %v", kind, r.err)
			errs = append(errs, r.err)
			continue
		case r.total == 0:
			if !dryRun {
				log.Info("No %s clusters found in namespace %s", kind, ns)
			}
			continue
		}
		log.Info("Found %d %s cluster(s) in namespace %s, will restart %d cluster(s)", r.total, kind, ns, len(r.toRestart))
		if dryRun {
			logDryRunCRs(log, kind, ns, r.toRestart, r.tooYoung, minAge)
		} else if len(r.toRestart) == 0 {
			log.Info("No %s clusters to restart in namespace %s", kind, ns)
		}
		for _, cr := range r.toRestart {
			targets = append(targets, crTarget{namespace: ns, name: cr.Name})
		}
	}
	return targets, errs
}

// restartCRTargets restarts the targets with at most parallel restarts in
// progress at any time (one restart = one cluster, including its wait). After
// a failure (see stopsLaunches) rs is tripped and no new restart is started,
// for this or any other resource type sharing rs; restarts in flight finish
// and the remaining targets are reported as "not started due to earlier
// failure". restart may trip rs itself as soon as it knows the restart failed,
// before waiting for what it already mutated. All errors are returned joined.
func restartCRTargets(ctx context.Context, log *logger.Logger, kind string, parallel int, skipWait bool, rs *RunStop, targets []crTarget, restart func(context.Context, crTarget, *RunStop) error) error {
	rs = rs.orNew()
	prefix := strings.ToLower(kind)
	jobErrs, notStarted := runRolloutJobs(parallel, len(targets), rs, func(i int) error {
		t := targets[i]
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s %s/%s not restarted: %w", prefix, t.namespace, t.name, err)
		}
		if err := restart(ctx, t, rs); err != nil {
			log.Warning("Failed to restart %s cluster %s/%s: %v", kind, t.namespace, t.name, err)
			return fmt.Errorf("%s %s/%s: %w", prefix, t.namespace, t.name, err)
		}
		return nil
	}, func(err error) bool { return stopsLaunches(err) })

	var errs []error
	failed := 0
	for i, err := range jobErrs {
		switch {
		case err == nil:
		case errors.Is(err, ErrNotStartedEarlierFailure):
			errs = append(errs, fmt.Errorf("%s %s/%s: %w", prefix, targets[i].namespace, targets[i].name, err))
		default:
			failed++
			errs = append(errs, err)
		}
	}
	if notStarted > 0 {
		log.Error("%d %s cluster(s) not started due to earlier failure", notStarted, kind)
	}
	if failed > 0 || notStarted > 0 {
		log.Error("%d of %d %s cluster restart(s) failed, %d not started", failed, len(targets), kind, notStarted)
	}
	return errors.Join(errs...)
}

// namedCR is a custom resource name with its creation time.
type namedCR struct {
	Name              string
	CreationTimestamp time.Time
}

// filterCRsByAge parses a CR list and splits it by minimum age.
func filterCRsByAge(raw []byte, minAge *time.Duration) (toRestart, tooYoung []namedCR, total int, err error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, 0, err
	}

	for _, item := range list.Items {
		cr := namedCR{Name: item.Metadata.Name, CreationTimestamp: item.Metadata.CreationTimestamp}
		if minAge != nil && time.Since(cr.CreationTimestamp) < *minAge {
			tooYoung = append(tooYoung, cr)
			continue
		}
		toRestart = append(toRestart, cr)
	}
	return toRestart, tooYoung, len(list.Items), nil
}

// logDryRunCRs prints what a dry run would restart and skip.
func logDryRunCRs(log *logger.Logger, kind, namespace string, toRestart, tooYoung []namedCR, minAge *time.Duration) {
	if len(toRestart) > 0 {
		log.Info("%s clusters that would be restarted in namespace %s:", kind, namespace)
		for _, cr := range toRestart {
			age := time.Since(cr.CreationTimestamp).Round(time.Second)
			log.Info("- %s/%s (age: %s)", namespace, cr.Name, age)
		}
	}

	if len(tooYoung) > 0 && minAge != nil {
		log.Info("\n%s clusters that would be skipped because they are too new in namespace %s:", kind, namespace)
		for _, cr := range tooYoung {
			age := time.Since(cr.CreationTimestamp).Round(time.Second)
			log.Info("- %s/%s (age: %s, required: %s)", namespace, cr.Name, age, *minAge)
		}
	}
}

// clusterWaitSpec describes the pods of one operator-managed cluster and how
// to decide that its restart has finished.
type clusterWaitSpec struct {
	kind      string
	namespace string
	name      string
	// selector is the label selector for the cluster's pods.
	selector string
	// filter optionally narrows the selected pods further.
	filter func(*corev1.Pod) bool
	// desired optionally returns the number of pods the operator wants to
	// run (from the CR / PodSet spec). It is re-read on every poll; without it
	// the pre-restart snapshot count is used.
	desired func(ctx context.Context) (int, error)
	// extraCheck optionally verifies operator-side status once all pods have
	// been replaced and are ready.
	extraCheck func(ctx context.Context) (bool, string, error)

	// oldUIDs and expected are filled in by recordPods before the restart;
	// expected is refreshed from desired while waiting.
	oldUIDs  map[types.UID]string
	expected int
}

// listPods returns the (filtered) pods of the cluster.
func (s *clusterWaitSpec) listPods(ctx context.Context, client K8sClient) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list %s pods for %s/%s: %w", s.kind, s.namespace, s.name, err)
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		if s.filter != nil && !s.filter(&list.Items[i]) {
			continue
		}
		pods = append(pods, list.Items[i])
	}
	return pods, nil
}

// recordPods snapshots the UIDs of the cluster's current pods. It must be
// called before the restart is triggered. A cluster without pods is an error:
// the restart could never be verified.
func (s *clusterWaitSpec) recordPods(ctx context.Context, client K8sClient) error {
	pods, err := s.listPods(ctx, client)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("no pods found for %s cluster %s/%s (selector %q)", s.kind, s.namespace, s.name, s.selector)
	}
	s.oldUIDs = make(map[types.UID]string, len(pods))
	for _, pod := range pods {
		s.oldUIDs[pod.UID] = pod.Name
	}
	s.expected = len(pods)
	return nil
}

// podReadyTrue reports whether the pod has a Ready condition with status
// True. A pod without a Ready condition is not ready.
func podReadyTrue(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podReplacementProgress summarises how far a restart has progressed.
type podReplacementProgress struct {
	oldRemaining int
	newPods      int
	ready        int
	expected     int
}

func (p podReplacementProgress) String() string {
	return fmt.Sprintf("%d old pod(s) remaining, %d/%d new pod(s) ready (%d new total)",
		p.oldRemaining, p.ready, p.expected, p.newPods)
}

// evaluatePodReplacement reports whether every recorded pod has been replaced,
// exactly expected (desired) pods exist and every one of them is Running and
// Ready=True. Extra pods that are not ready keep the restart unfinished.
func evaluatePodReplacement(pods []corev1.Pod, oldUIDs map[types.UID]string, expected int) (bool, podReplacementProgress) {
	progress := podReplacementProgress{expected: expected}
	for i := range pods {
		pod := &pods[i]
		if _, old := oldUIDs[pod.UID]; old {
			progress.oldRemaining++
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		progress.newPods++
		if pod.Status.Phase == corev1.PodRunning && podReadyTrue(pod) {
			progress.ready++
		}
	}
	done := progress.oldRemaining == 0 && progress.newPods == expected && progress.ready == progress.newPods
	return done, progress
}

// checkClusterRestart runs one poll iteration of the waiter.
func checkClusterRestart(ctx context.Context, client K8sClient, s *clusterWaitSpec) (bool, string, error) {
	pods, err := s.listPods(ctx, client)
	if err != nil {
		return false, "", err
	}
	if s.desired != nil {
		expected, err := s.desired(ctx)
		if err != nil {
			return false, "", err
		}
		s.expected = expected
	}
	done, progress := evaluatePodReplacement(pods, s.oldUIDs, s.expected)
	if !done {
		return false, progress.String(), nil
	}
	if s.extraCheck != nil {
		ok, status, err := s.extraCheck(ctx)
		if err != nil || !ok {
			return false, status, err
		}
	}
	return true, progress.String(), nil
}

// waitForClusterRestart blocks until the cluster described by s has replaced
// all recorded pods, has the expected number of ready pods and passes
// extraCheck, or until the timeout / parent context expires. Every API call
// uses the bounded context.
//
// An API error while polling (e.g. a forbidden pod List) is a failure of the
// run: trip is called with it at once, so no new restart starts anywhere,
// but polling goes on because the restart is in flight. The first such error
// is returned also when the restart completes afterwards. A nil trip is a
// no-op.
func waitForClusterRestart(ctx context.Context, client K8sClient, log *logger.Logger, timeoutSeconds int, s clusterWaitSpec, trip func(error)) error {
	log.Info("Waiting for %s cluster %s/%s to complete restart...", s.kind, s.namespace, s.name)
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	ticker := time.NewTicker(crPollInterval)
	defer ticker.Stop()

	lastStatus := "no status yet"
	var apiErr error
	for {
		done, status, err := checkClusterRestart(timeoutCtx, client, &s)
		switch {
		case err != nil:
			if timeoutCtx.Err() == nil {
				if apiErr == nil {
					apiErr = fmt.Errorf("API error while waiting for %s cluster %s/%s: %w", s.kind, s.namespace, s.name, err)
					if trip != nil {
						trip(apiErr)
					}
				}
				log.Warning("Error while waiting for %s cluster %s/%s: %v; no new restart is started, still waiting for the restart",
					s.kind, s.namespace, s.name, err)
				lastStatus = err.Error()
			}
		case done:
			log.Success("%s cluster %s/%s restarted: all %d pod(s) replaced and ready", s.kind, s.namespace, s.name, s.expected)
			if apiErr != nil {
				return fmt.Errorf("%w (the restart completed nevertheless)", apiErr)
			}
			return nil
		default:
			lastStatus = status
			log.Info("Waiting for %s cluster %s/%s: %s", s.kind, s.namespace, s.name, status)
		}

		select {
		case <-timeoutCtx.Done():
			var waitErr error
			if ctx.Err() != nil {
				waitErr = fmt.Errorf("interrupted while waiting for %s cluster %s/%s restart (last status: %s): %w",
					s.kind, s.namespace, s.name, lastStatus, ctx.Err())
			} else {
				waitErr = fmt.Errorf("timeout waiting for %s cluster %s/%s restart (last status: %s)",
					s.kind, s.namespace, s.name, lastStatus)
			}
			return joinErrs(waitErr, apiErr)
		case <-ticker.C:
		}
	}
}

// isRetryablePatchError reports whether a patch failed because the object
// changed concurrently (409 Conflict, or 422 from a failed JSON Patch test).
func isRetryablePatchError(err error) bool {
	if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Code == 422
	}
	return false
}

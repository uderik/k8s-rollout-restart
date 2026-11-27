package operations

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CrashLoopBackOffReason is the container waiting reason of a crash-looping
// container. It is governed by PodHealthPolicy.CrashLoopRestarts, not by the
// list of fatal reasons.
const CrashLoopBackOffReason = "CrashLoopBackOff"

// DefaultCrashLoopRestarts is the default --crashloop-restarts: a few
// restarts are tolerated so that a container waiting for a dependency at
// startup does not fail the rollout.
const DefaultCrashLoopRestarts = 3

// DefaultFailFastReasons is the default --fail-fast-reasons: container
// waiting reasons that do not resolve on their own within a rollout wait; the
// pod only becomes ready after someone fixes the image, registry access or
// the referenced config.
var DefaultFailFastReasons = []string{
	"ImagePullBackOff",
	"InvalidImageName",
	"ErrImageNeverPull",
	"CreateContainerConfigError",
}

// PodHealthPolicy decides when a pod of a new revision is broken, i.e.
// cannot become ready on its own, so that a rollout wait fails at once
// instead of running into --timeout. Slow pods (Pending, ContainerCreating,
// Running but not Ready, crash-looping below the threshold) are not broken.
// The zero value disables both checks.
type PodHealthPolicy struct {
	// CrashLoopRestarts: a container in CrashLoopBackOff with at least this
	// many restarts is broken. 0 disables the check.
	CrashLoopRestarts int
	// fatalReasons are container waiting reasons that make a pod broken
	// immediately. Empty disables the check.
	fatalReasons map[string]bool
}

// NewPodHealthPolicy builds a policy from --crashloop-restarts and
// --fail-fast-reasons. Empty reasons are ignored.
func NewPodHealthPolicy(crashLoopRestarts int, fatalReasons []string) PodHealthPolicy {
	p := PodHealthPolicy{CrashLoopRestarts: crashLoopRestarts, fatalReasons: make(map[string]bool, len(fatalReasons))}
	for _, r := range fatalReasons {
		if r = strings.TrimSpace(r); r != "" {
			p.fatalReasons[r] = true
		}
	}
	return p
}

// DefaultPodHealthPolicy returns the policy of the default flag values.
func DefaultPodHealthPolicy() PodHealthPolicy {
	return NewPodHealthPolicy(DefaultCrashLoopRestarts, DefaultFailFastReasons)
}

// enabled reports whether any check is active; when none is, the waiters do
// not list pods at all.
func (p PodHealthPolicy) enabled() bool {
	return p.CrashLoopRestarts > 0 || len(p.fatalReasons) > 0
}

// brokenPodReason describes why a pod cannot become ready on its own, or
// returns "" if it may still become ready. Terminating pods are ignored.
func (p PodHealthPolicy) brokenPodReason(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return ""
	}
	if r := p.brokenContainerReason("init container", pod.Status.InitContainerStatuses); r != "" {
		return r
	}
	return p.brokenContainerReason("container", pod.Status.ContainerStatuses)
}

func (p PodHealthPolicy) brokenContainerReason(kind string, statuses []corev1.ContainerStatus) string {
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch {
		case p.fatalReasons[w.Reason]:
			msg := fmt.Sprintf("%s %s: %s", kind, cs.Name, w.Reason)
			if w.Message != "" {
				msg += ": " + w.Message
			}
			return msg
		case w.Reason == CrashLoopBackOffReason && p.CrashLoopRestarts > 0 && int(cs.RestartCount) >= p.CrashLoopRestarts:
			msg := fmt.Sprintf("%s %s: %s (%d restarts", kind, cs.Name, CrashLoopBackOffReason, cs.RestartCount)
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg += fmt.Sprintf(", last exit: %s, code %d", t.Reason, t.ExitCode)
			}
			return msg + ")"
		}
	}
	return ""
}

// brokenPodsErr lists the pods matching selector in namespace, keeps those
// for which owned returns true and returns a terminal error naming every one
// of them that cannot become ready (nil if there is none). fetchErr reports a
// failed pod List.
func (p PodHealthPolicy) brokenPodsErr(ctx context.Context, client K8sClient, namespace, selector string, owned func(*corev1.Pod) bool) (terminal, fetchErr error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	var broken []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !owned(pod) {
			continue
		}
		if r := p.brokenPodReason(pod); r != "" {
			broken = append(broken, pod.Name+": "+r)
		}
	}
	if len(broken) == 0 {
		return nil, nil
	}
	sort.Strings(broken)
	return fmt.Errorf("pod(s) of the new revision cannot become ready: %s", strings.Join(broken, "; ")), nil
}

// controlledBy reports whether obj's controller ownerReference has the UID.
func controlledBy(obj metav1.Object, uid types.UID) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == uid
}

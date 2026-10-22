package operations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// defaultPollInterval is how often the rollout waiters re-check status.
const defaultPollInterval = 5 * time.Second

// ParseLabelsOrAnnotations parses a slice of "key=value" strings into a map.
// Malformed items (without '=' or with an empty key) are silently ignored; use
// ParseLabelsOrAnnotationsStrict to reject them instead.
func ParseLabelsOrAnnotations(items []string) map[string]string {
	result := make(map[string]string)
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		result[key] = value
	}
	return result
}

// ParseLabelsOrAnnotationsStrict parses a slice of "key=value" strings into a
// map and returns an error for every item that is not of that form (missing
// '=' or empty key). "key=" is valid and means "key present with an empty
// value".
func ParseLabelsOrAnnotationsStrict(items []string) (map[string]string, error) {
	result := make(map[string]string, len(items))
	var errs []error
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			errs = append(errs, fmt.Errorf("invalid filter %q: expected key=value", item))
			continue
		}
		result[key] = value
	}
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// isNotFoundErr reports whether err indicates that an API group/version,
// resource or object does not exist on the cluster. Typed API errors are
// recognised via apierrors.IsNotFound; the string matching is a fallback for
// errors that were wrapped into plain errors along the way.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "could not find") ||
		strings.Contains(msg, "the server could not find the requested resource")
}

// nonEmptyStrings returns the items of in that are not empty strings.
func nonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// PodMatchesAnyImage reports whether any container of the pod (including init
// and ephemeral containers) uses an image that contains any of the given
// substrings. Substring matching lets callers target an image by tag (e.g.
// "proxyv2:1.27.1") regardless of the registry prefix. Empty substrings are
// ignored (they would otherwise match every image); with no non-empty
// substrings the filter is inactive and it returns true.
func PodMatchesAnyImage(pod *corev1.Pod, substrings []string) bool {
	substrings = nonEmptyStrings(substrings)
	if len(substrings) == 0 {
		return true
	}
	images := make([]string, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers)+len(pod.Spec.EphemeralContainers))
	for _, c := range pod.Spec.Containers {
		images = append(images, c.Image)
	}
	for _, c := range pod.Spec.InitContainers {
		images = append(images, c.Image)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		images = append(images, c.Image)
	}
	for _, img := range images {
		for _, sub := range substrings {
			if strings.Contains(img, sub) {
				return true
			}
		}
	}
	return false
}

// MatchesRequirements checks if given labels/annotations match all required
// ones. A requirement only matches when the key is present, so "key=" matches
// a key with an empty value but not a missing key.
func MatchesRequirements(actual, required map[string]string) bool {
	for key, want := range required {
		if v, ok := actual[key]; !ok || v != want {
			return false
		}
	}
	return true
}

// restartPatch returns the strategic-merge patch that triggers a rolling
// restart of a pod template, the same way `kubectl rollout restart` does.
func restartPatch() []byte {
	return []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
		time.Now().Format(time.RFC3339)))
}

// forEachBounded calls fn(i) for every i in [0, n) with at most limit calls
// running concurrently, and returns once all calls have finished. Every index
// is visited; fn is responsible for checking ctx (and recording that it did
// not run) when the operation has been cancelled.
func forEachBounded(limit, n int, fn func(i int)) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// ErrNotStartedEarlierFailure marks rollout jobs that were never started
// because an earlier rollout failed (see runRolloutJobs).
var ErrNotStartedEarlierFailure = errors.New("not started due to earlier failure")

// RunStop is the run-wide "no new restarts" signal. One instance is shared by
// all resource types of a run (see the SetRunStop methods), so a failure of a
// Deployment also stops StatefulSet, Kafka and PostgreSQL launches and vice
// versa. Once tripped it stays tripped. A nil *RunStop is valid and never
// stopped.
type RunStop struct {
	stopped atomic.Bool
	mu      sync.Mutex
	cause   error
}

// NewRunStop returns an untripped RunStop.
func NewRunStop() *RunStop { return &RunStop{} }

// Trip records cause (the first one wins) and stops all further launches.
func (s *RunStop) Trip(cause error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cause == nil {
		s.cause = cause
	}
	s.mu.Unlock()
	s.stopped.Store(true)
}

// Stopped reports whether no new restart may be started any more.
func (s *RunStop) Stopped() bool { return s != nil && s.stopped.Load() }

// Cause returns the error that tripped the signal (nil if not tripped).
func (s *RunStop) Cause() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

// orNew returns s, or a new run-local RunStop when s is nil.
func (s *RunStop) orNew() *RunStop {
	if s == nil {
		return NewRunStop()
	}
	return s
}

// tripOnSelectionErrors returns rs (a new run-local RunStop when rs is nil)
// after tripping it if selecting the restart targets failed (e.g. listing a
// namespace was forbidden). It is called before the launch queue starts, so a
// selection error stops the whole run: no target is launched, every selected
// target is reported as "not started due to earlier failure", and no other
// resource type sharing rs starts a restart either.
func tripOnSelectionErrors(rs *RunStop, errs []error) *RunStop {
	rs = rs.orNew()
	if err := errors.Join(errs...); err != nil {
		rs.Trip(fmt.Errorf("target selection failed: %w", err))
	}
	return rs
}

// runRolloutJobs calls fn(i) for i in [0, n) with at most limit calls in
// flight. As soon as stop(err) is true for a finished call, rs is tripped and
// no further job is started; no job is started either once rs was tripped
// elsewhere (another worker, another resource type). Calls already in flight
// finish normally (within their own timeout) and every job that was not
// started gets an error wrapping ErrNotStartedEarlierFailure. errs[i] is the
// error of job i (nil on success). A nil rs is replaced by a run-local one.
//
// The stop flag is set before the failing call releases its slot, so the slot
// it frees can never be used to launch another rollout.
func runRolloutJobs(limit, n int, rs *RunStop, fn func(i int) error, stop func(error) bool) (errs []error, notStarted int) {
	if limit < 1 {
		limit = 1
	}
	rs = rs.orNew()
	errs = make([]error, n)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		if rs.Stopped() {
			<-sem
			for j := i; j < n; j++ {
				errs[j] = ErrNotStartedEarlierFailure
			}
			notStarted = n - i
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := fn(i)
			errs[i] = err
			if stop(err) {
				rs.Trip(err)
			}
			<-sem
		}(i)
	}
	wg.Wait()
	return errs, notStarted
}

// patchOutcomeAmbiguous reports whether a failed restart PATCH may
// nevertheless have been applied (transport error, cancelled context, 5xx or
// 408). Definite 4xx rejections are not ambiguous.
func patchOutcomeAmbiguous(err error) bool {
	return err != nil && !mutationDefinitelyFailed(err)
}

// rolloutJobError marks the result of one workload restart whose PATCH outcome
// may be ambiguous. Any failure (definite PATCH rejection, ambiguous PATCH,
// rollout timeout or terminal rollout failure) stops new launches, with or
// without --skip-wait (see stopsLaunches).
type rolloutJobError struct {
	err       error
	ambiguous bool // the restart PATCH may or may not have been applied
}

func (e *rolloutJobError) Error() string { return e.err.Error() }
func (e *rolloutJobError) Unwrap() error { return e.err }

// stopsLaunches reports whether a failed job must stop new launches: any
// failure does, so a run never keeps restarting after something went wrong.
func stopsLaunches(err error) bool {
	return err != nil
}

// rolloutTarget identifies a workload whose rollout is being awaited.
// minGeneration is the metadata.generation returned by the restart PATCH; the
// controller must have observed at least this generation before the status
// can be trusted.
type rolloutTarget struct {
	namespace     string
	name          string
	minGeneration int64
}

func (t rolloutTarget) String() string { return t.namespace + "/" + t.name }

// rolloutCheck fetches the current state of a target and reports whether its
// rollout has completed. reason explains why it is not done yet; a non-nil
// terminal error means the rollout failed and waiting further is pointless.
// fetchErr reports a transient error retrieving the object.
type rolloutCheck func(ctx context.Context, t rolloutTarget) (done bool, reason string, terminal error, fetchErr error)

// waitForRollouts polls check for every target until all of them are done,
// the timeout expires, or ctx is cancelled. The bounded timeout context is
// passed to every API call so a hung request cannot outlive --timeout.
//
// Readiness is re-evaluated for every target on every poll pass: success
// requires that, within a single pass, every remaining target reports done. A
// target that was ready once but regressed (e.g. a pod crashed after becoming
// Ready) keeps the wait going. Terminal failures are removed from further
// polling and returned together.
//
// An API error while polling (fetchErr) is a failure of the run: trip is
// called with it at once, so no new restart starts anywhere, but the target
// keeps being polled (its rollout is in flight). The first such error is
// returned also when every target became ready afterwards. A nil trip is a
// no-op.
func waitForRollouts(ctx context.Context, log *logger.Logger, kind string, timeout, interval time.Duration, targets []rolloutTarget, check rolloutCheck, trip func(error)) error {
	if len(targets) == 0 {
		return nil
	}
	if interval <= 0 {
		interval = defaultPollInterval
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pending := make(map[string]rolloutTarget, len(targets))
	for _, t := range targets {
		pending[t.String()] = t
	}
	var (
		failures []error
		// apiErr is the first API error seen while polling; it fails the
		// wait even if every target becomes ready afterwards.
		apiErr error
	)

	if len(targets) == 1 {
		log.Info("Waiting for %s %s to become ready (timeout %s)", kind, targets[0], timeout)
	} else {
		log.Info("Waiting for %d %s(s) to become ready (timeout %s)", len(targets), kind, timeout)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// reasons holds the last known reason why a target is not ready; it is
	// cleared when a target reports done so the final error stays accurate.
	reasons := make(map[string]string, len(targets))
	for {
		// One full pass: re-check every remaining target. notReady collects
		// the targets that are not ready in this pass only.
		var notReady []string
		for _, key := range sortedKeys(pending) {
			if timeoutCtx.Err() != nil {
				break
			}
			t := pending[key]
			done, reason, terminal, fetchErr := check(timeoutCtx, t)
			switch {
			case fetchErr != nil:
				if timeoutCtx.Err() == nil {
					if apiErr == nil {
						apiErr = fmt.Errorf("API error while waiting for %s %s: %w", kind, key, fetchErr)
						if trip != nil {
							trip(apiErr)
						}
					}
					log.Warning("Failed to get %s %s: %v; no new restart is started, still waiting for the rollout", kind, key, fetchErr)
					reasons[key] = fmt.Sprintf("failed to get status: %v", fetchErr)
				}
				notReady = append(notReady, key)
			case terminal != nil:
				log.Error("%s %s rollout failed: %v", kind, key, terminal)
				failures = append(failures, fmt.Errorf("%s %s: %w", kind, key, terminal))
				delete(pending, key)
				delete(reasons, key)
			case done:
				delete(reasons, key)
			default:
				if reason == "" {
					reason = "not ready"
				}
				reasons[key] = reason
				notReady = append(notReady, key)
			}
		}

		// Success only if the whole pass completed and every remaining
		// target was ready in it.
		if timeoutCtx.Err() == nil && len(notReady) == 0 {
			for _, key := range sortedKeys(pending) {
				log.Success("%s %s is ready", kind, key)
			}
			if apiErr != nil {
				failures = append(failures, fmt.Errorf("%w (the rollout completed nevertheless)", apiErr))
			}
			return errors.Join(failures...)
		}

		if timeoutCtx.Err() == nil {
			log.Info("Waiting for %s(s) to be ready (%d/%d ready)", kind, len(pending)-len(notReady), len(targets))
			for i, key := range notReady {
				if i == 5 {
					log.Info("... and %d more", len(notReady)-5)
					break
				}
				log.Info("%s %s not ready: %s", kind, key, reasons[key])
			}
		}

		select {
		case <-timeoutCtx.Done():
			failures = append(failures, waitInterruptedErr(timeoutCtx.Err(), kind, timeout, pending, reasons))
			if apiErr != nil {
				failures = append(failures, apiErr)
			}
			return errors.Join(failures...)
		case <-ticker.C:
		}
	}
}

// waitInterruptedErr builds an error describing why waiting stopped early,
// distinguishing a cancelled run (e.g. SIGINT) from an expired timeout.
func waitInterruptedErr(cause error, kind string, timeout time.Duration, pending map[string]rolloutTarget, reasons map[string]string) error {
	details := make([]string, 0, len(pending))
	for _, key := range sortedKeys(pending) {
		if r := reasons[key]; r != "" {
			details = append(details, fmt.Sprintf("%s (%s)", key, r))
		} else {
			details = append(details, key)
		}
	}
	list := strings.Join(details, ", ")
	if errors.Is(cause, context.Canceled) {
		return fmt.Errorf("cancelled while waiting for %s(s) to become ready; not ready: %s: %w", kind, list, cause)
	}
	return fmt.Errorf("timed out after %s waiting for %s(s) to become ready; not ready: %s: %w", timeout, kind, list, cause)
}

func sortedKeys(m map[string]rolloutTarget) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ShellQuote quotes s for a POSIX shell unless it only contains characters
// that need no quoting. It is used for the copy-paste kubectl hints.
func ShellQuote(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@=+,", r)
	}
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

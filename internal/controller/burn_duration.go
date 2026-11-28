package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

// maxBurnDays keeps a day count from overflowing time.Duration.
const maxBurnDays = math.MaxInt64 / int64(24*time.Hour)

// parseK6Duration reads a duration the way k6 does for a scenario's
// maxDuration: Go's own syntax plus a leading whole number of days ("7d",
// "1d12h"), or a bare number of milliseconds. ok is false for anything
// else, and for a duration that is not positive or does not fit: the caller
// then enforces no limit, which is what an unreadable value meant before
// the operator read this field at all.
func parseK6Duration(s string) (d time.Duration, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if ms, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(ms) || ms <= 0 || ms >= float64(math.MaxInt64/int64(time.Millisecond)) {
			return 0, false
		}
		// Rounded to whole nanoseconds, which can leave nothing.
		d = time.Duration(ms * float64(time.Millisecond))
		return d, d > 0
	}
	if days, rest, found := strings.Cut(s, "d"); found {
		n, err := strconv.ParseInt(days, 10, 64)
		if err != nil || n < 0 || n > maxBurnDays {
			return 0, false
		}
		d = time.Duration(n) * 24 * time.Hour
		s = rest
	}
	if s != "" {
		rest, err := time.ParseDuration(s)
		if err != nil || rest < 0 || rest > math.MaxInt64-d {
			return 0, false
		}
		d += rest
	}
	return d, d > 0
}

// burnLimit is how long tc may be under load in total, or ok false when it
// is not a Burn or its burnDuration cannot be read.
func burnLimit(tc *benchv1alpha1.TestCase) (time.Duration, bool) {
	if tc.Spec.Profile != benchv1alpha1.ProfileBurn {
		return 0, false
	}
	return parseK6Duration(tc.Spec.BurnDuration)
}

// burnConsumed is the load time tc used in stretches that have ended.
func burnConsumed(tc *benchv1alpha1.TestCase) time.Duration {
	return time.Duration(tc.Status.BurnConsumedSeconds) * time.Second
}

// burnDeadline is when the stretch of load running now uses up what is left
// of tc's burnDuration. ok is false while no stretch is running, or there
// is no limit.
func burnDeadline(tc *benchv1alpha1.TestCase) (deadline time.Time, ok bool) {
	limit, ok := burnLimit(tc)
	if !ok || tc.Status.BurnStretchSince == nil {
		return time.Time{}, false
	}
	return tc.Status.BurnStretchSince.Add(limit - burnConsumed(tc)), true
}

// burnExhausted reports whether tc has used its whole burnDuration and is
// not under load: the state a finished burn stays in, whatever else happens
// to the test case, until a longer burnDuration leaves it time again.
func burnExhausted(tc *benchv1alpha1.TestCase) bool {
	limit, ok := burnLimit(tc)
	return ok && tc.Status.BurnStretchSince == nil && burnConsumed(tc) >= limit
}

// syncBurnClock keeps tc's burn clock in step with its targets, and must run
// on every pass that rewrites status.deployments, after it did. A stretch
// starts with the first target to be given a start time and ends when no
// target is under load any more - stopped, or no longer burning at all -
// at which point its length is added to what tc has consumed. The clock is
// the test case's own, not derived from the targets each time: targets come
// and go, and each has its own start.
func syncBurnClock(tc *benchv1alpha1.TestCase, now time.Time) {
	start := burnStartAt(tc)
	since := tc.Status.BurnStretchSince
	switch {
	case start != nil && since == nil:
		tc.Status.BurnStretchSince = start.DeepCopy()
	case start == nil && since != nil:
		// The stretch ended when its targets were stopped, or just now if
		// they simply are not burning any more (a switch to Quick).
		end := now
		var stopped time.Time
		for _, st := range tc.Status.Deployments {
			if st.BurnStoppedAt != nil && st.BurnStoppedAt.After(since.Time) && st.BurnStoppedAt.After(stopped) {
				stopped = st.BurnStoppedAt.Time
			}
		}
		if !stopped.IsZero() && stopped.Before(end) {
			end = stopped
		}
		// A stretch stopped before its start gate opened used nothing.
		if length := end.Sub(since.Time); length > 0 {
			// Rounded up, so a burn stopped at its deadline has consumed
			// its whole limit rather than a fraction of a second less.
			tc.Status.BurnConsumedSeconds += int64(math.Ceil(length.Seconds()))
		}
		tc.Status.BurnStretchSince = nil
	}
}

// stopOnElapsedDuration stops a Burn TestCase's k6 load once it has been
// under load for its burnDuration, and keeps it stopped. k6 is given the
// same limit, but on reaching it k6 only exits, and its Deployment starts
// it again at once: without this the limit restarted the load rather than
// ending it. stopped reports that the caller is done, with the result to
// return.
func (r *TestCaseReconciler) stopOnElapsedDuration(ctx context.Context, tc *benchv1alpha1.TestCase) (stopped bool, _ ctrl.Result, _ error) {
	if deadline, running := burnDeadline(tc); running {
		if time.Now().Before(deadline) {
			return false, ctrl.Result{}, nil
		}
	} else if !burnExhausted(tc) {
		return false, ctrl.Result{}, nil
	}
	result, err := r.reconcileStopped(ctx, tc, benchv1alpha1.ReasonBurnFinished,
		fmt.Sprintf("k6 load stopped after its burnDuration of %s under load", tc.Spec.BurnDuration))
	return true, result, err
}

// requeueByBurnDeadline makes result come back no later than tc's burn uses
// up its burnDuration, so the load is stopped then rather than at the next
// unrelated event. A deadline already past is left alone: the pass that
// just ran is the one that stops such a burn, and if it could not (a runner
// cluster out of reach) it chose its own, slower retry.
func requeueByBurnDeadline(tc *benchv1alpha1.TestCase, result ctrl.Result) ctrl.Result {
	deadline, ok := burnDeadline(tc)
	if !ok {
		return result
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return result
	}
	if result.RequeueAfter == 0 || wait < result.RequeueAfter {
		result.RequeueAfter = wait
	}
	return result
}

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	benchv1alpha1 "github.com/authentik-labs/akbench/api/v1alpha1"
)

func TestParseK6Duration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"8760h":   8760 * time.Hour,
		"90m":     90 * time.Minute,
		"1h30m":   90 * time.Minute,
		"150s":    150 * time.Second,
		"7d":      7 * 24 * time.Hour,
		"1d12h":   36 * time.Hour,
		"1500":    1500 * time.Millisecond,
		" 2h ":    2 * time.Hour,
		"250ms":   250 * time.Millisecond,
		"0.5h":    30 * time.Minute,
		"1d0.5h":  24*time.Hour + 30*time.Minute,
		"106751d": 106751 * 24 * time.Hour,
	} {
		got, ok := parseK6Duration(in)
		if !ok || got != want {
			t.Errorf("parseK6Duration(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	// Not a duration, not positive, or too long to fit: no limit at all,
	// never a limit of zero or a negative one, which would end a burn the
	// moment it starts.
	for _, in := range []string{
		"", "soon", "-1h", "1.5d", "d", "1w", "0", "0s", "-0", "0d", "-1d",
		"200000d", "106752d", "106751d24h", "Inf", "NaN", "9e30", "-5",
		// Positive, but nothing once rounded to nanoseconds.
		"0.0000001", "1e-9",
	} {
		if got, ok := parseK6Duration(in); ok {
			t.Errorf("parseK6Duration(%q) = %v, true; want not ok", in, got)
		}
	}
}

func at(t time.Time) *metav1.Time { return &metav1.Time{Time: t} }

// burning is a Burn TestCase with the given burnDuration whose targets a
// and b have been under load since the given times (nil: not started).
func burning(duration string, a, b *metav1.Time) *benchv1alpha1.TestCase {
	return &benchv1alpha1.TestCase{
		Spec: benchv1alpha1.TestCaseSpec{Profile: benchv1alpha1.ProfileBurn, BurnDuration: duration},
		Status: benchv1alpha1.TestCaseStatus{Deployments: []benchv1alpha1.TestCaseDeploymentStatus{
			{Name: "a", BurningSince: a},
			{Name: "b", BurningSince: b},
		}},
	}
}

func stopAll(tc *benchv1alpha1.TestCase, when time.Time) {
	for i := range tc.Status.Deployments {
		if tc.Status.Deployments[i].BurningSince != nil {
			tc.Status.Deployments[i].BurnStoppedAt = at(when)
		}
	}
}

func TestBurnClockRunsAcrossStretches(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tc := burning("1h", at(t0), at(t0))

	if _, ok := burnDeadline(tc); ok {
		t.Error("a deadline before the clock has seen the stretch")
	}
	syncBurnClock(tc, t0)
	if deadline, ok := burnDeadline(tc); !ok || !deadline.Equal(t0.Add(time.Hour)) {
		t.Fatalf("deadline = %v, %v; want %v", deadline, ok, t0.Add(time.Hour))
	}

	// Stopped after 40 minutes (a full postgres volume, say): 20 are left.
	stopAll(tc, t0.Add(40*time.Minute))
	syncBurnClock(tc, t0.Add(40*time.Minute))
	if tc.Status.BurnStretchSince != nil || tc.Status.BurnConsumedSeconds != 40*60 {
		t.Fatalf("after the first stretch: since %v, consumed %ds; want none and 2400s",
			tc.Status.BurnStretchSince, tc.Status.BurnConsumedSeconds)
	}
	if burnExhausted(tc) {
		t.Error("exhausted with 20 minutes left")
	}
	if _, ok := burnDeadline(tc); ok {
		t.Error("a deadline while stopped")
	}

	// Resumed three hours later: the clock carries on, it does not restart.
	resumed := t0.Add(3 * time.Hour)
	for i := range tc.Status.Deployments {
		tc.Status.Deployments[i].BurningSince, tc.Status.Deployments[i].BurnStoppedAt = at(resumed), nil
	}
	syncBurnClock(tc, resumed)
	if deadline, ok := burnDeadline(tc); !ok || !deadline.Equal(resumed.Add(20*time.Minute)) {
		t.Fatalf("deadline after resuming = %v, %v; want %v", deadline, ok, resumed.Add(20*time.Minute))
	}

	stopAll(tc, resumed.Add(20*time.Minute))
	syncBurnClock(tc, resumed.Add(20*time.Minute))
	if !burnExhausted(tc) {
		t.Fatalf("not exhausted after 40m + 20m of a 1h limit: consumed %ds", tc.Status.BurnConsumedSeconds)
	}

	// A longer limit leaves time again; a shorter or equal one does not.
	tc.Spec.BurnDuration = "90m"
	if burnExhausted(tc) {
		t.Error("still exhausted after raising 1h to 90m")
	}
	tc.Spec.BurnDuration = "30m"
	if !burnExhausted(tc) {
		t.Error("not exhausted with a limit below what was used")
	}
}

// The end of a burn does not move when a target joins late: the clock is
// the test case's, started by its first target.
func TestBurnClockIgnoresLateTargets(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tc := burning("1h", at(t0), nil)
	syncBurnClock(tc, t0)

	tc.Status.Deployments[1].BurningSince = at(t0.Add(50 * time.Minute))
	syncBurnClock(tc, t0.Add(50*time.Minute))
	if deadline, _ := burnDeadline(tc); !deadline.Equal(t0.Add(time.Hour)) {
		t.Fatalf("deadline with a late target = %v, want %v", deadline, t0.Add(time.Hour))
	}

	stopAll(tc, t0.Add(time.Hour))
	syncBurnClock(tc, t0.Add(time.Hour))
	if !burnExhausted(tc) {
		t.Fatalf("a burn with a late target is not exhausted at its deadline: consumed %ds", tc.Status.BurnConsumedSeconds)
	}
}

func TestBurnClockEdges(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// Stopped during the start gate's lead: nothing was used.
	gated := burning("1h", at(t0.Add(2*time.Minute)), at(t0.Add(2*time.Minute)))
	syncBurnClock(gated, t0)
	stopAll(gated, t0.Add(time.Minute))
	syncBurnClock(gated, t0.Add(time.Minute))
	if gated.Status.BurnConsumedSeconds != 0 || gated.Status.BurnStretchSince != nil {
		t.Errorf("stopped before the gate opened: consumed %ds, since %v", gated.Status.BurnConsumedSeconds, gated.Status.BurnStretchSince)
	}

	// Switched to Quick: the targets are simply no longer burning, and the
	// stretch ends now.
	switched := burning("1h", at(t0), at(t0))
	syncBurnClock(switched, t0)
	switched.Spec.Profile = benchv1alpha1.ProfileQuick
	for i := range switched.Status.Deployments {
		switched.Status.Deployments[i].BurningSince = nil
	}
	syncBurnClock(switched, t0.Add(10*time.Minute))
	if switched.Status.BurnConsumedSeconds != 600 || switched.Status.BurnStretchSince != nil {
		t.Errorf("after a switch to Quick: consumed %ds, since %v; want 600s and none",
			switched.Status.BurnConsumedSeconds, switched.Status.BurnStretchSince)
	}
	if burnExhausted(switched) {
		t.Error("a Quick test case is never exhausted")
	}

	// An unreadable limit is no limit.
	unreadable := burning("whenever", at(t0), at(t0))
	syncBurnClock(unreadable, t0)
	if _, ok := burnDeadline(unreadable); ok {
		t.Error("an unreadable burnDuration gave a deadline")
	}
	if burnExhausted(unreadable) {
		t.Error("an unreadable burnDuration is exhausted")
	}

	// A limit lowered below what the running stretch already used puts the
	// deadline in the past.
	lowered := burning("10h", at(t0), at(t0))
	syncBurnClock(lowered, t0)
	lowered.Spec.BurnDuration = "1h"
	if deadline, ok := burnDeadline(lowered); !ok || !deadline.Equal(t0.Add(time.Hour)) {
		t.Errorf("deadline after lowering the limit = %v, %v", deadline, ok)
	}
}

func TestRequeueByBurnDeadline(t *testing.T) {
	soon := burning("1h", at(time.Now().Add(-50*time.Minute)), nil)
	syncBurnClock(soon, time.Now())
	if got := requeueByBurnDeadline(soon, ctrl.Result{}).RequeueAfter; got <= 9*time.Minute || got > 10*time.Minute {
		t.Errorf("requeue = %v, want about 10m", got)
	}
	if got := requeueByBurnDeadline(soon, ctrl.Result{RequeueAfter: time.Second}).RequeueAfter; got != time.Second {
		t.Errorf("requeue = %v, want the sooner 1s kept", got)
	}
	if got := requeueByBurnDeadline(soon, ctrl.Result{RequeueAfter: time.Hour}).RequeueAfter; got > 10*time.Minute {
		t.Errorf("requeue = %v, want no later than the deadline", got)
	}
	// Overdue: the pass that just ran stops it, or picked its own retry if
	// it could not. Never a tight loop on top of that.
	overdue := burning("1h", at(time.Now().Add(-2*time.Hour)), nil)
	syncBurnClock(overdue, time.Now())
	if got := requeueByBurnDeadline(overdue, ctrl.Result{}); got != (ctrl.Result{}) {
		t.Errorf("requeue for an overdue burn = %+v, want it left alone", got)
	}
	if got := requeueByBurnDeadline(overdue, ctrl.Result{RequeueAfter: 15 * time.Second}).RequeueAfter; got != 15*time.Second {
		t.Errorf("requeue for an overdue burn whose stop is pending = %v, want the pass's own 15s", got)
	}
	stopped := burning("1h", nil, nil)
	stopped.Status.BurnConsumedSeconds = 3600
	if got := requeueByBurnDeadline(stopped, ctrl.Result{}); got != (ctrl.Result{}) {
		t.Errorf("requeue for a finished burn = %+v, want none", got)
	}
}

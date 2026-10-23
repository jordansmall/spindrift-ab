package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
	"spindrift.dev/launcher/internal/outcome"
	"spindrift.dev/launcher/internal/testutil"
)

const gateIssue = "1"

func readyResult() dispatch.Result {
	return dispatch.Result{
		Resolved: outcome.Resolved{
			Found:   true,
			Outcome: outcome.Outcome{Issue: gateIssue, Landing: testPR, Status: "ready", Note: "ok"},
		},
	}
}

// gateForge scripts n identical rollup polls; the fake's queue drains to
// PENDING, so n must cover every poll the scenario makes.
func gateForge(state forge.RollupState, n int) *forge.Fake {
	states := make([]forge.RollupState, n)
	for i := range states {
		states[i] = state
	}
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: gateIssue, Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, states)
	return fc
}

func assertOutputHas(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q; got:\n%s", w, out)
		}
	}
}

// A PR that stays red through every fix pass ends the run failed, never
// merged, and reports both the exhaustion and the final failure.
func TestSettle_ReadyRedThroughFixCap(t *testing.T) {
	c := fixConfig(3)
	fc := gateForge(forge.StateFailure, 50)
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if got := fixPasses(d); len(got) != 3 {
		t.Errorf("fix passes = %v, want exactly 3", got)
	}
	if fc.Merged != "" {
		t.Errorf("red PR must not merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=fix-exhausted", "status=failed")
}

// A merge conflict rebases the branch and the retried merge lands it.
func TestSettle_ReadyConflictRebaseRetryMerges(t *testing.T) {
	c := baseConfig()
	c.MaxRebaseAttempts = 3
	fc := gateForge(forge.StateSuccess, 20)
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(dispatch.NewFake(), gateIssue, 0, readyResult()) })

	if len(fc.RebasedURLs) != 1 {
		t.Errorf("Rebase called %d times, want 1", len(fc.RebasedURLs))
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=rebase-retry", "status=verified-merged")
}

// A rebase that fails outright blocks the merge but keeps the issue
// complete: CI was green, so the work is not a failure.
func TestSettle_ReadyConflictRebaseFailsMergeBlocked(t *testing.T) {
	c := baseConfig()
	c.MaxRebaseAttempts = 3
	fc := gateForge(forge.StateSuccess, 20)
	fc.MergeErr = forge.ErrMergeConflict
	fc.RebaseErr = errors.New("checkout agent/issue-1: no such branch")
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(dispatch.NewFake(), gateIssue, 0, readyResult()) })

	if fc.Merged != "" {
		t.Errorf("blocked merge must not land; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
	if containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must not carry agent-failed; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=rebase-retry", "status=merge-blocked")
}

// A conflict-retry force-push that goes red draws a fix pass; the fixed head
// re-enters the merge path and lands.
func TestSettle_ForcePushRedFixPassThenMerges(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	fc := gateForge(forge.StateSuccess, 2)
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first green
		forge.StateFailure, forge.StateFailure, // red after force-push
		forge.StateSuccess, forge.StateSuccess, forge.StateSuccess, // fixed
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	s.Settle(d, gateIssue, 0, readyResult())

	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", got)
	}
	if fc.Merged != testPR {
		t.Errorf("fixed head must merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
}

// With the fix budget already spent, a red force-pushed head fails as
// landing-failed and the comment names the attempts used.
func TestSettle_ForcePushRedBudgetExhausted(t *testing.T) {
	c := fixConfig(0)
	c.MaxRebaseAttempts = 3
	fc := gateForge(forge.StateSuccess, 2)
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess,
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	s.Settle(d, gateIssue, 0, readyResult())

	if got := fixPasses(d); len(got) != 0 {
		t.Errorf("fix passes = %v, want none", got)
	}
	if fc.Merged != "" {
		t.Errorf("red head must not merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	found := false
	cms := fc.CommentCalls
	for _, cm := range cms {
		if strings.Contains(cm.Body, "still red after 0 fix attempt(s)") {
			found = true
		}
	}
	if !found {
		t.Errorf("no landing-failed comment naming attempts; comments=%v", cms)
	}
}

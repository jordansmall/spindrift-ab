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

// scriptedForge scripts CheckState's exact sequence for testPR, for scenarios
// where gateForge's uniform queue cannot express a red re-wait after a green.
func scriptedForge(states ...forge.RollupState) *forge.Fake {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: gateIssue, Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, states)
	return fc
}

// A force-pushed head that goes red on re-wait gets a fix pass from the
// shared budget, then the fixed head re-enters the merge path and lands.
func TestSettle_ForcePushRedFixPassThenMerges(t *testing.T) {
	c := fixConfig(1)
	c.MaxRebaseAttempts = 3
	// Gate green, merge conflicts, rebase force-pushes, re-wait red; fix pass;
	// gate green again, merge lands.
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", got)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged=%q, want the PR merged after the fix pass", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=post-force-push-wait", "status=verified-merged")
}

// With no fix budget, a red re-wait after a force-push settles Failed with
// landing-failed and no fix pass, naming the attempts used.
func TestSettle_ForcePushRedNoBudgetSettlesLandingFailed(t *testing.T) {
	c := fixConfig(0)
	c.MaxRebaseAttempts = 3
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if got := fixPasses(d); len(got) != 0 {
		t.Errorf("fix passes = %v, want none", got)
	}
	if fc.Merged != "" {
		t.Errorf("red PR must not merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=landing-failed")
	if !commentsContain(fc, "fix budget exhausted (0 of 0 fix pass(es) used)") {
		t.Errorf("last comment must name the attempts used; comments=%v", fc.CommentCalls)
	}
}

// The post-force-push red draws from the same budget as the pre-merge red:
// one pre-merge pass and one post-push pass exhaust MaxFixAttempts=2, and a
// further post-push red settles Failed naming both passes.
func TestSettle_ForcePushRedSharesFixBudget(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	fc := scriptedForge(
		forge.StateFailure,                     // pre-merge red: fix pass 1
		forge.StateSuccess, forge.StateSuccess, // green; merge conflicts
		forge.StateFailure,                     // post-push red: fix pass 2
		forge.StateSuccess, forge.StateSuccess, // green; merge conflicts
		forge.StateFailure, // post-push red, budget gone
	)
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	out := testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if got := fixPasses(d); len(got) != 2 {
		t.Errorf("fix passes = %v, want exactly 2", got)
	}
	if fc.Merged != "" {
		t.Errorf("red PR must not merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	assertOutputHas(t, out, "status=landing-failed")
	if !commentsContain(fc, "fix budget exhausted (2 of 2 fix pass(es) used)") {
		t.Errorf("last comment must name the attempts used; comments=%v", fc.CommentCalls)
	}
}

// commentsContain reports whether any comment the fake forge recorded holds
// substr; the settle commit path can post a trailing comment after the note.
func commentsContain(fc *forge.Fake, substr string) bool {
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, substr) {
			return true
		}
	}
	return false
}

// assertNeverGreenNoFixPass pins that errLandingNeverGreen (a re-wait timeout
// or a conflict-resolve dispatch or relay failure, unlike a red re-wait)
// settles Failed with agent-failed and never spends a fix pass, even with
// budget left.
func assertNeverGreenNoFixPass(t *testing.T, fc *forge.Fake, d *dispatch.Fake) {
	t.Helper()
	if got := fixPasses(d); len(got) != 0 {
		t.Errorf("fix passes = %v, want none for errLandingNeverGreen", got)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	if containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must NOT carry agent-complete; labels=%v", iss.Labels)
	}
}

// A re-wait timeout after a force-push is errLandingNeverGreen, not a red
// re-wait, so it must not dispatch a fix pass even with budget available.
func TestSettle_ForcePushRewaitTimeoutNoFixPass(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 0
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetIssue(forge.Issue{Number: gateIssue, Labels: []string{"agent-in-progress"}})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, gateIssue, 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
	assertNeverGreenNoFixPass(t, fc, d)
}

// A failed conflict-resolve dispatch before the re-wait is errLandingNeverGreen
// and must settle Failed with no fix pass, even with fix budget available.
func TestSettle_ConflictResolveDispatchFailureNoFixPass(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("agent could not resolve conflict")
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(d.ResolveConflictCalls) != 1 {
		t.Fatalf("ResolveConflict called %d times, want 1", len(d.ResolveConflictCalls))
	}
	if fc.Merged != "" {
		t.Errorf("PR must not merge after failed conflict-resolve; fc.Merged=%q", fc.Merged)
	}
	assertNeverGreenNoFixPass(t, fc, d)
}

// A failed bundle relay after a successful conflict-resolve is errLandingNeverGreen
// and must settle Failed with no fix pass, even with fix budget available.
func TestSettle_ConflictResolveRelayFailureNoFixPass(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.RelayBundleErr = errors.New("relay refused")
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(fc.RelayBundleCalls) != 1 {
		t.Fatalf("RelayBundle called %d times, want 1 (relay path not reached)", len(fc.RelayBundleCalls))
	}
	if fc.Merged != "" {
		t.Errorf("PR must not merge after failed relay; fc.Merged=%q", fc.Merged)
	}
	assertNeverGreenNoFixPass(t, fc, d)
}

// A conflict-resolve Box force-pushes too, so a red re-wait after it gets the
// same fix pass, and the fixed head re-enters the merge path.
func TestSettle_ForcePushRedAfterConflictResolveFixPassThenMerges(t *testing.T) {
	c := fixConfig(1)
	c.MaxRebaseAttempts = 3
	fc := scriptedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("ResolveConflict called %d times, want 1", len(d.ResolveConflictCalls))
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", got)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged=%q, want the PR merged after the fix pass", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
}

// A re-wait timeout that follows an earlier fix pass still settles Failed with
// no further fix pass, and the comment names the passes already used.
func TestSettle_ForcePushRewaitTimeoutAfterFixPassNamesAttempts(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 0
	fc := scriptedForge(forge.StateFailure, forge.StateSuccess, forge.StateSuccess)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly 1 (the pre-merge red)", got)
	}
	if fc.Merged != "" {
		t.Errorf("timed-out head must not merge; fc.Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
	if !commentsContain(fc, "(1 fix pass(es) used)") {
		t.Errorf("a comment must name the fix passes used; comments=%v", fc.CommentCalls)
	}
}

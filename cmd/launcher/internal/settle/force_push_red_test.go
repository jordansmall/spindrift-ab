package settle

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// newForcePushRedFixture builds the config and forge fake every force-push red
// test shares: issue 1 in-progress, the scripted merge errors, and the scripted
// CI sequence for testPR. Callers add scenario-specific fields before settling.
func newForcePushRedFixture(maxFixAttempts int, mergeErrs []error, states []forge.RollupState) (Config, *forge.Fake) {
	c := fixConfig(maxFixAttempts)
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = mergeErrs
	fc.SetCheckStates(testPR, states)
	return c, fc
}

// A genuinely red post-force-push re-wait dispatches the fix pass instead of
// settling Failed, and the head that pass turns green merges as a first-green
// head would (issue #5053).
func TestSelfHeal_ForcePushRed_FixPassTurnsGreenAndMerges(t *testing.T) {
	c, fc := newForcePushRedFixture(2, []error{forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-force-push re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
	}
	if !slices.Equal(fixPasses(d), []int{1}) {
		t.Errorf("fix passes = %v, want exactly [1]", fixPasses(d))
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete after the fixed head merges; labels=%v", iss.Labels)
	}
}

// The fix budget is shared with the pre-merge red-CI loop: a pass already spent
// there leaves none for a force-push red, so the landing fails without a second
// fix pass and the comment names the budget spent.
func TestSelfHeal_ForcePushRed_SharesFixBudgetWithPreMergeRed(t *testing.T) {
	c, fc := newForcePushRedFixture(1, []error{forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateFailure,                     // initial gate: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 1
		forge.StateFailure, // post-force-push re-wait: red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if !slices.Equal(fixPasses(d), []int{1}) {
		t.Errorf("fix passes = %v, want exactly [1]: the force-push red must not get a fresh allowance", fixPasses(d))
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
	}
	// The one pass was spent before any force-push, so the force-pushed head
	// itself accounts for 0 of the budget.
	if !strings.Contains(reason, "fix budget exhausted (1/1 fix pass(es) spent, 0 on the force-pushed head)") {
		t.Errorf("selfHeal reason = %q, want it to name the budget spent", reason)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("latched reason = %q, want %q", got, ReasonLandingFailed)
	}
	if len(fc.CommentCalls) == 0 || !strings.Contains(fc.CommentCalls[len(fc.CommentCalls)-1].Body, "fix budget exhausted (1/1 fix pass(es) spent") {
		t.Errorf("want a landing-failed comment naming the budget spent; got %+v", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
}

// A fix pass that leaves the force-pushed head still red exhausts the budget
// and settles Failed with ReasonLandingFailed, naming the fix pass it consumed.
func TestSelfHeal_ForcePushRed_FixPassStillRed_SettlesLandingFailed(t *testing.T) {
	c, fc := newForcePushRedFixture(1, []error{forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure, // post-force-push re-wait: red
		forge.StateFailure, // gate after fix pass 1: still red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if !slices.Equal(fixPasses(d), []int{1}) {
		t.Errorf("fix passes = %v, want exactly [1]", fixPasses(d))
	}
	if !strings.Contains(reason, "fix budget exhausted (1/1 fix pass(es) spent, 1 on the force-pushed head)") {
		t.Errorf("selfHeal reason = %q, want it to name the one fix pass", reason)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("latched reason = %q, want %q", got, ReasonLandingFailed)
	}
	if fc.Merged != "" {
		t.Errorf("a still-red head must not merge; fc.Merged=%q", fc.Merged)
	}
}

// With MaxFixAttempts=0 a red force-push re-wait has no pass to spend, so it
// settles Failed at once and the note reads the empty budget.
func TestSelfHeal_ForcePushRed_ZeroFixBudget_SettlesLandingFailed(t *testing.T) {
	c, fc := newForcePushRedFixture(0, []error{forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure, // post-force-push re-wait: red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix passes = %v, want none with MaxFixAttempts=0", fixPasses(d))
	}
	if !strings.Contains(reason, "fix budget exhausted (0/0 fix pass(es) spent, 0 on the force-pushed head)") {
		t.Errorf("selfHeal reason = %q, want it to name the empty budget", reason)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("latched reason = %q, want %q", got, ReasonLandingFailed)
	}
	if fc.Merged != "" {
		t.Errorf("a still-red head must not merge; fc.Merged=%q", fc.Merged)
	}
}

// A second force-push red after a fix pass already turned the first force-pushed
// head green counts only the latest head against the note's per-head figure,
// while the total still reflects every pass spent.
func TestSelfHeal_ForcePushRed_SecondForcePushAfterFixedHead_CountsLatestHead(t *testing.T) {
	c, fc := newForcePushRedFixture(2, []error{forge.ErrMergeConflict, forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // first force-push re-wait: red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 1: green
		forge.StateFailure, // second force-push re-wait: red
		forge.StateFailure, // gate after fix pass 2: still red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if !slices.Equal(fixPasses(d), []int{1, 2}) {
		t.Errorf("fix passes = %v, want exactly [1 2]", fixPasses(d))
	}
	if !strings.Contains(reason, "fix budget exhausted (2/2 fix pass(es) spent, 1 on the force-pushed head)") {
		t.Errorf("selfHeal reason = %q, want it to count only the latest force-pushed head", reason)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("latched reason = %q, want %q", got, ReasonLandingFailed)
	}
}

// A conflict-resolve that force-pushes a head which goes red is fixed the same
// way, so the conflict path is not a dead end for a red re-wait either.
func TestSelfHeal_ForcePushRed_AfterConflictResolve_FixPassMerges(t *testing.T) {
	c, fc := newForcePushRedFixture(2, []error{forge.ErrMergeConflict}, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // re-wait after conflict-resolve: red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
	})
	fc.RebaseErr = forge.ErrMergeConflict
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
	}
	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("ResolveConflict called %d times, want 1", len(d.ResolveConflictCalls))
	}
	if !slices.Equal(fixPasses(d), []int{1}) {
		t.Errorf("fix passes = %v, want exactly [1]", fixPasses(d))
	}
}

// A stale-base preflight force-push that goes red is fixed and then re-preflighted;
// the fake's NeedsUpdate stays true, so the second preflight rebases again.
func TestSelfHeal_ForcePushRed_AfterStalePreflight_FixPassMerges(t *testing.T) {
	c, fc := newForcePushRedFixture(2, nil, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // re-wait after preflight rebase: red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
		forge.StateSuccess, forge.StateSuccess, // re-wait after the second preflight rebase
	})
	c.PreflightStaleBase = true
	fc.SetNeedsUpdate(testPR, true)
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
	}
	if !slices.Equal(fixPasses(d), []int{1}) {
		t.Errorf("fix passes = %v, want exactly [1]", fixPasses(d))
	}
}

// errForcePushRed must keep wrapping errLandingNeverGreen, so callers that treat
// any never-green landing as landing-failed still do.
func TestForcePushRedSentinel_WrapsNeverGreen(t *testing.T) {
	if !errors.Is(errForcePushRed, errLandingNeverGreen) {
		t.Error("errForcePushRed must wrap errLandingNeverGreen so other callers still treat it as landing-failed")
	}
}

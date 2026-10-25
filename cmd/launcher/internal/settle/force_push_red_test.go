package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

func forcePushRedConfig(maxFixAttempts int) Config {
	c := fixConfig(maxFixAttempts)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	return c
}

// A red post-force-push re-wait gets a fix pass, and the fixed head re-enters
// the merge path and lands (issue #758 follow-up).
func TestSelfHeal_ForcePushRed_FixPassThenMerges(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-force-push re-wait: red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
	})
	s := newTestSettle(forcePushRedConfig(2), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after the fix pass", landing)
	}
	if got := fixPasses(d); len(got) != 1 || got[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// Once the shared budget is spent, a red post-force-push re-wait fails the
// landing and says how many fix attempts ran (MaxFixAttempts 0 allows none).
func TestSelfHeal_ForcePushRed_BudgetExhausted_EndsFailed(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess, forge.StateFailure})
	s := newTestSettle(forcePushRedConfig(0), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "still red after 0 fix attempt(s)") {
		t.Errorf("selfHeal reason = %q, want it to name the 0 fix attempts", reason)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("expected no fix call with MaxFixAttempts=0, got %+v", d.FixCalls)
	}
	if len(fc.CommentCalls) == 0 || !strings.Contains(fc.CommentCalls[len(fc.CommentCalls)-1].Body, "landing failed:") {
		t.Errorf("expected a landing-failed comment, got %+v", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
}

// The pre-merge fix pass and the post-force-push fix pass share one budget:
// with MaxFixAttempts=1 the pre-merge pass uses it up, so the red re-wait
// fails without a second fix call.
func TestSelfHeal_ForcePushRed_SharesBudgetWithPreMerge(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // initial gateToGreen: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the pre-merge fix pass
		forge.StateFailure, // post-force-push re-wait: red
	})
	s := newTestSettle(forcePushRedConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "still red after 1 fix attempt(s)") {
		t.Errorf("selfHeal reason = %q, want it to name the 1 shared fix attempt", reason)
	}
	if got := fixPasses(d); len(got) != 1 || got[0] != 1 {
		t.Errorf("fix passes = %v, want exactly [1]", got)
	}
}

// A conflict-resolve dispatch failure during the force-push path stays a
// never-green Failed, with no fix pass (issue #758).
func TestSelfHeal_ConflictResolveDispatchFailure_NoFixPass(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(forcePushRedConfig(2), fc, fc)
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("boom")

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want errLandingNeverGreen", reason)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("expected no fix call on conflict-resolve failure, got %+v", d.FixCalls)
	}
}

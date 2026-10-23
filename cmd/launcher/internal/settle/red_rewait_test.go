package settle

import (
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// A force-pushed head that goes red gets one fix pass, and the fixed head merges.
// The fixed head conflicts again, so the second rebase or conflict-resolve only
// runs if mergeImmediate's per-call budgets reset after the fix pass.
func TestSelfHeal_PostForcePushRed_FixPassThenMerges(t *testing.T) {
	cases := []struct {
		name         string
		rebaseErrs   []error
		mergeErrs    []error
		wantResolves int
	}{
		{"plain-rebase", nil, []error{forge.ErrMergeConflict, forge.ErrMergeConflict}, 0},
		{"conflict-resolve", []error{forge.ErrMergeConflict, forge.ErrMergeConflict}, []error{forge.ErrMergeConflict, forge.ErrMergeConflict}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 1
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.RebaseErrs = tc.rebaseErrs
			fc.MergeErrs = tc.mergeErrs
			fc.SetCheckStates(testPR, []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure,                     // rewait after the first force-push: red
				forge.StateFailure,                     // re-observed red before the fix pass
				forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
				forge.StateSuccess, forge.StateSuccess, // rewait after the second force-push: green
			})
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
			}
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("expected exactly fix-pass-1, got %v", passes)
			}
			if got := len(d.ResolveConflictCalls); got != tc.wantResolves {
				t.Errorf("ResolveConflict calls = %d, want %d", got, tc.wantResolves)
			}
			if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
				t.Errorf("last transition To=%v, want Complete", last.To)
			}
		})
	}
}

// The post-force-push red fix pass draws on the same budget as the pre-merge
// red-CI loop: one pre-merge fix spent against MaxFixAttempts=1 leaves none
// for the red re-wait, so the landing settles Failed with no second fix pass.
func TestSelfHeal_PostForcePushRed_SharesFixBudget(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateSuccess, forge.StateSuccess, // pre-merge red, fixed to green
		forge.StateFailure, // rewait after the rebase force-push: red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("want exactly 1 fix pass across the shared budget, got %d", len(d.FixCalls))
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want it to wrap %q", reason, errLandingNeverGreen.Error())
	}
	var comments string
	for _, c := range fc.CommentCalls {
		comments += c.Body + "\n"
	}
	if !strings.Contains(comments, "fix budget exhausted: 1 of 1 fix pass(es) used, 0 after a force-push") {
		t.Errorf("comment must name fix attempts used; comments:\n%s", comments)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
}

// Same shape as SharesFixBudget, but the one fix pass runs on a force-pushed
// head: the red re-wait after the first force-push spends the budget, the
// fixed head conflicts again, and the final red re-wait settles Failed with
// that fix pass counted as post-force-push.
func TestSelfHeal_PostForcePushRed_FixPassCountsAfterForcePush(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after the first force-push: red
		forge.StateFailure,                     // re-observed red before the fix pass
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
		forge.StateFailure, // rewait after the second force-push: red, budget exhausted
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	var comments string
	for _, c := range fc.CommentCalls {
		comments += c.Body + "\n"
	}
	if !strings.Contains(comments, "fix budget exhausted: 1 of 1 fix pass(es) used, 1 after a force-push") {
		t.Errorf("comment must name the force-push fix pass; comments:\n%s", comments)
	}
}

package settle

import (
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedSetup builds a Settle whose first merge conflicts, the clean
// rebase force-pushes, and the post-force-push re-wait reads the given states.
func forcePushRedSetup(maxFix int, states []forge.RollupState) (*Settle, *forge.Fake) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	c.MaxFixAttempts = maxFix
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

// A red post-force-push re-wait dispatches the fix pass; the head it turns
// green re-enters the merge path and lands.
func TestSelfHeal_ForcePushRed_FixPassThenMerged(t *testing.T) {
	s, fc := forcePushRedSetup(2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first green
		forge.StateFailure, forge.StateFailure, // re-wait red, re-poll red
		forge.StateSuccess, forge.StateSuccess, // after the fix pass
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// With no fix budget a red force-pushed head fails as landing-failed, never
// dispatching a pass.
func TestSelfHeal_ForcePushRed_NoBudgetFailsLandingFailed(t *testing.T) {
	s, fc := forcePushRedSetup(0, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateFailure,
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) || !strings.Contains(reason, "0 of 0") {
		t.Errorf("reason = %q, want never-green text naming 0 of 0 fix passes", reason)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix passes = %v, want none", fixPasses(d))
	}
	last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]
	if last.To != forge.Failed {
		t.Errorf("last transition = %+v, want Failed", last)
	}
}

// The post-force-push fix pass draws from the same budget as the pre-merge
// loop: a pre-merge fix spends the only pass, so a later force-push red has no
// fresh allowance, and the comment names the passes the force-pushed head used.
func TestSelfHeal_ForcePushRed_SharesPreMergeBudget(t *testing.T) {
	s, fc := forcePushRedSetup(1, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red
		forge.StateSuccess, forge.StateSuccess, // green after the pre-merge fix
		forge.StateFailure, forge.StateFailure, // re-wait red, re-poll red
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly the one pre-merge pass", passes)
	}
	if !strings.Contains(reason, "after 0 fix pass(es) (1 of 1 used overall)") {
		t.Errorf("reason = %q, want it to name 0 of 1 fix passes used by the force-pushed head", reason)
	}
	if lastComment := fc.CommentCalls[len(fc.CommentCalls)-1].Body; !strings.Contains(lastComment, "landing failed:") || !strings.Contains(lastComment, "after 0 fix pass(es) (1 of 1 used overall)") {
		t.Errorf("last comment = %q, want a landing-failed comment naming 0 of 1 fix passes", lastComment)
	}
	last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]
	if last.To != forge.Failed {
		t.Errorf("last transition = %+v, want Failed", last)
	}
}

// A force-pushed head whose fix pass leaves it red again exhausts the budget
// and names the passes it consumed.
func TestSelfHeal_ForcePushRed_FixStillRedNamesAttempts(t *testing.T) {
	s, _ := forcePushRedSetup(1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want 1", passes)
	}
	if !strings.Contains(reason, "after 1 fix pass(es) (1 of 1 used overall)") {
		t.Errorf("reason = %q, want it to name 1 force-push pass and 1 of 1 overall", reason)
	}
}

// On the push-only path (nil dispatcher) a red post-force-push re-wait cannot
// run a fix pass: it falls to the generic "landing blocked" manual branch. A
// real push-only forge skips the re-wait (s.pr == nil), so the test wires a PR
// forge to reach the defensive arm directly.
func TestLandPushOnly_ForcePushRed_LandingBlockedManual(t *testing.T) {
	s, fc := forcePushRedSetup(2, []forge.RollupState{
		forge.StateFailure, forge.StateFailure,
	})

	got := s.landPushOnly("1", 0, "agent/issue-1")

	if got != landingManual {
		t.Fatalf("landPushOnly = %v, want landingManual", got)
	}
	found := false
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, "landing blocked") {
			found = true
		}
	}
	if !found {
		t.Errorf("comments = %+v, want a landing blocked comment", fc.CommentCalls)
	}
}

package settle

import (
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedFixture is a rebase-retry landing whose post-force-push re-wait
// ends red. States: green gate, then the rewait's red, then whatever follows.
func forcePushRedFixture(maxFix int, states []forge.RollupState) (*Settle, *forge.Fake, *dispatch.Fake) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = maxFix
	c.MergePollTimeout = 100
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc, dispatch.NewFake()
}

// A red post-force-push re-wait dispatches the red-CI fix pass; a head the
// fix turns green re-enters the merge path and the issue completes.
func TestSelfHeal_RewaitAfterForcePushRed_FixPassThenMerges(t *testing.T) {
	s, fc, d := forcePushRedFixture(2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gate
		forge.StateFailure, forge.StateFailure, // rewait: red, then the loop's own re-poll
		forge.StateSuccess, forge.StateSuccess, // after fix pass
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("fix calls = %+v, want 1", d.FixCalls)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
	}
}

// The fix budget is shared with the pre-merge loop: an exhausted budget fails
// the landing and the comment names the attempts used.
func TestSelfHeal_RewaitAfterForcePushRed_BudgetSpentFails(t *testing.T) {
	s, fc, d := forcePushRedFixture(1, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red: uses the one fix pass
		forge.StateSuccess, forge.StateSuccess, // green after fix
		forge.StateFailure, forge.StateFailure, // rewait + re-poll: red, budget gone
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("fix calls = %+v, want 1 (shared budget)", d.FixCalls)
	}
	if !strings.Contains(reason, "after 1 fix pass(es)") {
		t.Errorf("reason = %q, want attempts named", reason)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-failed", iss.Labels)
	}
}

// With no fix budget at all the red re-wait fails without a fix pass.
func TestSelfHeal_RewaitAfterForcePushRed_NoBudgetNoFixPass(t *testing.T) {
	s, _, d := forcePushRedFixture(0, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateFailure,
	})

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix calls = %+v, want none", d.FixCalls)
	}
}

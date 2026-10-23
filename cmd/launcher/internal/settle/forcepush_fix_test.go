package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

func forcePushRedSettle(t *testing.T, maxFix int, states []forge.RollupState) (*Settle, *forge.Fake) {
	t.Helper()
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = maxFix
	c.MergePollTimeout = 100
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

// A force-pushed head whose re-wait ends red gets the red-CI fix pass, and a
// head that pass turns green re-enters the merge path.
func TestSelfHeal_ForcePushRed_FixPassThenMerges(t *testing.T) {
	s, fc := forcePushRedSettle(t, 1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first green
		forge.StateFailure,                     // post-rebase re-wait: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass
	})
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); len(got) != 1 || got[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-complete; labels=%v", iss.Labels)
	}
}

// The force-pushed head draws from the same budget as pre-merge red: one pass
// spent before the first green leaves only the remainder.
func TestSelfHeal_ForcePushRed_SharesFixBudget(t *testing.T) {
	s, _ := forcePushRedSettle(t, 2, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red: fix pass 1
		forge.StateSuccess, forge.StateSuccess, // green
		forge.StateFailure, // post-rebase re-wait: red, fix pass 2
		forge.StateSuccess, forge.StateSuccess,
	})
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("fix passes = %v, want [1 2]", got)
	}
}

// With the shared budget spent, a red force-pushed head is a landing failure,
// not ci-red/fix-exhausted, and the comment names the passes it consumed.
func TestSelfHeal_ForcePushRed_BudgetExhausted_LandingFailed(t *testing.T) {
	cases := []struct {
		name     string
		maxFix   int
		states   []forge.RollupState
		wantFix  int
		wantText string
	}{
		{
			name:   "pre-merge pass spent the budget",
			maxFix: 1,
			states: []forge.RollupState{
				forge.StateFailure,
				forge.StateSuccess, forge.StateSuccess,
				forge.StateFailure,
			},
			wantFix:  1,
			wantText: "after 0 of 1 fix pass(es) on the force-pushed head",
		},
		{
			name:   "fix pass on the force-pushed head stays red",
			maxFix: 1,
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess,
				forge.StateFailure, // re-wait red: fix pass 1
				forge.StateFailure, // still red, budget spent
			},
			wantFix:  1,
			wantText: "after 1 of 1 fix pass(es) on the force-pushed head",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, fc := forcePushRedSettle(t, tc.maxFix, tc.states)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != tc.wantFix {
				t.Errorf("fix calls = %d, want %d", len(d.FixCalls), tc.wantFix)
			}
			if strings.HasPrefix(reason, "ci-red") {
				t.Errorf("reason = %q, must not be the ci-red/fix-exhausted note", reason)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want it to name %q", reason, errLandingNeverGreen.Error())
			}
			var found bool
			for _, cc := range fc.CommentCalls {
				if strings.Contains(cc.Body, tc.wantText) {
					found = true
				}
			}
			if !found {
				t.Errorf("no comment contains %q; comments=%+v", tc.wantText, fc.CommentCalls)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

// A re-wait timeout or a failed conflict-resolve stays a landing failure with
// no fix pass, even with budget left.
func TestSelfHeal_ForcePushNonRed_NoFixPass(t *testing.T) {
	cases := []struct {
		name       string
		rebaseErr  error
		resolveErr bool
		timeout    int
		states     []forge.RollupState
	}{
		{name: "re-wait timeout", timeout: 0, states: []forge.RollupState{forge.StateSuccess, forge.StateSuccess}},
		{name: "conflict-resolve fails", rebaseErr: forge.ErrMergeConflict, resolveErr: true, timeout: 100,
			states: []forge.RollupState{forge.StateSuccess, forge.StateSuccess}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, fc := forcePushRedSettle(t, 3, tc.states)
			s.cfg.MergePollTimeout = tc.timeout
			fc.RebaseErr = tc.rebaseErr
			d := dispatch.NewFake()
			if tc.resolveErr {
				d.ResolveConflictErr = errors.New("box crashed")
			}

			landing, _ := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("fix calls = %v, want none", d.FixCalls)
			}
		})
	}
}

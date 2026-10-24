package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// newForcePushRedFixture sets up an immediate-mode landing whose first merge
// conflicts, so mergeImmediate rebases (force-push) and re-waits on the new
// head. The check script drives the gate outcomes.
func newForcePushRedFixture(t *testing.T, maxFixAttempts int, checkStates []forge.RollupState) (*Settle, *forge.Fake, *dispatch.Fake) {
	t.Helper()
	c := fixConfig(maxFixAttempts)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, checkStates)
	return newTestSettle(c, fc, fc), fc, dispatch.NewFake()
}

// A red re-wait after a force-push dispatches the red-CI fix pass rather than
// settling Failed, and a head the fix turns green merges on the normal path.
func TestSelfHeal_ForcePushRed_FixPassTurnsGreenMerges(t *testing.T) {
	s, fc, d := newForcePushRedFixture(t, 1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after the rebase: red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-complete; labels=%v", iss.Labels)
	}
}

// Force-push fix passes draw from the same budget as the pre-merge red loop:
// one pass already spent before the merge leaves none for the force-pushed
// head, so the red re-wait exhausts without dispatching.
func TestSelfHeal_ForcePushRed_SharesFixBudget(t *testing.T) {
	s, fc, d := newForcePushRedFixture(t, 1, []forge.RollupState{
		forge.StateFailure,                     // initial gate: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gate after the pre-merge fix
		forge.StateFailure, // rewait after the rebase: red again
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1 (the pre-merge pass only)", len(d.FixCalls))
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-failed and not agent-complete; labels=%v", iss.Labels)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settle reason = %q, want %q", got, ReasonLandingFailed)
	}
	want := "fix budget exhausted after 1 fix pass(es), 0 of them on the force-pushed head"
	if !commentContains(fc, want) {
		t.Errorf("want a comment containing %q; comments=%+v", want, fc.CommentCalls)
	}
}

// Each red gate read spends one fix pass until the budget runs out, the
// force-pushed head's passes included.
func TestSelfHeal_ForcePushRed_RepeatedRedSpendsBudget(t *testing.T) {
	s, _, d := newForcePushRedFixture(t, 2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after the rebase: red
		forge.StateFailure,                     // gate after fix pass 1: still red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 2
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if len(d.FixCalls) != 2 {
		t.Errorf("FixCalls = %d, want 2", len(d.FixCalls))
	}
}

// Spending the budget across a force-push names the passes the force-pushed
// head consumed: pass 1 ran before the merge, pass 2 after the rebase.
func TestSelfHeal_ForcePushRed_ExhaustedNamesForcePushPasses(t *testing.T) {
	s, fc, d := newForcePushRedFixture(t, 2, []forge.RollupState{
		forge.StateFailure,                     // initial gate: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 1
		forge.StateFailure, // rewait after the rebase: red
		forge.StateFailure, // gate after fix pass 2: still red
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 2 {
		t.Errorf("FixCalls = %d, want 2", len(d.FixCalls))
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settle reason = %q, want %q", got, ReasonLandingFailed)
	}
	want := "fix budget exhausted after 2 fix pass(es), 1 of them on the force-pushed head"
	if !commentContains(fc, want) {
		t.Errorf("want a comment containing %q; comments=%+v", want, fc.CommentCalls)
	}
}

// A second force-push that also goes red keeps counting from the first one:
// both fix passes were spent on force-pushed heads.
func TestSelfHeal_ForcePushRed_SecondForcePushKeepsBase(t *testing.T) {
	s, fc, d := newForcePushRedFixture(t, 2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after the first rebase: red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 1: green
		forge.StateFailure, // rewait after the second rebase: red
		forge.StateFailure, // gate after fix pass 2: still red
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if len(d.FixCalls) != 2 {
		t.Errorf("FixCalls = %d, want 2", len(d.FixCalls))
	}
	want := "fix budget exhausted after 2 fix pass(es), 2 of them on the force-pushed head"
	if !commentContains(fc, want) {
		t.Errorf("want a comment containing %q; comments=%+v", want, fc.CommentCalls)
	}
}

// A force-pushed head whose fix pass turns green, then hits a second conflict
// that times out on the re-wait, settles Failed with that pass counted as
// spent on the force-pushed head.
func TestSelfHeal_ForcePushNeverGreen_AfterFixPassNamesSpend(t *testing.T) {
	s, fc, d := newForcePushRedFixture(t, 2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after the first rebase: red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass 1: green
		// second rebase's re-wait: no reads left, so it times out
	})
	s.cfg.MergePollTimeout = 0
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settle reason = %q, want %q", got, ReasonLandingFailed)
	}
	want := "after 1 fix pass(es), 1 of them on the force-pushed head"
	if !commentContains(fc, want) {
		t.Errorf("want a comment containing %q; comments=%+v", want, fc.CommentCalls)
	}
}

// Only a red re-wait gets the fix pass. A timed-out re-wait or a failed
// conflict-resolve or its relay still settles Failed with errLandingNeverGreen
// and no pass.
func TestSelfHeal_ForcePushNotRed_NoFixPass(t *testing.T) {
	cases := []struct {
		name       string
		checkState []forge.RollupState
		pollTO     int
		resolveErr error
		rebaseErr  error
		relayErr   error
		wantText   string
	}{
		{
			name:       "rewait times out",
			checkState: []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			pollTO:     0,
			wantText:   "CI did not reach green after force-push",
		},
		{
			name:       "conflict-resolve dispatch fails",
			checkState: []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			pollTO:     100,
			rebaseErr:  forge.ErrMergeConflict,
			resolveErr: errors.New("dispatch refused"),
			wantText:   "conflict-resolve dispatch failed",
		},
		{
			name:       "conflict-resolve relay fails",
			checkState: []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			pollTO:     100,
			rebaseErr:  forge.ErrMergeConflict,
			relayErr:   errors.New("relay refused"),
			wantText:   "relay after conflict-resolve failed",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, fc, d := newForcePushRedFixture(t, 3, tc.checkState)
			s.cfg.MergePollTimeout = tc.pollTO
			fc.RebaseErr = tc.rebaseErr
			d.ResolveConflictErr = tc.resolveErr
			if tc.relayErr != nil {
				s.cfg.OutboxDir = func(string) string { return t.TempDir() }
				fc.RelayBundleErr = tc.relayErr
				s = newTestSettle(s.cfg, fc, fc.AsGithubReadOnly())
			}

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v (%q), want landingFailed", landing, reason)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
			}
			if !strings.Contains(reason, tc.wantText) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, tc.wantText)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("FixCalls = %d, want 0", len(d.FixCalls))
			}
		})
	}
}

func commentContains(fc *forge.Fake, want string) bool {
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, want) {
			return true
		}
	}
	return false
}

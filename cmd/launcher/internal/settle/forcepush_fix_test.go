package settle

import (
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushFixSetup builds a Settle whose first merge conflicts and whose
// rebase returns rebaseErr, polling the given check states in order.
func forcePushFixSetup(maxFix int, rebaseErr error, states []forge.RollupState) (*Settle, *forge.Fake, *dispatch.Fake) {
	c := fixConfig(maxFix)
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = rebaseErr
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc, dispatch.NewFake()
}

// assertMergedAfterOneFix checks the PR merged after exactly fix pass 1 and the
// issue ended Complete without agent-failed.
func assertMergedAfterOneFix(t *testing.T, fc *forge.Fake, d *dispatch.Fake, landing landingResult) {
	t.Helper()
	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
	iss, _ := fc.Issue("1")
	if containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must not carry agent-failed; labels=%v", iss.Labels)
	}
}

// assertLandingFailedSettled checks a landing-failed settle: latch reason, a
// comment containing want, the Failed transition and the agent-failed label.
func assertLandingFailedSettled(t *testing.T, s *Settle, fc *forge.Fake, want string) {
	t.Helper()
	s.settledMu.Lock()
	latch := s.settledLatch["1"].reason
	s.settledMu.Unlock()
	if latch != ReasonLandingFailed {
		t.Errorf("latch reason = %v, want ReasonLandingFailed", latch)
	}
	found := false
	for _, body := range issueComments(fc) {
		if strings.Contains(body, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("no comment containing %q; comments=%v", want, issueComments(fc))
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
}

// redThenGreenStates feeds, in poll order: the initial gate (green), the
// re-wait after the first force-push (red), the gate on the fixed head
// (green), then the re-wait after the second force-push (green).
var redThenGreenStates = []forge.RollupState{
	forge.StateSuccess, forge.StateSuccess,
	forge.StateFailure,
	forge.StateSuccess, forge.StateSuccess,
	forge.StateSuccess, forge.StateSuccess,
}

// A force-pushed head whose re-wait goes red gets a fix pass, and a head the
// fix turns green re-enters the merge path.
func TestSelfHeal_ForcePushRedFixSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rebaseErr    error
		wantResolves int
	}{
		{"clean rebase", nil, 0},
		{"conflict-resolve", forge.ErrMergeConflict, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fc, d := forcePushFixSetup(2, tc.rebaseErr,
				[]forge.RollupState{
					forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
					forge.StateFailure,                     // rewait after the force-push: red
					forge.StateSuccess, forge.StateSuccess, // gate on the fixed head
				})

			landing, _ := s.selfHeal(d, "1", 0, testPR)

			assertMergedAfterOneFix(t, fc, d, landing)
			if n := len(d.ResolveConflictCalls); n != tc.wantResolves {
				t.Errorf("ResolveConflict calls = %d, want %d", n, tc.wantResolves)
			}
		})
	}
}

// The stale-base preflight rebase force-pushes too, so a red re-wait there
// also gets a fix pass.
func TestSelfHeal_StaleBasePreflightRedFixSucceeds(t *testing.T) {
	s, fc, d := forcePushFixSetup(2, nil, redThenGreenStates)
	s.cfg.PreflightStaleBase = true
	fc.SetNeedsUpdate(testPR, true)
	fc.MergeErrs = nil

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	assertMergedAfterOneFix(t, fc, d, landing)
}

// The fixed head re-enters the merge path with fresh rebase budgets: with one
// rebase allowed per merge attempt, the second conflict is still rebased.
func TestSelfHeal_ForcePushRedFixResetsRebaseBudget(t *testing.T) {
	s, fc, d := forcePushFixSetup(2, nil, redThenGreenStates)
	s.cfg.MaxRebaseAttempts = 1
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict, nil}

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	assertMergedAfterOneFix(t, fc, d, landing)
	if len(fc.RebasedURLs) != 2 {
		t.Errorf("Rebase called %d times, want 2 (one per merge attempt)", len(fc.RebasedURLs))
	}
}

// The force-push fix pass draws on the shared budget: with the one pass spent
// pre-merge, a red re-wait settles landing-failed with no further pass.
func TestSelfHeal_ForcePushRedSharedBudgetExhausted(t *testing.T) {
	s, fc, d := forcePushFixSetup(1, nil,
		[]forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess, forge.StateFailure})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", passes)
	}
	want := "consumed 0 fix pass(es), 1 of 1 used in all"
	if !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want substring %q", reason, want)
	}
	assertLandingFailedSettled(t, s, fc, want)
}

// A force-pushed head the fix pass leaves red names the passes it consumed.
func TestSelfHeal_ForcePushRedFixExhausted(t *testing.T) {
	s, fc, d := forcePushFixSetup(1, nil,
		[]forge.RollupState{forge.StateSuccess, forge.StateSuccess, forge.StateFailure, forge.StateFailure})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", passes)
	}
	want := "consumed 1 fix pass(es), 1 of 1 used in all"
	if !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want substring %q", reason, want)
	}
	assertLandingFailedSettled(t, s, fc, want)
}

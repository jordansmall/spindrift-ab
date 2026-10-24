package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushFixConfig is an immediate-merge config whose first Merge attempt
// conflicts (see forcePushFake), forcing a clean rebase and post-force-push
// re-wait.
func forcePushFixConfig(maxFix int) Config {
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	return c
}

func forcePushFake(states ...forge.RollupState) *forge.Fake {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	return fc
}

func assertLandingFailed(t *testing.T, fc *forge.Fake) {
	t.Helper()
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-failed and not agent-complete; labels=%v", iss.Labels)
	}
}

func TestSelfHeal_RedAfterForcePush_FixPassGreenMerges(t *testing.T) {
	fc := forcePushFake(
		forge.StateSuccess, forge.StateSuccess, // first green
		forge.StateFailure,                     // re-wait after force-push: red
		forge.StateSuccess, forge.StateSuccess, // after the fix pass
	)
	s := newTestSettle(forcePushFixConfig(2), fc, fc)
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

func TestSelfHeal_RedAfterForcePush_BudgetExhaustedCommentsAttempts(t *testing.T) {
	fc := forcePushFake(
		forge.StateSuccess, forge.StateSuccess,
		forge.StateFailure,
		forge.StateFailure, // fix pass 1 did not help
	)
	s := newTestSettle(forcePushFixConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
	if n := len(fixPasses(d)); n != 1 {
		t.Errorf("fix calls = %d, want 1", n)
	}
	assertLandingFailed(t, fc)
	if !hasCommentContaining(fc, "1 of 1 fix pass") {
		t.Errorf("no comment naming attempts used; comments=%v", fc.CommentCalls)
	}
}

func TestSelfHeal_RedAfterForcePush_NoBudget_FailsWithoutFix(t *testing.T) {
	fc := forcePushFake(forge.StateSuccess, forge.StateSuccess, forge.StateFailure)
	s := newTestSettle(forcePushFixConfig(0), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix calls = %d, want 0", len(d.FixCalls))
	}
	assertLandingFailed(t, fc)
	if !hasCommentContaining(fc, "0 of 0 fix pass") {
		t.Errorf("no comment naming attempts used; comments=%v", fc.CommentCalls)
	}
}

func TestSelfHeal_RewaitTimeoutOrResolveFailure_NoFixPass(t *testing.T) {
	cases := []struct {
		name       string
		rebaseErr  error
		resolveErr error
		timeout    int
		states     []forge.RollupState
		wantReason string
	}{
		{name: "rewait timeout", rebaseErr: nil, timeout: 0,
			states:     []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			wantReason: "CI did not reach green after force-push"},
		{name: "conflict-resolve dispatch fails", rebaseErr: forge.ErrMergeConflict,
			resolveErr: errors.New("boom"), timeout: 100,
			states:     []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			wantReason: "conflict-resolve dispatch failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := forcePushFake(tc.states...)
			fc.RebaseErr = tc.rebaseErr
			c := forcePushFixConfig(3)
			c.MergePollTimeout = tc.timeout
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("fix calls = %d, want 0", len(d.FixCalls))
			}
			assertLandingFailed(t, fc)
		})
	}
}

func TestSelfHeal_ConflictResolveRelayFailure_NoFixPass(t *testing.T) {
	fc := forcePushFake(forge.StateSuccess, forge.StateSuccess)
	fc.RebaseErr = forge.ErrMergeConflict
	fc.RelayBundleErr = errors.New("bundle missing")
	c := forcePushFixConfig(3)
	c.ReadOnly = true
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "relay") {
		t.Errorf("reason = %q, want it to name the relay failure", reason)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix calls = %d, want 0", len(d.FixCalls))
	}
	assertLandingFailed(t, fc)
}

// The fix budget is shared with the pre-merge red-CI loop: a pass spent there
// is not refunded to the force-pushed head.
func TestSelfHeal_RedAfterForcePush_SharesFixBudgetWithPreMergeRed(t *testing.T) {
	fc := forcePushFake(
		forge.StateFailure,                     // pre-merge red
		forge.StateSuccess, forge.StateSuccess, // green after fix 1
		forge.StateFailure, // red after force-push
	)
	s := newTestSettle(forcePushFixConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly one", passes)
	}
	assertLandingFailed(t, fc)
	if !hasCommentContaining(fc, "1 of 1 fix pass(es) used, 0 on the force-pushed head") {
		t.Errorf("no comment splitting shared and force-pushed passes; comments=%v", fc.CommentCalls)
	}
}

func hasCommentContaining(fc *forge.Fake, sub string) bool {
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, sub) {
			return true
		}
	}
	return false
}

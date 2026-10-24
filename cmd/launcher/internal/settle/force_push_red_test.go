package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedConfig is an immediate-merge landing whose first merge conflicts,
// so mergeImmediate force-pushes a rebase and re-waits CI on the new head.
func forcePushRedConfig(maxFixAttempts int) Config {
	c := fixConfig(maxFixAttempts)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	return c
}

// A genuine red re-wait after an immediate-mode force-push gets a fix pass from
// the shared budget, and a head that the fix turns green merges (issue #5053).
func TestSelfHeal_ForcePushRedFixPassThenMerges(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-force-push re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // post-fix gateToGreen
	})
	s := newTestSettle(forcePushRedConfig(3), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged after a fix pass", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected a TransitionState call (Complete)")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// When the pre-merge red loop already spent the whole budget, a later red
// re-wait after a force-push gets no fix pass: it settles landing-failed with
// the attempts used named (issue #5053).
func TestSelfHeal_ForcePushRedBudgetExhausted_EndsLandingFailed(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // initial gate: red, spends the one fix pass
		forge.StateSuccess, forge.StateSuccess, // post-fix gateToGreen
		forge.StateFailure, // post-force-push re-wait: red with no budget left
	})
	s := newTestSettle(forcePushRedConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("expected exactly one fix pass (shared budget), got %v", passes)
	}
	if !strings.Contains(reason, "1/1 fix pass(es) used, 0 on the force-pushed head") {
		t.Errorf("reason = %q, want it to name the attempts used", reason)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition = %+v, want Failed", last)
	}
	if len(fc.CommentCalls) == 0 || !strings.Contains(fc.CommentCalls[len(fc.CommentCalls)-1].Body, "1/1 fix pass(es) used, 0 on the force-pushed head") {
		t.Errorf("expected a landing-failed comment naming the attempts used; comments=%+v", fc.CommentCalls)
	}
}

// With no fix budget a force-pushed red head ends landing-failed without any fix
// pass, matching the pre-change outcome.
func TestSelfHeal_ForcePushRedNoBudget_NoFixPass(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure, // post-force-push re-wait: red
	})
	s := newTestSettle(forcePushRedConfig(0), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 0 {
		t.Errorf("expected no fix pass with MaxFixAttempts=0, got %v", passes)
	}
}

// A re-wait that times out is not a genuine red, so it demotes at once even
// with fix budget left.
func TestSelfHeal_ForcePushRewaitTimeout_NoFixPass(t *testing.T) {
	c := forcePushRedConfig(3)
	c.MergePollTimeout = 0
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 0 {
		t.Errorf("expected no fix pass on re-wait timeout, got %v", passes)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
}

// A conflict-resolve dispatch failure is terminal: no fix pass runs even with
// budget left.
func TestSelfHeal_ConflictResolveDispatchFails_NoFixPass(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(forcePushRedConfig(3), fc, fc)
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("box exited 1")

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 0 {
		t.Errorf("expected no fix pass on conflict-resolve dispatch failure, got %v", passes)
	}
	if !strings.Contains(reason, "conflict-resolve dispatch failed") {
		t.Errorf("reason = %q, want the conflict-resolve dispatch failure", reason)
	}
}

// A conflict-resolve bundle relay failure is terminal too: no fix pass runs
// even with budget left.
func TestSelfHeal_ConflictResolveRelayFails_NoFixPass(t *testing.T) {
	c := forcePushRedConfig(3)
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.RelayBundleErr = errors.New("relay refused")
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	cf := fc.AsGithubReadOnly()
	s := newTestSettle(c, fc, cf)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 0 {
		t.Errorf("expected no fix pass on conflict-resolve relay failure, got %v", passes)
	}
	if !strings.Contains(reason, "relay after conflict-resolve failed") {
		t.Errorf("reason = %q, want the conflict-resolve relay failure", reason)
	}
}

// Only a genuine red re-wait is eligible for fix passes; a timeout must not
// match errForcePushRed, so it never reaches the fix loop.
func TestRewaitGateResultErr_TerminalIsNotForcePushRed(t *testing.T) {
	err := rewaitGateResultErr(gateTerminal, "", testPR)
	if errors.Is(err, errForcePushRed) {
		t.Errorf("rewaitGateResultErr(gateTerminal) = %v, must not match errForcePushRed", err)
	}
	if !errors.Is(err, errLandingNeverGreen) {
		t.Errorf("rewaitGateResultErr(gateTerminal) = %v, want wrapping errLandingNeverGreen", err)
	}
}

// A force-pushed head that spends its one fix pass and is still red after it
// ends landing-failed, and the reason counts that fix pass as spent on the
// force-pushed head (issue #5053).
func TestSelfHeal_ForcePushRedFixPassStillRed_EndsLandingFailed(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure, // post-force-push re-wait: genuine red
		forge.StateFailure, // post-fix gate: still red, budget spent
	})
	s := newTestSettle(forcePushRedConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("expected exactly one fix pass, got %v", passes)
	}
	const want = "1/1 fix pass(es) used, 1 on the force-pushed head"
	if !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want it to contain %q", reason, want)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
	if len(fc.CommentCalls) == 0 || !strings.Contains(fc.CommentCalls[len(fc.CommentCalls)-1].Body, want) {
		t.Errorf("expected a landing-failed comment naming the attempts used; comments=%+v", fc.CommentCalls)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition = %+v, want Failed", last)
	}
}

// The stale-base preflight's post-force-push re-wait routes a genuine red into
// the same fix loop as the clean rebase-retry path: one fix pass, then green
// merges (issue #5053). NeedsUpdate is sticky in the fake, so the post-fix
// mergeImmediate runs the preflight again and its re-wait consumes two greens.
func TestSelfHeal_PreflightStaleBaseRedRewaitFixPassThenMerges(t *testing.T) {
	c := forcePushRedConfig(3)
	c.PreflightStaleBase = true
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetNeedsUpdate(testPR, true)
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // preflight re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // post-fix gateToGreen
		forge.StateSuccess, forge.StateSuccess, // post-fix preflight re-wait
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("expected exactly one fix pass, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merge not called on the green head; fc.Merged=%q", fc.Merged)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition = %+v, want Complete", last)
	}
}

// A conflict-resolve Box dispatched from the rebase path leaves a head that goes
// red on re-wait; that red takes one fix pass and the fixed head then merges
// (issue #5053).
func TestSelfHeal_ConflictResolveRedRewaitFixPassThenMerges(t *testing.T) {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-resolve re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // post-fix gateToGreen
	})
	s := newTestSettle(forcePushRedConfig(3), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged", landing, reason)
	}
	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("expected one conflict-resolve dispatch, got %v", d.ResolveConflictCalls)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("expected exactly one fix pass, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merge not called on the green head; fc.Merged=%q", fc.Merged)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition = %+v, want Complete", last)
	}
}

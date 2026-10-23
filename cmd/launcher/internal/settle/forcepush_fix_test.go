package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushForge scripts a conflicting first merge so a rebase force-push
// follows; states drive every CheckState poll (a SUCCESS costs two polls, its
// confirm included).
func forcePushForge(states ...forge.RollupState) *forge.Fake {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.SetCheckStates(testPR, states)
	return fc
}

func forcePushConfig(maxFix int) Config {
	c := fixConfig(maxFix)
	c.MaxRebaseAttempts = 3
	return c
}

func lastComment(fc *forge.Fake) string {
	if len(fc.CommentCalls) == 0 {
		return ""
	}
	return fc.CommentCalls[len(fc.CommentCalls)-1].Body
}

func TestSelfHeal_ForcePushRed_FixPassThenMerges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F, S, S)
	s := newTestSettle(forcePushConfig(2), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); len(got) != 1 || got[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("labels = %v, want agent-complete", iss.Labels)
	}
}

// The pre-merge loop spent the whole budget, so a force-push red has none left.
func TestSelfHeal_ForcePushRed_BudgetSpentPreMergeFails(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(F, S, S, F)
	s := newTestSettle(forcePushConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly the pre-merge pass", got)
	}
	if !strings.Contains(reason, "consumed 0 fix pass(es) (1 of 1 used in total)") {
		t.Errorf("reason = %q, want attempt counts", reason)
	}
	if !strings.Contains(lastComment(fc), "consumed 0 fix pass(es)") {
		t.Errorf("comment = %q, want attempt counts", lastComment(fc))
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-failed", iss.Labels)
	}
	if fc.Merged != "" {
		t.Errorf("must not merge; fc.Merged=%q", fc.Merged)
	}
}

func TestSelfHeal_ForcePushRed_NoFixBudgetFails(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F)
	s := newTestSettle(forcePushConfig(0), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Fatalf("landing=%v fix calls=%d, want landingFailed with none", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, "0 of 0 used in total") {
		t.Errorf("reason = %q", reason)
	}
}

// Pre-merge used 1 of 3; the force-pushed head can use only the other 2, and
// the failure names what it consumed.
func TestSelfHeal_ForcePushRed_SharesBudget(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	// red, fix1, green -> force-push red, fix2, red, fix3, red -> exhausted.
	fc := forcePushForge(F, S, S, F, F, F)
	s := newTestSettle(forcePushConfig(3), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); len(got) != 3 {
		t.Errorf("fix passes = %v, want 3 in total", got)
	}
	if !strings.Contains(reason, "consumed 2 fix pass(es) (3 of 3 used in total)") {
		t.Errorf("reason = %q", reason)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-failed", iss.Labels)
	}
}

func TestSelfHeal_ForcePushRewaitTimeout_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	fc := forcePushForge(S, S)
	c := forcePushConfig(3)
	c.MergePollTimeout = 0
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Fatalf("landing=%v fix calls=%d, want landingFailed with none", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q", reason)
	}
}

func TestSelfHeal_ForcePushConflictResolveFailure_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	fc := forcePushForge(S, S)
	fc.RebaseErr = forge.ErrMergeConflict
	s := newTestSettle(forcePushConfig(3), fc, fc)
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("agent could not resolve")

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Fatalf("landing=%v fix calls=%d, want landingFailed with none", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q", reason)
	}
}

// Each force-push red is a fresh head: the failure counts only the passes the
// current one consumed, not the earlier head's.
func TestSelfHeal_ForcePushRedTwice_CountsCurrentHeadOnly(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F, S, S, F)
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict, nil}
	s := newTestSettle(forcePushConfig(1), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want 1", got)
	}
	if !strings.Contains(reason, "consumed 0 fix pass(es) (1 of 1 used in total)") {
		t.Errorf("reason = %q, want only the current head's passes", reason)
	}
}

// The fix-pass head's own main-loop poll is the ordinary pre-merge gate, so a
// timeout there is gate-terminal, not a post-force-push failure.
func TestSelfHeal_ForcePushRed_FixPassHeadTimesOut_GateTerminal(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F)
	s := newTestSettle(forcePushConfig(2), fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want exactly 1", got)
	}
	if !strings.Contains(reason, "ci-timeout") {
		t.Errorf("reason = %q, want ci-timeout", reason)
	}
	s.settledMu.Lock()
	got := s.settledLatch["1"].reason
	s.settledMu.Unlock()
	if got != ReasonGateTerminal {
		t.Errorf("settled reason = %q, want %q", got, ReasonGateTerminal)
	}
}

func TestSelfHeal_ForcePushConflictResolveRelayFailure_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{S, S})
	fc.RelayBundleErr = errors.New("ssh auth blip")
	c := forcePushConfig(3)
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Fatalf("landing=%v fix calls=%d, want landingFailed with none", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q", reason)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-failed", iss.Labels)
	}
}

func TestSelfHeal_PreflightRebaseForcePushRed_FixPassThenMerges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F, S, S, S, S)
	fc.MergeErrs = nil
	fc.SetNeedsUpdate(testPR, true)
	c := forcePushConfig(2)
	c.PreflightStaleBase = true
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want [1]", got)
	}
}

func TestSelfHeal_ConflictResolveForcePushRed_FixPassThenMerges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	fc := forcePushForge(S, S, F, S, S)
	fc.RebaseErr = forge.ErrMergeConflict
	s := newTestSettle(forcePushConfig(2), fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("ResolveConflict calls = %d, want 1", len(d.ResolveConflictCalls))
	}
	if got := fixPasses(d); len(got) != 1 {
		t.Errorf("fix passes = %v, want [1]", got)
	}
}

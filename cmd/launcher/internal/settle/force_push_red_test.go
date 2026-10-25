package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedFixture scripts a green PR whose first Merge conflicts and whose
// rebase force-push succeeds outright, so mergeImmediate goes straight to the
// post-force-push re-wait. states is the CheckState queue: the rewait's red is
// followed by selfHealGate's own re-poll of the same red head.
func forcePushRedFixture(maxFix int, states []forge.RollupState) (*Settle, *forge.Fake, *dispatch.Fake) {
	c := fixConfig(maxFix)
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc, dispatch.NewFake()
}

func commentBodies(fc *forge.Fake) string {
	var b strings.Builder
	for _, call := range fc.CommentCalls {
		b.WriteString(call.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// A red re-wait after a rebase force-push runs the normal fix pass instead of
// settling landing-failed; the fixed head then lands.
func TestSelfHeal_ForcePushRedRunsFixPass(t *testing.T) {
	s, fc, d := forcePushRedFixture(1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gate
		forge.StateFailure, forge.StateFailure, // rewait, re-poll
		forge.StateSuccess, forge.StateSuccess, // after the fix pass
	})

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want exactly [1]", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged = %q, want the fixed PR merged", fc.Merged)
	}
	if got := s.settledLatch["1"].reason; got != ReasonMerged {
		t.Errorf("settled reason = %q, want %q", got, ReasonMerged)
	}
}

// With MaxFixAttempts=0 no fix pass is allowed, so a red re-wait settles
// landing-failed and the comment names the counts.
func TestSelfHeal_ForcePushRedNoBudgetLandingFailed(t *testing.T) {
	s, fc, d := forcePushRedFixture(0, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess,
		forge.StateFailure, forge.StateFailure,
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if want := "exhausting 0 fix pass(es) (0 spent on the force-pushed head)"; !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want substring %q", reason, want)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
	}
	if n := len(fixPasses(d)); n != 0 {
		t.Errorf("fix passes = %d, want 0", n)
	}
	if c := commentBodies(fc); strings.Count(c, "landing failed:") != 1 || !strings.Contains(c, "0 spent on the force-pushed head") {
		t.Errorf("want exactly one landing-failed comment naming the counts; got %q", c)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
	}
}

// A pre-merge fix pass that already used the whole budget leaves none for the
// force-pushed head: no further pass, landing-failed, counts in the comment.
func TestSelfHeal_ForcePushRedBudgetAlreadySpent(t *testing.T) {
	s, fc, d := forcePushRedFixture(1, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red
		forge.StateSuccess, forge.StateSuccess, // after the pre-merge fix pass
		forge.StateFailure, forge.StateFailure, // force-pushed head: rewait, re-poll
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want only the pre-merge pass", passes)
	}
	if want := "exhausting 1 fix pass(es) (0 spent on the force-pushed head)"; !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want substring %q", reason, want)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
	}
	if c := commentBodies(fc); !strings.Contains(c, "landing failed: landing-failed: force-pushed head still red") {
		t.Errorf("comment missing landing-failed text; got %q", c)
	}
}

// The force-pushed head draws on the same MaxFixAttempts as the pre-merge
// phase: with 2 allowed and 1 spent pre-merge, exactly 1 more runs.
func TestSelfHeal_ForcePushRedSharesFixBudget(t *testing.T) {
	s, _, d := forcePushRedFixture(2, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red
		forge.StateSuccess, forge.StateSuccess, // after pass 1
		forge.StateFailure, forge.StateFailure, // force-pushed head: rewait, re-poll
		forge.StateFailure, // still red after pass 2
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 2 || passes[0] != 1 || passes[1] != 2 {
		t.Errorf("fix passes = %v, want [1 2] (one pre-merge, one force-push)", passes)
	}
	if want := "exhausting 2 fix pass(es) (1 spent on the force-pushed head)"; !strings.Contains(reason, want) {
		t.Errorf("reason = %q, want substring %q", reason, want)
	}
}

// A re-wait timeout and a failed conflict-resolve stay terminal landing-failed
// even with fix budget left.
func TestSelfHeal_ForcePushNonRedStaysTerminal(t *testing.T) {
	cases := []struct {
		name       string
		rebaseErr  error
		resolveErr error
		timeout    int
	}{
		{name: "rewait timeout", timeout: 0},
		{name: "conflict-resolve dispatch failure", rebaseErr: forge.ErrMergeConflict, resolveErr: errors.New("agent gave up"), timeout: 100},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, fc, d := forcePushRedFixture(2, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
			s.cfg.MergePollTimeout = tc.timeout
			fc.RebaseErr = tc.rebaseErr
			d.ResolveConflictErr = tc.resolveErr

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want substring %q", reason, errLandingNeverGreen.Error())
			}
			if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
				t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
			}
			if n := len(fixPasses(d)); n != 0 {
				t.Errorf("fix passes = %d, want 0", n)
			}
		})
	}
}

package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

func commentContains(fc *forge.Fake, sub string) bool {
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, sub) {
			return true
		}
	}
	return false
}

func forcePushSettle(maxFix int, states []forge.RollupState, rebaseErr error) (*Settle, *forge.Fake) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	c.MaxFixAttempts = maxFix
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = rebaseErr
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

// A red force-pushed head with MaxFixAttempts=0 spends no fix pass and
// settles landing-failed, naming zero consumed passes.
func TestSelfHeal_ForcePushedRed_ZeroBudget_NoFixPass(t *testing.T) {
	s, fc := forcePushSettle(0, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, forge.StateFailure,
	}, nil)
	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)
	if landing != landingFailed {
		t.Fatalf("landing = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix passes = %d, want 0", len(d.FixCalls))
	}
	if !strings.Contains(reason, "consumed 0 fix pass(es)") {
		t.Errorf("reason = %q, want it to name the consumed count", reason)
	}
	if !commentContains(fc, "consumed 0 fix pass(es)") {
		t.Errorf("issue comment = %+v, want it to name the consumed count", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-failed; labels=%v", iss.Labels)
	}
}

// A red post-force-push head gets a fix pass; once green it merges, whether
// the force-push was a clean rebase-retry, a conflict-resolve Box, or a
// stale-base preflight rebase.
func TestSelfHeal_ForcePushedRed_FixPassTurnsGreen_Merges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rebaseErr error
		preflight bool
	}{
		{"clean rebase retry", nil, false},
		{"conflict-resolve box", forge.ErrMergeConflict, false},
		{"stale-base preflight rebase", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fc := forcePushSettle(2, []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial green
				forge.StateFailure,                     // re-wait after rebase: red
				forge.StateSuccess, forge.StateSuccess, // after the fix pass
			}, tc.rebaseErr)
			if tc.preflight {
				s.cfg.PreflightStaleBase = true
				fc.MergeErrs = []error{nil}
				fc.SetNeedsUpdate(testPR, true)
			}
			fd := dispatch.NewFake()
			d := dispatch.Dispatcher(freshAfterFix{fd, fc})
			landing, _ := s.selfHeal(d, "1", 0, testPR)
			if landing != landingMerged {
				t.Fatalf("landing = %v, want landingMerged", landing)
			}
			if got := fixPasses(fd); len(got) != 1 || got[0] != 1 {
				t.Errorf("fix passes = %v, want [1]", got)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") {
				t.Errorf("want agent-complete; labels=%v", iss.Labels)
			}
		})
	}
}

// A head still red after its one fix pass exhausts the budget; the reason and
// the posted issue comment both name the consumed count.
func TestSelfHeal_ForcePushedRed_RedAfterFixPass_ExhaustsBudget(t *testing.T) {
	s, fc := forcePushSettle(1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial green
		forge.StateFailure, // re-wait after rebase: red -> fix pass 1
		forge.StateFailure, // still red, budget spent
	}, nil)
	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)
	if landing != landingFailed {
		t.Fatalf("landing = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("fix passes = %d, want exactly 1", len(d.FixCalls))
	}
	if !strings.Contains(reason, "consumed 1 fix pass(es) (1 of 1 used)") {
		t.Errorf("reason = %q, want the count", reason)
	}
	if !commentContains(fc, "consumed 1 fix pass(es) (1 of 1 used)") {
		t.Errorf("issue comment = %+v, want the count", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-failed; labels=%v", iss.Labels)
	}
}

// A fix pass spent on the initial red leaves none for the force-pushed head:
// the shared budget is exhausted with zero consumed by the force-push.
func TestSelfHeal_ForcePushedRed_SharesFixBudget(t *testing.T) {
	s, fc := forcePushSettle(1, []forge.RollupState{
		forge.StateFailure,                     // initial red -> fix pass 1
		forge.StateSuccess, forge.StateSuccess, // green, merge conflicts, rebase
		forge.StateFailure, // re-wait red, budget already spent
	}, nil)
	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)
	if landing != landingFailed {
		t.Fatalf("landing = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("fix passes = %d, want exactly 1", len(d.FixCalls))
	}
	if !strings.Contains(reason, "consumed 0 fix pass(es) (1 of 1 used)") {
		t.Errorf("reason = %q, want the count", reason)
	}
	if !commentContains(fc, "consumed 0 fix pass(es) (1 of 1 used)") {
		t.Errorf("issue comment = %+v, want the count", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-failed; labels=%v", iss.Labels)
	}
}

// A re-wait timeout (after a clean rebase-retry or a conflict-resolve) and a
// conflict-resolve dispatch or relay failure settle landing-failed with no fix
// pass, even with budget left.
func TestSelfHeal_ForcePushedTimeoutOrResolveFailure_NoFixPass(t *testing.T) {
	cases := []struct {
		name       string
		rebaseErr  error
		resolveErr error
		relayErr   error
		timeout    int
	}{
		{"rewait timeout after clean rebase retry", nil, nil, nil, 0},
		{"rewait timeout after conflict-resolve", forge.ErrMergeConflict, nil, nil, 0},
		{"conflict-resolve dispatch failure", forge.ErrMergeConflict, errors.New("box crashed"), nil, 100},
		{"relay failure after conflict-resolve", forge.ErrMergeConflict, nil, errors.New("relay down"), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fc := forcePushSettle(2, []forge.RollupState{forge.StateSuccess, forge.StateSuccess}, tc.rebaseErr)
			s.cfg.MergePollTimeout = tc.timeout
			if tc.relayErr != nil {
				fc.RelayBundleErr = tc.relayErr
				s.cfg.OutboxDir = func(string) string { return t.TempDir() }
			}
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr
			landing, _ := s.selfHeal(d, "1", 0, testPR)
			if landing != landingFailed {
				t.Fatalf("landing = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("fix passes = %d, want 0", len(d.FixCalls))
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

// freshAfterFix is a dispatcher whose fix pass also clears the PR's stale-base
// flag, as a real fix push would.
type freshAfterFix struct {
	*dispatch.Fake
	fc *forge.Fake
}

func (f freshAfterFix) Fix(pass int, summary string) dispatch.Disposition {
	f.fc.SetNeedsUpdate(testPR, false)
	return f.Fake.Fix(pass, summary)
}

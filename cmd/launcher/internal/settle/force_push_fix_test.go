package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedSetup scripts a first-green PR whose first Merge conflicts, so
// mergeImmediate rebases (rebaseErr nil: a clean force-push) and re-waits on
// the force-pushed head. preStates precede the first green; postStates follow
// the force-push.
func forcePushRedSetup(maxFix int, preStates, postStates []forge.RollupState) (*Settle, *forge.Fake, *dispatch.Fake) {
	c := fixConfig(maxFix)
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	states := append(append([]forge.RollupState{}, preStates...), forge.StateSuccess, forge.StateSuccess)
	fc.SetCheckStates(testPR, append(states, postStates...))
	return newTestSettle(c, fc, fc), fc, dispatch.NewFake()
}

func landingFailedComment(fc *forge.Fake) string {
	for _, c := range fc.CommentCalls {
		if strings.HasPrefix(c.Body, "landing failed:") {
			return c.Body
		}
	}
	return ""
}

// A force-pushed head whose re-wait goes red gets the red-CI fix pass; once
// the fix turns it green it re-enters the merge path and lands.
func TestSelfHeal_ForcePushedHeadRed_FixPassThenMerges(t *testing.T) {
	s, fc, d := forcePushRedSetup(2, nil, []forge.RollupState{
		forge.StateFailure,                     // re-wait after the rebase force-push
		forge.StateSuccess, forge.StateSuccess, // fix-pushed head
	})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-complete and no agent-failed", iss.Labels)
	}
}

// The force-pushed head draws from the same fix budget the pre-merge red loop
// uses: with it spent, the red head settles landing-failed with no further
// pass, and the comment names the attempts consumed.
func TestSelfHeal_ForcePushedHeadRed_BudgetExhaustedEndsLandingFailed(t *testing.T) {
	cases := []struct {
		name      string
		maxFix    int
		pre       []forge.RollupState
		wantFixes int
		wantUsed  string
	}{
		{name: "no fix budget", maxFix: 0, wantFixes: 0, wantUsed: "0 fix attempt(s) used"},
		{
			name:   "budget spent by the pre-merge red loop",
			maxFix: 1,
			// Pre-merge red consumes the single fix pass before first green.
			pre:       []forge.RollupState{forge.StateFailure},
			wantFixes: 1,
			wantUsed:  "1 fix attempt(s) used",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fc, d := forcePushRedSetup(tc.maxFix, tc.pre, []forge.RollupState{forge.StateFailure})

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
			}
			if len(d.FixCalls) != tc.wantFixes {
				t.Errorf("fix calls = %d, want %d", len(d.FixCalls), tc.wantFixes)
			}
			if body := landingFailedComment(fc); !strings.Contains(body, tc.wantUsed) {
				t.Errorf("landing-failed comment = %q, want it to name %q", body, tc.wantUsed)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
				t.Errorf("labels = %v, want agent-failed and no agent-complete", iss.Labels)
			}
		})
	}
}

// A re-wait timeout and a failed conflict-resolve dispatch or relay produce
// no red head to fix, so they settle landing-failed with no fix pass even
// while fix budget remains.
func TestSelfHeal_ForcePushNonRedFailures_NoFixPass(t *testing.T) {
	cases := []struct {
		name     string
		readOnly bool
		setup    func(c *Config, fc *forge.Fake, d *dispatch.Fake)
	}{
		{
			name: "re-wait timeout",
			setup: func(c *Config, _ *forge.Fake, _ *dispatch.Fake) {
				c.MergePollTimeout = 0
			},
		},
		{
			name: "conflict-resolve dispatch failure",
			setup: func(_ *Config, fc *forge.Fake, d *dispatch.Fake) {
				fc.RebaseErr = forge.ErrMergeConflict
				d.ResolveConflictErr = errors.New("box exited 1")
			},
		},
		{
			name:     "relay after conflict-resolve failure",
			readOnly: true,
			setup: func(c *Config, fc *forge.Fake, _ *dispatch.Fake) {
				fc.RebaseErr = forge.ErrMergeConflict
				c.OutboxDir = func(num string) string { return "/outbox/" + num }
				fc.RelayBundleErr = errors.New("relay: bundle rejected")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MaxRebaseAttempts = 3
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
			d := dispatch.NewFake()
			tc.setup(&c, fc, d)
			var cf forge.CodeForge = fc
			if tc.readOnly {
				cf = fc.AsGithubReadOnly()
			}
			s := newTestSettle(c, fc, cf)

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("fix calls = %+v, want none", d.FixCalls)
			}
			if landingFailedComment(fc) == "" {
				t.Errorf("no landing-failed comment; comments=%+v", fc.CommentCalls)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("labels = %v, want agent-failed", iss.Labels)
			}
		})
	}
}

// mergeImmediate surfaces a red re-wait as errForcePushedHeadRed, distinct
// from errLandingNeverGreen, so selfHealGate can route it to the fix pass.
func TestMergeImmediate_RedRewaitIsForcePushedHeadRed(t *testing.T) {
	c := baseConfig()
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure})
	s := newTestSettle(c, fc, fc)

	err := s.mergeImmediate("1", 0, testPR, dispatch.NewFake())

	if !errors.Is(err, errForcePushedHeadRed) || errors.Is(err, errLandingNeverGreen) {
		t.Errorf("mergeImmediate = %v, want errForcePushedHeadRed and not errLandingNeverGreen", err)
	}
}

// A fix-pushed head that conflicts again re-enters mergeImmediate with a fresh
// rebase budget, as a first-green head would: a single rebase attempt per
// entry still lands after one fix pass.
func TestSelfHeal_ForcePushedHeadRed_FixedHeadGetsFreshRebaseBudget(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 1
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict, nil}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first green
		forge.StateFailure,                     // re-wait after the first force-push
		forge.StateSuccess, forge.StateSuccess, // fix-pushed head
		forge.StateSuccess, forge.StateSuccess, // re-wait after the second force-push
	})
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want one", passes)
	}
}

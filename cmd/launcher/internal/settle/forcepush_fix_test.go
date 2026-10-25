package settle

import (
	"errors"
	"strings"
	"testing"
	"time"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
	"spindrift.dev/launcher/internal/retry"
)

// forcePushRig builds a settle config in immediate merge mode with a rebase
// allowance, plus the forge and dispatcher fakes. Callers script the merge
// conflict and the CI states.
func forcePushRig(maxFix int) (Config, *forge.Fake, *dispatch.Fake) {
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	return c, fc, dispatch.NewFake()
}

// A force-pushed head that goes red is fixed from the same budget and then
// re-enters the merge from scratch, whichever path force-pushed it.
func TestSelfHeal_ForcePushedHeadRed_FixPassThenMerges(t *testing.T) {
	ok, red := forge.StateSuccess, forge.StateFailure
	cases := []struct {
		name  string
		setup func(c *Config, fc *forge.Fake)
		// ranPath fails the test unless the force-push path named above ran.
		ranPath func(t *testing.T, fc *forge.Fake, d *dispatch.Fake)
	}{
		{
			name: "plain rebase-retry",
			ranPath: func(t *testing.T, fc *forge.Fake, d *dispatch.Fake) {
				if len(fc.RebasedURLs) == 0 || len(d.ResolveConflictCalls) != 0 {
					t.Errorf("rebases = %v, conflict-resolve calls = %v, want a rebase and no conflict-resolve Box", fc.RebasedURLs, d.ResolveConflictCalls)
				}
			},
			setup: func(c *Config, fc *forge.Fake) {
				fc.MergeErrs = []error{forge.ErrMergeConflict}
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok, red, ok, ok})
			},
		},
		{
			name: "conflict-resolve",
			ranPath: func(t *testing.T, fc *forge.Fake, d *dispatch.Fake) {
				if len(d.ResolveConflictCalls) == 0 {
					t.Error("want a conflict-resolve Box dispatched")
				}
			},
			setup: func(c *Config, fc *forge.Fake) {
				fc.MergeErrs = []error{forge.ErrMergeConflict}
				fc.RebaseErr = forge.ErrMergeConflict
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok, red, ok, ok})
			},
		},
		{
			name: "stale-base preflight",
			// No Merge conflict is scripted, so a rebase can only be the preflight's.
			ranPath: func(t *testing.T, fc *forge.Fake, d *dispatch.Fake) {
				if len(fc.RebasedURLs) == 0 {
					t.Error("want the preflight to rebase the stale branch")
				}
			},
			setup: func(c *Config, fc *forge.Fake) {
				c.PreflightStaleBase = true
				// The fake stays stale, so the re-entered merge preflights again.
				fc.SetNeedsUpdate(testPR, true)
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok, red, ok, ok, ok, ok})
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, fc, d := forcePushRig(2)
			tc.setup(&c, fc)
			s := newTestSettle(c, fc, fc)

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
			}
			tc.ranPath(t, fc, d)
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("fix passes = %v, want [1]", passes)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-complete and not agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

// With no fix budget left a red force-pushed head settles landing-failed, and
// the comment says how many fix attempts were used. The shared case proves the
// budget is not a fresh allowance per force-push.
func TestSelfHeal_ForcePushedHeadRed_BudgetSpent_EndsFailed(t *testing.T) {
	ok, red := forge.StateSuccess, forge.StateFailure
	cases := []struct {
		name       string
		maxFix     int
		states     []forge.RollupState
		wantPasses int
		wantNote   string
	}{
		{
			name:       "no fix budget",
			maxFix:     0,
			states:     []forge.RollupState{ok, ok, red},
			wantPasses: 0,
			wantNote:   "fix attempts used, shared with the pre-merge loop: 0 of 0",
		},
		{
			name:       "pre-merge red already spent the one pass",
			maxFix:     1,
			states:     []forge.RollupState{red, ok, ok, red},
			wantPasses: 1,
			wantNote:   "fix attempts used, shared with the pre-merge loop: 1 of 1",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, fc, d := forcePushRig(tc.maxFix)
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.SetCheckStates(testPR, tc.states)
			s := newTestSettle(c, fc, fc)

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want %q", reason, errLandingNeverGreen.Error())
			}
			if got := len(fixPasses(d)); got != tc.wantPasses {
				t.Errorf("fix passes = %d, want %d", got, tc.wantPasses)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
				t.Errorf("want agent-failed and not agent-complete; labels=%v", iss.Labels)
			}
			comments := issueComments(fc)
			if len(comments) != 1 || !strings.Contains(comments[0], tc.wantNote) {
				t.Errorf("comments = %q, want one naming %q", comments, tc.wantNote)
			}
		})
	}
}

// A force-pushed head that goes red with budget left is fixed once; when the
// fix head is still red the budget is spent, so it settles through the red-CI
// loop's fix-exhausted arm, as a pre-merge red would, rather than landing-failed.
func TestSelfHeal_ForcePushedHeadRed_FixHeadStillRed_FixExhausted(t *testing.T) {
	ok, red := forge.StateSuccess, forge.StateFailure
	c, fc, d := forcePushRig(1)
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{ok, ok, red, red})
	s := newTestSettle(c, fc, fc)

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
	}
	if want := "ci-red: still red after exhausting 1 fix pass(es)"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-failed and not agent-complete; labels=%v", iss.Labels)
	}
}

// A timeout or a failed conflict-resolve is not a red head: no fix pass runs
// even with budget left.
func TestSelfHeal_ForcePushNeverGreenNotRed_NoFixPass(t *testing.T) {
	ok := forge.StateSuccess
	cases := []struct {
		name     string
		readOnly bool
		setup    func(c *Config, fc *forge.Fake, d *dispatch.Fake)
	}{
		{
			name: "re-wait times out",
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				c.MergePollTimeout = 0
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok})
			},
		},
		{
			name: "conflict-resolve dispatch fails",
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				fc.RebaseErr = forge.ErrMergeConflict
				d.ResolveConflictErr = errors.New("conflict-resolve box exited 1")
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok})
			},
		},
		{
			name:     "conflict-resolve relay fails",
			readOnly: true,
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				c.Policy = retry.Policy{Max: 1, Unit: time.Millisecond}
				c.OutboxDir = func(num string) string { return "/outbox/" + num }
				fc.RebaseErr = forge.ErrMergeConflict
				fc.RelayBundleErr = errors.New("ssh auth blip")
				fc.SetCheckStates(testPR, []forge.RollupState{ok, ok})
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, fc, d := forcePushRig(3)
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			c.MergePollTimeout = 100
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
				t.Errorf("reason = %q, want %q", reason, errLandingNeverGreen.Error())
			}
			if passes := fixPasses(d); len(passes) != 0 {
				t.Errorf("fix passes = %v, want none", passes)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

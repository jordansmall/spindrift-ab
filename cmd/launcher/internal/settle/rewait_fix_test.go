package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// rewaitFixSettle builds an immediate-mode Settle whose first merge conflicts
// mergeConflicts times, each resolved by a clean rebase force-push.
func rewaitFixSettle(maxFix, mergeConflicts int, states []forge.RollupState) (*Settle, *forge.Fake) {
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	for i := 0; i < mergeConflicts; i++ {
		fc.MergeErrs = append(fc.MergeErrs, forge.ErrMergeConflict)
	}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

// A red post-force-push re-wait dispatches the red-CI fix pass; a head it turns
// green re-enters the merge path like a first-green head.
func TestSelfHeal_RewaitRed_FixPassGreen_Merges(t *testing.T) {
	s, fc := rewaitFixSettle(3, 1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gate
		forge.StateFailure,                     // re-wait after the rebase force-push
		forge.StateSuccess, forge.StateSuccess, // after the fix pass
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("labels = %v, want agent-complete and not agent-failed", iss.Labels)
	}
}

// A second red re-wait after the force-pushed head already spent the whole fix
// budget settles landing-failed without another pass, and says so.
func TestSelfHeal_RewaitRed_BudgetExhausted_EndsFailed(t *testing.T) {
	s, fc := rewaitFixSettle(1, 2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gate
		forge.StateFailure,                     // first re-wait: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1
		forge.StateFailure, // second re-wait: red, budget spent
	})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly one", passes)
	}
	assertLatchedLandingFailed(t, s, fc, "1", "consumed 1 fix attempt(s), 1 of 1 used")
}

// A force-pushed head still red after its fix passes settles landing-failed,
// not fix-exhausted, and names the passes that lineage consumed.
func TestSelfHeal_RewaitRed_FixPassStillRed_EndsLandingFailed(t *testing.T) {
	cases := []struct {
		name    string
		maxFix  int
		states  []forge.RollupState
		comment string
	}{
		{
			name:   "one pass",
			maxFix: 1,
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gate
				forge.StateFailure, // re-wait after the rebase force-push
				forge.StateFailure, // after fix pass 1
			},
			comment: "consumed 1 fix attempt(s), 1 of 1 used",
		},
		{
			name:   "two passes",
			maxFix: 2,
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gate
				forge.StateFailure, // re-wait after the rebase force-push
				forge.StateFailure, // after fix pass 1
				forge.StateFailure, // after fix pass 2
			},
			comment: "consumed 2 fix attempt(s), 2 of 2 used",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, fc := rewaitFixSettle(tc.maxFix, 1, tc.states)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
			}
			if passes := fixPasses(d); len(passes) != tc.maxFix {
				t.Errorf("fix passes = %v, want %d", passes, tc.maxFix)
			}
			assertLatchedLandingFailed(t, s, fc, "1", tc.comment)
		})
	}
}

// The re-wait fix pass shares the pre-merge red loop's budget: a pass spent on
// the first red leaves none for the force-pushed head.
func TestSelfHeal_RewaitRed_SharesPreMergeFixBudget(t *testing.T) {
	s, fc := rewaitFixSettle(1, 1, []forge.RollupState{
		forge.StateFailure,                     // initial gate: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1
		forge.StateFailure, // re-wait after the rebase force-push
	})
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want only the pre-merge [1]", passes)
	}
	assertLatchedLandingFailed(t, s, fc, "1", "consumed 0 fix attempt(s), 1 of 1 used")
}

// Only a genuinely red re-wait earns a fix pass: a re-wait timeout or a failed
// conflict-resolve dispatch or relay settles landing-failed with budget to spare.
func TestSelfHeal_ForcePushFailureWithoutRed_SkipsFixPass(t *testing.T) {
	cases := []struct {
		name        string
		pollTimeout int
		resolveErr  error
		relayErr    error
	}{
		{name: "re-wait times out", pollTimeout: 0},
		{name: "conflict-resolve dispatch fails", pollTimeout: 100, resolveErr: errors.New("agent gave up")},
		{name: "conflict-resolve relay fails", pollTimeout: 100, relayErr: errors.New("bundle missing")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.MergePollTimeout = tc.pollTimeout
			c.OutboxDir = func(num string) string { return "/outbox/" + num }
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.RebaseErr = forge.ErrMergeConflict
			fc.RelayBundleErr = tc.relayErr
			fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
			var cf forge.CodeForge = fc
			if tc.relayErr != nil {
				cf = fc.AsGithubReadOnly()
			}
			s := newTestSettle(c, fc, cf)
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, reason)
			}
			if passes := fixPasses(d); len(passes) != 0 {
				t.Errorf("fix passes = %v, want none", passes)
			}
			assertLatchedLandingFailed(t, s, fc, "1", "consumed 0 fix attempt(s), 0 of 3 used")
		})
	}
}

// A red re-wait earns the fix pass whichever force-push preceded it: a
// conflict-resolve Box (the #5053 incident) or the stale-base preflight's own
// rebase, not only the clean rebase-retry rewaitFixSettle scripts.
func TestSelfHeal_ConflictResolveRewaitRed_FixPassGreen_Merges(t *testing.T) {
	cases := []struct {
		name      string
		staleBase bool
		mergeErrs []error
		rebaseErr error
	}{
		{name: "conflict-resolve after merge conflict", mergeErrs: []error{forge.ErrMergeConflict}, rebaseErr: forge.ErrMergeConflict},
		{name: "stale-base preflight rebase", staleBase: true},
		{name: "stale-base preflight conflict-resolve", staleBase: true, rebaseErr: forge.ErrMergeConflict},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.PreflightStaleBase = tc.staleBase
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = tc.mergeErrs
			fc.RebaseErr = tc.rebaseErr
			fc.SetNeedsUpdate(testPR, tc.staleBase)
			// The fake's staleness never clears, so a stale-base run preflights
			// again after the fix pass; the trailing greens cover that re-wait.
			fc.SetCheckStates(testPR, []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gate
				forge.StateFailure,                     // re-wait after the force-push
				forge.StateSuccess, forge.StateSuccess, // after the fix pass
				forge.StateSuccess, forge.StateSuccess, // any second preflight re-wait
			})
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
			}
			if tc.rebaseErr != nil && len(d.ResolveConflictCalls) == 0 {
				t.Errorf("ResolveConflict never called; the force-push must come from the conflict-resolve Box")
			}
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("fix passes = %v, want [1]", passes)
			}
			if fc.Merged != testPR {
				t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("labels = %v, want agent-complete and not agent-failed", iss.Labels)
			}
		})
	}
}

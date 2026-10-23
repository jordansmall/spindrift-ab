package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// stS and stF shorten the scripted CheckState queues below.
const (
	stS = forge.StateSuccess
	stF = forge.StateFailure
)

// A red post-force-push re-wait dispatches the red-CI fix pass instead of
// settling Failed; a fixed head re-enters the merge path and lands.
func TestSelfHeal_ForcePushRed_FixPassThenMerges(t *testing.T) {
	cases := []struct {
		name        string
		mergeErrs   []error
		rebaseErr   error
		preflight   bool
		checkStates []forge.RollupState
	}{
		{
			name:        "rebase-retry",
			mergeErrs:   []error{forge.ErrMergeConflict},
			checkStates: []forge.RollupState{stS, stS, stF, stS, stS},
		},
		{
			name:        "conflict-resolve",
			mergeErrs:   []error{forge.ErrMergeConflict},
			rebaseErr:   forge.ErrMergeConflict,
			checkStates: []forge.RollupState{stS, stS, stF, stS, stS},
		},
		{
			// NeedsUpdate stays set, so the post-fix pass preflights and
			// re-waits again.
			name:        "preflight-stale-base",
			preflight:   true,
			checkStates: []forge.RollupState{stS, stS, stF, stS, stS, stS, stS},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.PreflightStaleBase = tc.preflight
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.SetNeedsUpdate(testPR, tc.preflight)
			fc.MergeErrs = tc.mergeErrs
			fc.RebaseErr = tc.rebaseErr
			fc.SetCheckStates(testPR, tc.checkStates)
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, note := s.selfHeal(d, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = (%v, %q), want landingMerged", landing, note)
			}
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("fix passes = %v, want [1]", passes)
			}
			if fc.Merged != testPR {
				t.Errorf("Merged = %q, want %q", fc.Merged, testPR)
			}
			last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]
			if last.To != forge.Complete {
				t.Errorf("last transition To=%v, want Complete", last.To)
			}
		})
	}
}

// With the fix budget spent — none allowed, or consumed by a pre-merge fix
// pass — a red re-wait settles landing-failed naming the attempts used. The
// second row proves the budget is shared rather than fresh per force-push.
func TestSelfHeal_ForcePushRed_BudgetExhausted_EndsLandingFailed(t *testing.T) {
	cases := []struct {
		name        string
		maxFix      int
		checkStates []forge.RollupState
		wantPasses  int
		wantUsed    string
	}{
		{name: "no budget", maxFix: 0, checkStates: []forge.RollupState{stS, stS, stF}, wantPasses: 0, wantUsed: "0 of 0"},
		{name: "spent pre-merge", maxFix: 1, checkStates: []forge.RollupState{stF, stS, stS, stF}, wantPasses: 1, wantUsed: "1 of 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(tc.maxFix)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.SetCheckStates(testPR, tc.checkStates)
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, note := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = (%v, %q), want landingFailed", landing, note)
			}
			if !strings.Contains(note, errForcePushRed.Error()) || !strings.Contains(note, tc.wantUsed) {
				t.Errorf("note = %q, want errForcePushRed naming %q", note, tc.wantUsed)
			}
			if got := len(d.FixCalls); got != tc.wantPasses {
				t.Errorf("fix passes = %d, want %d", got, tc.wantPasses)
			}
			assertLandingFailed(t, s, fc, tc.wantUsed)
		})
	}
}

// A re-wait timeout and a failed conflict-resolve dispatch or relay still
// settle landing-failed with no fix pass, even with budget left.
func TestSelfHeal_ForcePushNotRed_NoFixPass(t *testing.T) {
	cases := []struct {
		name        string
		rebaseErr   error
		resolveErr  error
		relayErr    error
		pollTimeout int
	}{
		{name: "re-wait timeout", pollTimeout: 0},
		{name: "conflict-resolve dispatch", rebaseErr: forge.ErrMergeConflict, resolveErr: errors.New("box crashed"), pollTimeout: 100},
		{name: "conflict-resolve relay", rebaseErr: forge.ErrMergeConflict, relayErr: errors.New("bad bundle"), pollTimeout: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.MergePollTimeout = tc.pollTimeout
			c.OutboxDir = func(num string) string { return "/outbox/" + num }
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.RebaseErr = tc.rebaseErr
			fc.RelayBundleErr = tc.relayErr
			fc.SetCheckStates(testPR, []forge.RollupState{stS, stS})
			s := newTestSettle(c, fc, fc.AsGithubReadOnly())
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			var landing landingResult
			var note string
			captureStdout(t, func() { landing, note = s.selfHeal(d, "1", 0, testPR) })

			if landing != landingFailed || !strings.Contains(note, errLandingNeverGreen.Error()) {
				t.Fatalf("selfHeal = (%v, %q), want landingFailed wrapping errLandingNeverGreen", landing, note)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("fix passes = %v, want none", fixPasses(d))
			}
			assertLandingFailed(t, s, fc, "")
		})
	}
}

func assertLandingFailed(t *testing.T, s *Settle, fc *forge.Fake, commentWant string) {
	t.Helper()
	last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]
	if r := s.settledLatch["1"].reason; last.To != forge.Failed || r != ReasonLandingFailed {
		t.Errorf("last transition = %+v reason=%q, want Failed/%s", last, s.settledLatch["1"].reason, ReasonLandingFailed)
	}
	if len(fc.CommentCalls) == 0 || !strings.Contains(fc.CommentCalls[len(fc.CommentCalls)-1].Body, commentWant) {
		t.Errorf("comments = %+v, want last naming %q", fc.CommentCalls, commentWant)
	}
}

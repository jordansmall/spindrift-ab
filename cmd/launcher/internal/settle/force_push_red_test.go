package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// A red re-wait after a force-push draws on the same fix budget as a
// pre-merge red: the fix pass runs, and a head it turns green merges.
func TestSelfHeal_RewaitRedAfterForcePush_FixPassThenMerges(t *testing.T) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // rewait after force-push: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged after the fix pass turns the force-pushed head green", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// A red re-wait draws on the shared MaxFixAttempts budget. Once it is spent the
// issue settles Failed with ReasonLandingFailed, and the note names how many fix
// passes the force-pushed head consumed.
func TestSelfHeal_RewaitRedAfterForcePush_SharesFixBudget(t *testing.T) {
	cases := []struct {
		name        string
		maxFix      int
		checkStates []forge.RollupState
		wantPasses  []int
		wantNote    string
	}{
		{
			name:   "force-pushed head spends the whole budget",
			maxFix: 2,
			checkStates: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure, // rewait after force-push: red
				forge.StateFailure, // after fix pass 1
				forge.StateFailure, // after fix pass 2
			},
			wantPasses: []int{1, 2},
			wantNote:   "consumed 2 fix pass(es)",
		},
		{
			name:   "pre-merge red already spent the shared budget",
			maxFix: 1,
			checkStates: []forge.RollupState{
				forge.StateFailure,                     // initial gateToGreen: genuine red
				forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green
				forge.StateFailure, // rewait after force-push: no budget left
			},
			wantPasses: []int{1},
			wantNote:   "consumed 0 fix pass(es)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.MaxFixAttempts = tc.maxFix
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.SetCheckStates(testPR, tc.checkStates)
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed once the shared fix budget is spent", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
			}
			if !strings.Contains(reason, tc.wantNote) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, tc.wantNote)
			}
			passes := fixPasses(d)
			if len(passes) != len(tc.wantPasses) {
				t.Fatalf("fix passes = %v, want %v", passes, tc.wantPasses)
			}
			for i := range passes {
				if passes[i] != tc.wantPasses[i] {
					t.Fatalf("fix passes = %v, want %v", passes, tc.wantPasses)
				}
			}
			if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
				t.Errorf("last transition = %+v, want Failed", last)
			}
			if r := s.settledLatch["1"].reason; r != ReasonLandingFailed {
				t.Errorf("settled reason = %q, want %q", r, ReasonLandingFailed)
			}
			if n := len(fc.CommentCalls); n == 0 || !strings.Contains(fc.CommentCalls[n-1].Body, tc.wantNote) {
				t.Errorf("last comment = %+v, want one naming %q", fc.CommentCalls, tc.wantNote)
			}
		})
	}
}

// A force-push that goes red, is fixed green, and is force-pushed again going
// red counts fix passes from the first red: the second force-push does not
// reset the count.
func TestSelfHeal_RewaitRedAfterForcePush_CountsFromFirstForcePush(t *testing.T) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = 2
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // first force-push rewait: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green
		forge.StateFailure, // second force-push rewait: red
		forge.StateFailure, // after fix pass 2: still red
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (reason %q), want landingFailed once the budget is spent", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 2 || passes[0] != 1 || passes[1] != 2 {
		t.Errorf("fix passes = %v, want [1 2]", passes)
	}
	if !strings.Contains(reason, "consumed 2 fix pass(es)") {
		t.Errorf("selfHeal reason = %q, want the cumulative count of 2 from the first force-push", reason)
	}
	if r := s.settledLatch["1"].reason; r != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", r, ReasonLandingFailed)
	}
}

// A stale-base preflight or a reactive conflict-resolve that succeeds, then
// force-pushes a head going red, takes a fix pass and merges once green.
func TestSelfHeal_ResolveRewaitRedAfterForcePush_FixPassThenMerges(t *testing.T) {
	cases := []struct {
		name        string
		preflight   bool
		rebaseErr   error
		rebaseErrs  []error
		mergeErrs   []error
		checkStates []forge.RollupState
	}{
		{
			name:      "reactive merge conflict resolved by a Box",
			rebaseErr: forge.ErrMergeConflict,
			mergeErrs: []error{forge.ErrMergeConflict, nil},
			checkStates: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure,                     // conflict-resolve rewait: red
				forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green
			},
		},
		{
			name:       "stale-base preflight resolved by a Box",
			preflight:  true,
			rebaseErrs: []error{forge.ErrMergeConflict, nil},
			mergeErrs:  []error{nil},
			checkStates: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure,                     // preflight conflict-resolve rewait: red
				forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green (post-fix gateToGreen poll)
				forge.StateSuccess, forge.StateSuccess, // second preflight rebase's re-wait: green
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.MaxFixAttempts = 3
			c.PreflightStaleBase = tc.preflight
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.SetNeedsUpdate(testPR, tc.preflight)
			fc.RebaseErr = tc.rebaseErr
			fc.RebaseErrs = tc.rebaseErrs
			fc.MergeErrs = tc.mergeErrs
			fc.SetCheckStates(testPR, tc.checkStates)
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (reason %q), want landingMerged after the fix pass turns the head green", landing, reason)
			}
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("fix passes = %v, want [1]", passes)
			}
			if len(d.ResolveConflictCalls) != 1 {
				t.Errorf("ResolveConflict called %d times, want 1", len(d.ResolveConflictCalls))
			}
			if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
				t.Errorf("last transition To=%v, want Complete", last.To)
			}
		})
	}
}

// A conflict-resolve that fails to dispatch or to relay its bundle is a
// landing that never went green, not a red head: it settles Failed with
// ReasonLandingFailed and spends no fix pass.
func TestSelfHeal_ResolveFailureAfterForcePush_NoFixPass(t *testing.T) {
	cases := []struct {
		name       string
		preflight  bool
		resolveErr error
		relayErr   error
	}{
		{name: "reactive conflict-resolve dispatch fails", resolveErr: errors.New("agent could not resolve conflict")},
		{name: "stale-base preflight conflict-resolve dispatch fails", preflight: true, resolveErr: errors.New("agent could not resolve conflict")},
		{name: "reactive conflict-resolve bundle relay fails", relayErr: errors.New("ssh auth blip")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.MaxFixAttempts = 3
			c.PreflightStaleBase = tc.preflight
			c.OutboxDir = func(num string) string { return "/outbox/" + num }
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.SetNeedsUpdate(testPR, tc.preflight)
			fc.RebaseErr = forge.ErrMergeConflict
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.RelayBundleErr = tc.relayErr
			fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
			s := newTestSettle(c, fc, fc)
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
			}
			if passes := fixPasses(d); len(passes) != 0 {
				t.Errorf("fix passes = %v, want none", passes)
			}
			if r := s.settledLatch["1"].reason; r != ReasonLandingFailed {
				t.Errorf("settled reason = %q, want %q", r, ReasonLandingFailed)
			}
		})
	}
}

// A post-force-push re-wait that times out is gateTerminal, not a red head, so
// it settles Failed without spending a fix pass even with budget left.
func TestSelfHeal_RewaitTimeoutAfterForcePush_NoFixPass(t *testing.T) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = 3
	c.MergePollTimeout = 0
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
	}
	if passes := fixPasses(d); len(passes) != 0 {
		t.Errorf("a timed-out re-wait must not run a fix pass; got %v", passes)
	}
}

// A stale-base preflight whose rebase is clean (no conflict, so no Box) still
// force-pushes, and its re-wait going red takes a fix pass and merges once green.
func TestSelfHeal_CleanPreflightRewaitRedAfterForcePush_FixPassThenMerges(t *testing.T) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = 3
	c.PreflightStaleBase = true
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetNeedsUpdate(testPR, true)
	fc.MergeErrs = []error{nil}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // preflight rebase's re-wait: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green (post-fix gateToGreen poll)
		forge.StateSuccess, forge.StateSuccess, // second preflight rebase's re-wait: green
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (reason %q), want landingMerged after the fix pass turns the head green", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("fix passes = %v, want [1]", passes)
	}
	if n := len(d.ResolveConflictCalls); n != 0 {
		t.Errorf("ResolveConflict called %d times, want 0 for a clean rebase", n)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

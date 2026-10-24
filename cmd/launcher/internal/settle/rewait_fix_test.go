package settle

import (
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

func rewaitFixSettle(maxFix int, states []forge.RollupState) (*Settle, *forge.Fake) {
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

func assertMergedAfterOneFix(t *testing.T, landing landingResult, d *dispatch.Fake, fc *forge.Fake) {
	t.Helper()
	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged after a fix pass on the red force-pushed head", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("want exactly fix-pass-1, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merged = %q, want %q", fc.Merged, testPR)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-complete and not agent-failed; labels=%v", iss.Labels)
	}
}

// A red post-force-push re-wait gets a red-CI fix pass instead of settling
// Failed, and the head it turns green lands like a first-green head.
func TestSelfHeal_RewaitRedAfterPlainRebase_FixPassThenMerges(t *testing.T) {
	s, fc := rewaitFixSettle(3, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gate
		forge.StateFailure,                     // rewait after rebase: red
		forge.StateSuccess, forge.StateSuccess, // gate after fix pass
	})
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	assertMergedAfterOneFix(t, landing, d, fc)
}

func TestSelfHeal_RewaitRedAfterConflictResolve_FixPassThenMerges(t *testing.T) {
	s, fc := rewaitFixSettle(3, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess,
		forge.StateFailure,
		forge.StateSuccess, forge.StateSuccess,
	})
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	assertMergedAfterOneFix(t, landing, d, fc)
}

func TestSelfHeal_RewaitRedAfterStaleBasePreflight_FixPassThenMerges(t *testing.T) {
	s, fc := rewaitFixSettle(3, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess,
		forge.StateFailure, // preflight rewait: red
		forge.StateSuccess, forge.StateSuccess,
		forge.StateSuccess, forge.StateSuccess, // second preflight rewait
	})
	s.cfg.PreflightStaleBase = true
	fc.MergeErrs = nil
	fc.SetNeedsUpdate(testPR, true)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	assertMergedAfterOneFix(t, landing, d, fc)
}

// The post-force-push fix pass draws from selfHealGate's single attempt
// budget: with MaxFixAttempts spent (by pre-merge red, or set to 0) a red
// re-wait settles Failed without another fix pass.
func TestSelfHeal_RewaitRed_SharesFixBudget_ExhaustedEndsFailed(t *testing.T) {
	cases := []struct {
		name     string
		maxFix   int
		states   []forge.RollupState
		wantFix  int
		wantNote string
	}{
		{
			name:     "budget zero",
			maxFix:   0,
			states:   []forge.RollupState{forge.StateSuccess, forge.StateSuccess, forge.StateFailure},
			wantFix:  0,
			wantNote: "after 0 fix attempt(s)",
		},
		{
			name:   "budget spent by pre-merge red",
			maxFix: 1,
			states: []forge.RollupState{
				forge.StateFailure,                     // pre-merge red: fix pass 1
				forge.StateSuccess, forge.StateSuccess, // green after fix
				forge.StateFailure, // rewait after rebase: red, budget spent
			},
			wantFix:  1,
			wantNote: "after 1 fix attempt(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fc := rewaitFixSettle(tc.maxFix, tc.states)
			d := dispatch.NewFake()

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("reason = %q, want it to contain %q", reason, errLandingNeverGreen.Error())
			}
			if len(d.FixCalls) != tc.wantFix {
				t.Errorf("FixCalls = %d, want %d", len(d.FixCalls), tc.wantFix)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-failed; labels=%v", iss.Labels)
			}
			found := false
			for _, c := range fc.CommentCalls {
				if strings.Contains(c.Body, "landing failed:") && strings.Contains(c.Body, tc.wantNote) {
					found = true
				}
			}
			if !found {
				t.Errorf("want a landing-failed comment naming %q; got %+v", tc.wantNote, fc.CommentCalls)
			}
		})
	}
}

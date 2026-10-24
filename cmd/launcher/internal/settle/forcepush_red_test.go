package settle

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

type forcePushRedCase struct {
	name        string
	rebaseErr   error
	preflight   bool
	maxFix      int
	pollTimeout int
	states      []forge.RollupState
	resolveErr  error
	headErr     error
}

func runForcePushRed(t *testing.T, tc forcePushRedCase) (landingResult, string, *forge.Fake, *dispatch.Fake) {
	t.Helper()
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = tc.maxFix
	c.MergePollTimeout = tc.pollTimeout
	c.PreflightStaleBase = tc.preflight
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	if tc.preflight {
		fc.SetNeedsUpdate(testPR, true)
		fc.MergeErrs = []error{nil}
	} else {
		fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	}
	fc.RebaseErr = tc.rebaseErr
	fc.HeadCommitSHAErr = tc.headErr
	fc.SetCheckStates(testPR, tc.states)
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()
	d.ResolveConflictErr = tc.resolveErr
	landing, reason := s.selfHeal(d, "1", 0, testPR)
	return landing, reason, fc, d
}

var (
	okS  = forge.StateSuccess
	redS = forge.StateFailure
)

func TestSelfHeal_ForcePushRed_FixPassThenMerges(t *testing.T) {
	cases := []forcePushRedCase{
		{name: "rebase-retry", maxFix: 1, pollTimeout: 100,
			states: []forge.RollupState{okS, okS, redS, redS, okS, okS}},
		{name: "conflict-resolve", rebaseErr: forge.ErrMergeConflict, maxFix: 1, pollTimeout: 100,
			states: []forge.RollupState{okS, okS, redS, redS, okS, okS}},
		{name: "preflight", preflight: true, maxFix: 1, pollTimeout: 100,
			states: []forge.RollupState{okS, okS, redS, redS, okS, okS, okS, okS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			landing, reason, fc, d := runForcePushRed(t, tc)
			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
			}
			if len(d.FixCalls) != 1 {
				t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
			}
			if fc.Merged != testPR {
				t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") {
				t.Errorf("labels=%v, want agent-complete", iss.Labels)
			}
		})
	}
}

func TestSelfHeal_ForcePushRed_BudgetExhausted_Fails(t *testing.T) {
	cases := []forcePushRedCase{
		{name: "no budget", maxFix: 0, pollTimeout: 100,
			states: []forge.RollupState{okS, okS, redS}},
		// The pre-merge red consumed the only pass, so the post-force-push red
		// has none left: the budget is shared.
		{name: "pre-merge red consumed budget", maxFix: 1, pollTimeout: 100,
			states: []forge.RollupState{redS, okS, okS, redS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			landing, _, fc, d := runForcePushRed(t, tc)
			if landing != landingFailed {
				t.Fatalf("selfHeal = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != tc.maxFix {
				t.Errorf("FixCalls = %d, want %d", len(d.FixCalls), tc.maxFix)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("labels=%v, want agent-failed", iss.Labels)
			}
			want := fmt.Sprintf("(%d of %d fix pass(es) used)", tc.maxFix, tc.maxFix)
			found := false
			for _, cm := range fc.CommentCalls {
				if strings.Contains(cm.Body, want) {
					found = true
				}
			}
			if !found {
				t.Errorf("no comment containing %q; comments=%+v", want, fc.CommentCalls)
			}
		})
	}
}

// A HeadCommitSHA failure is best-effort for the no-op check, but must still
// spend the fix budget, or a persistently red PR loops fix passes forever.
func TestSelfHeal_FixBudgetSpentWhenHeadSHAUnavailable(t *testing.T) {
	tc := forcePushRedCase{maxFix: 1, pollTimeout: 100,
		headErr: errors.New("head sha unavailable"),
		states:  []forge.RollupState{redS, redS}}
	landing, reason, _, d := runForcePushRed(t, tc)
	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "exhausting 1 fix pass") {
		t.Errorf("reason = %q, want fix-budget exhaustion, not a timeout", reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
	}
}

func TestSelfHeal_ForcePushNoFixPass_OnTimeoutOrDispatchFailure(t *testing.T) {
	cases := []forcePushRedCase{
		{name: "rewait timeout", rebaseErr: forge.ErrMergeConflict, maxFix: 2, pollTimeout: 0,
			states: []forge.RollupState{okS, okS}},
		{name: "conflict-resolve failure", rebaseErr: forge.ErrMergeConflict, maxFix: 2, pollTimeout: 100,
			resolveErr: errors.New("box exited 1"), states: []forge.RollupState{okS, okS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			landing, _, _, d := runForcePushRed(t, tc)
			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("FixCalls = %d, want 0", len(d.FixCalls))
			}
		})
	}
}

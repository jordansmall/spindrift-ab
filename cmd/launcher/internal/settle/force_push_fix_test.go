package settle

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
	"spindrift.dev/launcher/internal/retry"
)

// fixClearsStale stands in for a fix pass that leaves the PR no longer behind
// its base, so the stale-base preflight reads clean when mergeImmediate runs
// again instead of rebasing a second time.
type fixClearsStale struct {
	*dispatch.Fake
	fc *forge.Fake
}

func (f fixClearsStale) Fix(pass int, ciFailureSummary string) dispatch.Disposition {
	f.fc.SetNeedsUpdate(testPR, false)
	return f.Fake.Fix(pass, ciFailureSummary)
}

// A post-force-push re-wait that ends genuine red gets a fix pass while the
// shared fix budget lasts, and a fix pass that turns the head green merges it
// through the ordinary green path.
func TestSelfHeal_ForcePushRedGetsFixPass(t *testing.T) {
	cases := []struct {
		name      string
		mergeErrs []error
		rebaseErr error
		stale     bool
	}{
		{name: "clean rebase-retry", mergeErrs: []error{forge.ErrMergeConflict}},
		{name: "conflict-resolve", mergeErrs: []error{forge.ErrMergeConflict}, rebaseErr: forge.ErrMergeConflict},
		{name: "stale-base preflight", stale: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(1)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			c.PreflightStaleBase = tc.stale
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = tc.mergeErrs
			fc.RebaseErr = tc.rebaseErr
			fc.SetNeedsUpdate(testPR, tc.stale)
			fc.SetCheckStates(testPR, []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure,                     // post-force-push re-wait: genuine red
				forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
			})
			s := newTestSettle(c, fc, fc)
			fake := dispatch.NewFake()

			landing, _ := s.selfHeal(fixClearsStale{Fake: fake, fc: fc}, "1", 0, testPR)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v, want landingMerged after the fix pass turned the head green", landing)
			}
			if len(fake.FixCalls) != 1 || fake.FixCalls[0].Pass != 1 {
				t.Errorf("want exactly one fix pass numbered 1; got %+v", fake.FixCalls)
			}
			if fc.Merged != testPR {
				t.Errorf("fc.Merged = %q, want %q", fc.Merged, testPR)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("issue must carry agent-complete and not agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

// The post-force-push red fix pass draws from the same budget as the pre-merge
// red loop, so a pre-merge fix pass leaves none for a red re-wait, and a zero
// budget dispatches nothing. The failure note names how many passes were used.
func TestSelfHeal_ForcePushRedSharesFixBudget(t *testing.T) {
	cases := []struct {
		name    string
		maxFix  int
		states  []forge.RollupState
		wantFix int
	}{
		{
			name:   "pre-merge red spends the only fix pass",
			maxFix: 1,
			states: []forge.RollupState{
				forge.StateFailure,                     // initial gateToGreen: red
				forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
				forge.StateFailure, // post-force-push re-wait: red
			},
			wantFix: 1,
		},
		{
			name:   "zero budget dispatches no fix pass",
			maxFix: 0,
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
				forge.StateFailure, // post-force-push re-wait: red
			},
			wantFix: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(tc.maxFix)
			c.MergeMode = "immediate"
			c.MaxRebaseAttempts = 3
			fc := forge.NewFake(testDispatchLabels)
			fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
			fc.MergeErrs = []error{forge.ErrMergeConflict}
			fc.SetCheckStates(testPR, tc.states)
			s := newTestSettle(c, fc, fc)
			fake := dispatch.NewFake()

			landing, reason := s.selfHeal(fake, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed", landing)
			}
			if len(fake.FixCalls) != tc.wantFix {
				t.Errorf("fix passes = %d, want %d", len(fake.FixCalls), tc.wantFix)
			}
			want := fmt.Sprintf("%d of %d fix pass(es) used", tc.wantFix, tc.maxFix)
			if !strings.Contains(reason, want) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, want)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("issue must carry agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

// Only a red re-wait is fixable. A relay failure after conflict-resolve leaves
// no red head for a fix pass to act on, so it settles Failed with no fix pass
// even while budget remains.
func TestSelfHeal_ConflictResolveRelayFailure_NoFixPass(t *testing.T) {
	c := fixConfig(2)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.ReadOnly = true
	c.Policy = retry.Policy{Max: 0, Unit: time.Second}
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	fc.RelayBundleErr = errors.New("ssh auth blip")
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())
	fake := dispatch.NewFake()

	landing, reason := s.selfHeal(fake, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed (relay after conflict-resolve failed)", landing)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
	}
	if len(fake.FixCalls) != 0 {
		t.Errorf("fix passes = %d, want 0 for a relay failure", len(fake.FixCalls))
	}
}

package settle

import (
	"errors"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
	"spindrift.dev/launcher/internal/testutil"
)

// forcePushRedForge scripts the rollup polls (a success costs two: poll plus
// confirm) with a trailing green tail, and a conflict then clean merge.
func forcePushRedForge(states ...forge.RollupState) *forge.Fake {
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: gateIssue, Labels: []string{"agent-in-progress"}})
	for i := 0; i < 20; i++ {
		states = append(states, forge.StateSuccess)
	}
	fc.SetCheckStates(testPR, states)
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	return fc
}

func TestSettle_ForcePushedHeadRedDispatchesFixThenMerges(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	fc := forcePushRedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure)
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(d.FixCalls) != 1 {
		t.Errorf("fix passes = %v, want 1", fixPasses(d))
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") || fc.Merged == "" {
		t.Errorf("want merged and agent-complete; merged=%q labels=%v", fc.Merged, iss.Labels)
	}
}

func TestSettle_ForcePushedHeadRedBudgetShared(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		states    []forge.RollupState
		wantFixes int
		wantText  string
	}{
		{"pre-merge pass consumed the budget", 1, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess, forge.StateFailure}, 1, "1 of 1 fix attempt"},
		{"no fix passes allowed", 0, []forge.RollupState{forge.StateSuccess, forge.StateSuccess, forge.StateFailure}, 0, "0 of 0 fix attempt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(tc.max)
			c.MaxRebaseAttempts = 3
			fc := forcePushRedForge(tc.states...)
			d := dispatch.NewFake()
			s := newTestSettle(c, fc, fc)

			testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

			if got := len(d.FixCalls); got != tc.wantFixes {
				t.Errorf("fix passes = %d, want %d", got, tc.wantFixes)
			}
			iss, _ := fc.Issue(gateIssue)
			if !containsLabel(iss.Labels, "agent-failed") || fc.Merged != "" {
				t.Errorf("want failed, unmerged; merged=%q labels=%v", fc.Merged, iss.Labels)
			}
			if !strings.Contains(strings.Join(issueComments(fc), "\n"), tc.wantText) {
				t.Errorf("no comment containing %q", tc.wantText)
			}
		})
	}
}

func TestSettle_ForcePushedHeadNeverGreenSkipsFixPass(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*forge.Fake, *dispatch.Fake)
	}{
		{"rewait timeout", func(fc *forge.Fake, d *dispatch.Fake) {
			fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
		}},
		{"conflict-resolve dispatch failure", func(fc *forge.Fake, d *dispatch.Fake) {
			fc.RebaseErr = forge.ErrMergeConflict
			d.ResolveConflictErr = errors.New("resolve box exited 1")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(3)
			c.MaxRebaseAttempts = 3
			fc := forcePushRedForge(forge.StateSuccess, forge.StateSuccess)
			d := dispatch.NewFake()
			tc.setup(fc, d)
			s := newTestSettle(c, fc, fc)

			testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

			if len(d.FixCalls) != 0 {
				t.Errorf("fix passes = %v, want none", fixPasses(d))
			}
			iss, _ := fc.Issue(gateIssue)
			if !containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-failed; labels=%v", iss.Labels)
			}
		})
	}
}

func TestSettle_ForcePushedHeadRedStaleBasePreflight(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	c.PreflightStaleBase = true
	fc := forcePushRedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure)
	fc.MergeErrs = []error{nil}
	fc.SetNeedsUpdate(testPR, true)
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(d.FixCalls) != 1 {
		t.Errorf("fix passes = %v, want 1", fixPasses(d))
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") || fc.Merged == "" {
		t.Errorf("want merged and agent-complete; merged=%q labels=%v", fc.Merged, iss.Labels)
	}
}

func TestSettle_ForcePushedHeadRedAfterConflictResolve(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	fc := forcePushRedForge(forge.StateSuccess, forge.StateSuccess, forge.StateFailure)
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc)

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("resolve-conflict calls = %d, want 1", len(d.ResolveConflictCalls))
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("fix passes = %v, want 1", fixPasses(d))
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-complete") || fc.Merged == "" {
		t.Errorf("want merged and agent-complete; merged=%q labels=%v", fc.Merged, iss.Labels)
	}
}

func TestSettle_ConflictResolveRelayFailureSettlesFailed(t *testing.T) {
	c := fixConfig(2)
	c.MaxRebaseAttempts = 3
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forcePushRedForge(forge.StateSuccess, forge.StateSuccess)
	fc.RebaseErr = forge.ErrMergeConflict
	fc.RelayBundleErr = errors.New("relay: bundle unreadable")
	d := dispatch.NewFake()
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())

	testutil.CaptureStdout(t, func() { s.Settle(d, gateIssue, 0, readyResult()) })

	if len(fc.RelayBundleCalls) == 0 {
		t.Errorf("relay never attempted")
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("fix passes = %v, want none", fixPasses(d))
	}
	iss, _ := fc.Issue(gateIssue)
	if !containsLabel(iss.Labels, "agent-failed") || fc.Merged != "" {
		t.Errorf("want failed, unmerged; merged=%q labels=%v", fc.Merged, iss.Labels)
	}
}

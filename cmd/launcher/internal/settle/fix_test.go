package settle

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
	"spindrift.dev/launcher/internal/outcome"
	"spindrift.dev/launcher/internal/testutil"
)

func fixConfig(maxFixAttempts int) Config {
	c := baseConfig()
	c.MaxFixAttempts = maxFixAttempts
	return c
}

// fixPasses extracts the 1-based pass numbers recorded on a Fake Dispatcher.
func fixPasses(d *dispatch.Fake) []int {
	var passes []int
	for _, call := range d.FixCalls {
		passes = append(passes, call.Pass)
	}
	return passes
}

// On genuine red, selfHeal must forward fc.FailureDetail(pr) to Fix as the fix
// box's CI_FAILURE_SUMMARY (issue #426).
func TestSelfHeal_ForwardsFailureDetailToFix(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	fc.SetFailureDetail(testPR, "lint: FAILURE\n2 errors")
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged after one fix pass", landing)
	}
	if len(d.FixCalls) != 1 || d.FixCalls[0].CIFailureSummary != "lint: FAILURE\n2 errors" {
		t.Errorf("want fix pass forwarded the scripted failure detail; got %+v", d.FixCalls)
	}
}

// A FailureDetail that errors or returns "" must still dispatch the fix pass
// with an empty summary. The fetch is best-effort.
func TestSelfHeal_EmptyFailureDetailFallsBackWithNoError(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	fc.FailureDetailErr = errors.New("gh api graphql: 403 Forbidden")
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v; a FailureDetail fetch error must not block the fix pass", landing)
	}
	if len(d.FixCalls) != 1 || d.FixCalls[0].CIFailureSummary != "" {
		t.Errorf("want empty summary on fetch error, got %+v", d.FixCalls)
	}
}

func TestSelfHeal_SuccessFirstTry(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged on first-try SUCCESS", landing)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("expected no fix calls, got %+v", d.FixCalls)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected at least one TransitionState call (Complete)")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

func TestSelfHeal_GenuineRedMaxZero(t *testing.T) {
	c := fixConfig(0)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed (maxFixAttempts=0)", landing)
	}
	if !strings.Contains(reason, "ci-red:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "ci-red:")
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("expected no fix calls (maxFixAttempts=0), got %+v", d.FixCalls)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected TransitionState call for Failed")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
}

func TestSelfHeal_GenuineRedFixSucceeds(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	// First poll is FAILURE, then SUCCESS after the fix box, plus the
	// confirmation poll.
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after one fix pass", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected TransitionState call (Complete)")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// After a fix pass the box's outbox bundle must be relayed in
// (forge.BundleRelay) before the loop re-polls CI, on a Code Forge that
// implements it (issue #1919). A read-only fix Box holds no push-capable token
// and bundles its work instead (issue #1979), so without the relay the PR head
// never moves and CI re-reports the same failure forever.
func TestSelfHeal_ReadOnlyFixPassRelaysBundleBeforeRecheck(t *testing.T) {
	c := fixConfig(3)
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	cf := fc.AsGithubReadOnly()
	s := newTestSettle(c, fc, cf)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged after one fix pass", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if len(fc.RelayBundleCalls) != 1 {
		t.Fatalf("RelayBundle called %d times, want 1: %+v", len(fc.RelayBundleCalls), fc.RelayBundleCalls)
	}
	want := forge.RelayBundleCall{OutboxDir: "/outbox/1", Ref: cf.AgentBranch("1")}
	if fc.RelayBundleCalls[0] != want {
		t.Errorf("RelayBundle call = %+v, want %+v", fc.RelayBundleCalls[0], want)
	}
}

// The relay (issue #1979) must run before the no-op fix-pass detection (issue
// #1980) compares head SHAs. A read-only Box never pushes directly, so
// s.pr.HeadCommitSHA(pr) never moves until the relay lands the bundle. A
// no-op check running first would misread every read-only fix pass as a no-op
// and abort self-heal on the first attempt, whatever MaxFixAttempts says.
func TestSelfHeal_ReadOnlyFixPassRelaysBeforeNoOpCheck(t *testing.T) {
	c := fixConfig(3)
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	// The read-only Box's direct-push visibility never moves, whatever the fix
	// pass did. Only the relay call lands new work in production, and this
	// Fake's HeadCommitSHA model does not cover it.
	fc.SetHeadCommitSHAs(testPR, []string{"sha-1", "sha-1", "sha-1", "sha-1"})
	cf := fc.AsGithubReadOnly()
	s := newTestSettle(c, fc, cf)

	d := dispatch.NewFake()
	s.selfHeal(d, "1", 0, testPR)

	if len(fc.RelayBundleCalls) != 1 {
		t.Fatalf("RelayBundle called %d times, want 1 — it must run every read-only fix pass regardless of the no-op check's own head-SHA snapshot, or a genuine fix's PR head would never actually move", len(fc.RelayBundleCalls))
	}
}

// A RelayBundle failure after a fix pass (a crashed Box that left no bundle,
// say) is logged but must never block or crash the retry loop. It falls
// through to the next gateToGreen poll and exhausts normally rather than
// becoming its own terminal condition.
func TestSelfHeal_ReadOnlyFixPassRelayFailureIsNonFatal(t *testing.T) {
	c := fixConfig(1)
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateFailure})
	fc.RelayBundleErr = errors.New("bundle missing")
	cf := fc.AsGithubReadOnly()
	s := newTestSettle(c, fc, cf)

	d := dispatch.NewFake()
	var landing landingResult
	var reason string
	out := testutil.CaptureStdout(t, func() {
		landing, reason = s.selfHeal(d, "1", 0, testPR)
	})

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed (still red after the relay failure)", landing)
	}
	if !strings.Contains(reason, "ci-red:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "ci-red:")
	}
	if len(fc.RelayBundleCalls) != 1 {
		t.Errorf("RelayBundle called %d times, want 1: %+v", len(fc.RelayBundleCalls), fc.RelayBundleCalls)
	}
	if !strings.Contains(out, "fix-relay-failed") {
		t.Errorf("console output must log the relay failure; got: %q", out)
	}
}

func TestSelfHeal_ExhaustsAllPasses(t *testing.T) {
	c := fixConfig(2)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,
		forge.StateFailure,
		forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed after exhausting all fix passes", landing)
	}
	if !strings.Contains(reason, "ci-red:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "ci-red:")
	}
	passes := fixPasses(d)
	if len(passes) != 2 {
		t.Errorf("expected %d fix calls (maxFixAttempts), got %d: %v", c.MaxFixAttempts, len(passes), passes)
	}
	for i, p := range passes {
		if p != i+1 {
			t.Errorf("passes[%d]=%d, want %d", i, p, i+1)
		}
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected TransitionState call for Failed")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
}

// When d.Fix returns a failed Disposition, selfHeal must land failed right away instead of
// re-polling the unchanged head and burning the rest of the fix-pass budget
// against the identical cached rollup (issue #1980). The rollup is scripted
// FAILURE on every poll, standing in for a head that never advances, so a
// buggy loop would burn all 3 passes instead of stopping after the first.
func TestSelfHeal_FixFailureStopsImmediately(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	d.FixResult = dispatch.Failed(dispatch.Result{})
	var landing landingResult
	var reason string
	stderr := testutil.CaptureStderr(t, func() {
		landing, reason = s.selfHeal(d, "1", 0, testPR)
	})

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed after a failed fix pass", landing)
	}
	if !strings.Contains(reason, "fix-failed:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "fix-failed:")
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("expected exactly 1 fix call (no retry against the same failed head), got %+v", d.FixCalls)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected TransitionState call for Failed")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty when Result.Err is nil (ordinary exited-non-zero case)", stderr)
	}
}

// A skipped fix pass never started a Box: a sibling container for this
// issue is live and owns its in-progress claim, so selfHeal must abandon without
// a Failed transition, a comment, or another pass (issue #3655).
func TestSelfHeal_FixAlreadyInFlightLeavesIssueUntouched(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	d.FixResult = dispatch.Skipped()
	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingAbandoned {
		t.Errorf("selfHeal = %v, want landingAbandoned", landing)
	}
	if reason != "" {
		t.Errorf("selfHeal reason = %q, want empty", reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("expected exactly 1 fix call (no retry), got %+v", d.FixCalls)
	}
	assertNoTransitionOrComment(t, fc)
	assertClaimUntouched(t, fc)
}

// When d.Fix returns a Result whose Err is set, the box-never-launched case
// (dispatch/retry.go, issue #3119), selfHeal must print that reason on stderr
// alongside the status=fix-failed line, matching the "?? #N: %v" diagnostic
// convention retry.go and waves already use.
func TestSelfHeal_FixFailureWithErrSurfacesReason(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	boxErr := errors.New("box never launched: registry proxy dial failed")
	d.FixResult = dispatch.Failed(dispatch.Result{Err: boxErr})

	var landing landingResult
	var reason string
	stderr := testutil.CaptureStderr(t, func() {
		landing, reason = s.selfHeal(d, "1", 0, testPR)
	})

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed after a failed fix pass", landing)
	}
	if !strings.Contains(reason, "fix-failed:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "fix-failed:")
	}
	want := "    ?? #1: box never launched: registry proxy dial failed\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
}

// A Fix that returns a succeeded Disposition but leaves the PR head SHA unchanged must land
// failed right away instead of re-polling the identical cached rollup as a
// fresh genuine red (issue #1980). The no-op-confirm pause must also sleep
// through the injected clock rather than a bare time.Sleep (issue #2502):
// recordingClock only records the sleeps routed through it.
func TestSelfHeal_FixNoOpUnchangedHead(t *testing.T) {
	c := fixConfig(3)
	sleeps, clock := recordingClock()
	c.Clock = clock
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	// Same SHA before, immediately after, and on the confirm re-read: the fix
	// box exited zero but never pushed a new commit.
	fc.SetHeadCommitSHAs(testPR, []string{"sha-unchanged", "sha-unchanged", "sha-unchanged"})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed after a no-op fix pass", landing)
	}
	if !strings.Contains(reason, "fix-no-op:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "fix-no-op:")
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("expected exactly 1 fix call (no retry against the unchanged head), got %+v", d.FixCalls)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	wantSleeps := []time.Duration{time.Duration(c.MergePollInterval) * time.Second}
	if !slices.Equal(*sleeps, wantSleeps) {
		t.Errorf("clock sleeps = %v, want %v (no-op-confirm pause must sleep through s.clock)", *sleeps, wantSleeps)
	}
}

// A head-SHA read returning the same value right after Fix (GitHub API
// replication lag serving the pre-push snapshot) must not be mistaken for a
// no-op fix pass: a confirm re-read showing the head advanced lets the loop
// proceed to green, mirroring gateToGreen's confirm-poll for a SUCCESS rollup
// (issue #1980).
func TestSelfHeal_FixAdvanceConfirmedAfterTransientSameRead(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateFailure, forge.StateSuccess, forge.StateSuccess})
	// The reads are before, immediately after (stale, same as before), and the
	// confirm re-read that shows the real advance.
	fc.SetHeadCommitSHAs(testPR, []string{"sha-a", "sha-a", "sha-b"})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged (a transient same-read must not abort as no-op)", landing)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("expected exactly 1 fix call, got %+v", d.FixCalls)
	}
}

func TestSelfHeal_ErrorStateTriggersFixPass(t *testing.T) {
	c := fixConfig(1)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	// ERROR counts as genuine red just like FAILURE.
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateError, forge.StateSuccess, forge.StateSuccess})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after ERROR then SUCCESS with fix pass", landing)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("expected 1 fix call, got %+v", d.FixCalls)
	}
}

// This test also checks that the generic ci-timeout reason, not just the
// registration-guard one, is posted as an issue comment on gate-terminal
// failure rather than only logged to the console (issue #2476).
func TestSelfHeal_PendingTimeoutNoFix(t *testing.T) {
	c := fixConfig(3)
	c.MergePollTimeout = 0 // expire immediately
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StatePending})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed on PENDING timeout", landing)
	}
	if !strings.Contains(reason, "ci-timeout:") {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, "ci-timeout:")
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("expected no fix calls on PENDING timeout, got %+v", d.FixCalls)
	}
	if len(fc.TransitionStateCalls) == 0 {
		t.Fatal("expected TransitionState call for Failed")
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	if len(fc.CommentCalls) != 1 {
		t.Fatalf("expected exactly one comment posted on gate-terminal failure, got %d: %+v", len(fc.CommentCalls), fc.CommentCalls)
	}
	if !strings.Contains(fc.CommentCalls[0].Body, "ci-timeout:") {
		t.Errorf("comment body = %q, want a substring containing %q", fc.CommentCalls[0].Body, "ci-timeout:")
	}
}

// A fix pass's scan warnings and rejection warnings reach the sidecar through
// the same seam as the initial settle's, tagged with the pass (issue #3744).
func TestSelfHeal_FixPassWarningsAreRecorded(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	d.FixResult = dispatch.Failed(dispatch.Result{
		Warnings:             []string{"pass-manifest scan: boom"},
		IssueIntentsRejected: outcome.Rejections{Malformed: 2},
	})
	stderr := testutil.CaptureStderr(t, func() { s.selfHeal(d, "1", 0, testPR) })

	rejection := "fix pass 1: 2 malformed issue-intent line(s) rejected"
	want := [][]string{{"fix pass 1: pass-manifest scan: boom", rejection}}
	if !reflect.DeepEqual(d.RecordedWarnings, want) {
		t.Errorf("RecordedWarnings = %q, want %q", d.RecordedWarnings, want)
	}
	if !strings.Contains(stderr, "    ?? #1: "+rejection+"\n") {
		t.Errorf("stderr = %q, want the tagged rejection line", stderr)
	}
}

// A clean fix pass records nothing: under RecordWarnings' accumulation an
// empty batch would still delete an earlier run's sidecar when this Dispatch
// recorded none before it (an adopted settle).
func TestSelfHeal_CleanFixPassRecordsNothing(t *testing.T) {
	c := fixConfig(3)
	fc := forge.NewFake()
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure, forge.StateFailure, forge.StateFailure, forge.StateFailure,
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	d.FixResult = dispatch.Failed(dispatch.Result{})
	testutil.CaptureStderr(t, func() { s.selfHeal(d, "1", 0, testPR) })

	if len(d.RecordedWarnings) != 0 {
		t.Errorf("RecordedWarnings = %q, want none", d.RecordedWarnings)
	}
}

// commentContaining reports whether any comment fc recorded contains every sub.
func commentContaining(fc *forge.Fake, subs ...string) bool {
	for _, cc := range fc.CommentCalls {
		all := true
		for _, sub := range subs {
			all = all && strings.Contains(cc.Body, sub)
		}
		if all {
			return true
		}
	}
	return false
}

// A force-pushed head that goes genuinely red in the post-force-push re-wait
// draws a fix pass from the shared fix budget, and the head the pass turns green
// re-enters the merge path (issue #5053).
func TestSelfHeal_RedAfterForcePush_FixPassThenMerges(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first gate
		forge.StateFailure,                     // rewait after the clean rebase force-push: genuine red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after a fix pass on the force-pushed head", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// The stale-base preflight's clean force-push that goes red in the re-wait draws
// a fix pass too, and the head the pass turns green merges (issue #5053). The
// fake stays stale, so the preflight rebases again after the pass: this proves
// red -> fix -> green -> merge, not a single force-push.
func TestSelfHeal_RedAfterStaleBasePreflight_FixPassThenMerges(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.PreflightStaleBase = true
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetNeedsUpdate(testPR, true)
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first gate
		forge.StateFailure,                     // rewait after the preflight force-push: genuine red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
		forge.StateSuccess, forge.StateSuccess, forge.StateSuccess, // preflight rebases again: the fake never clears needs-update
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after a fix pass on the preflight-rebased head", landing)
	}
	if len(fc.RebasedURLs) == 0 {
		t.Error("stale-base preflight never rebased")
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// A conflict-resolve Box's force-push that goes red in the re-wait draws a fix
// pass too, and the head the pass turns green merges (issue #5053).
func TestSelfHeal_RedAfterConflictResolveBox_FixPassThenMerges(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first gate
		forge.StateFailure,                     // rewait after the resolved force-push: genuine red
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
	})
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Errorf("selfHeal = %v, want landingMerged after a fix pass on the resolved head", landing)
	}
	if len(d.ResolveConflictCalls) != 1 {
		t.Errorf("ResolveConflict called %d times, want 1", len(d.ResolveConflictCalls))
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// The fix pass on a force-pushed head that stays red spends the whole budget:
// the landing fails as landing-failed, not fix-exhausted, and the comment says
// how many passes the force-pushed head consumed.
func TestSelfHeal_RedAfterForcePush_FixPassStillRed_EndsLandingFailed(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // first gate
		forge.StateFailure, // rewait after the clean rebase force-push: red
		forge.StateFailure, // gate after the fix pass: still red
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly one", passes)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
	}
	if !commentContaining(fc, "consumed 1 fix pass", "1 of 1") {
		t.Errorf("no comment naming consumed 1 fix pass, 1 of 1; comments=%v", fc.CommentCalls)
	}
}

// The post-force-push red re-wait draws on the same fix budget as the pre-merge
// loop: with the only pass already spent, a second red fails the landing rather
// than earning a fresh allowance, and the comment names the attempts used.
func TestSelfHeal_RedAfterForcePush_SharedBudgetExhausted_EndsFailed(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // first gate: red, spends the only fix pass
		forge.StateSuccess, forge.StateSuccess, // gate after the fix pass
		forge.StateFailure, // rewait after the clean rebase force-push: red again
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("fix passes = %v, want exactly one (budget is shared)", passes)
	}
	last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]
	if last.To != forge.Failed {
		t.Errorf("last transition To=%v, want Failed", last.To)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
	}
	if !commentContaining(fc, "landing failed", "consumed 0 fix pass", "1 of 1") {
		t.Errorf("no landing-failed comment naming consumed 0 fix pass, 1 of 1; comments=%v", fc.CommentCalls)
	}
}

// With one of two fix passes spent before the force-push, the force-pushed head
// consumes the second: the comment counts only that pass against the head and
// both against the landing.
func TestSelfHeal_RedAfterForcePush_PartlySpentBudget_CountsHeadPasses(t *testing.T) {
	c := fixConfig(2)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // first gate: red, spends pass 1
		forge.StateSuccess, forge.StateSuccess, // gate after pass 1
		forge.StateFailure, // rewait after the clean rebase force-push: red
		forge.StateFailure, // gate after pass 2: still red
	})
	s := newTestSettle(c, fc, fc)

	d := dispatch.NewFake()
	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Errorf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 2 {
		t.Errorf("fix passes = %v, want exactly two", passes)
	}
	if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
		t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
	}
	if !commentContaining(fc, "consumed 1 fix pass", "2 of 2") {
		t.Errorf("no comment naming consumed 1 fix pass, 2 of 2; comments=%v", fc.CommentCalls)
	}
}

// A re-wait timeout and a failed conflict-resolve dispatch or relay are not
// genuine red CI: they settle Failed without spending a fix pass even with
// budget left.
func TestSelfHeal_NonRedLandingFailures_SpendNoFixPass(t *testing.T) {
	cases := []struct {
		name        string
		resolveErr  error
		relayErr    error
		pollTimeout int
		wantResolve bool // a resolve was dispatched, so the failure comes after the post-force-push step, not the first gate
	}{
		{name: "rewait times out", pollTimeout: 0, wantResolve: true},
		{name: "conflict-resolve dispatch fails", resolveErr: errors.New("box exited 1"), pollTimeout: 100},
		{name: "relay after conflict-resolve fails", relayErr: errors.New("relay exhausted"), pollTimeout: 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixConfig(2)
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
			s := newTestSettle(c, fc, fc.AsGithubReadOnly())
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			landing, _ := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed", landing)
			}
			if passes := fixPasses(d); len(passes) != 0 {
				t.Errorf("fix passes = %v, want none", passes)
			}
			if tc.wantResolve && len(d.ResolveConflictCalls) == 0 {
				t.Error("no conflict-resolve dispatched; failure did not come from the post-force-push re-wait")
			}
			if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Failed {
				t.Errorf("last transition To=%v, want Failed", last.To)
			}
			if got := s.settledLatch["1"].reason; got != ReasonLandingFailed {
				t.Errorf("settled reason = %q, want %q", got, ReasonLandingFailed)
			}
		})
	}
}

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

// A red re-wait after an immediate-merge force-push dispatches a fix pass just
// as a pre-merge red does, and the head it turns green re-enters the merge path
// (issue #3836 follow-up): here the re-entered Merge succeeds.
func TestSelfHeal_RewaitRedFixPassThenMerges(t *testing.T) {
	c := fixConfig(3)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-force-push re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged after the rewait fix pass", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merge did not land the fixed head; fc.Merged=%q", fc.Merged)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// With the shared fix budget already spent by a pre-merge red, a red re-wait
// settles Failed as landing-failed and its comment names the attempts the
// force-pushed head consumed (zero here: the pre-merge pass belonged to the
// earlier head).
func TestSelfHeal_RewaitRedBudgetExhaustedFailsWithAttemptCount(t *testing.T) {
	c := fixConfig(1)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // initial gateToGreen: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
		forge.StateFailure, // post-force-push re-wait: red with no budget left
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1 (the pre-merge pass), got %v", passes)
	}
	if !strings.Contains(reason, errLandingNeverGreen.Error()) {
		t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
	}
	if fc.Merged != "" {
		t.Errorf("Merge must not land a head that never went green; fc.Merged=%q", fc.Merged)
	}
	if n := len(fc.CommentCalls); n == 0 || !strings.Contains(fc.CommentCalls[n-1].Body, "consumed 0 fix attempt(s) (1/1 budget used)") {
		t.Errorf("want the final comment to name the attempts consumed; got %+v", fc.CommentCalls)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("issue must carry agent-failed and not agent-complete; labels=%v", iss.Labels)
	}
}

// The fix budget is shared across pre-merge and re-wait reds: with
// MaxFixAttempts=2, one pre-merge pass and one re-wait pass exhaust it, and the
// next red re-wait ends Failed, its comment counting the one re-wait pass.
func TestSelfHeal_RewaitRedSharesFixBudgetWithPreMerge(t *testing.T) {
	c := fixConfig(2)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateFailure,                     // initial gateToGreen: pre-merge red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after fix pass 1
		forge.StateFailure,                     // first re-wait: red, fix pass 2
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after fix pass 2
		forge.StateFailure, // second re-wait: red with the budget spent
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed once the shared budget is spent", landing)
	}
	if passes := fixPasses(d); len(passes) != 2 || passes[0] != 1 || passes[1] != 2 {
		t.Errorf("expected fix passes [1 2], got %v", passes)
	}
	if n := len(fc.CommentCalls); n == 0 || !strings.Contains(fc.CommentCalls[n-1].Body, "consumed 1 fix attempt(s) (2/2 budget used)") {
		t.Errorf("want the final comment to count the one re-wait pass against 2/2; got %+v", fc.CommentCalls)
	}
}

// A re-wait that times out, or a conflict-resolve whose dispatch or relay
// fails, never dispatches a fix pass, even with budget to spare: only a red
// head is fixable.
func TestSelfHeal_RewaitNotRed_NoFixPass(t *testing.T) {
	cases := []struct {
		name         string
		pollTimeout  int
		resolveErr   error
		relayErr     error
		checkStates  []forge.RollupState
		wantInReason string
	}{
		{
			name:         "re-wait times out",
			pollTimeout:  0,
			checkStates:  []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			wantInReason: "CI did not reach green",
		},
		{
			name:         "conflict-resolve dispatch fails",
			pollTimeout:  100,
			resolveErr:   errors.New("box exited 1"),
			checkStates:  []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			wantInReason: "conflict-resolve dispatch failed",
		},
		{
			name:         "relay after conflict-resolve fails",
			pollTimeout:  100,
			relayErr:     errors.New("bundle missing"),
			checkStates:  []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			wantInReason: "relay after conflict-resolve failed",
		},
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
			fc.SetCheckStates(testPR, tc.checkStates)
			var cf forge.CodeForge = fc
			if tc.relayErr != nil {
				cf = fc.AsGithubReadOnly()
			}
			s := newTestSettle(c, fc, cf)
			d := dispatch.NewFake()
			d.ResolveConflictErr = tc.resolveErr

			landing, reason := s.selfHeal(d, "1", 0, testPR)

			if landing != landingFailed {
				t.Errorf("selfHeal = %v, want landingFailed", landing)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("want no fix pass for a non-red landing failure, got %+v", d.FixCalls)
			}
			if !strings.Contains(reason, errLandingNeverGreen.Error()) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q", reason, errLandingNeverGreen.Error())
			}
			if !strings.Contains(reason, tc.wantInReason) {
				t.Errorf("selfHeal reason = %q, want a substring containing %q (the case's own failure path)", reason, tc.wantInReason)
			}
		})
	}
}

// A red re-wait after a conflict-resolve force-push dispatches a fix pass, and
// the head it turns green re-enters the merge path and lands (issue #3836
// follow-up): the reactive conflict loop's own re-wait, not the rebase-success
// one RewaitRedFixPassThenMerges covers.
func TestSelfHeal_RewaitRedAfterConflictResolveFixPassThenMerges(t *testing.T) {
	c := fixConfig(3)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-conflict-resolve re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
	})
	fc.MergeErrs = []error{forge.ErrMergeConflict, nil}
	fc.RebaseErr = forge.ErrMergeConflict
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged after the rewait fix pass", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merge did not land the fixed head; fc.Merged=%q", fc.Merged)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

// A red re-wait after a stale-base preflight rebase dispatches a fix pass, and
// the head it turns green lands. NeedsUpdate stays scripted, so the re-entered
// merge path preflights again: its re-wait is green.
func TestSelfHeal_RewaitRedAfterPreflightFixPassThenMerges(t *testing.T) {
	c := fixConfig(3)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.PreflightStaleBase = true
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.SetNeedsUpdate(testPR, true)
	fc.SetCheckStates(testPR, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // preflight rebase re-wait: genuine red
		forge.StateSuccess, forge.StateSuccess, // gateToGreen after the fix pass
		forge.StateSuccess, forge.StateSuccess, // re-wait after the re-entered preflight
	})
	s := newTestSettle(c, fc, fc)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged after the preflight rewait fix pass", landing, reason)
	}
	if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
		t.Errorf("expected exactly fix-pass-1, got %v", passes)
	}
	if fc.Merged != testPR {
		t.Errorf("Merge did not land the fixed head; fc.Merged=%q", fc.Merged)
	}
	if last := fc.TransitionStateCalls[len(fc.TransitionStateCalls)-1]; last.To != forge.Complete {
		t.Errorf("last transition To=%v, want Complete", last.To)
	}
}

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
	"spindrift.dev/launcher/internal/retry"
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

// postForcePushFixSettle runs selfHeal in immediate mode against a first merge
// that conflicts, so the landing force-pushes a rebase and re-waits on CI.
// readOnly swaps in the Fake's read-only GitHub shape, whose landing relays the
// outbox bundle.
func postForcePushFixSettle(t *testing.T, maxFix int, states []forge.RollupState, readOnly bool, mutate func(*Config, *forge.Fake, *dispatch.Fake)) (*forge.Fake, *dispatch.Fake, landingResult, string) {
	t.Helper()
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	d := dispatch.NewFake()
	if mutate != nil {
		mutate(&c, fc, d)
	}
	var code forge.CodeForge = fc
	if readOnly {
		code = fc.AsGithubReadOnly()
	}
	s := newTestSettle(c, fc, code)
	landing, note := s.selfHeal(d, "1", 0, testPR)
	return fc, d, landing, note
}

func commentBodies(fc *forge.Fake) string {
	var b strings.Builder
	for _, c := range fc.CommentCalls {
		b.WriteString(c.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// A force-pushed head whose post-force-push CI re-wait ends genuine red gets a
// fix pass, and a head the pass turns green re-enters the merge path (ADR 0026
// amendment), whichever route force-pushed it.
func TestSelfHeal_RedAfterForcePush_DispatchesFixPassAndMerges(t *testing.T) {
	cases := []struct {
		name   string
		states []forge.RollupState
		setup  func(*Config, *forge.Fake, *dispatch.Fake)
		check  func(*testing.T, *dispatch.Fake)
	}{
		{
			name: "rebase retry after merge conflict",
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial green + confirm
				forge.StateFailure,                     // rewait after the rebase force-push: red
				forge.StateSuccess, forge.StateSuccess, // after the fix pass: green + confirm
			},
		},
		{
			name: "stale-base preflight rebase",
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial green + confirm
				forge.StateFailure,                     // rewait after the preflight rebase: red
				forge.StateSuccess, forge.StateSuccess, // after the fix pass: green + confirm
				forge.StateSuccess, forge.StateSuccess, // rewait after the re-entry's preflight rebase
			},
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				c.PreflightStaleBase = true
				// The Fake never clears the flag, so the re-entered merge path
				// preflight-rebases once more.
				fc.SetNeedsUpdate(testPR, true)
				fc.MergeErrs = []error{nil}
			},
		},
		{
			name: "conflict-resolve Box",
			states: []forge.RollupState{
				forge.StateSuccess, forge.StateSuccess, // initial green + confirm
				forge.StateFailure,                     // rewait after the resolve force-push: red
				forge.StateSuccess, forge.StateSuccess, // after the fix pass: green + confirm
			},
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				fc.RebaseErr = forge.ErrMergeConflict
			},
			check: func(t *testing.T, d *dispatch.Fake) {
				if len(d.ResolveConflictCalls) < 1 {
					t.Errorf("want a ResolveConflict dispatch, got %d", len(d.ResolveConflictCalls))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc, d, landing, note := postForcePushFixSettle(t, 3, tc.states, false, tc.setup)

			if landing != landingMerged {
				t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, note)
			}
			if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
				t.Errorf("want exactly fix-pass-1, got %v", passes)
			}
			iss, _ := fc.Issue("1")
			if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
				t.Errorf("want agent-complete and not agent-failed; labels=%v", iss.Labels)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

// A fix-pass-green head that force-pushes again and goes red a second time
// keeps drawing from the one MaxFixAttempts budget: it earns a second pass
// while budget lasts, then settles landing-failed without a third.
func TestSelfHeal_RedAfterForcePush_RepeatedRedIsBounded(t *testing.T) {
	states := []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial green + confirm
		forge.StateFailure,                     // rewait after the first rebase: red
		forge.StateSuccess, forge.StateSuccess, // after fix pass 1: green + confirm
		forge.StateFailure,                     // rewait after the second rebase: red again
		forge.StateSuccess, forge.StateSuccess, // after fix pass 2: green + confirm
	}
	conflictTwice := func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
		fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict, nil}
	}

	t.Run("budget lasts", func(t *testing.T) {
		fc, d, landing, note := postForcePushFixSettle(t, 2, states, false, conflictTwice)

		if landing != landingMerged {
			t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, note)
		}
		if passes := fixPasses(d); len(passes) != 2 || passes[0] != 1 || passes[1] != 2 {
			t.Errorf("want fix passes [1 2], got %v", passes)
		}
		iss, _ := fc.Issue("1")
		if !containsLabel(iss.Labels, "agent-complete") || containsLabel(iss.Labels, "agent-failed") {
			t.Errorf("want agent-complete and not agent-failed; labels=%v", iss.Labels)
		}
	})

	t.Run("budget spent", func(t *testing.T) {
		fc, d, landing, note := postForcePushFixSettle(t, 1, states, false, conflictTwice)

		if landing != landingFailed {
			t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, note)
		}
		if passes := fixPasses(d); len(passes) != 1 || passes[0] != 1 {
			t.Errorf("want only fix-pass-1, got %v", passes)
		}
		iss, _ := fc.Issue("1")
		if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
			t.Errorf("want agent-failed and not agent-complete; labels=%v", iss.Labels)
		}
		if !strings.Contains(commentBodies(fc), "1 of 1 fix pass(es) used") {
			t.Errorf("failure comment must name fix passes used of the budget; comments:\n%s", commentBodies(fc))
		}
	})
}

// The post-force-push fix pass draws from the same MaxFixAttempts budget as the
// pre-merge red loop, so a spent budget settles landing-failed without another
// pass.
func TestSelfHeal_RedAfterForcePush_SharesFixBudget(t *testing.T) {
	fc, d, landing, note := postForcePushFixSettle(t, 1, []forge.RollupState{
		forge.StateFailure,                     // pre-merge red: consumes the one pass
		forge.StateSuccess, forge.StateSuccess, // green after the fix + confirm
		forge.StateFailure, // rewait after the rebase: red again
	}, false, nil)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, note)
	}
	if passes := fixPasses(d); len(passes) != 1 {
		t.Errorf("want only the pre-merge fix pass, got %v", passes)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-failed and not agent-complete; labels=%v", iss.Labels)
	}
	if !strings.Contains(commentBodies(fc), "1 of 1 fix pass(es) used") {
		t.Errorf("failure comment must name fix passes used of the budget; comments:\n%s", commentBodies(fc))
	}
}

// With no fix budget, red after a force-push settles landing-failed with no
// fix pass, as before.
func TestSelfHeal_RedAfterForcePush_NoBudgetFails(t *testing.T) {
	fc, d, landing, _ := postForcePushFixSettle(t, 0, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, forge.StateFailure,
	}, false, nil)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if len(d.FixCalls) != 0 {
		t.Errorf("want no fix pass, got %v", fixPasses(d))
	}
	if !strings.Contains(commentBodies(fc), "0 of 0 fix pass(es) used") {
		t.Errorf("comments:\n%s", commentBodies(fc))
	}
}

// Only genuine red earns a fix pass: a re-wait timeout, a failed
// conflict-resolve dispatch, and a failed relay after it stay landing-failed
// however much budget remains.
func TestSelfHeal_ForcePushNonRedFailures_GetNoFixPass(t *testing.T) {
	cases := []struct {
		name     string
		readOnly bool
		states   []forge.RollupState
		setup    func(*Config, *forge.Fake, *dispatch.Fake)
	}{
		{
			name:   "rewait times out",
			states: []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				// The initial gate passes on its two scripted polls; a zero poll
				// timeout then ends the re-wait as soon as it starts.
				c.MergePollTimeout = 0
			},
		},
		{
			name:   "conflict-resolve dispatch fails",
			states: []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				fc.RebaseErr = forge.ErrMergeConflict
				d.ResolveConflictErr = errors.New("box exited 1")
			},
		},
		{
			name:     "relay after conflict-resolve fails",
			readOnly: true,
			states:   []forge.RollupState{forge.StateSuccess, forge.StateSuccess},
			setup: func(c *Config, fc *forge.Fake, d *dispatch.Fake) {
				c.Policy = retry.Policy{Max: 3, Unit: time.Second}
				c.OutboxDir = func(num string) string { return "/outbox/" + num }
				fc.RebaseErr = forge.ErrMergeConflict
				fc.RelayBundleErr = errors.New("ssh auth blip")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, d, landing, note := postForcePushFixSettle(t, 3, tc.states, tc.readOnly, tc.setup)
			if landing != landingFailed {
				t.Fatalf("selfHeal = %v (%q), want landingFailed", landing, note)
			}
			if len(d.FixCalls) != 0 {
				t.Errorf("want no fix pass, got %v", fixPasses(d))
			}
		})
	}
}

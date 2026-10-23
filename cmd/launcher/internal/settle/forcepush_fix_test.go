package settle

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushFixSettle wires an immediate-merge Settle whose first Merge
// conflicts, so the rebase (rebaseErr nil) or conflict-resolve
// (rebaseErr ErrMergeConflict) force-pushes and the re-wait reads states.
func forcePushFixSettle(maxFix int, states []forge.RollupState, rebaseErr error, timeout int) (*Settle, *forge.Fake) {
	c := fixConfig(maxFix)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = timeout
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = rebaseErr
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

func assertSettledReason(t *testing.T, s *Settle, num, want string) {
	t.Helper()
	if got := s.settledLatch[num].reason; got != want {
		t.Errorf("settled reason = %q, want %q", got, want)
	}
}

func assertCommentContains(t *testing.T, fc *forge.Fake, want string) {
	t.Helper()
	comments := issueComments(fc)
	if !slices.ContainsFunc(comments, func(c string) bool { return strings.Contains(c, want) }) {
		t.Errorf("no comment contains %q; comments=%q", want, comments)
	}
}

// A force-pushed head whose re-wait goes red draws a fix pass; the fixed head
// re-enters the merge path and lands.
func TestSelfHeal_ForcePushRewaitRed_DispatchesFixPassThenMerges(t *testing.T) {
	s, fc := forcePushFixSettle(2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-force-push re-wait: red
		forge.StateSuccess, forge.StateSuccess, // fixed head
	}, nil, 100)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1}) {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-complete; labels=%v", iss.Labels)
	}
	assertSettledReason(t, s, "1", ReasonMerged)
}

// The pre-merge red loop and the post-force-push red share one fix budget: a
// pass spent before the first green leaves none for the force-pushed head.
func TestSelfHeal_ForcePushRewaitRed_SharesFixBudgetWithPreMergeLoop(t *testing.T) {
	s, fc := forcePushFixSettle(1, []forge.RollupState{
		forge.StateFailure,                     // initial red: fix pass 1
		forge.StateSuccess, forge.StateSuccess, // fixed head goes green
		forge.StateFailure, // post-force-push re-wait: red, budget spent
	}, nil, 100)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1}) {
		t.Errorf("fix passes = %v, want [1] (shared budget)", got)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-failed; labels=%v", iss.Labels)
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
	assertCommentContains(t, fc, "used 1 of 1 fix attempt(s), 0 since the first force-push")
}

// A fix pass spent on the force-pushed head that leaves it red exhausts the
// budget as landing-failed, and the comment counts that pass against the head.
func TestSelfHeal_ForcePushRewaitRed_FixLeavesRed_LandingFailedNamesAttempts(t *testing.T) {
	s, fc := forcePushFixSettle(1, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure, // post-force-push re-wait: red, fix pass 1
		forge.StateFailure, // fixed head still red
	}, nil, 100)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1}) {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
	assertCommentContains(t, fc, "used 1 of 1 fix attempt(s), 1 since the first force-push")
}

// A re-wait timeout is not red: it still settles landing-failed with no fix
// pass.
func TestSelfHeal_ForcePushRewaitTimeout_NoFixPass(t *testing.T) {
	s, _ := forcePushFixSettle(3, []forge.RollupState{forge.StateSuccess, forge.StateSuccess}, forge.ErrMergeConflict, 0)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(fixPasses(d)) != 0 {
		t.Fatalf("landing=%v fixCalls=%d, want landingFailed with none", landing, len(fixPasses(d)))
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
}

// A failed conflict-resolve dispatch never force-pushed a head to fix: it
// settles landing-failed with no fix pass.
func TestSelfHeal_ConflictResolveFails_NoFixPass(t *testing.T) {
	s, _ := forcePushFixSettle(3, []forge.RollupState{forge.StateSuccess, forge.StateSuccess}, forge.ErrMergeConflict, 100)
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("resolve boom")

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(fixPasses(d)) != 0 {
		t.Fatalf("landing=%v fixCalls=%d, want landingFailed with none", landing, len(fixPasses(d)))
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
}

// A conflict-resolve Box that dispatched fine but whose bundle cannot be
// relayed never force-pushed a head: landing-failed with no fix pass.
func TestSelfHeal_ConflictResolveRelayFails_NoFixPass(t *testing.T) {
	c := fixConfig(3)
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MergePollTimeout = 100
	c.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.RebaseErr = forge.ErrMergeConflict
	fc.SetCheckStates(testPR, []forge.RollupState{forge.StateSuccess, forge.StateSuccess})
	relayErr := errors.New("relay boom")
	fc.RelayBundleErr = relayErr
	s := newTestSettle(c, fc, fc.AsGithubReadOnly())
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(fixPasses(d)) != 0 {
		t.Fatalf("landing=%v fixCalls=%d, want landingFailed with none", landing, len(fixPasses(d)))
	}
	if len(fc.RelayBundleCalls) == 0 {
		t.Error("relay never attempted")
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
}

// A second force-push that goes red after a fix pass on the first keeps the
// count anchored at the first red: the comment counts every pass spent on
// force-pushed heads, not just those since the latest force-push.
func TestSelfHeal_ForcePushRewaitRed_TwiceCountsFromFirst(t *testing.T) {
	s, fc := forcePushFixSettle(3, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // first force-push re-wait: red, fix pass 1
		forge.StateSuccess, forge.StateSuccess, // fixed head green, merge conflicts again
		forge.StateFailure, // second force-push re-wait: red, fix pass 2
		forge.StateFailure, // fix 2 leaves red, fix pass 3
		forge.StateFailure, // fix 3 leaves red
	}, nil, 100)
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1, 2, 3}) {
		t.Errorf("fix passes = %v, want [1 2 3]", got)
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
	assertCommentContains(t, fc, "used 3 of 3 fix attempt(s), 3 since the first force-push")
}

// A timeout on a later force-push still names the fix attempts an earlier
// force-push red consumed.
func TestSelfHeal_ForcePushRedThenRewaitTimeout_NamesAttempts(t *testing.T) {
	s, fc := forcePushFixSettle(3, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // first force-push re-wait: red, fix pass 1
		forge.StateSuccess, forge.StateSuccess, // fixed head green, merge conflicts again
	}, nil, 100)
	fc.MergeErrs = []error{forge.ErrMergeConflict, forge.ErrMergeConflict}
	fc.RebaseErrs = []error{nil, forge.ErrMergeConflict}
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("resolve boom")

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1}) {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	assertSettledReason(t, s, "1", ReasonLandingFailed)
	assertCommentContains(t, fc, "used 1 of 3 fix attempt(s), 1 since the first force-push")
}

// The #5053 scenario: a conflict-resolve Box force-pushes a head whose re-wait
// goes red, a fix pass runs, and the green head merges.
func TestSelfHeal_ConflictResolveThenRed_FixPassThenMerges(t *testing.T) {
	s, _ := forcePushFixSettle(2, []forge.RollupState{
		forge.StateSuccess, forge.StateSuccess, // initial gateToGreen
		forge.StateFailure,                     // post-resolve re-wait: red
		forge.StateSuccess, forge.StateSuccess, // fixed head
	}, forge.ErrMergeConflict, 100)
	d := dispatch.NewFake()

	landing, _ := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v, want landingMerged", landing)
	}
	if got := fixPasses(d); !slices.Equal(got, []int{1}) {
		t.Errorf("fix passes = %v, want [1]", got)
	}
	assertSettledReason(t, s, "1", ReasonMerged)
}

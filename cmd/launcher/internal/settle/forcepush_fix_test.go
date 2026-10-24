package settle

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"spindrift.dev/launcher/internal/dispatch"
	"spindrift.dev/launcher/internal/forge"
)

// forcePushRedSetup scripts an immediate-mode landing whose first merge
// conflicts, so a plain rebase force-pushes and the re-wait polls states.
func forcePushRedSetup(maxFix int, states []forge.RollupState) (*Settle, *forge.Fake) {
	c := baseConfig()
	c.MergeMode = "immediate"
	c.MaxRebaseAttempts = 3
	c.MaxFixAttempts = maxFix
	fc := forge.NewFake(testDispatchLabels)
	fc.SetIssue(forge.Issue{Number: "1", Labels: []string{"agent-in-progress"}})
	fc.MergeErrs = []error{forge.ErrMergeConflict}
	fc.SetCheckStates(testPR, states)
	return newTestSettle(c, fc, fc), fc
}

func TestSelfHeal_RedAfterForcePush_FixPassThenMerges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F, S, S})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged {
		t.Fatalf("selfHeal = %v (%q), want landingMerged", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("want agent-complete; labels=%v", iss.Labels)
	}
}

func TestSelfHeal_RedAfterForcePush_BudgetExhausted_Fails(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	// Pre-merge fix pass spends the only attempt; the force-pushed head goes red.
	s, fc := forcePushRedSetup(1, []forge.RollupState{F, S, S, F})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "0 fix attempt(s) on this head; 1 of 1 shared fix attempt(s) used") {
		t.Errorf("reason = %q, want attempts used named", reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1 (no extra pass)", len(d.FixCalls))
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("want agent-failed; labels=%v", iss.Labels)
	}
}

func TestSelfHeal_RedAfterForcePush_BudgetShared(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	// 1 pre-merge pass, then one post-force-push pass; the head stays red.
	s, _ := forcePushRedSetup(2, []forge.RollupState{F, S, S, F, F})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("selfHeal = %v, want landingFailed", landing)
	}
	if !strings.Contains(reason, "1 fix attempt(s) on this head; 2 of 2 shared fix attempt(s) used") {
		t.Errorf("reason = %q", reason)
	}
	if len(d.FixCalls) != 2 {
		t.Errorf("FixCalls = %d, want 2", len(d.FixCalls))
	}
}

func TestSelfHeal_RewaitTimeout_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	s, fc := forcePushRedSetup(3, []forge.RollupState{S, S})
	s.cfg.MergePollTimeout = 0
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Errorf("landing=%v FixCalls=%d, want landingFailed and 0", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, "did not reach green") {
		t.Errorf("reason = %q, want never-green text", reason)
	}
}

func TestSelfHeal_ConflictResolveDispatchFails_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	s, fc := forcePushRedSetup(3, []forge.RollupState{S, S})
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()
	d.ResolveConflictErr = errors.New("box exploded")

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Errorf("landing=%v FixCalls=%d, want landingFailed and 0", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, "conflict-resolve dispatch failed") {
		t.Errorf("reason = %q", reason)
	}
}

// guardFlipForge lists clean files on the first ListPRFiles call (the
// pre-merge gate) and guarded files afterwards, as if a fix pass had touched
// a guarded path.
type guardFlipForge struct {
	*forge.Fake
	mu    sync.Mutex
	calls int
	late  []string
}

func (g *guardFlipForge) ListPRFiles(string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls == 1 {
		return []string{"src/main.go"}, nil
	}
	return g.late, nil
}

func guardAfterFixSetup(late []string) (*Settle, *forge.Fake) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F, S, S})
	s.cfg.MergeGuardPaths = ".github/**"
	s.pr = &guardFlipForge{Fake: fc, late: late}
	return s, fc
}

func TestSelfHeal_GuardHitByPostForcePushFix_DowngradesToManual(t *testing.T) {
	s, fc := guardAfterFixSetup([]string{".github/workflows/ci.yml"})
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingManual {
		t.Fatalf("selfHeal = %v (%q), want landingManual", landing, reason)
	}
	if len(d.FixCalls) != 1 {
		t.Errorf("FixCalls = %d, want 1", len(d.FixCalls))
	}
	if fc.Merged != "" {
		t.Errorf("Merge must not run after a guard hit; Merged=%q", fc.Merged)
	}
	iss, _ := fc.Issue("1")
	if containsLabel(iss.Labels, "agent-failed") {
		t.Errorf("must not be Failed; labels=%v", iss.Labels)
	}
	if len(fc.CommentCalls) != 1 || !strings.Contains(fc.CommentCalls[0].Body, "MERGE_GUARD_PATHS") {
		t.Errorf("want one guard comment; got %+v", fc.CommentCalls)
	}
}

func TestSelfHeal_GuardMissAfterPostForcePushFix_StillMerges(t *testing.T) {
	s, fc := guardAfterFixSetup([]string{"src/other.go"})

	landing, reason := s.selfHeal(dispatch.NewFake(), "1", 0, testPR)

	if landing != landingMerged || fc.Merged != testPR {
		t.Errorf("landing=%v (%q) Merged=%q, want merged", landing, reason, fc.Merged)
	}
}

func TestSelfHeal_RewaitTimeoutAfterFix_NamesHeadAttempts(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, _ := forcePushRedSetup(3, []forge.RollupState{S, S, F, S})
	s.cfg.MergePollTimeout = 0
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 1 {
		t.Fatalf("landing=%v FixCalls=%d (%q), want landingFailed and 1", landing, len(d.FixCalls), reason)
	}
	if !strings.Contains(reason, "1 fix attempt(s) on the force-pushed head") {
		t.Errorf("reason = %q", reason)
	}
}

func TestSelfHeal_ConflictResolve_RedThenFixPass_Merges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F, S, S})
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged || fc.Merged != testPR {
		t.Fatalf("landing=%v (%q) Merged=%q, want merged", landing, reason, fc.Merged)
	}
	if len(d.ResolveConflictCalls) != 1 || len(d.FixCalls) != 1 {
		t.Errorf("resolve=%d fix=%d, want 1 and 1", len(d.ResolveConflictCalls), len(d.FixCalls))
	}
}

func TestSelfHeal_PreflightStaleBase_RedThenFixPass_Merges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F, S, S})
	fc.MergeErrs = nil
	s.cfg.PreflightStaleBase = true
	fc.SetNeedsUpdate(testPR, true)
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged || fc.Merged != testPR {
		t.Fatalf("landing=%v (%q) Merged=%q, want merged", landing, reason, fc.Merged)
	}
	if len(fc.RebasedURLs) != 1 || len(d.FixCalls) != 1 {
		t.Errorf("rebases=%d fix=%d, want 1 and 1", len(fc.RebasedURLs), len(d.FixCalls))
	}
}

func TestSelfHeal_PreflightConflictResolve_RedThenFixPass_Merges(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F, S, S})
	fc.MergeErrs = nil
	s.cfg.PreflightStaleBase = true
	fc.SetNeedsUpdate(testPR, true)
	fc.RebaseErr = forge.ErrMergeConflict
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingMerged || fc.Merged != testPR {
		t.Fatalf("landing=%v (%q) Merged=%q, want merged", landing, reason, fc.Merged)
	}
	if len(d.ResolveConflictCalls) != 1 || len(d.FixCalls) != 1 {
		t.Errorf("resolve=%d fix=%d, want 1 and 1", len(d.ResolveConflictCalls), len(d.FixCalls))
	}
}

func TestSelfHeal_PostForcePushFixFails_SettlesFailedOnce(t *testing.T) {
	S, F := forge.StateSuccess, forge.StateFailure
	s, fc := forcePushRedSetup(2, []forge.RollupState{S, S, F})
	d := dispatch.NewFake()
	d.FixResult = dispatch.Failed(dispatch.Result{})

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed {
		t.Fatalf("landing=%v (%q), want landingFailed", landing, reason)
	}
	iss, _ := fc.Issue("1")
	if !containsLabel(iss.Labels, "agent-failed") || containsLabel(iss.Labels, "agent-complete") {
		t.Errorf("labels=%v, want agent-failed only", iss.Labels)
	}
	for _, c := range fc.CommentCalls {
		if strings.Contains(c.Body, "merge blocked after green CI") {
			t.Errorf("fix failure must not also settle as merge-blocked: %q", c.Body)
		}
	}
}

func TestSelfHeal_ConflictResolveRelayFails_NoFixPass(t *testing.T) {
	S := forge.StateSuccess
	s, fc := forcePushRedSetup(3, []forge.RollupState{S, S})
	s.cfg.OutboxDir = func(num string) string { return "/outbox/" + num }
	fc.RebaseErr = forge.ErrMergeConflict
	fc.RelayBundleErr = errors.New("relay refused")
	s = newTestSettle(s.cfg, fc, fc.AsGithubReadOnly())
	d := dispatch.NewFake()

	landing, reason := s.selfHeal(d, "1", 0, testPR)

	if landing != landingFailed || len(d.FixCalls) != 0 {
		t.Errorf("landing=%v FixCalls=%d, want landingFailed and 0", landing, len(d.FixCalls))
	}
	if !strings.Contains(reason, "relay after conflict-resolve failed") {
		t.Errorf("reason = %q", reason)
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// ADR-0089 D2 / F4: the reaper discovers every worktree authoritatively via
// `git worktree list`, then used to discard everything outside
// <cityPath>/.gc/worktrees/<rig>/ -- measured fleet-wide at 15% in scope, 85%
// silently invisible (neither reaped nor protected, no event). These tests
// pin the fix: the scope filter now recognizes the union of configured
// worktree roots (AC3: specifically the "<rig>/worktrees/<bead>" role-prompt
// convention, which the old filter discarded), and anything still outside
// that union is classified protected-with-reason rather than dropped -- so
// in-scope share is 100% regardless of whether every historical location
// family is individually enumerated.
//
// Bead IDs here stay short, single-hyphen, alnum suffixes (validBeadSuffix
// caps a suffix at 8 chars, and extractBeadIDFromWorktreeName's pairwise scan
// greedily matches the FIRST valid-shaped dash-pair, so an extra hyphen in a
// fixture bead ID silently resolves to the wrong, non-existent id instead of
// erroring -- this bit the first draft of these tests).
//
// Worktree paths are compared through pathutil.NormalizePathForCompare, the
// same normalization the reaper itself applies: a raw filepath.Join off
// t.TempDir() does not resolve macOS's /var -> /private/var symlink, so a
// byte-equality check against the reaper's (resolved) reported path fails
// even when they name the same file.

// addRoleConventionWorktree creates a worktree at "<rigRoot>/worktrees/<beadID>"
// -- the convention AC3 names as a path the pre-D2 filter discarded -- fully
// merged and pushed so it is also eligible to be REAPED outright, proving the
// new root is not just "recognized" but carries the full safety pipeline.
// Returns the NORMALIZED path, ready to compare against report fields.
func addRoleConventionWorktree(t *testing.T, rigRoot, beadID string) string {
	t.Helper()
	wtPath := filepath.Join(rigRoot, "worktrees", beadID)
	branch := "gc/" + beadID
	mustGit(t, rigRoot, "worktree", "add", "-b", branch, wtPath)
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "work ("+beadID+")")
	mustGit(t, wtPath, "push", "-u", "origin", branch)
	mustGit(t, rigRoot, "push", "origin", "--delete", branch)
	mustGit(t, rigRoot, "fetch", "--prune", "origin")
	backdateWorktreeGitFile(t, wtPath, 24*time.Hour)
	return pathutil.NormalizePathForCompare(wtPath)
}

func TestReapClosedBeadWorktrees_RigWorktreesConventionIsInScope(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRoleConventionWorktree(t, rigRoot, "ga-rolec1")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-rolec1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].Path != wt {
		t.Fatalf("Reaped = %+v, want exactly 1 (%s) -- the pre-D2 filter discarded this entire location family (AC3)\nProtected: %+v\nstderr:\n%s",
			report.Reaped, wt, report.Protected, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s still on disk after reap", wt)
	}
}

func TestReapClosedBeadWorktrees_UnrecognizedLocationIsProtectedNotDropped(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	// A location no configured root covers: a sibling of the rig root
	// entirely, not nested under it and not under any .gc/ path. This is the
	// residual ADR-0089 D5 names (ad-hoc siblings) -- the F4 fix's claim is
	// not that every such family is enumerated, but that NONE of them is
	// silently dropped any more.
	rawPath := filepath.Join(filepath.Dir(rigRoot), "elsewhere-"+filepath.Base(rigRoot))
	mustGit(t, rigRoot, "worktree", "add", "-b", "gc/ga-elsw1", rawPath)
	mustGit(t, rawPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "work")
	wtPath := pathutil.NormalizePathForCompare(rawPath)

	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-elsw1", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	rec := events.NewFake()
	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, rec, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want none -- an unrecognized location must never be deleted automatically", report.Reaped)
	}
	var protectedHere *reapDecision
	for i := range report.Protected {
		if report.Protected[i].Path == wtPath {
			protectedHere = &report.Protected[i]
		}
	}
	if protectedHere == nil {
		t.Fatalf("worktree %s does not appear in Protected at all -- this is the pre-D2 bug: silently dropped, neither reaped nor recorded.\nReaped: %+v\nProtected: %+v",
			wtPath, report.Reaped, report.Protected)
	}
	if protectedHere.Reason == "" {
		t.Fatalf("Protected entry for %s carries no reason", wtPath)
	}
	var sawSkipEvent bool
	for _, e := range rec.Events {
		if e.Type == events.BeadWorktreeReapSkipped {
			sawSkipEvent = true
		}
	}
	if !sawSkipEvent {
		t.Fatalf("no %s event recorded for the unrecognized-location path; silence must never be the signal for \"cannot see\" (ADR-0089)", events.BeadWorktreeReapSkipped)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("worktree %s removed from disk (err=%v); an unrecognized location must be protected, not acted on", wtPath, err)
	}
}

func TestReapClosedBeadWorktrees_InScopeShareIsComplete(t *testing.T) {
	// AC4's spirit without the external census tooling: every worktree git
	// worktree list knows about for this rig (besides the main tree itself)
	// ends up classified as Reaped OR Protected. None vanish silently.
	cityPath, rigRoot := initReapRig(t)
	reaped := addRoleConventionWorktree(t, rigRoot, "ga-shrr1")
	rawProtected := filepath.Join(filepath.Dir(rigRoot), "elsewhere-share")
	mustGit(t, rigRoot, "worktree", "add", "-b", "gc/ga-shrp1", rawProtected)
	mustGit(t, rawProtected, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unpushed")
	protectedUnknown := pathutil.NormalizePathForCompare(rawProtected)

	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-shrr1", Status: "closed"},
		{ID: "ga-shrp1", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, nil)

	classified := map[string]bool{}
	for _, d := range report.Reaped {
		classified[d.Path] = true
	}
	for _, d := range report.Protected {
		classified[d.Path] = true
	}
	if !classified[reaped] {
		t.Errorf("%s classified as neither reaped nor protected", reaped)
	}
	if !classified[protectedUnknown] {
		t.Errorf("%s classified as neither reaped nor protected -- the F4 silent-drop bug", protectedUnknown)
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// ADR-0089 D1: releaseOrphanedPoolAssignment must not return a bead to the
// pool while silently abandoning a worktree. These tests pin the ACTUAL
// behavior change over the first pass (which only reported the condition,
// vp-vt3wf-shaped): the release now evaluates ADR-0088's containment
// predicate (HEAD reachable from its own origin branch, AND clean tree) and
// stamps gc.salvage_required=1 + emits bead.worktree.salvage_required when it
// does not hold -- without ever blocking or rolling back the release itself.

// custodyTestWorktree creates a per-bead worktree on its own branch under
// rigRoot (a real git repo with a real "origin" remote, via initReapRig), then
// optionally pushes it to origin so tests can control whether the predicate's
// ancestor check holds. Returns the worktree path and branch name.
func custodyTestWorktree(t *testing.T, rigRoot, beadID string, push bool) (path, branch string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), beadID)
	branch = "gc/" + beadID
	mustGit(t, rigRoot, "worktree", "add", "-b", branch, wtPath)
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "work ("+beadID+")")
	if push {
		mustGit(t, wtPath, "push", "-u", "origin", branch)
	}
	return wtPath, branch
}

func custodyTestBead(id, workDir, branch string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to":                 "worker",
			beadmeta.WorkDirMetadataKey:    workDir,
			beadmeta.WorkBranchMetadataKey: branch,
		},
	}
}

func TestReleaseOrphanedPoolAssignment_StampsSalvageRequiredOnUnpushedWork(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wtPath, branch := custodyTestWorktree(t, rigRoot, "ga-unpushed1", true)
	// An extra LOCAL commit after the push -- HEAD is now ahead of
	// origin/<branch>, so HEAD is not reachable from it (not an ancestor).
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unpushed follow-up")

	store := beads.NewMemStore()
	wb, err := store.Create(custodyTestBead("ga-unpushed1", wtPath, branch))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	if !releaseOrphanedPoolAssignment(store, wb, false, rec) {
		t.Fatalf("release reported failure; ADR-0089 D1 must never block release on custody")
	}

	got, err := store.Get(wb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("release did not happen: status=%q assignee=%q", got.Status, got.Assignee)
	}
	if got.Metadata[beadmeta.SalvageRequiredMetadataKey] != "1" {
		t.Fatalf("%s = %q, want \"1\" (AC1: unpublished work must stamp)", beadmeta.SalvageRequiredMetadataKey, got.Metadata[beadmeta.SalvageRequiredMetadataKey])
	}
	var found bool
	for _, e := range rec.Events {
		if e.Type == events.BeadWorktreeSalvageRequired && e.Subject == wb.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event recorded for %s; events=%+v", events.BeadWorktreeSalvageRequired, wb.ID, rec.Events)
	}
}

func TestReleaseOrphanedPoolAssignment_NoStampWhenContainedAndClean(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wtPath, branch := custodyTestWorktree(t, rigRoot, "ga-clean1", true)

	store := beads.NewMemStore()
	wb, err := store.Create(custodyTestBead("ga-clean1", wtPath, branch))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	if !releaseOrphanedPoolAssignment(store, wb, false, rec) {
		t.Fatalf("release reported failure")
	}

	got, err := store.Get(wb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, ok := got.Metadata[beadmeta.SalvageRequiredMetadataKey]; ok {
		t.Fatalf("%s = %q, want absent (AC2: no false positives on a contained+clean worktree)", beadmeta.SalvageRequiredMetadataKey, v)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("unexpected events on a contained+clean release: %+v", rec.Events)
	}
}

func TestReleaseOrphanedPoolAssignment_StampsOnDirtyTree(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wtPath, branch := custodyTestWorktree(t, rigRoot, "ga-dirty1", true)
	// Pushed (HEAD == origin/branch), but the tree itself is dirty.
	if err := os.WriteFile(filepath.Join(wtPath, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	store := beads.NewMemStore()
	wb, err := store.Create(custodyTestBead("ga-dirty1", wtPath, branch))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	releaseOrphanedPoolAssignment(store, wb, false, rec)

	got, err := store.Get(wb.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.SalvageRequiredMetadataKey] != "1" {
		t.Fatalf("%s = %q, want \"1\" (a dirty tree is never disposable even if fully pushed)", beadmeta.SalvageRequiredMetadataKey, got.Metadata[beadmeta.SalvageRequiredMetadataKey])
	}
}

func TestReleaseOrphanedPoolAssignment_NoWorkDirNoCustodyCheck(t *testing.T) {
	store := beads.NewMemStore()
	wb, err := store.Create(beads.Bead{
		ID: "ga-nowd1", Status: "in_progress", Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	if !releaseOrphanedPoolAssignment(store, wb, false, rec) {
		t.Fatalf("release reported failure")
	}
	got, _ := store.Get(wb.ID)
	if _, ok := got.Metadata[beadmeta.SalvageRequiredMetadataKey]; ok {
		t.Fatalf("stamped salvage_required on a bead with no recorded worktree at all")
	}
	if len(rec.Events) != 0 {
		t.Fatalf("unexpected events with no work_dir: %+v", rec.Events)
	}
}

func TestReleaseOrphanedPoolAssignment_MissingWorktreeOnDiskNoCustodyCheck(t *testing.T) {
	store := beads.NewMemStore()
	wb, err := store.Create(custodyTestBead("ga-gone1", filepath.Join(t.TempDir(), "never-existed"), "gc/ga-gone1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	if !releaseOrphanedPoolAssignment(store, wb, false, rec) {
		t.Fatalf("release reported failure")
	}
	got, _ := store.Get(wb.ID)
	if _, ok := got.Metadata[beadmeta.SalvageRequiredMetadataKey]; ok {
		t.Fatalf("stamped salvage_required for a work_dir that no longer exists on disk -- absence is not ambiguity")
	}
}

func TestReleaseOrphanedPoolAssignment_LegacyWorkDirKeyFallback(t *testing.T) {
	_, rigRoot := initReapRig(t)
	wtPath, branch := custodyTestWorktree(t, rigRoot, "ga-legacy1", true)
	mustGit(t, wtPath, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unpushed")

	store := beads.NewMemStore()
	wb, err := store.Create(beads.Bead{
		ID: "ga-legacy1", Status: "in_progress", Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.routed_to":                    "worker",
			beadmeta.LegacyWorkDirMetadataKey: wtPath,
			beadmeta.WorkBranchMetadataKey:    branch,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()

	releaseOrphanedPoolAssignment(store, wb, false, rec)

	got, _ := store.Get(wb.ID)
	if got.Metadata[beadmeta.SalvageRequiredMetadataKey] != "1" {
		t.Fatalf("legacy work_dir key was not honored as a fallback; %s = %q", beadmeta.SalvageRequiredMetadataKey, got.Metadata[beadmeta.SalvageRequiredMetadataKey])
	}
}

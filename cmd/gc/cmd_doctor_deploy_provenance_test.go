package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestBuildDoctorChecks_DeployProvenanceReceivesLinkedCommit pins the wiring
// the check cannot work without.
//
// Every gc this repo builds is built -buildvcs=false — `make build` (and so
// `make install`) and `make artifact` both pass it, because the toolchain
// stamps an enclosing repository's commit when the build runs from a linked
// worktree (ga-u7fb). debug.ReadBuildInfo therefore reports no vcs.revision in
// any deployed gc, and the linker-injected `commit` is the only revision the
// binary carries. Registering the check without it degrades every run to
// "provenance not asserted", silently retiring the lineage assertion the
// check's own documentation calls its load-bearing half — which is what the
// deployed fleet did from 2026-08-07 until this wiring landed (ga-bq4qs).
func TestBuildDoctorChecks_DeployProvenanceReceivesLinkedCommit(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	captureBinaryDivergencePID(t)

	got := ""
	seen := false
	old := newDoctorDeployProvenanceCheck
	newDoctorDeployProvenanceCheck = func(linkedRevision string) *doctor.DeployProvenanceCheck {
		got, seen = linkedRevision, true
		return doctor.NewDeployProvenanceCheck(linkedRevision)
	}
	t.Cleanup(func() { newDoctorDeployProvenanceCheck = old })

	// A sentinel only `commit` can carry, so the assertions below distinguish
	// it from `date` and `version` (all three are "unknown"/"dev" otherwise).
	oldCommit := commit
	commit = "seam-sentinel-commit"
	wantCommit := commit
	t.Cleanup(func() { commit = oldCommit })

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	buildDoctorChecks(doctorCityDir(t), cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	if !seen {
		t.Fatal("deploy-provenance check was never constructed")
	}
	// Assert IDENTITY against a value only `commit` can hold, not inequality
	// against `commit` itself. In a test binary the ldflags are absent, so
	// `commit`, `version` and `date` all fall back to their placeholders and
	// `commit` is literally "unknown" -- so `got != commit` is satisfied by
	// ANY non-empty build-metadata variable. Substituting `date` for `commit`
	// at the registration site passes this test unchanged; substituting "" is
	// the only mutation it catches. The guard then proves "a non-empty build
	// variable is passed", which is weaker than its own name claims.
	//
	// Pinning a sentinel through the real `commit` variable closes that: only
	// the variable this check is contracted to read can carry it.
	if got != wantCommit {
		t.Errorf("linked revision = %q, want the value of this binary's `commit` variable %q", got, wantCommit)
	}
	if got == date || got == version {
		t.Errorf("linked revision = %q, which is the value of `date`/`version`, not `commit`: the registration site is passing the wrong build variable", got)
	}
}

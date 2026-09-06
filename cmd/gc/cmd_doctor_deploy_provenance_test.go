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

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	buildDoctorChecks(doctorCityDir(t), cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	if !seen {
		t.Fatal("deploy-provenance check was never constructed")
	}
	if got != commit {
		t.Errorf("linked revision = %q, want this binary's commit %q", got, commit)
	}
}

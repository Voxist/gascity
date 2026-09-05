package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Suspension is the third input to the desired state, and the demand
// snapshot's two fingerprints are blind to it.
//
// `gc resume` writes .gc/runtime/suspension-state.json. On a supervisor-managed
// city apiClient returns nil (no standalone [api] port), so the CLI mutates the
// file directly and never pokes the controller: the resume is observed on a
// patrol tick or not at all. A suspended city has no live sessions and no
// ready-set movement, so the session and ready-demand fingerprints are both
// stable across the resume, and the cached — empty, because
// buildDesiredStateWithSessionBeadsAt returns DesiredStateResult{} while the
// city is suspended — snapshot was reused until runtimeDemandSnapshotBackstopMaxAge
// expired. `gc resume` therefore took up to five minutes to restart the agents
// it had just resumed, which is what TestE2E_SuspendResume_City times out on.
func TestDemandSnapshotRebuildsWhenTheCityIsResumed(t *testing.T) {
	cr := bindingFingerprintRuntime(t, beads.NewMemStore(), beads.NewMemStore())
	if !cr.demandSnapshotsEnabled() {
		t.Fatal("the fixture is not event-backed, so it does not exercise the cached-snapshot path this test is about")
	}

	suspended := true
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cr.cityPath, &suspended); err != nil {
		t.Fatalf("suspending the city: %v", err)
	}

	// A snapshot taken while the city is suspended: fresh, no sessions, and
	// nothing in the ready set.
	const sessionFingerprint = "no-open-sessions"
	cr.demandSnapshot = &runtimeDemandSnapshot{
		createdAt:             time.Now(),
		sessionFingerprint:    sessionFingerprint,
		suspensionFingerprint: cr.suspensionSnapshotFingerprint(),
	}

	// Control: while the suspension state is unchanged the snapshot is still
	// reusable, so a failure below is the resume and not a cache that never
	// engages.
	if cr.shouldRefreshDemandSnapshot("patrol", false, sessionFingerprint, cr.suspensionSnapshotFingerprint()) {
		t.Fatal("an unchanged suspension state rebuilt the demand snapshot; the cache never engages, so the assertion below would pass for the wrong reason")
	}

	resumed := false
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cr.cityPath, &resumed); err != nil {
		t.Fatalf("resuming the city: %v", err)
	}

	if !cr.shouldRefreshDemandSnapshot("patrol", false, sessionFingerprint, cr.suspensionSnapshotFingerprint()) {
		t.Fatalf("gc resume left the cached demand snapshot in place, so the controller keeps serving the suspended city's empty desired state for up to %v before it restarts anything", runtimeDemandSnapshotBackstopMaxAge)
	}
}

// A rig suspend/resume moves the same file and changes the desired state the
// same way, so it must invalidate the snapshot too.
func TestDemandSnapshotRebuildsWhenARigIsResumed(t *testing.T) {
	cr := bindingFingerprintRuntime(t, beads.NewMemStore(), beads.NewMemStore())

	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "alpha", &suspended); err != nil {
		t.Fatalf("suspending rig alpha: %v", err)
	}
	const sessionFingerprint = "no-open-sessions"
	cr.demandSnapshot = &runtimeDemandSnapshot{
		createdAt:             time.Now(),
		sessionFingerprint:    sessionFingerprint,
		suspensionFingerprint: cr.suspensionSnapshotFingerprint(),
	}

	resumed := false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cr.cityPath, "alpha", &resumed); err != nil {
		t.Fatalf("resuming rig alpha: %v", err)
	}

	if !cr.shouldRefreshDemandSnapshot("patrol", false, sessionFingerprint, cr.suspensionSnapshotFingerprint()) {
		t.Error("gc rig resume left the cached demand snapshot in place")
	}
}

// The fingerprint distinguishes the three states a preference can be in —
// absent, explicitly resumed, explicitly suspended — because "no preference"
// and "explicitly resumed" merge differently with suspended_on_start.
func TestSuspensionStateFingerprintSeparatesUnsetFromResumed(t *testing.T) {
	unset := suspensionStateFingerprint(suspensionstate.State{})

	no, yes := false, true
	resumed := suspensionStateFingerprint(suspensionstate.State{City: suspensionstate.Override{Suspended: &no}})
	suspended := suspensionStateFingerprint(suspensionstate.State{City: suspensionstate.Override{Suspended: &yes}})

	if unset == resumed {
		t.Error("an absent city preference hashes the same as an explicit resume; the two merge differently with suspended_on_start")
	}
	if resumed == suspended {
		t.Error("explicit resume and explicit suspend hash the same")
	}

	// Rig identity has to be part of the hash: suspending a different rig is a
	// different desired state.
	alpha := suspensionStateFingerprint(suspensionstate.State{Rigs: map[string]suspensionstate.Override{"alpha": {Suspended: &yes}}})
	beta := suspensionStateFingerprint(suspensionstate.State{Rigs: map[string]suspensionstate.Override{"beta": {Suspended: &yes}}})
	if alpha == beta {
		t.Error("suspending rig alpha hashes the same as suspending rig beta")
	}
}

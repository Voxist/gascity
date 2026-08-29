package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// partialSnapshotFixture builds the one shape that separates the two outcomes:
// a live pool-routed claim whose assignee maps to no open session bead. With a
// COMPLETE snapshot that claim is genuinely orphaned and must be released; with
// a NOT-OBSERVED snapshot it must be left alone. Both arms below run this same
// fixture, so the control proves the release machinery is armed and the guard
// arm proves the fix disarms it for the right reason.
func partialSnapshotFixture(t *testing.T) (beads.Store, *config.City, beads.Bead) {
	t.Helper()
	work := beads.NewMemStore()
	wb, err := work.Create(beads.Bead{
		ID:       "ga-claimed",
		Title:    "pool-routed work held by a live seat",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-session",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := work.Update(wb.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	wb.Status = inProgress
	cfg := &config.City{Agents: []config.Agent{
		{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
	}}
	return work, cfg, wb
}

// runReleaseGate reproduces the beadReconcileTick wiring verbatim: load the
// snapshot through the real accessor, fold its partial flag into the result the
// way the tick does, then put that result through the release gate. Asserting on
// the RELEASES rather than on the flag keeps the premise under test out of the
// assertion.
func runReleaseGate(t *testing.T, cr *CityRuntime, work beads.Store, wb beads.Bead) []releasedPoolAssignment {
	t.Helper()
	snapshot, sessionQueryPartial := cr.loadSessionBeadSnapshotWithPartial()
	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		ScaleCheckCounts:      map[string]int{"worker": 0},
		AssignedWorkBeads:     []beads.Bead{wb},
		AssignedWorkStores:    []beads.Store{work},
		AssignedWorkStoreRefs: []string{""},
	}
	result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	return releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
		work, cr.sessionsBeadStore(), cr.cfg, cr.cityPath, snapshot.OpenInfos(), result, nil,
	)
}

// TestLoadSessionBeadSnapshotWithPartial_NilSessionsStoreHoldsTheOrphanReleaseGate
// is the ADR-0091 P4 behavioral guard for site 3.
//
// A nil sessions store means the session-bead read NEVER HAPPENED. Reporting
// that as a complete observation is indistinguishable from "the fleet is idle",
// and the orphan-release gate acts on the difference: it reopens every
// pool-routed claim whose assignee is absent from the snapshot. sessionBeadSnapshot
// is nil-safe (OpenInfos returns an empty slice, no panic), so nothing downstream
// can tell the two apart — which is why the distinction has to be carried by the
// partial flag at the boundary.
//
// The sessions store goes nil independently of the work store once
// [storage.classes] relocates the sessions class (resolveClassStore honors the
// storage routes), so the beadReconcileTick `if store == nil { return }` guard —
// which tests the WORK store — does not stand in front of this path.
func TestLoadSessionBeadSnapshotWithPartial_NilSessionsStoreHoldsTheOrphanReleaseGate(t *testing.T) {
	t.Run("control: a complete snapshot with no open sessions DOES release", func(t *testing.T) {
		work, cfg, wb := partialSnapshotFixture(t)
		cr := &CityRuntime{
			cityPath:            t.TempDir(),
			cityName:            "relocation-city",
			cfg:                 cfg,
			sp:                  runtime.NewFake(),
			standaloneCityStore: work,
			sessionDrains:       newDrainTracker(),
			rec:                 events.Discard,
			stdout:              &bytes.Buffer{},
			stderr:              &bytes.Buffer{},
		}
		// No storage routes: the sessions class falls through to the work store,
		// the read succeeds, and the snapshot is genuinely complete.
		if cr.sessionsBeadStore().Store == nil {
			t.Fatal("control arm wants a served sessions store, got nil")
		}
		if released := runReleaseGate(t, cr, work, wb); len(released) != 1 {
			t.Fatalf("control released %d claims, want 1 — the fixture is not destructive, so the guard arm below would pass vacuously", len(released))
		}
	})

	t.Run("guard: a nil sessions store releases nothing and reports itself", func(t *testing.T) {
		work, cfg, wb := partialSnapshotFixture(t)
		var stderr bytes.Buffer
		cr := &CityRuntime{
			cityPath: t.TempDir(),
			cityName: "relocation-city",
			cfg:      cfg,
			sp:       runtime.NewFake(),
			// The work store is healthy; only the relocated sessions class is
			// unserved. This is the shape the tick-level work-store guard misses.
			standaloneCityStore: work,
			storageRoutes: &storageRoutes{
				stores: map[coordclass.Class]beads.Store{coordclass.ClassSessions: nil},
			},
			sessionDrains: newDrainTracker(),
			rec:           events.Discard,
			logPrefix:     "relocation-city",
			stdout:        &bytes.Buffer{},
			stderr:        &stderr,
		}
		if cr.sessionsBeadStore().Store != nil {
			t.Fatal("guard arm wants an unserved sessions store, got a served one")
		}

		released := runReleaseGate(t, cr, work, wb)
		if len(released) != 0 {
			t.Fatalf("released %d pool claims on a session read that never happened, want 0", len(released))
		}
		got, err := work.Get(wb.ID)
		if err != nil {
			t.Fatalf("Get work after guarded tick: %v", err)
		}
		if got.Status != "in_progress" || got.Assignee != "worker-session" {
			t.Fatalf("unobserved sessions store released live work: status=%q assignee=%q", got.Status, got.Assignee)
		}
		// ADR-0091 P5: non-observation must be REPORTED, not merely handled.
		if !strings.Contains(stderr.String(), "sessions bead store unavailable") {
			t.Fatalf("stderr = %q, want a non-observation report", stderr.String())
		}
	})
}

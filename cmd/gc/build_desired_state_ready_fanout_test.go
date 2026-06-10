package main

import (
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// makeSleepySession builds an open, asleep pool session bead whose assignee
// identity (its session_name) drives one entry in the controller-demand
// ready-assignee set.
func makeSleepySession(t *testing.T, store beads.Store, sessionName string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "session " + sessionName,
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create session bead %q: %v", sessionName, err)
	}
	return b
}

// makeReadyWork builds an open ready task assigned to assignee.
func makeReadyWork(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
	})
	if err != nil {
		t.Fatalf("create work bead %q: %v", title, err)
	}
	return b
}

func sortedIDs(in []beads.Bead) []string {
	ids := make([]string, 0, len(in))
	for _, b := range in {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestCollectAssignedWorkBeads_ReadyFanOutCollapsesToOneReadPerScope is the
// core P2.5 (#3218) guarantee: for a scope with K ready-assignees the
// desired-state builder must issue exactly ONE live Ready read against the
// backing store, not K. The former per-assignee fan-out scaled as
// stores × open sessions and was the dominant idle bd-subprocess amplifier.
func TestCollectAssignedWorkBeads_ReadyFanOutCollapsesToOneReadPerScope(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}

	const k = 4
	var sessions []beads.Bead
	var wantIDs []string
	for i := 0; i < k; i++ {
		name := "worker-" + string(rune('a'+i))
		sessions = append(sessions, makeSleepySession(t, store, name))
		work := makeReadyWork(t, store, "ready for "+name, name)
		wantIDs = append(wantIDs, work.ID)
	}
	// Ready work for an assignee that has no session bead must NOT be collected:
	// it is outside the demand assignee set. This guards the in-memory filter.
	makeReadyWork(t, store, "ready for stranger", "worker-no-session")
	// Ready unassigned work is never assigned work and must be excluded too.
	makeReadyWork(t, store, "ready unassigned", "")

	snapshot := newSessionBeadSnapshot(sessions)

	got, _, _, partial := collectAssignedWorkBeadsWithStores(&config.City{}, store, nil, nil, snapshot)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}

	// Exactly one live Ready read per scope — the collapse invariant.
	if len(store.readyQueries) != 1 {
		t.Fatalf("Ready read count = %d (queries=%#v), want exactly 1 per scope for %d assignees", len(store.readyQueries), store.readyQueries, k)
	}
	if store.readyQueries[0].Assignee != "" {
		t.Fatalf("collapsed Ready query carried an Assignee filter %q, want a single unfiltered scope read", store.readyQueries[0].Assignee)
	}
	if store.readyQueries[0].TierMode != beads.TierBoth {
		t.Fatalf("collapsed Ready query TierMode = %v, want TierBoth", store.readyQueries[0].TierMode)
	}

	sort.Strings(wantIDs)
	if gotIDs := sortedIDs(got); !equalStringSlices(gotIDs, wantIDs) {
		t.Fatalf("collected ready IDs = %v, want exactly the per-assignee set %v", gotIDs, wantIDs)
	}
}

// TestPartitionReadyByAssignee_MatchesPerAssigneeFilter verifies the in-memory
// partition reproduces, bead-for-bead, what the old per-assignee
// Ready(Assignee=…) fan-out would have returned from the same ready set:
// only beads whose assignee is in the demand set, capped per assignee, in
// assignee order, and never duplicated.
func TestPartitionReadyByAssignee_MatchesPerAssigneeFilter(t *testing.T) {
	ready := []beads.Bead{
		{ID: "a1", Assignee: "alice"},
		{ID: "b1", Assignee: "bob"},
		{ID: "a2", Assignee: "alice"},
		{ID: "x1", Assignee: "stranger"}, // not in demand set
		{ID: "c1", Assignee: "carol"},
		{ID: "u1", Assignee: ""}, // unassigned, never attributed
		{ID: "a3", Assignee: "alice"},
	}
	assignees := []string{"alice", "bob", "carol"}

	t.Run("unbounded preserves order and excludes strangers/unassigned", func(t *testing.T) {
		got := partitionReadyByAssignee(ready, assignees, 0)
		want := []string{"a1", "a2", "a3", "b1", "c1"}
		if gotIDs := idSlice(got); !equalStringSlices(gotIDs, want) {
			t.Fatalf("got %v, want %v (assignee order, strangers/unassigned excluded)", gotIDs, want)
		}
	})

	t.Run("per-assignee limit caps each group like the old per-query Limit", func(t *testing.T) {
		got := partitionReadyByAssignee(ready, assignees, 2)
		// alice capped at 2 (a1,a2 — a3 dropped); bob/carol unaffected.
		want := []string{"a1", "a2", "b1", "c1"}
		if gotIDs := idSlice(got); !equalStringSlices(gotIDs, want) {
			t.Fatalf("got %v, want %v (alice capped at 2)", gotIDs, want)
		}
	})

	t.Run("empty inputs return nil", func(t *testing.T) {
		if got := partitionReadyByAssignee(nil, assignees, 0); got != nil {
			t.Fatalf("nil ready: got %#v, want nil", got)
		}
		if got := partitionReadyByAssignee(ready, nil, 0); got != nil {
			t.Fatalf("nil assignees: got %#v, want nil", got)
		}
	})
}

func idSlice(in []beads.Bead) []string {
	out := make([]string, 0, len(in))
	for _, b := range in {
		out = append(out, b.ID)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

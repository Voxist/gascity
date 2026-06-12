package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// gateCallCountingStore counts the store round-trips made by a gate
// evaluation. The flat-membership gate must stay within a fixed number of
// List calls regardless of wisp-tree size and must never issue the per-node
// Get/DepList calls of the historical O(tree) walk (incident 12 / vc-6qh1).
type gateCallCountingStore struct {
	beads.Store
	lists    int
	gets     int
	depLists int
}

func (s *gateCallCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(q)
}

func (s *gateCallCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

func (s *gateCallCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depLists++
	return s.Store.DepList(id, direction)
}

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating bead %q: %v", b.Title, err)
	}
	return created
}

func mustClose(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("closing bead %s: %v", id, err)
	}
}

// TestHasOpenWorkStrictFlatEvaluation is the correctness table for the
// flat-membership open-work gate: every blocking and non-blocking shape the
// historical walk handled must evaluate identically through the flat path.
func TestHasOpenWorkStrictFlatEvaluation(t *testing.T) {
	const scoped = "digest"
	orderRunLabel := "order-run:" + scoped

	cases := []struct {
		name string
		seed func(t *testing.T, store beads.Store)
		want bool
	}{
		{
			name: "no order beads",
			seed: func(_ *testing.T, _ beads.Store) {},
			want: false,
		},
		{
			name: "open tracking bead blocks",
			seed: func(t *testing.T, store beads.Store) {
				mustCreate(t, store, beads.Bead{
					Title:  "order:" + scoped,
					Labels: []string{orderRunLabel, labelOrderTracking},
				})
			},
			want: true,
		},
		{
			name: "orphan open wisp root with no members does not block",
			seed: func(t *testing.T, store beads.Store) {
				mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
			},
			want: false,
		},
		{
			name: "root-only wisp candidate blocks",
			seed: func(t *testing.T, store beads.Store) {
				mustCreate(t, store, beads.Bead{
					Title:    "wisp-digest",
					Labels:   []string{orderRunLabel},
					Metadata: map[string]string{"gc.kind": "wisp"},
				})
			},
			want: true,
		},
		{
			name: "open stamped member blocks",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				mustCreate(t, store, beads.Bead{
					Title:    "step",
					Metadata: map[string]string{"gc.root_bead_id": root.ID},
				})
			},
			want: true,
		},
		{
			name: "all-closed stamped members do not block",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				member := mustCreate(t, store, beads.Bead{
					Title:    "step",
					Metadata: map[string]string{"gc.root_bead_id": root.ID},
				})
				mustClose(t, store, member.ID)
			},
			want: false,
		},
		{
			name: "open unstamped ParentID child blocks",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				mustCreate(t, store, beads.Bead{
					Title:    "step",
					ParentID: root.ID,
				})
			},
			want: true,
		},
		{
			name: "deep unstamped open chain blocks",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				c1 := mustCreate(t, store, beads.Bead{Title: "c1", ParentID: root.ID})
				mustCreate(t, store, beads.Bead{Title: "c2", ParentID: c1.ID})
			},
			want: true,
		},
		{
			name: "open child under closed stamped intermediate blocks",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				mid := mustCreate(t, store, beads.Bead{
					Title:    "mid",
					Metadata: map[string]string{"gc.root_bead_id": root.ID},
				})
				mustClose(t, store, mid.ID)
				mustCreate(t, store, beads.Bead{Title: "leaf", ParentID: mid.ID})
			},
			want: true,
		},
		{
			name: "lone transient nudge does not block",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				mustCreate(t, store, beads.Bead{
					Title:    "nudge:agent",
					Type:     nudgeBeadType,
					ParentID: root.ID,
					Labels:   []string{nudgeBeadLabel},
				})
			},
			want: false,
		},
		{
			name: "real work under skipped nudge blocks",
			seed: func(t *testing.T, store beads.Store) {
				root := mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				nudge := mustCreate(t, store, beads.Bead{
					Title:    "nudge:agent",
					Type:     nudgeBeadType,
					ParentID: root.ID,
					Labels:   []string{nudgeBeadLabel},
				})
				mustCreate(t, store, beads.Bead{Title: "real work", ParentID: nudge.ID})
			},
			want: true,
		},
		{
			name: "unrelated open beads do not block",
			seed: func(t *testing.T, store beads.Store) {
				mustCreate(t, store, beads.Bead{
					Title:  "mol-digest",
					Type:   "molecule",
					Labels: []string{orderRunLabel},
				})
				other := mustCreate(t, store, beads.Bead{
					Title:  "mol-other",
					Type:   "molecule",
					Labels: []string{"order-run:other"},
				})
				mustCreate(t, store, beads.Bead{Title: "other step", ParentID: other.ID})
				mustCreate(t, store, beads.Bead{Title: "free-floating task"})
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			tc.seed(t, store)
			ad := &memoryOrderDispatcher{}
			got, err := ad.hasOpenWorkStrict(store, scoped)
			if err != nil {
				t.Fatalf("hasOpenWorkStrict: %v", err)
			}
			if got != tc.want {
				t.Errorf("hasOpenWorkStrict = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasOpenWorkStrictBoundedStoreCalls is the incident-12 cost guard: the
// gate must evaluate an N-bead wisp tree in a fixed number of List calls
// (roots + membership + one whole-scope open scan) with zero per-node
// Get/DepList round-trips — for both the blocking (open chain) and the
// idle-confirmation (all-closed tree) directions. The historical walk issued
// O(N) subprocess calls for the idle case, which is exactly the load shape
// that blew past the gate timeout under Dolt contention (#2893, vc-6qh1).
func TestHasOpenWorkStrictBoundedStoreCalls(t *testing.T) {
	const n = 60
	const maxLists = 3

	build := func(t *testing.T, store beads.Store, closeChain bool) {
		root := mustCreate(t, store, beads.Bead{
			Title:  "mol-digest",
			Type:   "molecule",
			Labels: []string{"order-run:digest"},
		})
		parent := root.ID
		ids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			c := mustCreate(t, store, beads.Bead{Title: "step", ParentID: parent})
			ids = append(ids, c.ID)
			parent = c.ID
		}
		if closeChain {
			for _, id := range ids {
				mustClose(t, store, id)
			}
		}
	}

	for _, tc := range []struct {
		name       string
		closeChain bool
		want       bool
	}{
		{name: "open chain blocks", closeChain: false, want: true},
		{name: "all-closed tree confirms idle", closeChain: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counting := &gateCallCountingStore{Store: beads.NewMemStore()}
			build(t, counting.Store, tc.closeChain)

			ad := &memoryOrderDispatcher{}
			got, err := ad.hasOpenWorkStrict(counting, "digest")
			if err != nil {
				t.Fatalf("hasOpenWorkStrict: %v", err)
			}
			if got != tc.want {
				t.Errorf("hasOpenWorkStrict = %v, want %v", got, tc.want)
			}
			if counting.lists > maxLists {
				t.Errorf("gate issued %d List calls for a %d-bead tree, want <= %d (flat evaluation must not scale store calls with tree size)", counting.lists, n, maxLists)
			}
			if counting.gets != 0 || counting.depLists != 0 {
				t.Errorf("gate issued %d Get / %d DepList calls, want 0/0 (no per-node walk)", counting.gets, counting.depLists)
			}
		})
	}
}

// TestHasOpenWorkStrictTruncatedScanFallsBackToWalk pins the overflow rule:
// when the whole-scope open scan hits its limit, the snapshot is incomplete
// and the gate must fall back to the authoritative walk instead of declaring
// the root idle from partial data — single-flight is never weakened by the
// flat path's bound.
func TestHasOpenWorkStrictTruncatedScanFallsBackToWalk(t *testing.T) {
	prev := orderGateFlatScanLimit
	orderGateFlatScanLimit = 2
	defer func() { orderGateFlatScanLimit = prev }()

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{
		Title:  "mol-digest",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	// Open work reachable only through a closed UNSTAMPED intermediate: the
	// truncated in-memory closure cannot see it, so only the walk fallback
	// can find it.
	mid := mustCreate(t, store, beads.Bead{Title: "mid", ParentID: root.ID})
	mustCreate(t, store, beads.Bead{Title: "leaf", ParentID: mid.ID})
	mustClose(t, store, mid.ID)
	// Unrelated open beads push the open scan past the truncation limit.
	for i := 0; i < 4; i++ {
		mustCreate(t, store, beads.Bead{Title: "noise"})
	}

	ad := &memoryOrderDispatcher{}
	got, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !got {
		t.Fatal("truncated open scan must fall back to the walk and find the open leaf under the closed unstamped intermediate")
	}
}

// TestHasOpenWorkStrictAncestorBudgetFallsBackToWalk pins the second
// overflow rule: when ancestor resolution would exceed its Get budget, the
// gate falls back to the authoritative walk instead of guessing — the open
// descendant must still block.
func TestHasOpenWorkStrictAncestorBudgetFallsBackToWalk(t *testing.T) {
	prevBudget := orderGateFlatAncestorGetBudget
	orderGateFlatAncestorGetBudget = 0
	defer func() { orderGateFlatAncestorGetBudget = prevBudget }()

	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{
		Title:  "mol-digest",
		Type:   "molecule",
		Labels: []string{"order-run:digest"},
	})
	mid := mustCreate(t, store, beads.Bead{Title: "mid", ParentID: root.ID})
	mustCreate(t, store, beads.Bead{Title: "leaf", ParentID: mid.ID})
	mustClose(t, store, mid.ID)

	ad := &memoryOrderDispatcher{}
	got, err := ad.hasOpenWorkStrict(store, "digest")
	if err != nil {
		t.Fatalf("hasOpenWorkStrict: %v", err)
	}
	if !got {
		t.Fatal("exhausted ancestor budget must fall back to the walk and find the open leaf")
	}
}

// TestGateFailOpenEmitsTypedEvent pins the incident-12 tripwire: every
// fail-open (idempotent order dispatched on gate timeout) must emit a typed
// order.gate_timeout_fail_open event carrying order, scope, and elapsed — a
// rising count is the early warning that store contention is degrading gate
// evaluation. Fail-closed outcomes and non-timeout errors must NOT emit it.
func TestGateFailOpenEmitsTypedEvent(t *testing.T) {
	slowGate := func() (bool, error) {
		time.Sleep(200 * time.Millisecond)
		return false, nil
	}
	_, timeoutErr := gateOpenWorkBounded(context.Background(), time.Millisecond, "digest:rig:demo", slowGate)
	if timeoutErr == nil || !errors.Is(timeoutErr, errGateTimeout) {
		t.Fatalf("expected gate timeout error, got %v", timeoutErr)
	}

	t.Run("idempotent timeout fails open and emits", func(t *testing.T) {
		rec := events.NewFake()
		m := &memoryOrderDispatcher{stderr: lockedStderr(io.Discard), rec: rec}
		a := orders.Order{Name: "digest", Rig: "demo", Idempotent: true}
		if m.gateFailClosed(context.Background(), a, a.ScopedName(), timeoutErr) {
			t.Fatal("idempotent order on gate timeout should fail OPEN")
		}
		var got []events.Event
		for _, e := range rec.Events {
			if e.Type == events.OrderGateTimeoutFailOpen {
				got = append(got, e)
			}
		}
		if len(got) != 1 {
			t.Fatalf("want exactly 1 %s event, got %d", events.OrderGateTimeoutFailOpen, len(got))
		}
		e := got[0]
		if e.Subject != "digest:rig:demo" {
			t.Errorf("event subject = %q, want %q", e.Subject, "digest:rig:demo")
		}
		var payload events.OrderGateTimeoutFailOpenPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decoding payload: %v", err)
		}
		if payload.Order != "digest" {
			t.Errorf("payload.Order = %q, want %q", payload.Order, "digest")
		}
		if payload.Scope != "demo" {
			t.Errorf("payload.Scope = %q, want %q", payload.Scope, "demo")
		}
		if payload.ElapsedSeconds <= 0 {
			t.Errorf("payload.ElapsedSeconds = %v, want > 0", payload.ElapsedSeconds)
		}
	})

	t.Run("non-idempotent timeout fails closed without event", func(t *testing.T) {
		rec := events.NewFake()
		m := &memoryOrderDispatcher{stderr: lockedStderr(io.Discard), rec: rec}
		a := orders.Order{Name: "sweep", Idempotent: false}
		if !m.gateFailClosed(context.Background(), a, a.ScopedName(), timeoutErr) {
			t.Fatal("non-idempotent order on gate timeout should fail CLOSED")
		}
		if len(rec.Events) != 0 {
			t.Errorf("fail-closed must not emit events, got %d", len(rec.Events))
		}
	})

	t.Run("non-timeout error never emits", func(t *testing.T) {
		rec := events.NewFake()
		m := &memoryOrderDispatcher{stderr: lockedStderr(io.Discard), rec: rec}
		a := orders.Order{Name: "digest", Idempotent: true}
		if !m.gateFailClosed(context.Background(), a, a.ScopedName(), errors.New("dolt: read failed")) {
			t.Fatal("a real store error must fail CLOSED even for idempotent orders")
		}
		if len(rec.Events) != 0 {
			t.Errorf("non-timeout errors must not emit events, got %d", len(rec.Events))
		}
	})

	t.Run("nil recorder does not panic", func(t *testing.T) {
		m := &memoryOrderDispatcher{stderr: lockedStderr(io.Discard)}
		a := orders.Order{Name: "digest", Idempotent: true}
		if m.gateFailClosed(context.Background(), a, a.ScopedName(), timeoutErr) {
			t.Fatal("idempotent order on gate timeout should fail OPEN")
		}
	})
}

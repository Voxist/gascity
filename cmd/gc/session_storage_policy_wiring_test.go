package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// capableBackingStore is a backend that DOES implement the optional
// StorageCreateStore capability, like *beads.BdStore. It stands in for the
// deployments where session beads already route to the wisps table.
type capableBackingStore struct {
	beads.Store
	created []beads.Bead
}

func (s *capableBackingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.created = append(s.created, b)
	return s.Store.Create(b)
}

func (s *capableBackingStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	b.Ephemeral = storage == beads.StorageEphemeral
	b.NoHistory = storage == beads.StorageNoHistory
	s.created = append(s.created, b)
	return s.Store.Create(b)
}

// incapableBackingStore is a backend that does NOT implement
// StorageCreateStore — the shape *beads.NativeDoltStore had before this change,
// and the shape any future or third-party backend may have, since the
// capability is optional by design.
//
// It records the bead exactly as it arrives, so the test can see whether the
// no-history storage class survived the trip. Routing to the wisps table is
// decided downstream by the beads library on issue.NoHistory
// (beads internal/storage/dolt/issues.go:26), so a bead that arrives with
// NoHistory=false lands in the committed issues table and costs a DOLT_COMMIT.
type incapableBackingStore struct {
	beads.Store
	created []beads.Bead
}

func (s *incapableBackingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.created = append(s.created, b)
	return s.Store.Create(b)
}

func (s *incapableBackingStore) lastCreated(t *testing.T) beads.Bead {
	t.Helper()
	if len(s.created) == 0 {
		t.Fatal("no create reached the backing store")
	}
	return s.created[len(s.created)-1]
}

func sessionCreateSpec() session.CreateSpec {
	return session.CreateSpec{
		Title:     "voxist.planner",
		AgentName: "voxist.planner",
		Metadata:  map[string]string{"state": "start_pending"},
	}
}

// controllerCityStore composes a backing store exactly as the controller does
// (cmd/gc/api_state.go): openStoreResultAtForCityWithMode policy-wraps the
// opened store, then wrapWithCachingStore unwraps it, inserts the CachingStore
// and re-wraps the policy layer on the outside.
func controllerCityStore(t *testing.T, backing beads.Store) beads.Store {
	t.Helper()
	cfg := &config.City{}
	cityStore := wrapWithCachingStore(context.Background(), wrapStoreWithBeadPolicies(backing, cfg), nil, false)
	return beads.SessionStore{Store: resolveSessionStore(nil, cityStore, cfg, t.TempDir(), events.Discard)}.Store
}

// TestSessionCreateRoutesToNoHistoryOnCapableBackend pins the behavior that
// already works, so the pair of tests localizes the defect rather than just
// reporting one. On a backend that implements StorageCreateStore the session
// policy survives end to end.
func TestSessionCreateRoutesToNoHistoryOnCapableBackend(t *testing.T) {
	backing := &capableBackingStore{Store: beads.NewMemStore()}

	if _, err := sessionFrontDoor(controllerCityStore(t, backing)).CreateSessionInfo(sessionCreateSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	if len(backing.created) == 0 {
		t.Fatal("no create reached the backing store")
	}
	if got := backing.created[len(backing.created)-1]; !got.NoHistory {
		t.Fatalf("session create on a capable backend: NoHistory = false, want true")
	}
}

// TestSessionCreateRoutesToNoHistoryOnIncapableBackend is the vp-ia76 guard.
//
// A backend that cannot honor a storage class must not cause the class to be
// discarded: CachingStore.CreateWithStorage stamps it onto the bead's own
// fields instead (internal/beads/caching_store_writes.go). Discarding it would
// be ADR-0043 Cause 1 — an unsupported capability coerced into the quiet
// default, with no error, no warning, and no signal of any kind.
//
// The consequence is measurable on the live fleet, not theoretical. Measured on
// hq 2026-07-31: 727 of 730 session beads created in 24h landed in the
// committed issues table; 21,911 of 22,427 rows in hq.issues (97.7%) are
// session beads; and over a 6h window 2,885 of 3,363 Dolt commits were
// "bd: update <session-id>". Scratch-store measurement (bd 1.1.0, server mode,
// the fleet's configuration): a session bead in issues costs one Dolt commit on
// create and one on every update; the same bead in wisps costs zero for both.
//
// It asserts the bead the backend actually receives — the value the beads
// library routes on — not that some wrapper was called.
func TestSessionCreateRoutesToNoHistoryOnIncapableBackend(t *testing.T) {
	backing := &incapableBackingStore{Store: beads.NewMemStore()}

	if _, err := sessionFrontDoor(controllerCityStore(t, backing)).CreateSessionInfo(sessionCreateSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}
	got := backing.lastCreated(t)
	if !got.NoHistory {
		t.Fatal("session create reached a storage-class-incapable backend with NoHistory = false, want true: " +
			"the no-history policy was dropped, so the bead lands in the committed issues table " +
			"and costs a DOLT_COMMIT on create and on every subsequent update")
	}
	if got.Ephemeral {
		t.Fatal("session create reached the backend with Ephemeral = true, want false: " +
			"ephemeral beads are GC/TTL-eligible and are declared incompatible for sessions")
	}
}

// TestPolicyStoreCompositionCreatesSessionsSilently is the guard for the
// storage-policy MARKER contract.
//
// internal/session recognizes cmd/gc's policy wrapper structurally: if the
// marker method disappears from either side, the front door stops recognizing
// the wrapper and falls through to the last-resort path, which warns on stderr
// and imposes its own hardcoded class over the configured one. Nothing else
// fails — the bead is still created, the storage class still ends up
// no_history by coincidence of the default, and every other test in this
// package stays green. The observable symptom is the warning, so that is what
// this asserts: stderr SILENCE through the REAL composition
// (wrapStoreWithBeadPolicies + wrapWithCachingStore + the session front door).
//
// Its compile-time half lives in bead_policy_store.go
// (`var _ session.StoragePolicySelfApplying = (*beadPolicyStore)(nil)`), which
// catches a rename on either side at build time. This catches the wiring: a
// composition that never puts the marked wrapper where the front door looks.
func TestPolicyStoreCompositionCreatesSessionsSilently(t *testing.T) {
	// The warning ledger is process-wide and keyed by store TYPE, and the
	// sibling tests above create sessions through this very type. Without the
	// reset, a broken marker contract would warn once for them and then stay
	// quiet here — this guard would pass for the wrong reason.
	session.ResetStorageWarningsForTest()
	t.Cleanup(session.ResetStorageWarningsForTest)

	backing := &incapableBackingStore{Store: beads.NewMemStore()}
	store := controllerCityStore(t, backing)

	if _, ok := store.(session.StoragePolicySelfApplying); !ok {
		t.Fatalf("the controller's session store composition is %T, which the session "+
			"front door cannot recognize as policy-self-applying; it will warn and "+
			"impose its own storage class instead of the configured one", store)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close() //nolint:errcheck
	old := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = old
		w.Close() //nolint:errcheck
	}
	// Deferred, so a t.Fatalf below cannot strand os.Stderr on a dead pipe.
	defer restore()

	if _, err := sessionFrontDoor(store).CreateSessionInfo(sessionCreateSpec()); err != nil {
		t.Fatalf("CreateSessionInfo: %v", err)
	}

	restore()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if strings.Contains(buf.String(), "session storage policy NOT applied") {
		t.Fatalf("creating a session through the real policy composition warned that the "+
			"storage policy was not applied; the marker contract between "+
			"beadPolicyStore.AppliesBeadStoragePolicy and "+
			"session.StoragePolicySelfApplying is broken. got: %q", buf.String())
	}
}

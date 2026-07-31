package main

import (
	"context"
	"testing"

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
// StorageCreateStore. This is the shape *beads.NativeDoltStore has today: it
// implements Create (and ApplyGraphPlanWithStorage) but no single-bead
// CreateWithStorage. It is the store the live hq city runs on — proven by the
// "gc: update bead <id>" commit messages in hq.dolt_log, which are emitted only
// by NativeDoltStore.Update (internal/beads/native_dolt_store.go:1004).
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
	return beads.SessionStore{Store: resolveSessionStore(cityStore, cfg, t.TempDir(), nil)}.Store
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
// discarded. CachingStore.CreateWithStorage currently falls back to plain
// Create when the backing store is not a StorageCreateStore
// (internal/beads/caching_store_writes.go:20-22), dropping the policy silently
// — no error, no warning, no signal of any kind. That is ADR-0043 Cause 1:
// an unsupported capability coerced into the quiet default.
//
// The consequence is measurable on the live fleet, not theoretical. Measured on
// hq 2026-07-31: 727 of 730 session beads created in 24h landed in the
// committed issues table; 21,911 of 22,427 rows in hq.issues (97.7%) are
// session beads; and over a 6h window 2,885 of 3,363 Dolt commits were
// "bd: update <session-id>". Scratch-store measurement (bd 1.1.0, server mode,
// the fleet's configuration): a session bead in issues costs one Dolt commit on
// create and one on every update; the same bead in wisps costs zero for both.
//
// Reverting the fix must turn this red. It asserts the bead the backend
// actually receives — the value the beads library routes on — not that some
// wrapper was called.
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

// TestNativeDoltStoreDeclaresStorageCreateCapability pins the capability
// itself. The live city store is a *beads.NativeDoltStore; without this
// assertion the drop above is reachable again the moment the fallback is
// touched.
func TestNativeDoltStoreDeclaresStorageCreateCapability(t *testing.T) {
	var store any = (*beads.NativeDoltStore)(nil)
	if _, ok := store.(beads.StorageCreateStore); !ok {
		t.Fatal("*beads.NativeDoltStore does not implement beads.StorageCreateStore: " +
			"policy-selected storage classes are silently discarded on the store the live city runs on")
	}
}

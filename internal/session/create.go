package session

import (
	"fmt"
	"os"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// CreateSpec captures the typed vocabulary for creating a session bead through
// the front door. It is the byte-identical replacement for the inline
// beads.Bead{Type: session, Labels: [gc:session, agent:<name>], ...} literals
// the raw create sites assemble (cmd/gc/session_beads.go and
// cmd/gc/session_name_lookup.go).
//
// The front door owns the bead envelope — the session Type and the
// [LabelSession, "agent:<AgentName>"] label pair — so no caller re-declares
// that shape. The caller still assembles the metadata vocabulary (the create
// sites build it inline from template/provider/pool inputs, which are not
// session-domain concerns) and passes it verbatim as Metadata; CreateSession
// does not interpret or mutate it.
type CreateSpec struct {
	// ID, when non-empty, is the explicit bead id to assign (the pool create
	// site pre-allocates an id for deterministic pool-session naming). When
	// empty the store assigns an id, which CreateSession returns.
	ID string

	// Title is the bead Title (the agent name for configured-named sessions,
	// the target basename or agent name for pool sessions).
	Title string

	// AgentName drives the "agent:<AgentName>" label that selects a session by
	// its owning agent. It is the same value the raw sites passed to the
	// "agent:" + agentName label construction.
	AgentName string

	// Metadata is the assembled session-bead metadata map, written verbatim.
	Metadata map[string]string
}

// CreateSessionInfo creates a session bead from spec and returns the projected
// session.Info of the just-created bead. It is the write-returns-Info create
// front door: the store's Create returns the persisted bead, so the Info is a
// LOCAL InfoFromPersistedBead fold on that bead — never a post-create Get. The
// session Type and the [LabelSession, "agent:<AgentName>"] label pair are
// confined here, so no caller constructs a Type="session" bead directly, and the
// emitted Create is byte-identical to the raw store.Create the create sites
// performed.
//
// Error contract: on a store Create error, NO bead is persisted and (Info{}, err)
// is returned — there is no silent half-create. On success the projection is
// total (InfoFromPersistedBead never fails over a just-created session bead), so
// the created bead is always reported as Info; a caller must never receive a
// created-but-unreported bead. CreateSession is the id-only sibling for callers
// that need only the id.
//
// Backend parity: because this projects the Create ECHO instead of re-Getting, the
// guarantee that the returned Info equals a subsequent Get's projection rests on the
// store backend faithfully echoing the created bead's fields on Create (memstore
// clones the stored bead; the CachingStore Get-refreshes write-through; BdStore and
// the Dolt stores reconstruct the bead from bd's create response). That parity is
// pinned across every backend by the beadstest conformance case
// CreateEchoMatchesGetOnMetadata, not just by the memstore-backed oracle here.
func (s *Store) CreateSessionInfo(spec CreateSpec) (Info, error) {
	bead := beads.Bead{
		ID:       spec.ID,
		Title:    spec.Title,
		Type:     BeadType,
		Labels:   []string{LabelSession, "agent:" + spec.AgentName},
		Metadata: spec.Metadata,
	}
	created, err := s.createSessionBead(bead)
	if err != nil {
		return Info{}, err
	}
	return infoFromPersistedBead(created), nil
}

// createSessionBead persists a session bead under the SESSION STORAGE POLICY.
//
// WHY THIS EXISTS (vp-ia76, phase 1 of vp-9u1). This front door used to call
// s.store.Create() directly, which carries no storage class. gascity's own policy
// (cmd/gc/bead_policy_store.go: beadPolicySession -> beadStorageNoHistory) was
// therefore dropped on the floor, and every session bead landed in the Dolt-COMMITTED
// issues table instead of the dolt_ignore'd wisps table — one DOLT_COMMIT each.
// Measured 2026-07-30: 262 sessions/24h through this door into issues, against 110
// into wisps through the policy-honoring controller path. That commit volume is what
// grows the hq store, drives compaction, and rebuilds the push backlog faster than the
// 15s listener window can ship it.
//
// no_history, NOT ephemeral. They are not interchangeable:
//   - no_history: row in wisps, ephemeral=0, no DOLT_COMMIT, NOT GC/TTL-eligible,
//     reads keep working. The 204 session rows already in hq.wisps have exactly this
//     shape (no_history=1, ephemeral=0) — this matches them.
//   - ephemeral:  also GC/TTL-eligible, sets ephemeral=1, which gascity's own policy
//     declares incompatible for sessions (bead_policy_store.go) and which
//     matchesTier (internal/beads/query.go) silently DROPS from results.
//
// THE FALLBACK IS LOUD ON PURPOSE. CachingStore.CreateWithStorage silently degrades to
// a plain Create when its backing store does not implement StorageCreateStore — and
// NativeDoltStore does not implement it. So a chain assembled the wrong way would take
// this fix, report success, and keep writing to issues exactly as before: the fix would
// be INERT AND INVISIBLE, which is the failure mode this change exists to end
// (ADR-0043: an unknown must propagate, not be coerced into the quiet answer). When the
// class cannot be honored we say so rather than pretend, once per process so a hot path
// cannot spam the ops tail.
func (s *Store) createSessionBead(b beads.Bead) (beads.Bead, error) {
	storageStore, ok := s.store.Store.(beads.StorageCreateStore)
	if !ok {
		warnSessionStorageUnsupported(s.store.Store)
		return s.store.Create(b)
	}
	return storageStore.CreateWithStorage(b, beads.StorageNoHistory)
}

var sessionStorageWarnOnce sync.Once

// warnSessionStorageUnsupported reports, once per process, that the session storage
// policy could not be applied. Silence here would mean the caller believes sessions are
// staying out of the committed table while they are not.
func warnSessionStorageUnsupported(store any) {
	sessionStorageWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"gc: session storage policy NOT applied: %T does not implement "+
				"beads.StorageCreateStore, so session beads are being written to the "+
				"committed issues table (one DOLT_COMMIT each) instead of wisps. "+
				"This is vp-ia76 / vp-9u1 and it silently inflates the store.\n",
			store)
	})
}

// CreateSession creates a session bead from spec and returns its id. It is the
// id-only sibling of CreateSessionInfo (the single front door for session-bead
// creation) and delegates to it, so both emit the byte-identical Create; callers
// that need the projected Info without a post-create Get use CreateSessionInfo.
func (s *Store) CreateSession(spec CreateSpec) (string, error) {
	info, err := s.CreateSessionInfo(spec)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

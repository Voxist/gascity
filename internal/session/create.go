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

// StoragePolicySelfApplying marks a store that resolves and applies the bead
// storage policy inside its own Create. A caller that cannot reach
// CreateWithStorage through such a store has NOT lost the policy, and must not
// warn: the plain Create is already policy-correct.
//
// It is also the only party that can see a CONFIGURED policy — the storage class
// for a policy name is read from the city config
// ([beads.policies.session] storage = ...), which internal/session has no access
// to. So a store that declares this marker is authoritative about the session
// bead's storage class, and createSessionBead defers to it.
//
// cmd/gc's policy layer is the implementation; the coupling is pinned there by
// `var _ session.StoragePolicySelfApplying = (*beadPolicyStore)(nil)`, so
// renaming either side is a compile error rather than a silent downgrade to the
// warn path.
type StoragePolicySelfApplying interface {
	AppliesBeadStoragePolicy()
}

// createSessionBead persists a session bead under the SESSION STORAGE POLICY:
// no_history, never ephemeral.
//
// no_history and ephemeral are NOT interchangeable, and picking the wrong one
// is worse than picking neither:
//   - no_history sets no_history=1, ephemeral=0. Retention is dropped, nothing
//     else is: the bead is NOT GC/TTL-eligible and reads keep finding it. (On
//     the Dolt backend this is the dolt_ignore'd wisps table, at zero
//     DOLT_COMMITs; other backends express the same tier their own way.)
//   - ephemeral sets ephemeral=1, which is ALSO GC/TTL-eligible, which
//     gascity's own policy declares incompatible for sessions
//     (cmd/gc/bead_policy_store.go), and which ListQuery.matchesTier
//     (internal/beads/query.go) silently DROPS from default-tier results. A
//     session that vanishes from reads would look like a successful fix.
//
// Three routes to that class, in precedence order:
//
//  1. The store applies the policy itself (StoragePolicySelfApplying). A plain
//     Create through it is already policy-correct. It wins over route 2 on
//     purpose: the class named in route 2 is a hardcoded default, while a
//     self-applying store resolves the CONFIGURED class and can honor a
//     [beads.policies.session] storage override that this package cannot see.
//  2. The store accepts a class out of band (beads.StorageCreateStore). Here the
//     class is hardcoded to StorageNoHistory, the policy DEFAULT for sessions;
//     a configured override is not visible on this route. That divergence is
//     bounded by route 1 taking precedence: gascity's own wiring always hands
//     this front door a policy-wrapped store (cmd/gc/class_store.go
//     resolveSessionStore over the policy-wrapped city store), so route 2 is
//     reached only by a caller that passes a bare store directly — a fixture or
//     an embedder — where there is no city config to diverge from.
//  3. Neither. The class is stamped directly onto the bead's own NoHistory
//     field — the same field routing the caching store's own fallback relies on
//     (internal/beads/caching_store_writes.go) — so backends that route on the
//     field still place the bead correctly, and the loss is reported.
//
// Route 3 warns rather than fails. Observability must never break the caller: a
// session that cannot be created is worse than one created in the wrong tier.
// But it must be REPORTED, not silent — an unhonored capability coerced into
// the quiet default is exactly ADR-0043 Cause 1, and it would let this whole
// mechanism ship inert and invisible.
func (s *Store) createSessionBead(b beads.Bead) (beads.Bead, error) {
	if _, ok := s.store.Store.(StoragePolicySelfApplying); ok {
		return s.store.Create(b)
	}
	if storageStore, ok := s.store.Store.(beads.StorageCreateStore); ok {
		return storageStore.CreateWithStorage(b, beads.StorageNoHistory)
	}
	warnSessionStorageUnsupported(s.store.Store)
	b.NoHistory = true
	b.Ephemeral = false
	return s.store.Create(b)
}

var (
	sessionStorageWarnMu    sync.Mutex
	sessionStorageWarnTypes = map[string]bool{}
)

// ResetStorageWarningsForTest clears the per-store-type warning ledger.
//
// The ledger is process-wide by design, which makes "this composition does not
// warn" depend on whether an earlier test in the same binary already warned for
// the same store type — a silence assertion that passes for the wrong reason is
// a guard that cannot fail. Cross-package tests (cmd/gc's policy-composition
// guard) reset it first so they observe the FIRST-warning behavior.
func ResetStorageWarningsForTest() {
	sessionStorageWarnMu.Lock()
	sessionStorageWarnTypes = map[string]bool{}
	sessionStorageWarnMu.Unlock()
}

// warnSessionStorageUnsupported reports that the session storage policy could
// not be requested through a store, ONCE PER STORE TYPE per process.
//
// The key is the store type, not the process, because a single benign warning
// would otherwise mute a later, genuinely incapable chain of a DIFFERENT type —
// a silent failure inside the warning that exists to prevent silent failure.
// Keying on the type bounds the noise (one line per offending type) without
// ever muting a new offender.
//
// The message names the store TYPE and the POLICY that was lost, not a table or
// a commit count: the same code path serves FileStore and MemStore chains where
// there is no Dolt, no issues table, and no commit to count. What is universally
// true is that the policy could not be expressed through this store, so the
// tier now depends on the backend honoring a field rather than on gascity
// having asked for it.
func warnSessionStorageUnsupported(store any) {
	typeName := fmt.Sprintf("%T", store)
	sessionStorageWarnMu.Lock()
	seen := sessionStorageWarnTypes[typeName]
	sessionStorageWarnTypes[typeName] = true
	sessionStorageWarnMu.Unlock()
	if seen {
		return
	}
	fmt.Fprintf(os.Stderr,
		"gc: session storage policy NOT applied through %s: it implements neither "+
			"beads.StorageCreateStore nor session.StoragePolicySelfApplying, so the "+
			"no_history class could not be requested. The bead is stamped no_history "+
			"on its own field and created anyway — backends that route on that field "+
			"still place it in the no-history tier, but a backend that ignores the "+
			"field keeps session beads in its default, fully-retained tier and the "+
			"session storage policy is lost there (vp-ia76 / vp-9u1).\n",
		typeName)
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

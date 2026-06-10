//go:build loadharness

package loadharness

import (
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// probeCustomType is the required custom bead type the write-path conformance
// probe creates. It is one of doctor.RequiredCustomTypes ("session"); a scope
// that rejects it is exhibiting the a74fefde8 application-class write rejection.
// It is named here as a constant rather than imported to keep the harness free
// of a doctor dependency and its sentence-level role semantics.
const probeCustomType = "session"

// errStoreUnavailable is the harness stand-in for beads.ErrStoreUnavailable —
// the typed transport failure the plan's Phase 1 propagates instead of
// rendering "no work". The harness models the fault at its own layer (it must
// not depend on the sibling resilience/storehealth branches compiling here),
// so it carries its own sentinel.
var errStoreUnavailable = errors.New("loadharness: store unavailable (transport)")

// errTypeRejected is the harness stand-in for an application-class write
// rejection: a store/scope that REJECTS a required custom bead type. This
// simulates the a74fefde8 write-rejection (an embedded-DB misroute creating a
// fresh typeless DB) that a transport-only breaker cannot see — the condition
// the plan's 1.5 write-path conformance probe and 2.4 preflight target.
var errTypeRejected = errors.New("loadharness: invalid issue type (write rejected)")

// fault describes a deterministic, scripted store fault for a scope. Faults are
// seeded from a fixed scenario table — never from wall-clock randomness — so
// runs are reproducible.
type fault int

const (
	// faultNone is a healthy scope.
	faultNone fault = iota
	// faultTransport rejects every operation with a transport-class error
	// (the proxy-poison / endpoint-down shape). A connection breaker SHOULD
	// observe it.
	faultTransport
	// faultTypeRejected accepts reads but rejects writes of a required custom
	// type (the a74fefde8 application-class shape). A transport-only breaker
	// would MISS it; a write-path conformance probe SHOULD observe it.
	faultTypeRejected
	// faultResolutionFailed simulates endpoint resolution returning nothing:
	// the managed server handle is unresolvable. The factory must fall back —
	// never silently open an empty store and render zero work.
	faultResolutionFailed
)

// opClass labels the store operation being amplified, used to charge a
// deterministic per-op latency from the fixed cost table.
type opClass int

const (
	opReady opClass = iota
	opList
	opCreate
	opClose
	opResolve
)

// opCost is the fixed, deterministic simulated cost of one amplified store
// operation through the subprocess runner. These are illustrative model
// constants (a bd subprocess spawn dominated by process start + a proxied
// round-trip), NOT measured production numbers; the harness measures the
// SHAPE (fan-out × per-op cost) so cutover steps can compare relative change.
// They are intentionally centralized here rather than scattered as magic
// numbers.
var opCost = map[opClass]time.Duration{
	opReady:   3 * time.Millisecond,
	opList:    2 * time.Millisecond,
	opCreate:  2 * time.Millisecond,
	opClose:   1 * time.Millisecond,
	opResolve: 1 * time.Millisecond,
}

// ampStore is the harness model of one scope's controller-side store access:
// CachingStore over a per-scope subprocess runner. Every operation charges one
// simulated subprocess spawn and a deterministic latency, and applies the
// scope's scripted fault. It delegates actual bead bookkeeping to an in-memory
// beads.Store (MemStore) so reads/writes have real semantics without Dolt.
//
// ampStore is the amplifier under measurement. A native-in-process store (the
// Phase 2 target) would not Inc the spawn counter — wiring that variant in is
// how a later cutover step proves the amplifier is gone.
type ampStore struct {
	scope   string
	inner   beads.Store
	fault   fault
	spawns  *SpawnCounter
	storeOp *int64
	// fleet holds the scope's synthetic open sessions on the fake runtime
	// provider; its assignee names drive the per-session Ready fan-out.
	fleet *sessionFleet
	// simNanos accumulates this scope's simulated latency for the current
	// tick. The controller-tick model reads and resets it per tick.
	simNanos int64
}

// newAmpStore builds an amplifying store for a scope over a fresh MemStore,
// materializing openSessions synthetic sessions on the fake runtime provider.
func newAmpStore(scope string, f fault, openSessions int, spawns *SpawnCounter, storeOp *int64) *ampStore {
	return &ampStore{
		scope:   scope,
		inner:   beads.NewMemStore(),
		fault:   f,
		spawns:  spawns,
		storeOp: storeOp,
		fleet:   newSessionFleet(scope, openSessions),
	}
}

// charge records one simulated subprocess spawn, the operation's deterministic
// latency, and one store op. It returns the scope's fault error for the op
// class, or nil. Reads survive a faultTypeRejected scope (the application
// rejection is write-only); writes survive a faultTransport scope only to be
// rejected at the transport gate first.
func (s *ampStore) charge(class opClass, isWrite bool) error {
	s.spawns.Inc()
	*s.storeOp++
	s.simNanos += int64(opCost[class])
	switch s.fault {
	case faultTransport:
		return errStoreUnavailable
	case faultResolutionFailed:
		// Endpoint resolution returns nothing. Treated as transport-class
		// unavailability so the harness can assert the factory falls back
		// rather than rendering silent-empty.
		return errStoreUnavailable
	case faultTypeRejected:
		if isWrite {
			return errTypeRejected
		}
		return nil
	default:
		return nil
	}
}

// drainSimNanos returns and resets the scope's accumulated simulated latency.
func (s *ampStore) drainSimNanos() time.Duration {
	d := time.Duration(s.simNanos)
	s.simNanos = 0
	return d
}

// Ready amplifies a single Ready lookup (one bd subprocess in the BdStore
// model). The per-assignee fan-out is modeled by the caller invoking Ready
// once per open session.
func (s *ampStore) Ready(query beads.ReadyQuery) ([]beads.Bead, error) {
	if err := s.charge(opReady, false); err != nil {
		return nil, err
	}
	return s.inner.Ready(query)
}

// list amplifies a List lookup.
func (s *ampStore) list(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.charge(opList, false); err != nil {
		return nil, err
	}
	return s.inner.List(query)
}

// create amplifies a bead create (a write — subject to type rejection).
func (s *ampStore) create(b beads.Bead) (beads.Bead, error) {
	if err := s.charge(opCreate, true); err != nil {
		return beads.Bead{}, err
	}
	return s.inner.Create(b)
}

// closeBead amplifies a bead close (a write).
func (s *ampStore) closeBead(id string) error {
	if err := s.charge(opClose, true); err != nil {
		return err
	}
	return s.inner.Close(id)
}

// resolveEndpoint amplifies an endpoint resolution probe. A faultResolutionFailed
// scope returns errStoreUnavailable so the controller-tick model can assert the
// factory falls back instead of opening an empty store.
func (s *ampStore) resolveEndpoint() error {
	return s.charge(opResolve, false)
}

package beads

import "errors"

// AvailabilityGate reports whether the backing store transport is
// currently believed reachable. The production implementation is the
// per-scope *resilience.Breaker wired by the controller; the methods
// must never mutate breaker state (probe admission stays on the
// operation path).
type AvailabilityGate interface {
	// Available reports the gate is closed (store believed reachable).
	Available() bool
	// ProbeDue reports an unavailable gate would currently admit a
	// recovery probe. Periodic loops use it to keep probing while
	// otherwise skipping cycles cheaply.
	ProbeDue() bool
}

// SetAvailabilityGate wires the transport availability gate.
//
// The gate is an OPTIMISATION, not a second opinion about what the cache may
// serve. While it reports unavailable, reads return ErrStoreUnavailable rather
// than making a backing call that would fail with the same error after spawning
// a doomed bd subprocess, and the reconciler skips cycles except when a
// recovery probe is due. It never changes WHICH queries are answered from
// memory: that belongs to cacheServableLocked and readCacheWithOverlay, which
// already refuse stale, dirty and history-shaped reads.
//
// That boundary is deliberate. An earlier version short-circuited above the
// cached read path to serve "last-good" data during an outage, which bypassed
// the overlay's dirty-row refresh and suppression set, answered closed-only and
// parent-history queries from an active-only map (a convoy tally read "zero
// completed work"), and answered Live queries — which exist precisely to demand
// a backing read — from stale memory. It also fought cacheDegraded: a sustained
// outage flips the cache to that state, disabling serving, so last-good serving
// switched itself off partway through the outage it existed for.
//
// A nil gate (the default) disables gating.
func (c *CachingStore) SetAvailabilityGate(g AvailabilityGate) {
	if g == nil {
		// atomic.Value rejects a nil value; an unset gate is the zero Value,
		// which availabilityGateRef already reads as "no gate".
		return
	}
	c.availabilityGate.Store(g)
}

// availabilityGateRef returns the configured gate (nil when unset).
//
// Read without holding mu: every cached read consults it, including while
// another goroutine holds mu for writing, and a lock-taking read here
// deadlocks those reads against an in-flight reconcile and defeats their
// context cancellation.
func (c *CachingStore) availabilityGateRef() AvailabilityGate {
	g, _ := c.availabilityGate.Load().(AvailabilityGate)
	return g
}

// storeUnavailable reports whether the backing transport is believed down, so
// a call into it would fail rather than do useful work.
func (c *CachingStore) storeUnavailable() bool {
	g := c.availabilityGateRef()
	return g != nil && !g.Available()
}

// Degraded reports whether reads may be answered from a cache that repeated
// reconcile failures marked stale, or whose backing transport is believed
// unavailable.
func (c *CachingStore) Degraded() bool {
	if c.storeUnavailable() {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == cacheDegraded
}

// reconcileUnavailableSkip reports whether the current reconciliation
// cycle must be skipped because the gate is unavailable and no recovery
// probe is due. The first skip of an episode records one problem
// ("emit once"); recovery re-arms the log for the next episode.
func (c *CachingStore) reconcileUnavailableSkip() bool {
	g := c.availabilityGateRef()
	if g == nil || g.Available() {
		c.mu.Lock()
		c.unavailableSkipLogged = false
		c.mu.Unlock()
		return false
	}
	if g.ProbeDue() {
		return false
	}
	c.mu.Lock()
	logged := c.unavailableSkipLogged
	c.unavailableSkipLogged = true
	c.mu.Unlock()
	if !logged {
		c.recordProblem("reconcile skipped", errors.New("store unavailable (circuit breaker open); skipping reconcile cycles until a recovery probe is due"))
	}
	return true
}

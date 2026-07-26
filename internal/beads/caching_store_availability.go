package beads

import (
	"errors"
	"fmt"
)

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

// SetAvailabilityGate wires the transport availability gate. While the
// gate reports unavailable, List and Get serve last-good cached data
// tagged degraded (or ErrStoreUnavailable when the cache cannot answer)
// and the reconciler skips cycles except when a recovery probe is due.
// A nil gate (the default) disables gating.
func (c *CachingStore) SetAvailabilityGate(g AvailabilityGate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.availabilityGate = g
}

// availabilityGateRef returns the configured gate (nil when unset).
func (c *CachingStore) availabilityGateRef() AvailabilityGate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.availabilityGate
}

// servingDegraded reports whether reads must avoid the backing store
// because the availability gate says the transport is unavailable.
func (c *CachingStore) servingDegraded() bool {
	g := c.availabilityGateRef()
	return g != nil && !g.Available()
}

// Degraded reports whether the cache is currently serving degraded data:
// the availability gate says the store is unreachable, or repeated
// reconcile failures pushed the cache into the degraded state.
func (c *CachingStore) Degraded() bool {
	if c.servingDegraded() {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == cacheDegraded
}

// listLastGood answers a List query purely from the in-memory cache while
// the backing store is unavailable. An unprimed cache returns
// ErrStoreUnavailable — unavailable must never read as empty.
func (c *CachingStore) listLastGood(query ListQuery) ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheUninitialized {
		c.mu.RUnlock()
		return nil, fmt.Errorf("listing beads: %w", ErrStoreUnavailable)
	}
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		cached = append(cached, cloneBead(b))
	}
	c.mu.RUnlock()
	c.degradedReads.Add(1)
	sortBeadsForQuery(cached, query.Sort)
	if query.Limit > 0 && len(cached) > query.Limit {
		cached = cached[:query.Limit]
	}
	return cached, nil
}

// getLastGood answers a Get purely from the in-memory cache while the
// backing store is unavailable. A bead absent from the cache returns
// ErrStoreUnavailable: the cache cannot distinguish "missing" from
// "unreachable" (closed beads are not fully cached).
func (c *CachingStore) getLastGood(id string) (Bead, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, deleted := c.deletedSeq[id]; deleted {
		return Bead{}, ErrNotFound
	}
	if b, ok := c.beads[id]; ok {
		c.degradedReads.Add(1)
		return cloneBead(b), nil
	}
	return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrStoreUnavailable)
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

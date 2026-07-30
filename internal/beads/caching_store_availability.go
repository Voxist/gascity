package beads

import (
	"context"
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
	if g == nil {
		// atomic.Value rejects a nil value; an unset gate is the zero Value,
		// which availabilityGateRef already reads as "no gate".
		return
	}
	c.availabilityGate.Store(g)
}

// availabilityGateRef returns the configured gate (nil when unset).
func (c *CachingStore) availabilityGateRef() AvailabilityGate {
	g, _ := c.availabilityGate.Load().(AvailabilityGate)
	return g
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

// listLastGood answers a List query purely from the in-memory cache while the
// backing store is unavailable. It DECLINES with ErrStoreUnavailable rather
// than answering approximately: unavailable must never read as empty, and a
// silently truncated list is worse than an error because callers act on it.
//
// Three ways the cache cannot answer, all of which must decline:
//
//   - Not servable. cacheServableLocked is the same gate every other cached
//     read uses; it additionally requires primePartialErr == nil. A half-loaded
//     cache from a failed prime would otherwise be served as an authoritative
//     complete answer.
//   - History-shaped query. The cache holds active beads (PrimeActive loads
//     open + in_progress), so it has no complete closed-only or parent-history
//     view. The normal read path routes these to the backing store for exactly
//     that reason; degraded serving must not silently answer them from an
//     active-only map. A convoy tally counting closed children would otherwise
//     read "zero completed work" during an outage and mis-drive the graph.
//   - Suppressed deletions. The overlay read path drops rows the backing store
//     reported ErrNotFound (invariant I6); without that filter, beads deleted
//     out-of-band reappear in degraded listings.
func (c *CachingStore) listLastGood(query ListQuery) ([]Bead, error) {
	// Closed-only, closed-inclusive and parent-history shapes need the backing
	// store's view, which is precisely what is unavailable.
	if query.Status == "closed" || query.IncludeClosed || query.ParentID != "" {
		return nil, fmt.Errorf("listing beads (cache holds active beads only, cannot answer this query shape): %w", ErrStoreUnavailable)
	}
	c.mu.RLock()
	if !c.cacheServableLocked() {
		c.mu.RUnlock()
		return nil, fmt.Errorf("listing beads: %w", ErrStoreUnavailable)
	}
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if _, suppressed := c.deletedSeq[b.ID]; suppressed {
			continue
		}
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
	// Same servability contract as every other cached read: a half-loaded cache
	// from a failed prime cannot prove a bead's absence either.
	if !c.cacheServableLocked() {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrStoreUnavailable)
	}
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

// countLastGood answers a Count from the in-memory cache while the backing
// store is unavailable. It reuses listLastGood's declining behavior so a
// count and a list of the same query can never disagree about whether the
// cache can answer: both serve, or both return ErrStoreUnavailable.
func (c *CachingStore) countLastGood(_ context.Context, query ListQuery, excludeTypes []string) (int, error) {
	beadsOut, err := c.listLastGood(query)
	if err != nil {
		return 0, err
	}
	if len(excludeTypes) == 0 {
		return len(beadsOut), nil
	}
	excluded := make(map[string]struct{}, len(excludeTypes))
	for _, t := range excludeTypes {
		excluded[t] = struct{}{}
	}
	n := 0
	for _, b := range beadsOut {
		if _, skip := excluded[b.Type]; skip {
			continue
		}
		n++
	}
	return n, nil
}

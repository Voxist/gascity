package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// List returns beads matching the query. Active-bead queries are served from
// cache when available. IncludeClosed queries merge cached active results with
// backing-store history when possible, preserving partial backing rows when bd
// reports corrupt entries and returning partial-result errors when backing
// history cannot be fully read.
func (c *CachingStore) List(query ListQuery) ([]Bead, error) {
	return c.ListCtx(context.Background(), query)
}

// ListCtx implements the optional CtxLister capability. It answers the
// active-bead cache path exactly as List does — that path is pure in-memory
// lookup, so it needs no ctx — but every branch that falls through to the
// backing store (the query.Live/ParentID bypass, the IncludeClosed history
// merge, and the cache-not-yet-servable fallback) routes through
// backingListCtx so a canceled ctx can abort the backing read instead of
// leaving a caller's abandoned goroutine (statusListStoreWithTimeout) to hold
// the connection past its own deadline. The cache-not-servable fallback
// matters most for a cold cache (fresh after a supervisor restart or long
// idle): that is exactly when this path is taken.
//
// Restored in the 2026-08-31 resync. Dropping this method did not fail to
// compile and broke nothing loudly — it simply stopped CachingStore from
// satisfying CtxLister, so every `store.(CtxLister)` assertion silently took
// the uncancellable fallback.
func (c *CachingStore) ListCtx(ctx context.Context, query ListQuery) ([]Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	// Breaker open: do not dial a store proven down by real command outcomes.
	// listLastGood answers what the snapshot can answer honestly — active and
	// ParentID shapes serve rows (nil error, preserving this path's contract
	// for error-intolerant consumers), a partially-primed snapshot is tagged
	// partial, and Live, closed-only and unprimed shapes refuse with
	// ErrStoreUnavailable.
	if c.servingDegraded() {
		return c.listLastGood(query, false)
	}
	if query.Live || query.ParentID != "" {
		c.mu.RLock()
		startSeq := c.mutationSeq
		c.mu.RUnlock()
		items, err := c.backingListCtx(ctx, query)
		if err == nil {
			items = c.refreshCachedBeads(query, startSeq, items)
		}
		return items, err
	}

	// Active-bead path: serve from cache after a bounded per-ID refresh of any
	// dirty rows. PrimeActive loads the full active set (open + in_progress),
	// so active-only queries are complete even before the history prime
	// finishes. On overlay error the read takes the old full-scan fallback.
	var cached []Bead
	if err := c.readCacheWithOverlay(c.cacheServableLocked, func(suppressed map[string]struct{}) {
		cached = make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			if _, gone := suppressed[b.ID]; gone {
				continue
			}
			if !query.Matches(b) {
				continue
			}
			cached = append(cached, cloneBead(b))
		}
	}); err == nil {
		finish := func(items []Bead, err error) ([]Bead, error) {
			sortBeadsForQuery(items, query.Sort)
			if query.Limit > 0 && len(items) > query.Limit {
				items = items[:query.Limit]
			}
			return items, err
		}

		if !query.IncludesClosed() {
			return finish(cached, nil)
		}

		// The cache never has a complete closed-only or parent-history view, so
		// preserve the old backing-store behavior for those query shapes.
		if query.Status == "closed" || query.ParentID != "" {
			return c.backingListCtx(ctx, liveListQuery(query))
		}

		all, err := c.backingListCtx(ctx, liveListQuery(query))
		if err != nil {
			if !IsPartialResult(err) {
				c.recordProblem("list include closed backing failure", err)
				return finish(cached, &PartialResultError{
					Op:  "cache list include closed",
					Err: err,
				})
			}
		}

		seen := make(map[string]bool, len(cached))
		for _, b := range cached {
			seen[b.ID] = true
		}
		for _, b := range all {
			if seen[b.ID] {
				continue
			}
			cached = append(cached, b)
			seen[b.ID] = true
		}
		return finish(cached, err)
	}
	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()
	items, err := c.backingListCtx(ctx, liveListQuery(query))
	if err == nil {
		// Fold the fresh rows into the snapshot so consecutive degraded reads
		// cannot travel backwards in time: a read that succeeds and a read that
		// falls back moments later must not disagree by hours.
		c.absorbBackingListLocked(items, startSeq)
		return items, nil
	}
	// A partial answer IS an answer: the store served rows and told us the set
	// is incomplete. Falling back would replace a truthful partial with a
	// snapshot and DROP the PartialResultError, hiding the degradation the
	// caller must see (TestCachingStoreRunReconciliationDegradesImmediatelyOnPartialResult).
	if IsPartialResult(err) {
		return items, err
	}
	// The store did not answer. Serve the last-good snapshot for every shape it
	// can answer honestly (listLastGood owns that boundary; a refusal surfaces
	// the backing failure unchanged; known-incomplete snapshots arrive tagged
	// partial). This is what makes a backend outage degrade reads to
	// stale-but-correct instead of amplifying into a read outage (ga-2p81g).
	fallback, ferr := c.listLastGood(query, true)
	if ferr != nil && !IsPartialResult(ferr) {
		return items, err
	}
	c.recordProblem("list served last-good after backing failure", err)
	return fallback, ferr
}

func liveListQuery(query ListQuery) ListQuery {
	query.Live = true
	return query
}

// Count returns the number of beads List would return for query, minus
// beads whose Type is in excludeTypes. Active-bead queries are answered
// from the in-memory cache when it is live and clean; everything else
// (Live queries, ParentID lookups, closed history, dirty/unprimed cache)
// delegates to the backing store's Counter. Backing stores without a
// Counter return ErrCountUnsupported so callers can fall back to List. Limited
// queries are unsupported because Count must match List cardinality, including
// List's post-sort limit cap.
func (c *CachingStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	if !query.HasFilter() && !query.AllowScan {
		return 0, fmt.Errorf("counting beads: %w", ErrQueryRequiresScan)
	}
	if query.Limit > 0 {
		return 0, fmt.Errorf("counting beads: %w", ErrCountUnsupported)
	}
	// Context BEFORE any lock acquisition: servingDegraded() reads gate state
	// under c.mu, so checking the gate first would make a cancelled caller wait
	// on the cache lock — the exact contract
	// TestCachingStoreCountContextCancelsWhileWaitingForLock pins.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	// Breaker open: the store is proven down, so dialing it for a count defeats
	// the gate. Serve the snapshot count for shapes it can answer honestly;
	// refuse the rest rather than reporting a number nobody can stand behind.
	if c.servingDegraded() {
		// A backing store with no Counter cannot count regardless of the gate;
		// callers fall back to List, so the contract must stay
		// ErrCountUnsupported rather than becoming ErrStoreUnavailable.
		if _, hasCounter := c.backing.(Counter); !hasCounter {
			return 0, fmt.Errorf("counting beads: backing store: %w", ErrCountUnsupported)
		}
		if n, ok := c.lastGoodCount(ctx, query, excludeTypes); ok {
			return n, nil
		}
		return 0, fmt.Errorf("counting beads: %w", ErrStoreUnavailable)
	}
	if !query.Live && query.ParentID == "" && !query.IncludesClosed() {
		n, ok, err := c.cachedCountContext(ctx, query, excludeTypes)
		if err != nil {
			return 0, err
		}
		if ok {
			return n, nil
		}
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	counter, ok := c.backing.(Counter)
	if !ok {
		return 0, fmt.Errorf("counting beads: backing store: %w", ErrCountUnsupported)
	}
	n, err := counter.Count(ctx, liveListQuery(query), excludeTypes...)
	if err == nil || errors.Is(err, ErrCountUnsupported) || IsPartialResult(err) || (ctx != nil && ctx.Err() != nil) {
		return n, err
	}
	// Reality first, snapshot as backstop — the same contract as List: the
	// store was dialed and did not answer within the caller's budget, so an
	// active-shape count is served from last-good rather than surfacing the
	// outage to the caller.
	if fn, ok := c.lastGoodCount(ctx, query, excludeTypes); ok {
		c.recordProblem("count served last-good after backing failure", err)
		return fn, nil
	}
	return n, err
}

// cachedCountContext serves only a clean active snapshot. Dirty overlays use
// context-blind Store.Get calls, so a deadline-sensitive Count delegates those
// cases to the backing Counter instead. Lock acquisition and the scan both
// observe ctx, ensuring a cache writer cannot strand the caller's goroutine.
func (c *CachingStore) cachedCountContext(ctx context.Context, query ListQuery, excludeTypes []string) (int, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if !c.mu.TryRLock() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for !c.mu.TryRLock() {
			select {
			case <-ctx.Done():
				return 0, false, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	defer c.mu.RUnlock()

	if !c.cacheServableLocked() || len(c.dirty) > 0 {
		return 0, false, nil
	}
	var n int
	for _, b := range c.beads {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if query.Matches(b) && !slices.Contains(excludeTypes, b.Type) {
			n++
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// CachedList returns query results from the in-memory cache only. The boolean
// reports whether the cache was initialized and clean enough to answer without
// touching the backing store.
//
// This strict cache-only handle intentionally keeps the conservative
// "dirty ⇒ decline" contract: it must answer without any backing I/O and
// without serving a row it is not certain matches the backing. The bounded
// per-ID dirty overlay (readCacheWithOverlay) applies only to the read paths
// that already fall back to the backing store (List/Count/Ready), where a
// refresh-and-serve is invisible to callers.
func (c *CachingStore) CachedList(query ListQuery) ([]Bead, bool) {
	if query.IncludesClosed() {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil, false
	}
	if c.primePartialErr != nil || len(c.dirty) > 0 {
		return nil, false
	}
	return c.collectCachedListLocked(query), true
}

// ObservedList returns a detached active-only cache census and an opaque stamp
// that may be conditionally consumed with WithCurrentObservation. It never
// performs backing-store I/O. The stamp fences only this process's cache
// projection; it does not certify durable-store lineage or event delivery.
func (c *CachingStore) ObservedList(query ListQuery) ([]Bead, CacheObservation, bool) {
	if query.Validate() != nil ||
		(!query.HasFilter() && !query.AllowScan) ||
		query.Live ||
		query.IncludesClosed() ||
		query.ParentID != "" ||
		len(query.ParentIDs) > 0 {
		return nil, CacheObservation{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.observationAdmissibleLocked() {
		return nil, CacheObservation{}, false
	}
	return c.collectCachedListLocked(query), CacheObservation{owner: c, revision: c.observationRevision}, true
}

// WithCurrentObservation runs publish while holding the originating cache's
// read lock only when observation still describes a clean active cache. The
// callback must perform bounded in-memory work and must not call the cache,
// backing store, or wait for other work.
func (c *CachingStore) WithCurrentObservation(observation CacheObservation, publish func() error) (bool, error) {
	if publish == nil {
		return false, fmt.Errorf("using cache observation: nil callback")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if observation.owner != c || observation.revision == 0 || observation.revision != c.observationRevision ||
		!c.observationAdmissibleLocked() {
		return false, nil
	}
	return true, publish()
}

// observationAdmissibleLocked reports whether an active-only cache census can
// be observed and conditionally used. Caller must hold c.mu.
func (c *CachingStore) observationAdmissibleLocked() bool {
	return (c.state == cacheLive || c.state == cachePartial) &&
		c.primePartialErr == nil &&
		len(c.dirty) == 0 &&
		c.observationRevision != 0
}

// collectCachedListLocked materializes CachedList and ObservedList results.
// Caller must hold c.mu for reading or writing.
func (c *CachingStore) collectCachedListLocked(query ListQuery) []Bead {
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		cached = append(cached, cloneBead(b))
	}
	sortBeadsForQuery(cached, query.Sort)
	if query.Limit > 0 && len(cached) > query.Limit {
		cached = cached[:query.Limit]
	}
	return cached
}

func (c *CachingStore) refreshCachedBeads(query ListQuery, startSeq uint64, items []Bead) []Bead {
	refreshedParents := make(map[string]Bead)
	removedParents := make(map[string]struct{})
	refreshedLiveMissing := make(map[string]Bead)
	removedLiveMissing := make(map[string]struct{})
	for _, id := range c.staleParentCacheIDs(query.ParentID, items) {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			refreshedParents[id] = cloneBead(fresh)
		case errors.Is(err, ErrNotFound):
			removedParents[id] = struct{}{}
		default:
			c.recordProblem("refresh parent cache during list", fmt.Errorf("%s: %w", id, err))
		}
	}
	for _, id := range c.staleLiveCacheIDs(query, items) {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			refreshedLiveMissing[id] = cloneBead(fresh)
		case errors.Is(err, ErrNotFound):
			removedLiveMissing[id] = struct{}{}
		default:
			c.recordProblem("refresh live cache during list", fmt.Errorf("%s: %w", id, err))
		}
	}
	if len(items) == 0 && len(refreshedParents) == 0 && len(removedParents) == 0 &&
		len(refreshedLiveMissing) == 0 && len(removedLiveMissing) == 0 {
		return items
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return items
	}
	now := time.Now()
	refreshed := make([]Bead, 0, len(items))
	for _, item := range items {
		if c.deletedSeq[item.ID] > startSeq {
			continue
		}
		if c.beadSeq[item.ID] > startSeq {
			current, ok := c.beads[item.ID]
			if ok && query.Matches(current) {
				refreshed = append(refreshed, cloneBead(current))
			}
			continue
		}
		if current, keep := c.recentLocalBeadConflictLocked(item.ID, item, now, false); keep {
			if query.Matches(current) {
				refreshed = append(refreshed, current)
			}
			continue
		}
		if c.beadSeq[item.ID] == startSeq {
			current, ok := c.beads[item.ID]
			if ok && current.Status == "closed" && item.Status != "closed" {
				continue
			}
		}
		c.absorbFreshLocked(item.ID, item, now, absorbOpts{
			depsMode:   depsFromFieldsIfCarried,
			seqMode:    seqClearGuarded,
			clearDirty: true,
		})
		if query.Matches(item) {
			refreshed = append(refreshed, cloneBead(item))
		}
	}
	for id, bead := range refreshedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if _, keep := c.recentLocalBeadConflictLocked(id, bead, now, false); keep {
			continue
		}
		c.absorbFreshLocked(id, bead, now, absorbOpts{
			depsMode:   depsFromFieldsIfCarried,
			seqMode:    seqClearGuarded,
			clearDirty: true,
		})
	}
	for id := range removedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if current, ok := c.beads[id]; ok && current.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
			continue
		}
		c.evictLocked(id)
	}
	for id, bead := range refreshedLiveMissing {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if _, keep := c.recentLocalBeadConflictLocked(id, bead, now, false); keep {
			continue
		}
		c.absorbFreshLocked(id, bead, now, absorbOpts{
			depsMode:   depsFromFieldsIfCarried,
			seqMode:    seqClearGuarded,
			clearDirty: true,
		})
	}
	for id := range removedLiveMissing {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		if current, ok := c.beads[id]; ok && current.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
			continue
		}
		c.evictLocked(id)
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	return refreshed
}

func (c *CachingStore) staleParentCacheIDs(parentID string, fresh []Bead) []string {
	if parentID == "" {
		return nil
	}

	freshIDs := make(map[string]struct{}, len(fresh))
	for _, item := range fresh {
		freshIDs[item.ID] = struct{}{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil
	}

	var stale []string
	for id, bead := range c.beads {
		if bead.ParentID != parentID {
			continue
		}
		if _, ok := freshIDs[id]; ok {
			continue
		}
		stale = append(stale, id)
	}
	return stale
}

func (c *CachingStore) staleLiveCacheIDs(query ListQuery, fresh []Bead) []string {
	if !query.Live || query.Limit > 0 || query.IncludesClosed() {
		return nil
	}

	freshIDs := make(map[string]struct{}, len(fresh))
	for _, item := range fresh {
		freshIDs[item.ID] = struct{}{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil
	}

	var stale []string
	for id, bead := range c.beads {
		if _, ok := freshIDs[id]; ok {
			continue
		}
		if !query.Matches(bead) {
			continue
		}
		stale = append(stale, id)
	}
	return stale
}

// ListOpen returns all cached beads, optionally filtered by status.
func (c *CachingStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return c.List(query)
}

// getBackingOrLastGood dials the backing store and, when the store gives no
// answer at all, serves the last-good cached bead — the same reality-first
// contract List uses. A genuine miss (ErrNotFound) PROPAGATES: the cache
// cannot prove absence, so a miss must never be manufactured from a snapshot
// (TestCachingStoreDownGetServesLastGoodAndPropagatesMisses).
func (c *CachingStore) getBackingOrLastGood(id string) (Bead, error) {
	b, err := c.backing.Get(id)
	if err == nil || errors.Is(err, ErrNotFound) {
		return b, err
	}
	if lg, lerr := c.getLastGood(id); lerr == nil {
		c.recordProblem("get served last-good after backing failure", err)
		return lg, nil
	}
	return b, err
}

// Get returns a single bead by ID from the cache or backing store.
func (c *CachingStore) Get(id string) (Bead, error) {
	// Breaker open: serve the last-good cached bead; an uncached ID is
	// ErrStoreUnavailable because the cache cannot prove absence.
	if c.servingDegraded() {
		return c.getLastGood(id)
	}
	c.mu.RLock()
	if _, deleted := c.deletedSeq[id]; deleted {
		c.mu.RUnlock()
		return Bead{}, ErrNotFound
	}
	if _, mutated := c.beadSeq[id]; mutated {
		if _, dirty := c.dirty[id]; !dirty {
			if b, ok := c.beads[id]; ok {
				invalid := c.readyProjectionInvalidLocked(id)
				c.mu.RUnlock()
				return projectCachedBead(b, invalid), nil
			}
		}
	}
	if c.state == cacheLive || c.state == cachePartial {
		if _, ok := c.dirty[id]; ok {
			startSeq := c.mutationSeq
			c.mu.RUnlock()
			fresh, err := c.backing.Get(id)
			if err != nil {
				return Bead{}, err
			}
			c.mu.Lock()
			if c.state != cacheLive && c.state != cachePartial {
				c.mu.Unlock()
				return fresh, nil
			}
			switch {
			case c.deletedSeq[id] > startSeq:
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			case c.beadSeq[id] > startSeq:
				if _, stillDirty := c.dirty[id]; stillDirty {
					c.mu.Unlock()
					return c.getBackingOrLastGood(id)
				}
				if current, ok := c.beads[id]; ok {
					invalid := c.readyProjectionInvalidLocked(id)
					c.mu.Unlock()
					return projectCachedBead(current, invalid), nil
				}
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			}
			c.absorbFreshLocked(id, fresh, time.Now(), absorbOpts{
				depsMode:   depsFromFields,
				seqMode:    seqClearBeadSeqOnly,
				clearDirty: true,
			})
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return fresh, nil
		}
		if b, ok := c.beads[id]; ok {
			invalid := c.readyProjectionInvalidLocked(id)
			c.mu.RUnlock()
			return projectCachedBead(b, invalid), nil
		}
		c.mu.RUnlock()
		return c.getBackingOrLastGood(id)
	}
	c.mu.RUnlock()
	return c.getBackingOrLastGood(id)
}

// Ready returns open beads whose blocking deps are all closed.
func (c *CachingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	if readyQueryFromArgs(query) != (ReadyQuery{}) {
		return c.backing.Ready(query...)
	}
	var (
		statusByID   map[string]string
		depsByID     map[string][]Dep
		openBeads    []Bead
		readyInvalid map[string]struct{}
		unanswerable bool
	)
	// Ready requires a fully live cache with complete dependency coverage and a
	// ready projection the backing store can actually serve; the overlay
	// refreshes any dirty rows first, then computes readiness from the cache.
	// On overlay error the read takes the old full backing.Ready scan.
	if err := c.readCacheWithOverlay(
		func() bool {
			return c.state == cacheLive && c.depsComplete && c.primePartialErr == nil &&
				!c.readyReadsMustGoLive()
		},
		func(suppressed map[string]struct{}) {
			statusByID = make(map[string]string, len(c.beads))
			openBeads = make([]Bead, 0, len(c.beads))
			now := time.Now().UTC()
			for _, b := range c.beads {
				if _, gone := suppressed[b.ID]; gone {
					continue
				}
				statusByID[b.ID] = b.Status
				if IsReadyCandidate(b, now) {
					if c.readyProjectionUnknownLocked(b.ID) {
						unanswerable = true
						return
					}
					openBeads = append(openBeads, cloneBead(b))
				}
			}
			depsByID = make(map[string][]Dep, len(openBeads))
			for _, b := range openBeads {
				depsByID[b.ID] = cloneDeps(c.deps[b.ID])
			}
			readyInvalid = c.readyProjectionInvalidSnapshotLocked(openBeads)
		},
	); err != nil {
		return c.backing.Ready(query...)
	}
	if unanswerable {
		// One candidate whose verdict the cache cannot vouch for costs this
		// read the cache, not correctness: the live scan is slower and right.
		return c.backing.Ready(query...)
	}

	var result []Bead
	for _, b := range openBeads {
		if cachedBeadReady(b, statusByID, depsByID[b.ID], mapHasKey(readyInvalid, b.ID)) {
			result = append(result, cloneBead(b))
		}
	}
	// c.beads is a map, so the scan above yields a different order per
	// call; impose the canonical ready order so cache-served results
	// match the SQL-backed ready readers (#3208).
	sortBeadsReadyOrder(result)
	return result, nil
}

// ReadyContext answers only from the dependency-complete active cache. It
// deliberately does not fall back to the context-blind backing Ready method:
// deadline-sensitive callers must receive ErrCacheUnavailable instead of
// abandoning database work after their context expires.
func (c *CachingStore) ReadyContext(ctx context.Context, query ...ReadyQuery) ([]Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := c.cachedReadyCompleteOnly(ctx, readyQueryFromArgs(query))
	if err != nil {
		return rows, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// CachedReady returns ready beads from the in-memory active read model.
// The boolean reports whether the cache was initialized enough to answer
// without touching the backing store. Unlike Ready, this can answer from a
// partial active cache only when each open bead has known dependency coverage.
//
// Like CachedList, this strict cache-only handle keeps the conservative
// "dirty ⇒ decline" contract so a caller relying on cache-only semantics never
// observes a row refreshed behind its back or a stale ready candidate (#2210).
func (c *CachingStore) CachedReady() ([]Bead, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil, false
	}
	if c.primePartialErr != nil || len(c.dirty) > 0 || c.readyReadsMustGoLive() {
		return nil, false
	}

	statusByID := make(map[string]string, len(c.beads))
	openBeads := make([]Bead, 0, len(c.beads))
	now := time.Now().UTC()
	for _, b := range c.beads {
		statusByID[b.ID] = b.Status
		if IsReadyCandidate(b, now) {
			if c.readyProjectionUnknownLocked(b.ID) {
				return nil, false
			}
			openBeads = append(openBeads, cloneBead(b))
		}
	}

	result := make([]Bead, 0, len(openBeads))
	for _, b := range openBeads {
		deps, ok := c.deps[b.ID]
		switch {
		case ok:
		case c.depsComplete:
			deps = nil
		default:
			return nil, false
		}
		if cachedBeadReady(b, statusByID, deps, c.readyProjectionInvalidLocked(b.ID)) {
			result = append(result, cloneBead(b))
		}
	}
	// Map-scan order is nondeterministic; match the canonical ready order of
	// the SQL-backed ready readers (#3208).
	sortBeadsReadyOrder(result)
	return result, true
}

// projectionInvalid is ADR-0094's replacement for the nil sentinel: when the
// cache has invalidated this row's is_blocked verdict and not yet re-observed
// it, the cached value must be ignored and readiness derived from the
// dependency edges instead — the same fallback an IsBlocked == nil row takes.
// The verdict itself stays in the row so the reconcile differ keeps comparing
// like with like (see CachingStore.readyProjectionInvalid).
func cachedBeadReady(b Bead, statusByID map[string]string, deps []Dep, projectionInvalid bool) bool {
	if b.IsBlocked != nil && !projectionInvalid {
		return !*b.IsBlocked
	}
	for _, dep := range deps {
		if !isReadyBlockingDependencyType(dep.Type) {
			continue
		}
		if status, ok := statusByID[dep.DependsOnID]; ok && status != "closed" {
			return false
		}
	}
	return true
}

// Children returns beads with the given parent ID.
func (c *CachingStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
	})
}

// ListByLabel returns beads matching the given label. By default, serves from
// cache only (non-closed beads). Pass IncludeClosed to also query the backing
// store for closed beads and merge results.
func (c *CachingStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with matching status.
func (c *CachingStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return c.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata filters beads by metadata key-value pairs. By default, serves
// from cache only (non-closed beads). Pass IncludeClosed to also query the
// backing store for closed beads and merge results.
func (c *CachingStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

func matchesMetadata(b Bead, filters map[string]string) bool {
	for k, v := range filters {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepList returns dependencies for a bead in the given direction.
func (c *CachingStore) DepList(id, direction string) ([]Dep, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		if direction == "down" || direction == "" {
			if !c.depsComplete {
				c.mu.RUnlock()
				return c.backing.DepList(id, direction)
			}
			if deps, ok := c.deps[id]; ok {
				c.mu.RUnlock()
				return cloneDeps(deps), nil
			}
			// Dep not cached yet - fetch from backing and cache it.
			c.mu.RUnlock()
			deps, err := c.backing.DepList(id, direction)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.deps[id] = cloneDeps(deps)
			c.mu.Unlock()
			return deps, nil
		}
		// Reverse lookups are only partially cached; defer to the backing
		// store so callers do not observe incomplete results.
		c.mu.RUnlock()
		return c.backing.DepList(id, direction)
	}
	c.mu.RUnlock()
	return c.backing.DepList(id, direction)
}

// Ping delegates to the backing store.
func (c *CachingStore) Ping() error {
	return c.backing.Ping()
}

// absorbBackingListLocked folds rows returned by a successful degraded-path
// dial into the snapshot, so last-good converges toward reality during an
// outage instead of staying pinned at the pre-outage state. Guards mirror
// the Get dirty-refresh path: a row whose local delete or write is newer
// than the dial (its seq moved past startSeq) keeps the local truth, and an
// unprimed cache absorbs nothing — a fallback snapshot must never be
// fabricated from one filtered read. Rows are updated or added, never
// evicted: absence from a filtered result is not evidence of deletion.
func (c *CachingStore) absorbBackingListLocked(items []Bead, startSeq uint64) {
	if len(items) == 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == cacheUninitialized {
		return
	}
	for _, b := range items {
		if c.deletedSeq[b.ID] > startSeq || c.beadSeq[b.ID] > startSeq {
			continue
		}
		c.absorbFreshLocked(b.ID, b, now, absorbOpts{
			// List rows are shape-reduced: unlike backing.Get results they
			// often omit dep fields, and depsFromFields would wipe cached
			// deps (Ready would then treat every bead as unblocked). Only
			// recompute deps when the row actually carries them.
			depsMode:   depsFromFieldsIfCarried,
			seqMode:    seqClearBeadSeqOnly,
			clearDirty: true,
		})
	}
}

// mapHasKey reports set membership for the ADR-0094 invalidation snapshots,
// which are nil when nothing is invalid.
func mapHasKey(set map[string]struct{}, id string) bool {
	_, ok := set[id]
	return ok
}

// backingListCtx routes a backing-store list read through the backing's
// optional CtxLister capability when it has one, so a canceled ctx aborts the
// read; stores without the capability fall back to the plain List.
func (c *CachingStore) backingListCtx(ctx context.Context, query ListQuery) ([]Bead, error) {
	if cl, ok := c.backing.(CtxLister); ok {
		return cl.ListCtx(ctx, query)
	}
	return c.backing.List(query)
}

// projectCachedBead clones a cached row for an EXTERNAL reader, withholding an
// is_blocked verdict this cache has invalidated and not yet re-observed.
//
// ADR-0094 keeps the invalidated verdict in c.beads on purpose: the reconcile
// differ must compare what the backing last reported, or a cache-internal
// "re-ask" reads as a store-side transition and floods bead.updated. But every
// reader OUTSIDE this package reads Bead.IsBlocked as the backing's answer, and
// several treat nil as their fail-open case (bindNamedSessionTriggerBead's
// staleness test, computeAwakeBridge's blocked test, gc bead state). Handing
// them a verdict this cache has already disowned makes them act on a value the
// cache itself does not trust.
//
// So the value is STORED but not SERVED: the differ keeps its like-for-like
// comparison, and callers see the same nil the in-band sentinel used to give
// them. The readiness readers do not go through here — they take the
// projectionInvalid flag directly (see cachedBeadReady).
func projectCachedBead(b Bead, projectionInvalid bool) Bead {
	out := cloneBead(b)
	if projectionInvalid {
		out.IsBlocked = nil
	}
	return out
}

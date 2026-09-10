package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
)

// L1's tick-path isolation for the vc-ny00 store-warming plan.
//
// THE BREAKER HAS TWO READERS. The reconciler consults it through its own
// CachingStore (caching_store_reconcile.go). The order-tracking sweep
// watchdogs do not: they run inside the SERIAL TICK and open a fresh
// beads.Store per scope on every pass, so nothing they hold knows what the
// reconciler has already proven about that store. Left unfiltered, the
// sweeps walk straight into the store whose breaker is open and serialize
// the tick behind it — which is the 2026-09-06 failure exactly, since
// dispatchOrders runs in the same serial tick body and the whole point of
// bounding the tick is that order dispatch stays alive while a store is
// degraded.
//
// WHY SKIPPING IS SAFE HERE. Both watchdogs are best-effort recovery passes
// that run on a cadence (stale-tracking close and closed-tracking
// retention). Skipping a degraded store defers its recovery to the next
// pass after the breaker closes; it never loses work, because the stale
// tracking beads it would have closed are still stale on the next pass. The
// alternative — blocking the tick on a store that cannot answer — is the
// outage this plan exists to convert into a degradation.

// residency:allow — a caller's own list, filtered. It takes the []beads.Store
// the sweep already resolved and returns a SUBSET of it; it enumerates
// nothing, resolves no owner, and can only ever remove entries.
//
// filterDegradedSweepStores drops stores whose vc-ny00 breaker is open and
// announces what it skipped. A store whose scope cannot be identified is
// always kept: "unknown" must never mean "skip", or a store-type change
// would silently disable the sweep.
func filterDegradedSweepStores(stores []beads.Store, stderr io.Writer, logPrefix string) []beads.Store {
	if len(stores) == 0 {
		return stores
	}
	kept := make([]beads.Store, 0, len(stores))
	var skipped []string
	for _, store := range stores {
		prefix, ok := sweepStorePrefix(store)
		if !ok || !beads.StoreIsDegraded(prefix) {
			kept = append(kept, store)
			continue
		}
		skipped = append(skipped, prefix)
	}
	if len(skipped) > 0 && stderr != nil {
		// Announced every pass, not once per episode: unlike the
		// reconciler's own skip, this one runs on the tick cadence and the
		// operator's question during an incident is "is the tick still
		// moving", which a per-pass line answers and a once-per-episode
		// line does not.
		msg := fmt.Sprintf("%s: order tracking sweep: skipping degraded store(s) %v; "+
			"serving the rest of the sweep (vc-ny00 L1)\n", logPrefix, skipped)
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
	}
	return kept
}

// sweepStorePrefix recovers a sweep store's bead-id prefix. Reports ok=false
// when the store does not expose one.
func sweepStorePrefix(store beads.Store) (string, bool) {
	if store == nil {
		return "", false
	}
	prefixer, ok := store.(interface{ IDPrefix() string })
	if !ok {
		return "", false
	}
	prefix := prefixer.IDPrefix()
	if prefix == "" {
		return "", false
	}
	return prefix, true
}

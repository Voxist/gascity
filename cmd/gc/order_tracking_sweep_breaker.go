package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Tick-path isolation for the order-tracking sweep (vc-ny00).
//
// THE BREAKER IS THE SCOPE'S TRANSPORT BREAKER. There is one per scope
// (bdScopeBreaker, configured by [beads.resilience]); the bd runner records
// every invocation's outcome into it — a bd call killed at its per-command
// deadline counts as a transport failure (ga-2bo4m) — and the scope's
// CachingStore reads it as its availability gate, so the reconciler already
// skips a scope it holds open.
//
// The order-tracking sweep watchdogs need their own check. They run inside
// the SERIAL TICK and may open a fresh beads.Store per scope on every pass;
// left unfiltered, they would walk into a scope the breaker has already
// declared unavailable. dispatchOrders runs in the same serial tick body, and
// order dispatch must stay alive while a store is degraded.
//
// WHY SKIPPING IS SAFE HERE. Both watchdogs are best-effort recovery passes
// that run on a cadence (stale-tracking close and closed-tracking
// retention). Skipping a degraded store defers its recovery to the next
// pass after the breaker closes; it never loses work, because the stale
// tracking beads it would have closed are still stale on the next pass. The
// alternative — blocking the tick on a store that cannot answer — is the
// 2026-09-06 outage (vc-5gui).

// residency:allow — a caller's own list, filtered. It takes the []beads.Store
// the sweep already resolved and returns a SUBSET of it; it enumerates
// nothing, resolves no owner, and can only ever remove entries.
//
// filterDegradedSweepStores drops stores whose scope transport breaker is
// not closed and announces what it skipped. A store whose scope root cannot
// be identified is always kept: "unknown" must never mean "skip", or a
// store-type change would silently disable the sweep.
func filterDegradedSweepStores(cityPath string, stores []beads.Store, stderr io.Writer, logPrefix string) []beads.Store {
	if len(stores) == 0 {
		return stores
	}
	kept := make([]beads.Store, 0, len(stores))
	var skipped []string
	for _, store := range stores {
		scoped, ok := store.(orderTrackingSweepScopedStore)
		if !ok || strings.TrimSpace(scoped.scopeRoot) == "" ||
			bdScopeBreaker(cityPath, scoped.scopeRoot).Available() {
			kept = append(kept, store)
			continue
		}
		skipped = append(skipped, scoped.label)
	}
	if len(skipped) > 0 && stderr != nil {
		// Announced every pass, not once per episode: unlike the
		// reconciler's own skip, this one runs on the tick cadence and the
		// operator's question during an incident is "is the tick still
		// moving", which a per-pass line answers and a once-per-episode
		// line does not.
		msg := fmt.Sprintf("%s: order tracking sweep: skipping degraded store(s) %v "+
			"(scope circuit breaker open); serving the rest of the sweep (vc-ny00)\n", logPrefix, skipped)
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
	}
	return kept
}

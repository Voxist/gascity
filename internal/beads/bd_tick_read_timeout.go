package beads

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// L1 of the vc-ny00 store-warming plan: the tick-context read bound.
//
// THE DEFECT. Every bd call from the supervisor is bounded — but at 120s
// (bdCommandTimeout / bdReadCommandTimeout), four times the listener's own
// settled steady-state deadline of read_timeout_millis=30000. The
// reconciler tick is ONE serial goroutine (city_runtime.runTick →
// safeTick(cr.tick)), so a store that stops answering does not cost the
// tick one slow read: it costs every read of every phase, serially, at up
// to 120s each. Measured live on 2026-09-06 (vc-5gui): first
// deadline-exceeded at 00:32:49Z, then supervisor-wide log silence
// 00:33-00:48 and 00:53-01:05, every one of 20 checked cooldown orders
// simultaneously stale, and the order-liveness probe paging the fleet for
// a supervisor that was not dead but blocked.
//
// THE BOUND. Inside a tick frame, reads of the bd read class
// (count|list|ready|show|sql|stats|version) get bdTickReadTimeout instead
// — 10s by default. The relation 10s < 30000ms is LOAD-BEARING, not a
// round number: the client must give up BEFORE the server's own deadline
// reaps the query, so the tick pays the client bound and not the wall. It
// also keeps a fully-degraded fleet's worst-case tick at stores × 10s once
// (and then breaker-zero — see storeWarmingTracker) instead of unbounded.
//
// WHAT IT DOES NOT DO. It does not change read_timeout_millis, which stays
// at the operator-settled 30000 everywhere in steady state (decision B,
// vc-t6xp / ADR-0064); the only raised value in the tree remains the nested
// delivery-window server's 600000. It does not touch non-tick callers —
// CLI commands, hooks and sling keep 120s, because their cost model is a
// human waiting for one command, not a serial loop over every store.

// defaultBdTickReadTimeout is the tick-context read bound. See the file
// comment for why it must stay below the listener's steady-state wall.
const defaultBdTickReadTimeout = 10 * time.Second

// bdTickReadTimeoutEnv is the kill switch and the tuning knob.
// GC_BD_TICK_READ_TIMEOUT_S=0 (or a negative/garbage value) disables the
// tick bound entirely, restoring byte-identical pre-plan behavior: read
// class calls fall through to bdReadCommandTimeout in every context. Set
// it to 120 for the same effect stated as a value rather than an absence.
const bdTickReadTimeoutEnv = "GC_BD_TICK_READ_TIMEOUT_S"

// bdTickReadTimeoutFn resolves the bound. A package var so tests can drive
// the timeout without mutating process environment, matching the seam
// convention used elsewhere in this package.
var bdTickReadTimeoutFn = resolveBdTickReadTimeout

// resolveBdTickReadTimeout reads the env knob on every call rather than
// caching at init. A bd call already spawns a subprocess, so one getenv is
// free at this granularity, and resolving live means an operator can turn
// the bound off on a running supervisor by editing the unit environment and
// reloading — no rebuild, no restart of the reconciler loop.
//
// Returns ok=false when the mechanism is disabled, so the caller falls
// through to the unmodified read path instead of substituting a zero
// duration (which context.WithTimeout would treat as already expired).
func resolveBdTickReadTimeout() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(bdTickReadTimeoutEnv))
	if raw == "" {
		return defaultBdTickReadTimeout, true
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// bdTickReadBound returns the bound that applies to a read-class bd call
// made from the current goroutine: the tick bound inside a tick frame, and
// bdReadCommandTimeout everywhere else.
func bdTickReadBound() time.Duration {
	if bound, ok := bdTickReadTimeoutFn(); ok && InReconcilerTick() {
		return bound
	}
	return bdReadCommandTimeout
}

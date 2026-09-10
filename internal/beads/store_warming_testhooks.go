package beads

import "time"

// Test hooks for the vc-ny00 store-warming machinery.
//
// These are exported because the layers that CONSUME the state machine live
// in cmd/gc — the tick-path sweep filter, the durable state file, the
// warming pass — and their tests must drive the real state machine rather
// than a reimplementation of it. A cmd/gc test that hand-set a degraded flag
// would pass while the production predicate diverged, which is the failure
// mode these hooks exist to prevent.
//
// They are in a non-test file because Go does not export test helpers across
// package boundaries. Nothing in production calls them; the naming
// convention (…ForTest) matches NewCachingStoreForTest.

// ResetStoreWarmingRegistryForTest drops every tracked store so one test's
// degraded store is never another's starting condition. The registry is
// process-global by design (one verdict per logical store, shared by every
// holder of it), which is exactly why tests must be able to clear it.
func ResetStoreWarmingRegistryForTest() {
	resetStoreWarmingRegistryForTest()
}

// RecordStoreProbeForTest folds one synthetic probe into a store's real
// state machine, so a test can drive a store to degraded through the
// production transition rules rather than by setting a flag.
// The bound passed is the non-tick read bound, so the probe is judged
// against the listener wall — the steady-state case a caller outside a tick
// frame actually sees.
func RecordStoreProbeForTest(prefix string, elapsed time.Duration, failed bool) {
	st, changed := storeWarmingTrackerFor(prefix).recordProbe(time.Now(), elapsed, failed, bdReadCommandTimeout)
	if changed && storeWarmingEnabled() {
		publishStoreWarmingState(st)
	}
}

// TickReadBoundForTest reports the read bound that applies inside a
// reconciler tick frame — what L1 actually bounds a tick-context read at.
func TickReadBoundForTest() time.Duration {
	prev := SetReconcilerTickTrigger("test")
	defer RestoreReconcilerTickTrigger(prev)
	return bdTickReadBound()
}

// ReadCommandTimeoutForTest reports the pre-plan read bound that non-tick
// callers still use, so a test can assert the tick bound improved on it.
func ReadCommandTimeoutForTest() time.Duration {
	return bdReadCommandTimeout
}

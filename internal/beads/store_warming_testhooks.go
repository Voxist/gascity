package beads

import "time"

// Test hooks for the vc-ny00 store-warming machinery.
//
// These are exported because the layers that CONSUME the state machine and
// the tick read bound live in cmd/gc — the durable state file, the warming
// pass, the bd runner that counts a tick-bound timeout against the scope
// breaker — and their tests must drive the real mechanism rather than a
// reimplementation of it.
//
// They are in a non-test file because Go does not export test helpers across
// package boundaries. Nothing in production calls them; the naming
// convention (…ForTest) matches NewCachingStoreForTest.

// ResetStoreWarmingRegistryForTest drops every tracked store so one test's
// warming store is never another's starting condition. The registry is
// process-global by design (one verdict per logical store, shared by every
// holder of it), which is exactly why tests must be able to clear it.
func ResetStoreWarmingRegistryForTest() {
	resetStoreWarmingRegistryForTest()
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

// SetTickReadTimeoutForTest pins the tick-context read bound to d for the
// duration of a test, so a test can drive a read past it without waiting the
// production 10s. Restored on cleanup.
func SetTickReadTimeoutForTest(t interface{ Cleanup(func()) }, d time.Duration) {
	prev := bdTickReadTimeoutFn
	bdTickReadTimeoutFn = func() (time.Duration, bool) { return d, true }
	t.Cleanup(func() { bdTickReadTimeoutFn = prev })
}

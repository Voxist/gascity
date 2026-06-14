// provider_health_registry.go — in-process provider-health registry fed by the
// model proxy. Replaces the file-polling gate (provider_health_gate.go snapshot)
// with a live in-memory view that survives store/dispatch failure.
//
// Design (ADR-0013 A1 M3b):
//   - RecordResponse observes HTTP status from the reverse proxy.
//   - After 3 consecutive 429/401 within a 5-minute window, the provider goes red.
//   - 120 seconds with no 429/401 causes automatic failback to green.
//   - SelectHealthy chain-walks the configured failover chain to the first non-red entry.
//   - All state is in-memory; no file I/O.
package main

import (
	"sync"
	"time"
)

const (
	// registryRedThreshold is the number of consecutive quota/auth errors before
	// a provider is marked red.
	registryRedThreshold = 3

	// registryRedWindow is the sliding window in which registryRedThreshold errors
	// must occur to trip the circuit.
	registryRedWindow = 5 * time.Minute

	// registryRecoveryWindow is the quiet period with no errors that causes
	// automatic failback from red to green.
	registryRecoveryWindow = 120 * time.Second
)

// providerEntry tracks per-provider health state in the registry.
type providerEntry struct {
	// recentErrors holds the timestamps of recent 429/401 responses within the
	// red window. Entries older than registryRedWindow are pruned on each call.
	recentErrors []time.Time
	// lastError is the timestamp of the most recent 429/401.
	lastError time.Time
	// red is true when the provider has tripped the red threshold.
	red bool
}

// providerHealthRegistry is the in-memory registry of per-provider health.
// It is safe for concurrent use.
type providerHealthRegistry struct {
	mu      sync.Mutex
	entries map[string]*providerEntry
}

// newProviderHealthRegistry allocates an empty registry.
func newProviderHealthRegistry() *providerHealthRegistry {
	return &providerHealthRegistry{
		entries: make(map[string]*providerEntry),
	}
}

// RecordResponse records an HTTP status from the model proxy for the named provider.
// Only 429 (rate limited) and 401 (unauthorized) are treated as health signals;
// all other statuses are treated as successful probes.
func (r *providerHealthRegistry) RecordResponse(provider string, status int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entryFor(provider)

	isError := status == 429 || status == 401
	if !isError {
		// Successful response: prune old errors; if quiet long enough, go green.
		e.recentErrors = pruneOldErrors(e.recentErrors, now, registryRedWindow)
		if e.red && now.Sub(e.lastError) >= registryRecoveryWindow {
			e.red = false
		}
		return
	}

	e.lastError = now
	e.recentErrors = append(pruneOldErrors(e.recentErrors, now, registryRedWindow), now)
	if len(e.recentErrors) >= registryRedThreshold {
		e.red = true
	}
}

// Check returns the health of a provider.
//   - healthy=true, present=false: no record → fail-open (caller must treat as green).
//   - healthy=false, present=true: provider is red; gate respawn.
//   - healthy=true, present=true: provider is green; allow respawn.
func (r *providerHealthRegistry) Check(provider string) (healthy, present bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[provider]
	if !ok {
		return true, false
	}
	return !e.red, true
}

// SelectHealthy returns the first provider in chain that is not red.
// If all entries in chain are red, returns "".
// Providers absent from the registry are treated as green (fail-open).
func (r *providerHealthRegistry) SelectHealthy(chain []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range chain {
		e, ok := r.entries[p]
		if !ok || !e.red {
			return p
		}
	}
	return ""
}

// Snapshot returns a providerHealthSnapshot for compatibility with the
// existing reconciler gate path. The snapshot is immutable once returned.
func (r *providerHealthRegistry) Snapshot() *providerHealthSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 0 {
		return &providerHealthSnapshot{present: false}
	}
	snap := &providerHealthSnapshot{
		present: true,
		entries: make(map[string]bool, len(r.entries)),
	}
	for p, e := range r.entries {
		snap.entries[p] = !e.red
	}
	return snap
}

// entryFor returns the providerEntry for name, creating it if absent. Must be
// called with r.mu held.
func (r *providerHealthRegistry) entryFor(name string) *providerEntry {
	if e, ok := r.entries[name]; ok {
		return e
	}
	e := &providerEntry{}
	r.entries[name] = e
	return e
}

// pruneOldErrors removes timestamps older than window from the slice and
// returns the trimmed slice.
func pruneOldErrors(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

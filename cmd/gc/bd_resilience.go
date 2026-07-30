package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
)

// bdResilience holds the process-wide breaker registries, one per city.
// Breakers are keyed (scope root, opClass) inside each registry, so one
// scope's wedged transport quarantines only that scope (plan items
// 1.2+1.3). Settings are read from the city's [beads.resilience] config
// once per process; changing them requires a controller restart.
var bdResilience = struct {
	mu         sync.Mutex
	registries map[string]*resilience.Registry
	// provisional marks registries built from an unreadable config, so the
	// first clean read can replace them with the operator's real settings.
	provisional map[string]bool
}{
	registries:  make(map[string]*resilience.Registry),
	provisional: make(map[string]bool),
}

// bdResilienceSettingsForCity resolves [beads.resilience] for a city,
// falling back to defaults when the config cannot be loaded.
func bdResilienceSettingsForCity(cityPath string) (resilience.Settings, bool) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return resilience.DefaultSettings(), false
	}
	r := cfg.Beads.Resilience
	return resilience.Settings{
		Enabled:             r.EnabledOrDefault(),
		ConsecutiveFailures: r.ConsecutiveFailuresOrDefault(),
		OpenBase:            r.OpenBaseOrDefault(),
		OpenMax:             r.OpenMaxOrDefault(),
		HalfOpenInterval:    r.HalfOpenIntervalOrDefault(),
	}, true
}

// bdResilienceRegistryForCity returns the breaker registry for a city,
// creating it (with the city's configured settings) on first use.
func bdResilienceRegistryForCity(cityPath string) *resilience.Registry {
	key := filepath.Clean(cityPath)
	bdResilience.mu.Lock()
	if reg, ok := bdResilience.registries[key]; ok {
		bdResilience.mu.Unlock()
		return reg
	}
	bdResilience.mu.Unlock()

	// Config load happens outside the lock (file I/O); recheck after.
	settings, configured := bdResilienceSettingsForCity(key)
	bdResilience.mu.Lock()
	defer bdResilience.mu.Unlock()
	if reg, ok := bdResilience.registries[key]; ok {
		if !bdResilience.provisional[key] || !configured {
			return reg
		}
		// The cached registry was built from an unreadable config; the config
		// now reads cleanly, so replace it with the operator's real settings.
		// This happens at most once per city and costs only the breaker state
		// accumulated during the unreadable window.
	}
	reg := resilience.NewRegistry(settings)
	reg.SetOnStateChange(breakerStateChangeEmitter(key))
	// Always memoize, even when provisional. A registry rebuilt on every call
	// hands out a fresh breaker with failures=0, so the consecutive-failure
	// count never accumulates and the breaker can never trip; worse, a store
	// that captured a gate from a discarded registry keeps it for the process
	// lifetime. Tracking the provisional flag fixes the stale-settings problem
	// without giving up accumulation.
	bdResilience.registries[key] = reg
	bdResilience.provisional[key] = !configured
	return reg
}

// breakerStateChangeEmitter returns a resilience state-change callback that
// records a typed breaker.state_changed event into the city's event log.
// Emission is best-effort: a recorder open/marshal failure must never block
// or panic the bd transport path that triggered the transition. The callback
// is invoked synchronously from the state-changing call, so it stays cheap —
// one append per transition (transitions are rare: trip, probe, recover).
func breakerStateChangeEmitter(cityPath string) func(resilience.Transition) {
	return func(t resilience.Transition) {
		payload, err := json.Marshal(events.BreakerStateChangedPayload{
			Scope:     t.Scope,
			OpClass:   t.OpClass,
			From:      t.From.String(),
			To:        t.To.String(),
			Failures:  t.Failures,
			BackoffMs: t.Backoff.Milliseconds(),
		})
		if err != nil {
			return
		}
		rec, err := events.NewFileRecorder(filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"), io.Discard)
		if err != nil {
			return
		}
		defer rec.Close() //nolint:errcheck // best-effort: emission must not surface I/O errors
		rec.Record(events.Event{
			Type:    events.BreakerStateChanged,
			Actor:   eventActor(),
			Subject: t.Scope,
			Payload: payload,
		})
	}
}

// bdScopeBreaker returns the shared transport breaker for a scope root.
// The same instance gates the bd subprocess runner (fail fast with
// beads.ErrStoreUnavailable, zero subprocess) and the scope's
// CachingStore (serve last-good reads tagged degraded), so a trip at
// either chokepoint protects both.
func bdScopeBreaker(cityPath, dir string) *resilience.Breaker {
	scope := strings.TrimSpace(dir)
	if scope == "" {
		scope = cityPath
	}
	return bdResilienceRegistryForCity(cityPath).Breaker(filepath.Clean(scope), resilience.OpClassBd)
}

// wireStoreAvailabilityGate attaches the scope's transport breaker to a
// (possibly policy-wrapped) CachingStore so breaker-open reads serve
// last-good cached data and the reconciler skips cycles cheaply. No-op
// for stores without a caching layer.
func wireStoreAvailabilityGate(store beads.Store, cityPath, scopeRoot string) {
	if store == nil {
		return
	}
	base, _, _ := unwrapBeadPolicyStore(store)
	if cache, ok := base.(*beads.CachingStore); ok {
		cache.SetAvailabilityGate(bdScopeBreaker(cityPath, scopeRoot))
	}
}

// recordBdBreakerOutcome reports one bd invocation's final outcome to the
// scope breaker. transportFailure must come from the pinned
// bdTransportRetryableMarkers classification: only transport-class
// failures count toward tripping; success or application-class errors
// (bd reached the store and answered) reset the consecutive count.
func recordBdBreakerOutcome(breaker *resilience.Breaker, transportFailure, conclusive bool) {
	if breaker == nil {
		return
	}
	if !conclusive {
		// An unclassifiable error is not evidence the transport is healthy, so
		// it must not reset the consecutive-failure count. But it must still
		// RESOLVE a probe that Allow() already consumed: leaving a half-open
		// breaker unresolved pins the scope in degraded mode forever, because
		// it neither closes (no success) nor re-arms its backoff (no failure),
		// and once open the reconcile probe is the only bd traffic left.
		if breaker.State() == resilience.StateHalfOpen {
			breaker.RecordFailure()
		}
		return
	}
	if transportFailure {
		breaker.RecordFailure()
		return
	}
	breaker.RecordSuccess()
}

// bdBreakerOutcomeFor classifies one bd invocation for breaker accounting.
//
// Deliberately NOT bdTransportRetryableError. That answers "is another attempt
// worth making?", and a command timeout is not — it already burned the full
// budget. But a timeout IS evidence the transport never answered, which is what
// the breaker must count. Conflating the two made a hung backend record as a
// SUCCESS, resetting the failure count, so the breaker could never trip for the
// subprocess pile-up it exists to prevent.
//
// The third state is not a way to opt scopes out of the breaker: an error that
// is neither a known transport marker nor a no-answer signal is genuinely
// ambiguous, so it neither counts as a failure nor certifies health.
func bdBreakerOutcomeFor(cityPath, dir string, env map[string]string, err error) (transportFailure, conclusive bool) {
	if err == nil {
		return false, true
	}
	if bdTransportRetryableError(cityPath, dir, env, err) {
		return true, true
	}
	if bdErrorIndicatesNoAnswer(err) {
		return true, true
	}
	return false, false
}

// bdErrorIndicatesNoAnswer reports whether err means bd never got a reply, as
// opposed to getting one it did not like. A wedged backend produces these
// shapes once marker matching has declined the error: bd blocked until the
// command budget expired, so there is no stderr marker to match.
func bdErrorIndicatesNoAnswer(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timed out after") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "signal: killed")
}

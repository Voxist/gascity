package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"

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
}{registries: make(map[string]*resilience.Registry)}

// bdResilienceSettingsForCity resolves [beads.resilience] for a city,
// falling back to defaults when the config cannot be loaded.
func bdResilienceSettingsForCity(cityPath string) resilience.Settings {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return resilience.DefaultSettings()
	}
	r := cfg.Beads.Resilience
	return resilience.Settings{
		Enabled:             r.EnabledOrDefault(),
		ConsecutiveFailures: r.ConsecutiveFailuresOrDefault(),
		OpenBase:            r.OpenBaseOrDefault(),
		OpenMax:             r.OpenMaxOrDefault(),
		HalfOpenInterval:    r.HalfOpenIntervalOrDefault(),
	}
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
	settings := bdResilienceSettingsForCity(key)
	bdResilience.mu.Lock()
	defer bdResilience.mu.Unlock()
	if reg, ok := bdResilience.registries[key]; ok {
		return reg
	}
	reg := resilience.NewRegistry(settings)
	reg.SetOnStateChange(breakerStateChangeEmitter(key))
	bdResilience.registries[key] = reg
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

// recordBdBreakerOutcome reports one bd invocation's final outcome to the
// scope breaker. An INCONCLUSIVE outcome leaves the breaker untouched: an error
// we cannot classify is not evidence the transport is healthy, and recording it
// as success would reset the consecutive-failure count and let a wedged backend
// hide behind unclassifiable errors.
func recordBdBreakerOutcome(breaker *resilience.Breaker, transportFailure, conclusive bool) {
	if breaker == nil || !conclusive {
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
// Deliberately NOT bdTransportRetryableError. That function answers "is another
// attempt worth making?", and a command timeout is not — it already burned the
// full budget, so retrying doubles the stall. But a timeout IS evidence the
// transport never answered, which is precisely what the breaker must count.
// Conflating the two made the breaker record a hung backend as a SUCCESS,
// resetting the failure count, so it could never trip for the pile-up it exists
// to prevent.
//
// The third state matters as much as the other two: an error that is neither a
// known transport marker nor a no-answer signal (an application-class failure,
// or anything on a scope whose backend gc does not manage) is reported
// inconclusive rather than healthy.
func bdBreakerOutcomeFor(cityPath, dir string, env map[string]string, err error) (transportFailure, conclusive bool) {
	if err == nil {
		return false, true // bd reached the store and answered
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
// opposed to getting one it did not like. Timeouts and kill signals are the
// shapes a wedged backend produces once the marker-based classification has
// already declined it: bd blocked until the command budget expired, so there is
// no stderr marker to match. isBdAmbiguousWriteError treats "timed out after"
// as transport-family for the same reason.
func bdErrorIndicatesNoAnswer(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timed out after") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "signal: killed")
}

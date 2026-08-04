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

// bdTimeoutMarker is the text the beads exec layer produces when a bd command
// is killed at its per-command deadline. bd never answered, so there is no
// stderr marker to classify — a wedged backend is exactly the failure the
// stderr-marker tables cannot see.
const bdTimeoutMarker = "timed out after"

// bdCallerDeadlineMarker distinguishes the caller's own budget expiring from
// the per-command timer firing. A short caller budget (the claim-time
// current_run_id write, for example) says nothing about the backend's health,
// so it must not count against the transport breaker.
const bdCallerDeadlineMarker = "(caller deadline)"

// bdInvocationTimedOut reports whether err is a bd command killed at its own
// per-command deadline — the wedged-backend shape.
//
// Scoped deliberately on both axes the classification can get wrong:
//
//   - name must be "bd". The beads exec runner is shared, and BdStore also
//     drives `dolt sql` through it, so a name-agnostic check would attribute a
//     slow embedded-Dolt fallback to the bd transport and trip the breaker for
//     the whole scope while bd itself is healthy.
//   - the caller-deadline branch is excluded. That branch fires when the
//     caller's budget was the binding one, which is a statement about the
//     caller, not the store.
//
// Unlike the marker tables this sits beside, it deliberately does NOT gate the
// retry or managed-recovery paths — see the call site for why.
func bdInvocationTimedOut(name string, err error) bool {
	if err == nil || name != "bd" {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, bdCallerDeadlineMarker) {
		return false
	}
	return strings.Contains(msg, bdTimeoutMarker)
}

// recordBdBreakerOutcome reports one bd invocation's final outcome to the
// scope breaker. transportFailure must come from the pinned
// bdTransportRetryableMarkers classification: only transport-class
// failures count toward tripping; success or application-class errors
// (bd reached the store and answered) reset the consecutive count.
func recordBdBreakerOutcome(breaker *resilience.Breaker, transportFailure bool) {
	if breaker == nil {
		return
	}
	if transportFailure {
		breaker.RecordFailure()
		return
	}
	breaker.RecordSuccess()
}

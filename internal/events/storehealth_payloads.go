package events

// Breaker, controller-heartbeat, and doctor event types and their typed
// payloads (city-scale architecture plan item 1.9). These are
// emitted by the resilience breaker registry (internal/resilience, wired
// through the bd runner), the controller reconcile loop, and the
// supervisor-cadence doctor.
//
// The constants are declared in events.go, where every event type lives
// (TestEveryDeclaredEventTypeHasAnAuthorship scans that file), and are in
// KnownEventTypes so TestEveryKnownEventTypeHasRegisteredPayload forces a
// registered payload to exist for each one.

// BreakerStateChangedPayload is the typed payload for
// breaker.state_changed events. Mirrors resilience.Transition without
// importing the resilience package into events (Layer-0 keeps no upward
// dependency on the resilience registry).
type BreakerStateChangedPayload struct {
	Scope     string `json:"scope" doc:"Store scope (canonical scope root path)."`
	OpClass   string `json:"op_class" doc:"Operation class, e.g. bd."`
	From      string `json:"from" doc:"Breaker state before the transition."`
	To        string `json:"to" doc:"Breaker state after the transition."`
	Failures  int    `json:"failures,omitempty" doc:"Consecutive transport-failure count at the change."`
	BackoffMs int64  `json:"backoff_ms,omitempty" doc:"Open-state backoff chosen for this episode, in milliseconds."`
}

// IsEventPayload marks BreakerStateChangedPayload as an events.Payload variant.
func (BreakerStateChangedPayload) IsEventPayload() {}

// ControllerTickCompletedPayload is the typed payload for
// controller.tick_completed events — the controller heartbeat. Duration
// and Phase identify what work the tick did; ThresholdBreach flags a tick
// whose duration reached the slow-tick threshold (a multiple of the
// configured patrol interval — see the controller heartbeat constants),
// which the supervisor doctor's slow_ticks check consumes.
type ControllerTickCompletedPayload struct {
	DurationMs      int64  `json:"duration_ms" doc:"Wall-clock duration of the completed tick, in milliseconds."`
	Phase           string `json:"phase" doc:"Tick trigger phase: patrol, poke, control-dispatcher, etc."`
	ThresholdBreach bool   `json:"threshold_breach,omitempty" doc:"True when the tick's duration reached the slow-tick threshold (a multiple of the configured patrol interval)."`
}

// IsEventPayload marks ControllerTickCompletedPayload as an events.Payload variant.
func (ControllerTickCompletedPayload) IsEventPayload() {}

// DoctorAlertPayload is the typed payload for doctor.alert events emitted
// by the supervisor-cadence doctor when a cheap check goes red.
type DoctorAlertPayload struct {
	Check   string `json:"check" doc:"Name of the doctor check that went red."`
	City    string `json:"city,omitempty" doc:"City name the check evaluated, when scoped to a city."`
	Detail  string `json:"detail" doc:"Human-readable description of the red condition."`
	Subject string `json:"subject,omitempty" doc:"Optional subject identifier (scope, path) the alert concerns."`
}

// IsEventPayload marks DoctorAlertPayload as an events.Payload variant.
func (DoctorAlertPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BreakerStateChanged, BreakerStateChangedPayload{})
	RegisterPayload(ControllerTickCompleted, ControllerTickCompletedPayload{})
	RegisterPayload(DoctorAlert, DoctorAlertPayload{})
}

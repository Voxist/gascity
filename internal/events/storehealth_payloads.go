package events

// Breaker, controller-heartbeat, and doctor event types and their typed
// payloads (city-scale architecture plan items 1.5 and 1.9). These are
// emitted by the resilience breaker registry (internal/resilience, wired
// through the bd runner), the controller reconcile loop, and the
// supervisor-cadence doctor.
//
// Every constant below is added to KnownEventTypes (events.go) so the
// Principle-7 coverage test (TestEveryKnownEventTypeHasRegisteredPayload)
// forces a registered payload to exist for each one.

const (
	// BreakerStateChanged fires on every resilience breaker state
	// transition (closed/open/half-open), wired from the breaker
	// registry's state-change callback.
	BreakerStateChanged = "breaker.state_changed"
	// ControllerTickCompleted is the controller heartbeat, emitted once
	// per completed reconcile tick. The unsampled stream is both the
	// supervisor doctor's tick-age/slow-tick signal and a sound basis for
	// tick-period arithmetic — the earlier breach-or-every-10th sampling
	// made it a biased sample that was complete only by coincidence
	// (vp-qvqk). One event per tick is patrol-cadence volume, not a hot
	// path.
	ControllerTickCompleted = "controller.tick_completed"
	// DoctorAlert fires when the supervisor-cadence doctor evaluates a
	// cheap check to red. It is the detector that closes the
	// detection-at-human-cadence hole (incidents 5 and 11).
	DoctorAlert = "doctor.alert"
)

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

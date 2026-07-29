package events

// BreakerStateChanged fires on every resilience breaker state transition
// (closed/open/half-open), wired from the breaker registry's state-change
// callback.
const BreakerStateChanged = "breaker.state_changed"

// BreakerStateChangedPayload is the typed payload for breaker.state_changed
// events. It mirrors resilience.Transition without importing the resilience
// package into events: this is Layer 0 and must keep no upward dependency on
// the breaker registry.
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

func init() {
	RegisterPayload(BreakerStateChanged, BreakerStateChangedPayload{})
}

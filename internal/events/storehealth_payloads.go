package events

// Store-health, breaker, controller-heartbeat, proxy-reap, and doctor
// event types and their typed payloads (city-scale architecture plan
// items 1.5 and 1.9). These are emitted by the controller-internal store
// health patrol (internal/storehealth), the resilience breaker registry
// (internal/resilience, wired through the patrol/runner), the controller
// reconcile loop, and the supervisor-cadence doctor.
//
// Every constant below is added to KnownEventTypes (events.go) so the
// Principle-7 coverage test (TestEveryKnownEventTypeHasRegisteredPayload)
// forces a registered payload to exist for each one.

// Store-degradation classes carried in StoreDegradedPayload.Class. The
// patrol distinguishes a wedged transport (the proxy/connection path) from
// a backend fault (the managed sql-server itself) and from a runtime
// write rejection (the store answers reads but rejects custom-type writes,
// the post-cutover a74fefde8 class a transport-only breaker cannot see).
const (
	// StoreDegradedClassTransport marks degradation where a fresh routed
	// read fails while the direct backend probe succeeds (proxy poison).
	StoreDegradedClassTransport = "transport"
	// StoreDegradedClassBackend marks degradation where the managed
	// sql-server itself is unreachable or read-only. The patrol never
	// auto-kills the sql-server in this class.
	StoreDegradedClassBackend = "backend"
	// StoreDegradedClassWriteRejection marks degradation where the store
	// is reachable but persistently rejects a RequiredCustomType write.
	StoreDegradedClassWriteRejection = "write-rejection"
)

const (
	// StoreDegraded fires when the store health patrol trips a scope's
	// breaker after a confirmed two-probe divergence (or a persistent
	// write-path rejection). Carries the degradation class so subscribers
	// can distinguish transport poison from a backend fault.
	StoreDegraded = "store.degraded"
	// StoreRecovered fires when a previously degraded scope's routed
	// probe passes again and the breaker closes.
	StoreRecovered = "store.recovered"
	// StoreProbeFailed fires for a single failed patrol probe before the
	// consecutive-failure threshold is reached. It is the per-cycle
	// forensic breadcrumb, not the degradation decision.
	StoreProbeFailed = "store.probe_failed"
	// ProxyReaped fires when the patrol reaps a scope's db-proxy child
	// after capturing forensics. Carries the quarantine artifact path so
	// the reap can never be evidence-free.
	ProxyReaped = "proxy.reaped"
	// BreakerStateChanged fires on every resilience breaker state
	// transition (closed/open/half-open), wired from the breaker
	// registry's state-change callback.
	BreakerStateChanged = "breaker.state_changed"
	// ControllerTickCompleted is the controller heartbeat. It is emitted
	// at a patrol multiple or when a tick's duration breaches a threshold
	// — never on every tick — so the supervisor doctor can compute tick
	// age without the event log itself becoming a hot path.
	ControllerTickCompleted = "controller.tick_completed"
	// DoctorAlert fires when the supervisor-cadence doctor evaluates a
	// cheap check to red. It is the detector that closes the
	// detection-at-human-cadence hole (incidents 5 and 11).
	DoctorAlert = "doctor.alert"
)

// StoreDegradedPayload is the typed payload for store.degraded events.
type StoreDegradedPayload struct {
	Scope            string `json:"scope" doc:"Canonical scope root path whose store degraded."`
	Class            string `json:"class" doc:"Degradation class: transport, backend, or write-rejection."`
	Reason           string `json:"reason,omitempty" doc:"Human-readable cause from the failing probe."`
	ConsecutiveFails int    `json:"consecutive_fails,omitempty" doc:"Consecutive failed probe cycles at the trip."`
}

// IsEventPayload marks StoreDegradedPayload as an events.Payload variant.
func (StoreDegradedPayload) IsEventPayload() {}

// StoreRecoveredPayload is the typed payload for store.recovered events.
type StoreRecoveredPayload struct {
	Scope string `json:"scope" doc:"Canonical scope root path whose store recovered."`
	Class string `json:"class,omitempty" doc:"Degradation class that recovered, if known."`
}

// IsEventPayload marks StoreRecoveredPayload as an events.Payload variant.
func (StoreRecoveredPayload) IsEventPayload() {}

// StoreProbeFailedPayload is the typed payload for store.probe_failed events.
type StoreProbeFailedPayload struct {
	Scope  string `json:"scope" doc:"Canonical scope root path of the failing probe."`
	Probe  string `json:"probe" doc:"Which probe failed: routed (probe A) or backend (probe B)."`
	Reason string `json:"reason,omitempty" doc:"Human-readable cause from the failing probe."`
}

// IsEventPayload marks StoreProbeFailedPayload as an events.Payload variant.
func (StoreProbeFailedPayload) IsEventPayload() {}

// ProxyReapedPayload is the typed payload for proxy.reaped events. The
// QuarantineDir path is always populated when a reap fires: the patrol
// captures forensics before signaling, so a reap is never evidence-free.
type ProxyReapedPayload struct {
	Scope         string `json:"scope" doc:"Canonical scope root path whose db-proxy child was reaped."`
	QuarantineDir string `json:"quarantine_dir" doc:"Directory holding the pre-reap forensic artifacts."`
	PIDsSignaled  int    `json:"pids_signaled" doc:"Number of db-proxy-child PIDs signaled."`
	RateLimited   bool   `json:"rate_limited,omitempty" doc:"True when a second poison inside the window suppressed the reap (forensics kept, alert-only)."`
}

// IsEventPayload marks ProxyReapedPayload as an events.Payload variant.
func (ProxyReapedPayload) IsEventPayload() {}

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
// and Phase identify what work the tick did; ThresholdBreach is true when
// the event was emitted because the tick exceeded the duration threshold
// rather than because it landed on the patrol multiple.
type ControllerTickCompletedPayload struct {
	DurationMs      int64  `json:"duration_ms" doc:"Wall-clock duration of the completed tick, in milliseconds."`
	Phase           string `json:"phase" doc:"Tick trigger phase: patrol, poke, control-dispatcher, etc."`
	ThresholdBreach bool   `json:"threshold_breach,omitempty" doc:"True when emitted due to a duration-threshold breach rather than the patrol multiple."`
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
	RegisterPayload(StoreDegraded, StoreDegradedPayload{})
	RegisterPayload(StoreRecovered, StoreRecoveredPayload{})
	RegisterPayload(StoreProbeFailed, StoreProbeFailedPayload{})
	RegisterPayload(ProxyReaped, ProxyReapedPayload{})
	RegisterPayload(BreakerStateChanged, BreakerStateChangedPayload{})
	RegisterPayload(ControllerTickCompleted, ControllerTickCompletedPayload{})
	RegisterPayload(DoctorAlert, DoctorAlertPayload{})
}

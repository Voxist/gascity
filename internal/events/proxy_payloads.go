package events

// Proxy-lifecycle event types and their typed payloads. These belong to the
// proxied-mode store subsystem rather than to the generic store-health
// surface in storehealth_payloads.go, so they are kept in their own file:
// the generic payloads travel with the store-health patrol, this one travels
// with the proxy.
//
// The constant below is in KnownEventTypes (events.go) like every other, so
// TestEveryKnownEventTypeHasRegisteredPayload holds the registration below
// in place.

const (
	// ProxyReaped fires when the patrol reaps a scope's db-proxy child
	// after capturing forensics. Carries the quarantine artifact path so
	// the reap can never be evidence-free.
	ProxyReaped = "proxy.reaped"
)

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

func init() {
	RegisterPayload(ProxyReaped, ProxyReapedPayload{})
}

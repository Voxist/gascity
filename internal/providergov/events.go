package providergov

import "github.com/gastownhall/gascity/internal/events"

// QuotaObservedPayload is the typed payload for provider.quota_observed
// events. One event is emitted per account per successful poll. Reset
// timestamps are RFC 3339 strings, empty when the API reports null.
// OpusUtil/SonnetUtil are nil when the account has no per-model weekly
// bucket (the API reports those windows as null).
type QuotaObservedPayload struct {
	Provider         string   `json:"provider"`
	FiveHourUtil     float64  `json:"five_hour_util"`
	FiveHourResetsAt string   `json:"five_hour_resets_at,omitempty"`
	SevenDayUtil     float64  `json:"seven_day_util"`
	SevenDayResetsAt string   `json:"seven_day_resets_at,omitempty"`
	OpusUtil         *float64 `json:"opus_util,omitempty"`
	SonnetUtil       *float64 `json:"sonnet_util,omitempty"`
}

// IsEventPayload marks QuotaObservedPayload as an events.Payload variant.
func (QuotaObservedPayload) IsEventPayload() {}

// QuotaPollFailedPayload is the typed payload for
// provider.quota_poll_failed events. ReasonClass is one of the
// ReasonClass* constants; the envelope Message carries the cause.
type QuotaPollFailedPayload struct {
	Provider    string `json:"provider"`
	ReasonClass string `json:"reason_class"`
}

// IsEventPayload marks QuotaPollFailedPayload as an events.Payload variant.
func (QuotaPollFailedPayload) IsEventPayload() {}

// Decision reason classes emitted (Phase 2) or noted (Phase 1):
//
//   - "overflow"        — tier overflow-ok routed to the vendor pool head.
//   - "stay"            — active account is under FlipThreshold.
//   - "flip"            — active account crossed FlipThreshold; sibling selected.
//   - "lowest-pressure" — no active account; lowest-pressure account selected.
//   - "cascade"         — all Claude accounts dark; overflow pool serves claude-required.
//   - "reentry"         — cascade→Claude revert: FiveHourUtil < ReentryThreshold AND
//     cooldown elapsed. No event is emitted in Phase 1; Phase 2 will emit it.
//
// No personal data, account credentials, or usage payloads are emitted in any of
// these reason classes. The event carries only the decision label and the provider
// name, which is configuration data, not user data (GDPR-neutral, no consent required).
func init() {
	events.RegisterPayload(events.ProviderQuotaObserved, QuotaObservedPayload{})
	events.RegisterPayload(events.ProviderQuotaPollFailed, QuotaPollFailedPayload{})
}

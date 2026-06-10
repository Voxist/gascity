package providergov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// DefaultPollInterval is the default delay between quota poll cycles.
// Usage polling is metadata-only (no model tokens), so the cost of a
// cycle is one HTTPS request per account.
const DefaultPollInterval = 120 * time.Second

// defaultRequestTimeout bounds a single usage request when the caller's
// context carries no earlier deadline.
const defaultRequestTimeout = 30 * time.Second

// pollerActor is the Actor stamped on events emitted by the poller.
const pollerActor = "provider-governor"

// Account identifies one Claude account the governor monitors: the
// provider name from its [providers.<name>] block and the expanded
// monitor_config_dir holding the account's monitoring credential.
type Account struct {
	Name      string
	ConfigDir string
}

// PollerOptions configures a quota Poller.
type PollerOptions struct {
	// Accounts are the monitor-enabled accounts (see AccountsFromConfig).
	// At least one is required.
	Accounts []Account
	// Recorder receives the emitted provider.* events. Required.
	Recorder events.Recorder
	// Interval between poll cycles; defaults to DefaultPollInterval.
	Interval time.Duration
	// Client is the HTTP client used for usage requests; defaults to
	// http.DefaultClient.
	Client *http.Client
	// BaseURL overrides the API origin; defaults to DefaultBaseURL.
	// Tests point this at an httptest server.
	BaseURL string
}

// Poller polls each configured account's subscription usage and emits
// typed provider.quota_observed / provider.quota_poll_failed events.
// It holds no derived state: every cycle re-reads the credential file
// and re-measures the endpoint (NDI — observers stay idempotent).
type Poller struct {
	accounts []Account
	recorder events.Recorder
	interval time.Duration
	client   *http.Client
	baseURL  string
}

// NewPoller validates opts and builds a Poller.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if len(opts.Accounts) == 0 {
		return nil, fmt.Errorf("providergov: poller requires at least one account")
	}
	for _, acc := range opts.Accounts {
		if acc.Name == "" || acc.ConfigDir == "" {
			return nil, fmt.Errorf("providergov: account %+v missing name or config dir", acc)
		}
	}
	if opts.Recorder == nil {
		return nil, fmt.Errorf("providergov: poller requires an event recorder")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Poller{
		accounts: append([]Account(nil), opts.Accounts...),
		recorder: opts.Recorder,
		interval: interval,
		client:   client,
		baseURL:  opts.BaseURL,
	}, nil
}

// Run polls immediately, then on every interval tick, until ctx is
// canceled. Event recording is best-effort (the events tier never
// returns errors to emitters), so Run has no error to report.
func (p *Poller) Run(ctx context.Context) {
	p.PollOnce(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce polls every account once, emitting one event per account:
// provider.quota_observed on success, provider.quota_poll_failed with a
// mechanical reason class on failure.
func (p *Poller) PollOnce(ctx context.Context) {
	for _, acc := range p.accounts {
		snap, err := p.pollAccount(ctx, acc)
		if err != nil {
			p.recordFailure(acc, err)
			continue
		}
		p.recordObserved(acc, snap)
	}
}

// pollAccount delegates to PollAccount with the poller's client and base URL.
func (p *Poller) pollAccount(ctx context.Context, acc Account) (UsageSnapshot, error) {
	return PollAccount(ctx, p.client, p.baseURL, acc)
}

// PollAccount reads one account's monitoring credential and fetches its
// usage snapshot. Credential failures are wrapped as *PollError with
// ReasonClassCredential so callers classify uniformly. When ctx carries
// no deadline, a default request timeout is applied.
func PollAccount(ctx context.Context, client *http.Client, baseURL string, acc Account) (UsageSnapshot, error) {
	token, err := ReadMonitorToken(acc.ConfigDir)
	if err != nil {
		return UsageSnapshot{}, &PollError{ReasonClass: ReasonClassCredential, Err: err}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}
	return FetchUsage(ctx, client, baseURL, token)
}

// recordObserved emits a provider.quota_observed event for one account.
func (p *Poller) recordObserved(acc Account, snap UsageSnapshot) {
	payload := ObservedPayloadFromSnapshot(acc.Name, snap)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		// Marshal of a flat struct should never fail; without a payload
		// there is nothing useful to record.
		return
	}
	p.recorder.Record(events.Event{
		Type:    events.ProviderQuotaObserved,
		Actor:   pollerActor,
		Subject: acc.Name,
		Message: fmt.Sprintf("5h %.1f%% / 7d %.1f%%", payload.FiveHourUtil, payload.SevenDayUtil),
		Payload: payloadBytes,
	})
}

// recordFailure emits a provider.quota_poll_failed event for one account.
func (p *Poller) recordFailure(acc Account, err error) {
	payloadBytes, merr := json.Marshal(QuotaPollFailedPayload{
		Provider:    acc.Name,
		ReasonClass: ReasonClassOf(err),
	})
	if merr != nil {
		return
	}
	p.recorder.Record(events.Event{
		Type:    events.ProviderQuotaPollFailed,
		Actor:   pollerActor,
		Subject: acc.Name,
		Message: err.Error(),
		Payload: payloadBytes,
	})
}

// ReasonClassOf extracts the mechanical reason class from a poll error,
// defaulting to network for unclassified failures.
func ReasonClassOf(err error) string {
	var pe *PollError
	if errors.As(err, &pe) {
		return pe.ReasonClass
	}
	return ReasonClassNetwork
}

// ObservedPayloadFromSnapshot flattens a usage snapshot into the typed
// quota_observed payload shape for the given provider name.
func ObservedPayloadFromSnapshot(provider string, snap UsageSnapshot) QuotaObservedPayload {
	payload := QuotaObservedPayload{Provider: provider}
	if snap.FiveHour != nil {
		payload.FiveHourUtil = snap.FiveHour.Utilization
		payload.FiveHourResetsAt = formatReset(snap.FiveHour.ResetsAt)
	}
	if snap.SevenDay != nil {
		payload.SevenDayUtil = snap.SevenDay.Utilization
		payload.SevenDayResetsAt = formatReset(snap.SevenDay.ResetsAt)
	}
	if snap.SevenDayOpus != nil {
		util := snap.SevenDayOpus.Utilization
		payload.OpusUtil = &util
	}
	if snap.SevenDaySonnet != nil {
		util := snap.SevenDaySonnet.Utilization
		payload.SonnetUtil = &util
	}
	return payload
}

// formatReset renders a reset time as RFC 3339 UTC, or "" for nil.
func formatReset(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

package providergov

import (
	"fmt"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Tier values mirror the config.Agent tier enum so callers outside the
// config package can use the governor without importing config.
const (
	// TierClaudeRequired routes through the governed Claude account pool.
	TierClaudeRequired = config.AgentTierClaudeRequired
	// TierOverflowOK always routes to the overflow vendor pool.
	TierOverflowOK = config.AgentTierOverflowOK
)

// Decision reasons. Mechanical labels for which rule produced the
// decision; surfaced in CLI output and (Phase 2) lifecycle events.
const (
	// ReasonOverflow: tier overflow-ok routed to the vendor pool head.
	ReasonOverflow = "overflow"
	// ReasonStay: the active account is below the flip threshold.
	ReasonStay = "stay"
	// ReasonFlip: the active account crossed the flip threshold (or went
	// dark) and a sibling account was selected.
	ReasonFlip = "flip"
	// ReasonLowestPressure: no active account; the lowest-pressure
	// account was selected.
	ReasonLowestPressure = "lowest-pressure"
	// ReasonCascade: every Claude account is dark; the overflow pool
	// serves claude-required work (degrade quality rather than stop).
	ReasonCascade = "cascade"
	// ReasonReentry: cascade-to-Claude revert that passed both the headroom
	// gate (FiveHourUtil < ReentryThreshold) and the cooldown gate
	// (now − lastCascadeAt ≥ ReentryCooldown). Phase 2 will emit this as
	// a lifecycle event; Phase 1 surfaces it in CLI/test output only.
	ReasonReentry = "reentry"
)

// AccountState is the measured quota state of one Claude account, as
// observed by the quota poller. Callers must only include accounts with
// a successful, current observation — an unreachable account is unknown,
// not zero, and must be omitted rather than passed with zero utilization.
type AccountState struct {
	// Name is the provider name from [providers.<name>].
	Name string
	// Active marks the account currently serving claude-required work.
	// At most one account should be active (one-active-account
	// alternation); when several are marked, the first by Name order
	// wins the stay check.
	Active bool
	// FiveHourUtil is the 5-hour rolling window utilization percent.
	FiveHourUtil float64
	// SevenDayUtil is the 7-day window utilization percent.
	SevenDayUtil float64
	// OpusUtil is the Opus weekly bucket utilization percent, nil when
	// the account has no separate Opus bucket.
	OpusUtil *float64
	// SonnetUtil is the Sonnet weekly bucket utilization percent, nil
	// when the account has no separate Sonnet bucket.
	SonnetUtil *float64
}

// Policy carries the configured thresholds for SelectProvider. All
// decision inputs live here (ZFC: thresholds are configuration, not
// code); DefaultPolicy documents the defaults.
type Policy struct {
	// FlipThreshold: when the active account's FiveHourUtil exceeds this
	// percent, claude-required work flips to the sibling account.
	FlipThreshold float64
	// DarkThreshold: an account whose five-hour or seven-day utilization
	// is at or above this percent is dark (window exhausted).
	DarkThreshold float64
	// SevenDayWeight scales the seven-day utilization's contribution to
	// the account-pressure score max(five_hour, weight × seven_day).
	SevenDayWeight float64
	// DegradeOpusThreshold: when the selected account's Opus bucket is
	// at or above this percent, the model-degrade signal engages.
	DegradeOpusThreshold float64
	// DegradeSonnetHeadroom: minimum Sonnet headroom percent
	// (100 − sonnet_util) required for the model-degrade signal.
	DegradeSonnetHeadroom float64
	// OverflowProviders is the overflow vendor pool in priority order.
	// Serves overflow-ok work always and claude-required work on cascade.
	OverflowProviders []string
	// ReentryThreshold: when cascading, a Claude account is re-entry eligible
	// only when its FiveHourUtil is strictly below this percent. Distinct from
	// DarkThreshold: accounts in [ReentryThreshold, DarkThreshold) are not dark
	// and can serve existing active work but cannot trigger a cascade revert.
	ReentryThreshold float64
	// ReentryCooldown: minimum time since the last cascade switch before re-entry
	// from cascade is allowed. Prevents oscillation at the reset boundary.
	ReentryCooldown time.Duration
}

// DefaultPolicy returns the documented default thresholds:
// flip at 85% five-hour utilization, dark at 99% on either window,
// unweighted seven-day pressure, Opus degrade at 85% with at least 15%
// Sonnet headroom, re-entry threshold at 65% five-hour utilization,
// re-entry cooldown of 10 minutes, and an empty overflow pool.
func DefaultPolicy() Policy {
	return Policy{
		FlipThreshold:         85,
		DarkThreshold:         99,
		SevenDayWeight:        1.0,
		DegradeOpusThreshold:  85,
		DegradeSonnetHeadroom: 15,
		ReentryThreshold:      65,
		ReentryCooldown:       10 * time.Minute,
	}
}

// Decision is the outcome of SelectProvider.
type Decision struct {
	// Provider is the selected provider name.
	Provider string
	// PreferSonnet signals model-tier degrade: the selected Claude
	// account's Opus bucket is near cap while its Sonnet bucket has
	// headroom, so claude-required work should run on a Sonnet model on
	// this account before any account switch.
	PreferSonnet bool
	// Reason is the Reason* constant naming the rule that fired.
	Reason string
}

// DecisionInput carries the caller-supplied time and cascade context for
// SelectProviderFor. Passing Cascading:false reproduces SelectProvider's
// legacy behavior byte-for-byte (the re-entry path is unreachable).
type DecisionInput struct {
	// Now is the caller's wall-clock time, injected for determinism.
	Now time.Time
	// LastCascadeAt is the time of the most recent cascade switch. Zero
	// when the caller has never cascaded.
	LastCascadeAt time.Time
	// Cascading reports whether the caller is currently serving
	// claude-required work from the overflow pool. When false the
	// re-entry gate is bypassed and SelectProviderFor behaves identically
	// to SelectProvider.
	Cascading bool
}

// SelectProviderFor is SelectProvider with injected time and cascade
// context, enabling the re-entry gate without wall-clock reads inside the
// function. Pure: identical inputs → identical decision.
//
// When input.Cascading is false, behavior is byte-identical to SelectProvider.
//
// Re-entry gate (when input.Cascading is true):
//   - Headroom condition: a Claude account is re-entry eligible only when
//     its FiveHourUtil < policy.ReentryThreshold.
//   - Cooldown condition: input.Now − input.LastCascadeAt ≥ policy.ReentryCooldown.
//   - Both must hold; if either fails, cascade continues.
//   - On re-entry, selects the lowest-pressure eligible account (pressureScore).
func SelectProviderFor(input DecisionInput, tier string, accounts []AccountState, policy Policy) (Decision, error) {
	switch tier {
	case TierOverflowOK:
		if len(policy.OverflowProviders) > 0 {
			return Decision{Provider: policy.OverflowProviders[0], Reason: ReasonOverflow}, nil
		}
	case TierClaudeRequired:
		// Handled below.
	default:
		return Decision{}, fmt.Errorf("providergov: unknown tier %q (want %q or %q)",
			tier, TierClaudeRequired, TierOverflowOK)
	}

	candidates := make([]AccountState, 0, len(accounts))
	hadActive := false
	for _, acc := range accounts {
		if acc.Active {
			hadActive = true
		}
		if !isDark(acc, policy) {
			candidates = append(candidates, acc)
		}
	}
	if len(candidates) == 0 {
		if len(policy.OverflowProviders) > 0 {
			return Decision{Provider: policy.OverflowProviders[0], Reason: ReasonCascade}, nil
		}
		return Decision{}, fmt.Errorf(
			"providergov: no usable Claude account for tier %q (%d measured, all dark) and no overflow providers configured",
			tier, len(accounts),
		)
	}

	// Re-entry gate: when cascading, require both headroom and cooldown
	// before reverting to Claude. Accounts in [ReentryThreshold, DarkThreshold)
	// are not dark but are not re-entry eligible.
	if input.Cascading {
		reentryEligible := make([]AccountState, 0, len(candidates))
		for _, acc := range candidates {
			if acc.FiveHourUtil < policy.ReentryThreshold {
				reentryEligible = append(reentryEligible, acc)
			}
		}
		cooldownElapsed := !input.LastCascadeAt.IsZero() &&
			input.Now.Sub(input.LastCascadeAt) >= policy.ReentryCooldown
		if len(reentryEligible) == 0 || !cooldownElapsed {
			if len(policy.OverflowProviders) > 0 {
				return Decision{Provider: policy.OverflowProviders[0], Reason: ReasonCascade}, nil
			}
			return Decision{}, fmt.Errorf(
				"providergov: cascading with no overflow providers configured",
			)
		}
		sort.Slice(reentryEligible, func(i, j int) bool { return reentryEligible[i].Name < reentryEligible[j].Name })
		best := reentryEligible[0]
		bestScore := pressureScore(best, policy)
		for _, acc := range reentryEligible[1:] {
			if score := pressureScore(acc, policy); score < bestScore {
				best, bestScore = acc, score
			}
		}
		return decisionFor(best, ReasonReentry, policy), nil
	}

	// Deterministic order: by name, so score ties and the multi-active
	// edge case resolve identically across runs.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	// Stay on the active account while it is under the flip threshold.
	for _, acc := range candidates {
		if acc.Active && acc.FiveHourUtil <= policy.FlipThreshold {
			return decisionFor(acc, ReasonStay, policy), nil
		}
	}

	best := candidates[0]
	bestScore := pressureScore(best, policy)
	for _, acc := range candidates[1:] {
		if score := pressureScore(acc, policy); score < bestScore {
			best, bestScore = acc, score
		}
	}
	reason := ReasonLowestPressure
	if hadActive {
		reason = ReasonFlip
	}
	return decisionFor(best, reason, policy), nil
}

// SelectProvider resolves which provider should serve work of the given
// tier from measured account state and configured policy. It is a thin
// shim over SelectProviderFor with Cascading:false, preserving the legacy
// signature and behavior byte-for-byte.
func SelectProvider(tier string, accounts []AccountState, policy Policy) (Decision, error) {
	return SelectProviderFor(DecisionInput{}, tier, accounts, policy)
}

// StateFromPayload builds an AccountState from an observed quota
// payload. Callers mark the currently-active account via active.
func StateFromPayload(payload QuotaObservedPayload, active bool) AccountState {
	return AccountState{
		Name:         payload.Provider,
		Active:       active,
		FiveHourUtil: payload.FiveHourUtil,
		SevenDayUtil: payload.SevenDayUtil,
		OpusUtil:     payload.OpusUtil,
		SonnetUtil:   payload.SonnetUtil,
	}
}

// isDark reports whether either window is at or above the dark threshold.
func isDark(acc AccountState, policy Policy) bool {
	return acc.FiveHourUtil >= policy.DarkThreshold || acc.SevenDayUtil >= policy.DarkThreshold
}

// pressureScore is the account-pressure metric used for selection:
// max(five_hour_util, SevenDayWeight × seven_day_util).
func pressureScore(acc AccountState, policy Policy) float64 {
	weighted := policy.SevenDayWeight * acc.SevenDayUtil
	if acc.FiveHourUtil > weighted {
		return acc.FiveHourUtil
	}
	return weighted
}

// decisionFor builds the decision for a selected account, computing the
// model-degrade signal from its per-model weekly buckets.
func decisionFor(acc AccountState, reason string, policy Policy) Decision {
	d := Decision{Provider: acc.Name, Reason: reason}
	if acc.OpusUtil != nil && acc.SonnetUtil != nil &&
		*acc.OpusUtil >= policy.DegradeOpusThreshold &&
		(100-*acc.SonnetUtil) >= policy.DegradeSonnetHeadroom {
		d.PreferSonnet = true
	}
	return d
}

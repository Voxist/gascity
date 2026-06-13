package providergov

import (
	"testing"
	"time"
)

// T-001: Cooldown blocks re-entry even when a sub-threshold account exists.
func TestReentryCooldownBlocks(t *testing.T) {
	policy := DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}
	policy.ReentryThreshold = 65
	policy.ReentryCooldown = 10 * time.Minute

	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	input := DecisionInput{
		Cascading:     true,
		LastCascadeAt: now.Add(-5 * time.Minute), // within cooldown (5m < 10m)
		Now:           now,
	}

	accounts := []AccountState{
		{Name: "claude", FiveHourUtil: 60, SevenDayUtil: 10}, // 60% < ReentryThreshold=65
	}

	got, err := SelectProviderFor(input, TierClaudeRequired, accounts, policy)
	if err != nil {
		t.Fatalf("SelectProviderFor: %v", err)
	}
	if got.Reason != ReasonCascade {
		t.Errorf("within-cooldown: got reason %q, want %q", got.Reason, ReasonCascade)
	}
}

// T-002: Re-entry allowed after cooldown; lowest-pressure account wins when two qualify.
func TestReentryEligibleAfterCooldown(t *testing.T) {
	policy := DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}
	policy.ReentryThreshold = 65
	policy.ReentryCooldown = 10 * time.Minute

	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	input := DecisionInput{
		Cascading:     true,
		LastCascadeAt: now.Add(-15 * time.Minute), // cooldown elapsed (15m > 10m)
		Now:           now,
	}

	t.Run("single eligible account gets reentry", func(t *testing.T) {
		accounts := []AccountState{
			{Name: "claude", FiveHourUtil: 60, SevenDayUtil: 10}, // 60% < ReentryThreshold=65
		}
		got, err := SelectProviderFor(input, TierClaudeRequired, accounts, policy)
		if err != nil {
			t.Fatalf("SelectProviderFor: %v", err)
		}
		if got.Reason != ReasonReentry {
			t.Errorf("got reason %q, want %q", got.Reason, ReasonReentry)
		}
		if got.Provider != "claude" {
			t.Errorf("got provider %q, want claude", got.Provider)
		}
	})

	t.Run("two eligible accounts: lowest pressure wins", func(t *testing.T) {
		accounts := []AccountState{
			// score = max(5h, 7d): claude = max(60,40)=60; claude2 = max(50,10)=50
			{Name: "claude", FiveHourUtil: 60, SevenDayUtil: 40},
			{Name: "claude2", FiveHourUtil: 50, SevenDayUtil: 10},
		}
		got, err := SelectProviderFor(input, TierClaudeRequired, accounts, policy)
		if err != nil {
			t.Fatalf("SelectProviderFor: %v", err)
		}
		if got.Reason != ReasonReentry {
			t.Errorf("got reason %q, want %q", got.Reason, ReasonReentry)
		}
		if got.Provider != "claude2" {
			t.Errorf("got provider %q, want claude2 (lower pressure)", got.Provider)
		}
	})
}

// T-003: Accounts in [ReentryThreshold, DarkThreshold) band are not re-entry eligible.
func TestReentryBandNotEligible(t *testing.T) {
	policy := DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}
	policy.ReentryThreshold = 65
	policy.ReentryCooldown = 10 * time.Minute

	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	input := DecisionInput{
		Cascading:     true,
		LastCascadeAt: now.Add(-15 * time.Minute), // cooldown elapsed
		Now:           now,
	}

	// 70%: not dark (< 99%) but not re-entry eligible (>= ReentryThreshold=65%)
	accounts := []AccountState{
		{Name: "claude", FiveHourUtil: 70, SevenDayUtil: 10},
	}

	got, err := SelectProviderFor(input, TierClaudeRequired, accounts, policy)
	if err != nil {
		t.Fatalf("SelectProviderFor: %v", err)
	}
	if got.Reason != ReasonCascade {
		t.Errorf("band [65,99%%): got reason %q, want %q", got.Reason, ReasonCascade)
	}
}

// T-005: DefaultPolicy exposes expected ReentryThreshold and ReentryCooldown defaults.
func TestDefaultPolicyReentryFields(t *testing.T) {
	p := DefaultPolicy()
	if p.ReentryThreshold != 65 {
		t.Errorf("ReentryThreshold: got %v, want 65", p.ReentryThreshold)
	}
	if p.ReentryCooldown != 10*time.Minute {
		t.Errorf("ReentryCooldown: got %v, want 10m0s", p.ReentryCooldown)
	}
}

// T-004: Legacy SelectProvider delegates correctly; re-entry path unreachable when Cascading=false.
func TestLegacySelectProviderDelegates(t *testing.T) {
	policy := DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}

	// All-dark accounts must still cascade via the legacy signature.
	accounts := []AccountState{
		{Name: "claude", FiveHourUtil: 100, SevenDayUtil: 30},
	}
	got, err := SelectProvider(TierClaudeRequired, accounts, policy)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	if got.Reason != ReasonCascade {
		t.Errorf("legacy all-dark: got reason %q, want %q", got.Reason, ReasonCascade)
	}
	if got.Provider != "zai" {
		t.Errorf("legacy all-dark: got provider %q, want zai", got.Provider)
	}
}

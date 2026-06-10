package providergov

import (
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestSelectProviderTable(t *testing.T) {
	policy := DefaultPolicy()
	policy.OverflowProviders = []string{"zai", "openrouter"}

	cases := []struct {
		name     string
		tier     string
		accounts []AccountState
		policy   Policy
		want     Decision
		wantErr  string
	}{
		{
			name:   "overflow-ok always routes to the vendor pool head",
			tier:   TierOverflowOK,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", FiveHourUtil: 0, SevenDayUtil: 0},
			},
			want: Decision{Provider: "zai", Reason: ReasonOverflow},
		},
		{
			name:   "overflow-ok with empty vendor pool falls back to claude selection",
			tier:   TierOverflowOK,
			policy: DefaultPolicy(),
			accounts: []AccountState{
				{Name: "claude", FiveHourUtil: 10, SevenDayUtil: 5},
				{Name: "claude2", FiveHourUtil: 50, SevenDayUtil: 5},
			},
			want: Decision{Provider: "claude", Reason: ReasonLowestPressure},
		},
		{
			name:   "alternation stays on the active account below the flip threshold",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 84, SevenDayUtil: 30},
				{Name: "claude2", FiveHourUtil: 5, SevenDayUtil: 5},
			},
			want: Decision{Provider: "claude", Reason: ReasonStay},
		},
		{
			name:   "alternation flips to the sibling above the flip threshold",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 86, SevenDayUtil: 30},
				{Name: "claude2", FiveHourUtil: 5, SevenDayUtil: 5},
			},
			want: Decision{Provider: "claude2", Reason: ReasonFlip},
		},
		{
			name:   "no active account picks the lowest pressure score",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", FiveHourUtil: 10, SevenDayUtil: 70},
				{Name: "claude2", FiveHourUtil: 40, SevenDayUtil: 20},
			},
			// claude score = max(10, 70) = 70; claude2 = max(40, 20) = 40.
			want: Decision{Provider: "claude2", Reason: ReasonLowestPressure},
		},
		{
			name: "seven-day weight scales the weekly contribution",
			tier: TierClaudeRequired,
			policy: func() Policy {
				p := policy
				p.SevenDayWeight = 0.5
				return p
			}(),
			accounts: []AccountState{
				{Name: "claude", FiveHourUtil: 10, SevenDayUtil: 70},
				{Name: "claude2", FiveHourUtil: 40, SevenDayUtil: 20},
			},
			// claude score = max(10, 35) = 35; claude2 = max(40, 10) = 40.
			want: Decision{Provider: "claude", Reason: ReasonLowestPressure},
		},
		{
			name:   "dark active account flips even below the flip threshold",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 50, SevenDayUtil: 99},
				{Name: "claude2", FiveHourUtil: 60, SevenDayUtil: 10},
			},
			want: Decision{Provider: "claude2", Reason: ReasonFlip},
		},
		{
			name:   "model degrade prefers sonnet when opus is near cap with sonnet headroom",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 40, SevenDayUtil: 30, OpusUtil: f64(90), SonnetUtil: f64(10)},
			},
			want: Decision{Provider: "claude", Reason: ReasonStay, PreferSonnet: true},
		},
		{
			name:   "no degrade without sonnet headroom",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 40, SevenDayUtil: 30, OpusUtil: f64(90), SonnetUtil: f64(95)},
			},
			want: Decision{Provider: "claude", Reason: ReasonStay},
		},
		{
			name:   "no degrade when opus window is absent",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 40, SevenDayUtil: 30, SonnetUtil: f64(0)},
			},
			want: Decision{Provider: "claude", Reason: ReasonStay},
		},
		{
			name:   "cascade to the overflow pool when every account is dark",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude", Active: true, FiveHourUtil: 100, SevenDayUtil: 30},
				{Name: "claude2", FiveHourUtil: 99, SevenDayUtil: 10},
			},
			want: Decision{Provider: "zai", Reason: ReasonCascade},
		},
		{
			name:    "all dark with no overflow pool is an error",
			tier:    TierClaudeRequired,
			policy:  DefaultPolicy(),
			wantErr: "no overflow providers",
			accounts: []AccountState{
				{Name: "claude", FiveHourUtil: 100, SevenDayUtil: 30},
			},
		},
		{
			name:   "no accounts cascades to overflow",
			tier:   TierClaudeRequired,
			policy: policy,
			want:   Decision{Provider: "zai", Reason: ReasonCascade},
		},
		{
			name:    "no accounts and no overflow is an error",
			tier:    TierClaudeRequired,
			policy:  DefaultPolicy(),
			wantErr: "no overflow providers",
		},
		{
			name:    "unknown tier is an error",
			tier:    "platinum",
			policy:  policy,
			wantErr: "unknown tier",
		},
		{
			name:   "score ties break deterministically by name",
			tier:   TierClaudeRequired,
			policy: policy,
			accounts: []AccountState{
				{Name: "claude2", FiveHourUtil: 40, SevenDayUtil: 10},
				{Name: "claude", FiveHourUtil: 40, SevenDayUtil: 10},
			},
			want: Decision{Provider: "claude", Reason: ReasonLowestPressure},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectProvider(tc.tier, tc.accounts, tc.policy)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got decision %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectProvider: %v", err)
			}
			if got != tc.want {
				t.Errorf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDefaultPolicyValues(t *testing.T) {
	p := DefaultPolicy()
	if p.FlipThreshold != 85 {
		t.Errorf("FlipThreshold = %v, want 85", p.FlipThreshold)
	}
	if p.DarkThreshold != 99 {
		t.Errorf("DarkThreshold = %v, want 99", p.DarkThreshold)
	}
	if p.SevenDayWeight != 1.0 {
		t.Errorf("SevenDayWeight = %v, want 1.0", p.SevenDayWeight)
	}
	if p.DegradeOpusThreshold != 85 {
		t.Errorf("DegradeOpusThreshold = %v, want 85", p.DegradeOpusThreshold)
	}
	if p.DegradeSonnetHeadroom != 15 {
		t.Errorf("DegradeSonnetHeadroom = %v, want 15", p.DegradeSonnetHeadroom)
	}
	if len(p.OverflowProviders) != 0 {
		t.Errorf("OverflowProviders = %v, want empty", p.OverflowProviders)
	}
}

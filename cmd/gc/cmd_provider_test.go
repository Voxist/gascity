package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/providergov"
)

const providerQuotaSampleJSON = `{
  "five_hour":        { "utilization": 42.0, "resets_at": "2026-06-10T02:40:00Z" },
  "seven_day":        { "utilization":  8.0, "resets_at": "2026-06-15T20:00:00Z" },
  "seven_day_opus":   null,
  "seven_day_sonnet": { "utilization":  0.0, "resets_at": null }
}`

func writeMonitorCredential(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	cred := `{"claudeAiOauth":{"accessToken":"` + token + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeProviderTestCity creates a temporary city directory with the given
// city.toml content and ensures builtin runtime assets are available.
// Returns the city path.
func writeProviderTestCity(t *testing.T, cityToml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	return dir
}

func TestCollectAccountQuotaMixedOutcomes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "good") {
			w.Write([]byte(providerQuotaSampleJSON)) //nolint:errcheck
			return
		}
		http.Error(w, "scope", http.StatusForbidden)
	}))
	defer srv.Close()

	accounts := []providergov.Account{
		{Name: "claude", ConfigDir: writeMonitorCredential(t, "good-token")},
		{Name: "claude2", ConfigDir: writeMonitorCredential(t, "bad-token")},
	}
	rows := collectAccountQuota(context.Background(), srv.Client(), srv.URL, accounts, 5*time.Second)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Err != nil {
		t.Errorf("claude row error: %v", rows[0].Err)
	}
	if rows[0].Payload.FiveHourUtil != 42.0 {
		t.Errorf("claude FiveHourUtil = %v, want 42.0", rows[0].Payload.FiveHourUtil)
	}
	if rows[1].Err == nil {
		t.Error("claude2 row: expected auth error")
	}
}

func TestBuildProviderQuotaReportDecisions(t *testing.T) {
	policy := providergov.DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}
	rows := []accountQuotaRow{
		{
			Account: providergov.Account{Name: "claude"},
			Payload: providergov.QuotaObservedPayload{Provider: "claude", FiveHourUtil: 90, SevenDayUtil: 10},
		},
		{
			Account: providergov.Account{Name: "claude2"},
			Payload: providergov.QuotaObservedPayload{Provider: "claude2", FiveHourUtil: 5, SevenDayUtil: 5},
		},
	}
	report := buildProviderQuotaReport(rows, "claude", policy)

	if len(report.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(report.Accounts))
	}
	if len(report.Decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(report.Decisions))
	}
	byTier := map[string]providerQuotaDecisionReport{}
	for _, d := range report.Decisions {
		byTier[d.Tier] = d
	}
	cr := byTier["claude-required"]
	// Active claude is above the flip threshold (90 > 85) → flip to claude2.
	if cr.Provider != "claude2" || cr.Reason != providergov.ReasonFlip {
		t.Errorf("claude-required = %+v, want flip to claude2", cr)
	}
	of := byTier["overflow-ok"]
	if of.Provider != "zai" || of.Reason != providergov.ReasonOverflow {
		t.Errorf("overflow-ok = %+v, want zai/overflow", of)
	}
}

func TestBuildProviderQuotaReportSkipsFailedAccountsInDecisions(t *testing.T) {
	policy := providergov.DefaultPolicy()
	rows := []accountQuotaRow{
		{
			Account: providergov.Account{Name: "claude"},
			Err:     &providergov.PollError{ReasonClass: providergov.ReasonClassAuth, Err: context.DeadlineExceeded},
		},
	}
	report := buildProviderQuotaReport(rows, "", policy)
	if report.Accounts[0].OK {
		t.Error("failed account reported OK")
	}
	if report.Accounts[0].ReasonClass != providergov.ReasonClassAuth {
		t.Errorf("ReasonClass = %q, want auth", report.Accounts[0].ReasonClass)
	}
	// No measured accounts and no overflow pool → claude-required errors.
	byTier := map[string]providerQuotaDecisionReport{}
	for _, d := range report.Decisions {
		byTier[d.Tier] = d
	}
	if byTier["claude-required"].Error == "" {
		t.Error("claude-required decision: expected error with no measured accounts and no overflow pool")
	}
}

func TestRenderProviderQuotaText(t *testing.T) {
	opus := 90.0
	sonnet := 10.0
	report := providerQuotaReport{
		Accounts: []providerQuotaAccountReport{
			{
				Provider: "claude",
				OK:       true,
				Quota: &providergov.QuotaObservedPayload{
					Provider:         "claude",
					FiveHourUtil:     42.0,
					FiveHourResetsAt: "2026-06-10T02:40:00Z",
					SevenDayUtil:     8.0,
					OpusUtil:         &opus,
					SonnetUtil:       &sonnet,
				},
			},
			{Provider: "claude2", ReasonClass: "auth", Error: "usage endpoint returned 403"},
		},
		Decisions: []providerQuotaDecisionReport{
			{Tier: "claude-required", Provider: "claude", PreferSonnet: true, Reason: "stay"},
			{Tier: "overflow-ok", Error: "no overflow providers"},
		},
	}
	var buf bytes.Buffer
	renderProviderQuotaText(&buf, report)
	out := buf.String()
	for _, want := range []string{
		"claude", "42.0", "2026-06-10T02:40:00Z", "90.0", "10.0",
		"auth: usage endpoint returned 403",
		"claude [prefer sonnet]", "stay",
		"ERROR", "no overflow providers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderProviderQuotaTextNoAccounts(t *testing.T) {
	report := buildProviderQuotaReport(nil, "", providergov.DefaultPolicy())
	var buf bytes.Buffer
	renderProviderQuotaText(&buf, report)
	if !strings.Contains(buf.String(), "quota_monitor") {
		t.Errorf("output should mention the quota_monitor config key:\n%s", buf.String())
	}
}

func TestProviderQuotaReportJSONShape(t *testing.T) {
	policy := providergov.DefaultPolicy()
	policy.OverflowProviders = []string{"zai"}
	rows := []accountQuotaRow{
		{
			Account: providergov.Account{Name: "claude"},
			Payload: providergov.QuotaObservedPayload{Provider: "claude", FiveHourUtil: 10, SevenDayUtil: 5},
		},
	}
	report := buildProviderQuotaReport(rows, "", policy)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["accounts"] == nil || decoded["decisions"] == nil {
		t.Errorf("JSON missing accounts/decisions keys: %s", data)
	}
	if decoded["schema_version"] != "1" {
		t.Errorf("schema_version = %v, want \"1\"", decoded["schema_version"])
	}

	// Empty report still serializes accounts as [] (not null) so the
	// declared result schema's array type holds.
	empty, err := json.Marshal(buildProviderQuotaReport(nil, "", providergov.DefaultPolicy()))
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if !strings.Contains(string(empty), `"accounts": []`) && !strings.Contains(string(empty), `"accounts":[]`) {
		t.Errorf("empty report accounts not []: %s", empty)
	}
}

// TestProviderRotateKeyRefusesAndWritesNothing pins the disable guard on
// `gc provider rotate-key` (bead ga-i86nb). Every invocation shape must exit
// non-zero, name the reason and the bead on stderr, and reach tmux not at all:
// the fake is installed with a live matching session and a provider spec whose
// base URL is a ${VAR} ref — the exact shape the enabled command corrupts — so
// a guard that stopped biting would show up as a non-zero write count here,
// not merely as a changed message.
//
// Delete this test when the redesign re-enables the command.
func TestProviderRotateKeyRefusesAndWritesNothing(t *testing.T) {
	testCity := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
env = {ANTHROPIC_API_KEY = "${ANTHROPIC_AUTH_TOKEN_ZAI}", ANTHROPIC_BASE_URL = "${ZAI_BASE_URL}"}
`)

	cases := []struct {
		name string
		args []string
	}{
		{"plain", []string{"provider", "rotate-key", "--city", testCity, "zai", "sk-ant-new"}},
		{"dry-run", []string{"provider", "rotate-key", "--city", testCity, "--dry-run", "zai", "sk-ant-new"}},
		{"unknown provider", []string{"provider", "rotate-key", "--city", testCity, "no-such-provider", "sk-ant-new"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := newFakeRotateTmux(map[string]map[string]string{
				"session-zai": {"GC_PROVIDER": "zai", "ANTHROPIC_BASE_URL": "https://api.z.ai/api/anthropic"},
			})
			factoryCalls := 0
			orig := rotateTmuxFactory
			rotateTmuxFactory = func() RotateTmux {
				factoryCalls++
				return spy
			}
			t.Cleanup(func() { rotateTmuxFactory = orig })

			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code == 0 {
				t.Fatalf("run(%v) = 0; want non-zero\nstdout: %s\nstderr: %s", tc.args, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "disabled") {
				t.Errorf("stderr = %q; want to name the command as disabled", stderr.String())
			}
			if !strings.Contains(stderr.String(), "ga-i86nb") {
				t.Errorf("stderr = %q; want to name bead ga-i86nb", stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q; want empty — a refusal must not look like a rotation report", stdout.String())
			}
			if factoryCalls != 0 {
				t.Errorf("rotateTmuxFactory called %d times; want 0 — the refusal must precede any tmux client", factoryCalls)
			}
			if spy.writes != 0 {
				t.Errorf("tmux writes = %d; want 0 (callsSet=%v)", spy.writes, spy.callsSet)
			}
			if got := spy.sessions["session-zai"]["ANTHROPIC_BASE_URL"]; got != "https://api.z.ai/api/anthropic" {
				t.Errorf("session ANTHROPIC_BASE_URL = %q; want unchanged", got)
			}
		})
	}

	t.Run("help marks the command disabled", func(t *testing.T) {
		for _, args := range [][]string{
			{"provider", "--help"},
			{"provider", "rotate-key", "--help"},
		} {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%v) = %d; want 0\nstderr: %s", args, code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "DISABLED") {
				t.Errorf("run(%v) stdout = %q; want to advertise the command as DISABLED", args, stdout.String())
			}
		}
	})
}

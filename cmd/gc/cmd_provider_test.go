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

func TestProviderRotateKeyCLI(t *testing.T) {
	testCity := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
env = {ANTHROPIC_API_KEY = "${ANTHROPIC_AUTH_TOKEN_ZAI}", ANTHROPIC_AUTH_TOKEN = "${ANTHROPIC_AUTH_TOKEN_ZAI}"}
`)

	// Inject a fake so no real tmux is required.
	fake := newFakeRotateTmux(map[string]map[string]string{})
	orig := rotateTmuxFactory
	rotateTmuxFactory = func() RotateTmux { return fake }
	t.Cleanup(func() { rotateTmuxFactory = orig })

	t.Run("rotate zai exits 0 and names source var", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"provider", "rotate-key", "--city", testCity, "zai", "sk-ant-new"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d; want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "ANTHROPIC_AUTH_TOKEN_ZAI") {
			t.Errorf("stdout = %q; want to contain ANTHROPIC_AUTH_TOKEN_ZAI", stdout.String())
		}
	})

	t.Run("unknown provider exits non-zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"provider", "rotate-key", "--city", testCity, "unknown-provider", "sk-ant-new"}, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("run() = 0; want non-zero for unknown provider")
		}
		if !strings.Contains(stderr.String(), "provider not found") {
			t.Errorf("stderr = %q; want to contain 'provider not found'", stderr.String())
		}
	})

	t.Run("provider --help exits 0", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"provider", "--help"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(provider --help) = %d; want 0\nstderr: %s", code, stderr.String())
		}
	})
}

func TestProviderRotateKeyDryRunFlag(t *testing.T) {
	testCity := writeProviderTestCity(t, `
[workspace]
name = "test"

[beads]
provider = "file"

[providers.zai]
env = {ANTHROPIC_API_KEY = "${ANTHROPIC_AUTH_TOKEN_ZAI}", ANTHROPIC_AUTH_TOKEN = "${ANTHROPIC_AUTH_TOKEN_ZAI}"}
`)

	spy := newFakeRotateTmux(map[string]map[string]string{
		"session-zai": {"GC_PROVIDER": "zai"},
	})
	orig := rotateTmuxFactory
	rotateTmuxFactory = func() RotateTmux { return spy }
	t.Cleanup(func() { rotateTmuxFactory = orig })

	var stdout, stderr bytes.Buffer
	code := run([]string{"provider", "rotate-key", "--city", testCity, "--dry-run", "zai", "sk-ant-new"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run) = %d; want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "would update") {
		t.Errorf("stdout = %q; want to contain 'would update'", stdout.String())
	}
	if len(spy.callsSet) != 0 {
		t.Errorf("dry-run: SetGlobalEnvironment called %d times; want 0 (spy.callsSet=%v)", len(spy.callsSet), spy.callsSet)
	}
	if spy.sessions["session-zai"]["ANTHROPIC_API_KEY"] != "" {
		t.Errorf("dry-run: session env modified; want untouched")
	}
}

package providergov

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleUsageJSON = `{
  "five_hour":        { "utilization": 42.0, "resets_at": "2026-06-10T02:40:00Z" },
  "seven_day":        { "utilization":  8.0, "resets_at": "2026-06-15T20:00:00Z" },
  "seven_day_opus":   null,
  "seven_day_sonnet": { "utilization":  0.0, "resets_at": null },
  "extra_usage":      { "is_enabled": false, "used_credits": null }
}`

func TestFetchUsageDecodesLiveShape(t *testing.T) {
	var gotAuth, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q, want /api/oauth/usage", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleUsageJSON)) //nolint:errcheck
	}))
	defer srv.Close()

	snap, err := FetchUsage(context.Background(), srv.Client(), srv.URL, "tok-123")
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
	if gotBeta != oauthBetaValue {
		t.Errorf("anthropic-beta = %q, want %q", gotBeta, oauthBetaValue)
	}
	if snap.FiveHour == nil || snap.FiveHour.Utilization != 42.0 {
		t.Errorf("FiveHour = %+v, want utilization 42.0", snap.FiveHour)
	}
	wantReset := time.Date(2026, 6, 10, 2, 40, 0, 0, time.UTC)
	if snap.FiveHour.ResetsAt == nil || !snap.FiveHour.ResetsAt.Equal(wantReset) {
		t.Errorf("FiveHour.ResetsAt = %v, want %v", snap.FiveHour.ResetsAt, wantReset)
	}
	if snap.SevenDay == nil || snap.SevenDay.Utilization != 8.0 {
		t.Errorf("SevenDay = %+v, want utilization 8.0", snap.SevenDay)
	}
	if snap.SevenDayOpus != nil {
		t.Errorf("SevenDayOpus = %+v, want nil (JSON null)", snap.SevenDayOpus)
	}
	if snap.SevenDaySonnet == nil || snap.SevenDaySonnet.Utilization != 0.0 {
		t.Errorf("SevenDaySonnet = %+v, want utilization 0.0", snap.SevenDaySonnet)
	}
	if snap.SevenDaySonnet.ResetsAt != nil {
		t.Errorf("SevenDaySonnet.ResetsAt = %v, want nil (JSON null)", snap.SevenDaySonnet.ResetsAt)
	}
}

func TestFetchUsageClassifies403AsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `OAuth token does not meet scope requirement user:profile`, http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := FetchUsage(context.Background(), srv.Client(), srv.URL, "setup-token")
	assertReasonClass(t, err, ReasonClassAuth)
}

func TestFetchUsageClassifies401AsAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := FetchUsage(context.Background(), srv.Client(), srv.URL, "expired")
	assertReasonClass(t, err, ReasonClassAuth)
}

func TestFetchUsageClassifies500AsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := FetchUsage(context.Background(), srv.Client(), srv.URL, "tok")
	assertReasonClass(t, err, ReasonClassHTTPError)
}

func TestFetchUsageClassifiesTimeout(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := FetchUsage(ctx, srv.Client(), srv.URL, "tok")
	assertReasonClass(t, err, ReasonClassTimeout)
}

func TestFetchUsageClassifiesConnectionRefusedAsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening anymore

	_, err := FetchUsage(context.Background(), http.DefaultClient, url, "tok")
	assertReasonClass(t, err, ReasonClassNetwork)
}

func TestFetchUsageClassifiesMalformedBodyAsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	_, err := FetchUsage(context.Background(), srv.Client(), srv.URL, "tok")
	assertReasonClass(t, err, ReasonClassDecode)
}

func assertReasonClass(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pe *PollError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *PollError", err, err)
	}
	if pe.ReasonClass != want {
		t.Errorf("ReasonClass = %q, want %q (err: %v)", pe.ReasonClass, want, err)
	}
}

func TestReadMonitorTokenReadsCredentialFile(t *testing.T) {
	dir := t.TempDir()
	cred := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc","refreshToken":"r","scopes":["user:profile"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := ReadMonitorToken(dir)
	if err != nil {
		t.Fatalf("ReadMonitorToken: %v", err)
	}
	if tok != "sk-ant-oat01-abc" {
		t.Errorf("token = %q, want sk-ant-oat01-abc", tok)
	}
}

func TestReadMonitorTokenMissingFile(t *testing.T) {
	_, err := ReadMonitorToken(t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing .credentials.json")
	}
}

func TestReadMonitorTokenMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadMonitorToken(dir)
	if err == nil {
		t.Fatal("expected error for malformed credential JSON")
	}
}

func TestReadMonitorTokenEmptyToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadMonitorToken(dir)
	if err == nil {
		t.Fatal("expected error for empty access token")
	}
}

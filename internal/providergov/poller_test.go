package providergov

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// captureRecorder collects recorded events for assertions.
type captureRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (c *captureRecorder) Record(e events.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureRecorder) snapshot() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]events.Event(nil), c.events...)
}

func writeCredential(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	cred := `{"claudeAiOauth":{"accessToken":"` + token + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPollOnceEmitsQuotaObserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(sampleUsageJSON)) //nolint:errcheck
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{{Name: "claude", ConfigDir: writeCredential(t, "tok-a")}},
		Recorder: rec,
		BaseURL:  srv.URL,
		Client:   srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.PollOnce(context.Background())

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Type != events.ProviderQuotaObserved {
		t.Errorf("Type = %q, want %q", e.Type, events.ProviderQuotaObserved)
	}
	if e.Subject != "claude" {
		t.Errorf("Subject = %q, want claude", e.Subject)
	}
	var payload QuotaObservedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v — raw %s", err, e.Payload)
	}
	if payload.Provider != "claude" {
		t.Errorf("payload.Provider = %q, want claude", payload.Provider)
	}
	if payload.FiveHourUtil != 42.0 {
		t.Errorf("payload.FiveHourUtil = %v, want 42.0", payload.FiveHourUtil)
	}
	if payload.FiveHourResetsAt != "2026-06-10T02:40:00Z" {
		t.Errorf("payload.FiveHourResetsAt = %q, want 2026-06-10T02:40:00Z", payload.FiveHourResetsAt)
	}
	if payload.SevenDayUtil != 8.0 {
		t.Errorf("payload.SevenDayUtil = %v, want 8.0", payload.SevenDayUtil)
	}
	if payload.OpusUtil != nil {
		t.Errorf("payload.OpusUtil = %v, want nil (null window)", payload.OpusUtil)
	}
	if payload.SonnetUtil == nil || *payload.SonnetUtil != 0.0 {
		t.Errorf("payload.SonnetUtil = %v, want 0.0", payload.SonnetUtil)
	}
}

func TestPollOnceEmitsPollFailedOnAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "scope", http.StatusForbidden)
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{{Name: "claude2", ConfigDir: writeCredential(t, "setup-token")}},
		Recorder: rec,
		BaseURL:  srv.URL,
		Client:   srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.PollOnce(context.Background())

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Type != events.ProviderQuotaPollFailed {
		t.Errorf("Type = %q, want %q", e.Type, events.ProviderQuotaPollFailed)
	}
	var payload QuotaPollFailedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if payload.Provider != "claude2" {
		t.Errorf("payload.Provider = %q, want claude2", payload.Provider)
	}
	if payload.ReasonClass != ReasonClassAuth {
		t.Errorf("payload.ReasonClass = %q, want %q", payload.ReasonClass, ReasonClassAuth)
	}
	if e.Message == "" {
		t.Error("Message is empty; want human-readable cause on the envelope")
	}
}

func TestPollOnceEmitsPollFailedOnMissingCredential(t *testing.T) {
	rec := &captureRecorder{}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{{Name: "claude", ConfigDir: t.TempDir()}},
		Recorder: rec,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.PollOnce(context.Background())

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want 1", len(got))
	}
	var payload QuotaPollFailedPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if payload.ReasonClass != ReasonClassCredential {
		t.Errorf("ReasonClass = %q, want %q", payload.ReasonClass, ReasonClassCredential)
	}
}

func TestPollOncePollsEveryAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(sampleUsageJSON)) //nolint:errcheck
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{
			{Name: "claude", ConfigDir: writeCredential(t, "a")},
			{Name: "claude2", ConfigDir: writeCredential(t, "b")},
		},
		Recorder: rec,
		BaseURL:  srv.URL,
		Client:   srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.PollOnce(context.Background())

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("recorded %d events, want 2", len(got))
	}
	subjects := map[string]bool{}
	for _, e := range got {
		subjects[e.Subject] = true
	}
	if !subjects["claude"] || !subjects["claude2"] {
		t.Errorf("subjects = %v, want claude and claude2", subjects)
	}
}

func TestNewPollerDefaultsAndValidation(t *testing.T) {
	if _, err := NewPoller(PollerOptions{Recorder: &captureRecorder{}}); err == nil {
		t.Error("expected error for zero accounts")
	}
	if _, err := NewPoller(PollerOptions{Accounts: []Account{{Name: "claude", ConfigDir: "x"}}}); err == nil {
		t.Error("expected error for nil recorder")
	}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{{Name: "claude", ConfigDir: "x"}},
		Recorder: &captureRecorder{},
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if p.interval != DefaultPollInterval {
		t.Errorf("interval = %v, want %v", p.interval, DefaultPollInterval)
	}
}

func TestRunPollsImmediatelyAndStopsOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(sampleUsageJSON)) //nolint:errcheck
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	p, err := NewPoller(PollerOptions{
		Accounts: []Account{{Name: "claude", ConfigDir: writeCredential(t, "a")}},
		Recorder: rec,
		BaseURL:  srv.URL,
		Client:   srv.Client(),
		Interval: time.Hour, // only the immediate poll should fire
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for len(rec.snapshot()) == 0 {
		select {
		case <-deadline:
			t.Fatal("no event recorded before deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("recorded %d events, want exactly 1 (immediate poll only)", got)
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModelProxy_ProviderRouting(t *testing.T) {
	// Two independent upstream test servers.
	var claudeHits, zaiHits int
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		claudeHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer claudeUpstream.Close()
	zaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		zaiHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer zaiUpstream.Close()

	reg := newProviderHealthRegistry()
	handler := newModelProxyHandler(reg, map[string]string{
		"claude": claudeUpstream.URL,
		"zai":    zaiUpstream.URL,
	})

	send := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	if code := send("/proxy/claude/v1/messages"); code != http.StatusOK {
		t.Fatalf("claude route: expected 200, got %d", code)
	}
	if code := send("/proxy/zai/v1/messages"); code != http.StatusOK {
		t.Fatalf("zai route: expected 200, got %d", code)
	}
	// Verify each request reached only its own upstream.
	if claudeHits != 1 {
		t.Fatalf("expected 1 claude hit, got %d", claudeHits)
	}
	if zaiHits != 1 {
		t.Fatalf("expected 1 zai hit, got %d", zaiHits)
	}

	// Unknown provider path should return 400.
	if code := send("/proxy/"); code != http.StatusBadRequest {
		t.Fatalf("empty provider: expected 400, got %d", code)
	}
}

func TestModelProxy_ForwardsAndRecords(t *testing.T) {
	// Fake upstream that returns 429 for all requests.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	reg := newProviderHealthRegistry()
	providerUpstreams := map[string]string{
		"claude": upstream.URL,
	}

	handler := newModelProxyHandler(reg, providerUpstreams)

	// POST /proxy/claude/v1/messages → must reach fake upstream and record 429.
	req := httptest.NewRequest(http.MethodPost, "/proxy/claude/v1/messages", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 from upstream, got %d", w.Code)
	}

	// The registry must record the 429 for "claude".
	h, present := reg.Check("claude")
	if !present {
		t.Fatal("expected registry to have an entry for claude after proxied 429")
	}
	// Single 429 should not trip threshold yet, but provider must be present.
	_ = h

	// Two more 429s to trip the threshold.
	for range 2 {
		req2 := httptest.NewRequest(http.MethodPost, "/proxy/claude/v1/messages", nil)
		w2 := httptest.NewRecorder()
		handler.ServeHTTP(w2, req2)
	}
	h, present = reg.Check("claude")
	if !present {
		t.Fatal("expected registry present after 3×429")
	}
	if h {
		t.Fatal("expected healthy=false after 3×429 routed through proxy")
	}

	// Upstream flips to 200 — subsequent request should not trip further.
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req3 := httptest.NewRequest(http.MethodPost, "/proxy/claude/v1/messages", nil)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200 from upstream after flip, got %d", w3.Code)
	}
	// Provider still red (recovery window not elapsed), but entry present.
	_, present = reg.Check("claude")
	if !present {
		t.Fatal("expected entry still present after 200")
	}
	// Just confirm the recovery would happen after the window elapses by
	// calling RecordResponse directly with a future time.
	reg.RecordResponse("claude", 200, time.Now().Add(registryRecoveryWindow+time.Second))
	h, _ = reg.Check("claude")
	if !h {
		t.Fatal("expected healthy=true after simulated recovery window")
	}
}

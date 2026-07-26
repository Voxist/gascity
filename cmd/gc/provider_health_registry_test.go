package main

import (
	"testing"
	"time"
)

func TestProviderHealthRegistry_BasicRecord(t *testing.T) {
	reg := newProviderHealthRegistry()

	// No record yet → fail-open (healthy=true, present=false).
	h, present := reg.Check("claude")
	if present {
		t.Fatal("expected present=false before any record")
	}
	if !h {
		t.Fatal("expected healthy=true (fail-open) before any record")
	}

	// Record a 200 — should be healthy.
	reg.RecordResponse("claude", 200, time.Now())
	h, present = reg.Check("claude")
	if !present {
		t.Fatal("expected present=true after RecordResponse")
	}
	if !h {
		t.Fatal("expected healthy=true after a 200 response")
	}

	// Record a 429 — still healthy until threshold.
	reg.RecordResponse("claude", 429, time.Now())
	h, present = reg.Check("claude")
	if !present {
		t.Fatal("expected present=true after 429")
	}
	// Single 429 does not trip threshold (3 required), so should still be healthy.
	if !h {
		t.Fatal("expected healthy=true after single 429 (threshold not met)")
	}
}

func TestProviderHealthRegistry_Cooldown(t *testing.T) {
	reg := newProviderHealthRegistry()
	base := time.Now()

	// Three consecutive 429s within the red window → provider goes red.
	// All recorded at base so lastError == base (simplifies recovery arithmetic).
	for range 3 {
		reg.RecordResponse("claude", 429, base)
	}
	h, present := reg.Check("claude")
	if !present {
		t.Fatal("expected present=true after 3×429")
	}
	if h {
		t.Fatal("expected healthy=false after 3 consecutive 429s")
	}

	// A 200 response before the recovery window elapses does not flip back to green.
	reg.RecordResponse("claude", 200, base.Add(10*time.Second))
	h, _ = reg.Check("claude")
	if h {
		t.Fatal("expected still red: recovery window (120s) not yet elapsed")
	}

	// A 200 response after the recovery window (120s quiet) → back to green.
	reg.RecordResponse("claude", 200, base.Add(registryRecoveryWindow+time.Second))
	h, _ = reg.Check("claude")
	if !h {
		t.Fatal("expected healthy=true after recovery window with no 429s")
	}
}

func TestProviderHealthRegistry_SelectHealthy(t *testing.T) {
	reg := newProviderHealthRegistry()
	base := time.Now()

	// Mark "claude" red.
	for i := range 3 {
		reg.RecordResponse("claude", 429, base.Add(time.Duration(i)*time.Second))
	}

	// SelectHealthy should skip red "claude" and return "zai" (present=false → fail-open).
	got := reg.SelectHealthy([]string{"claude", "zai"})
	if got != "zai" {
		t.Fatalf("expected SelectHealthy to return %q, got %q", "zai", got)
	}

	// All-red chain returns "".
	for i := range 3 {
		reg.RecordResponse("zai", 429, base.Add(time.Duration(i)*time.Second))
	}
	got = reg.SelectHealthy([]string{"claude", "zai"})
	if got != "" {
		t.Fatalf("expected empty string when all providers red, got %q", got)
	}

	// Empty chain returns "".
	got = reg.SelectHealthy([]string{})
	if got != "" {
		t.Fatal("expected empty string for empty chain")
	}
}

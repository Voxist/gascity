package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// loadBeadsResilienceConfig writes content to a temp city.toml and loads it.
func loadBeadsResilienceConfig(t *testing.T, content string) *City {
	t.Helper()
	path := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestBeadsResilienceDefaults(t *testing.T) {
	var r BeadsResilienceConfig
	if !r.EnabledOrDefault() {
		t.Error("EnabledOrDefault() = false for zero config, want true")
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 3 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 3", got)
	}
	if got := r.OpenBaseOrDefault(); got != time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 1s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 60*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 60s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 15*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 15s", got)
	}
}

func TestBeadsResilienceParsesFromTOML(t *testing.T) {
	cfg := loadBeadsResilienceConfig(t, `
[workspace]
name = "test"

[beads.resilience]
enabled = false
consecutive_failures = 5
open_base = "2s"
open_max = "30s"
half_open_interval = "10s"
`)
	r := cfg.Beads.Resilience
	if r.EnabledOrDefault() {
		t.Error("EnabledOrDefault() = true, want false (explicit enabled = false)")
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 5 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 5", got)
	}
	if got := r.OpenBaseOrDefault(); got != 2*time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 2s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 30*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 30s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 10*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 10s", got)
	}
}

func TestBeadsResilienceInvalidValuesFallBackToDefaults(t *testing.T) {
	r := BeadsResilienceConfig{
		ConsecutiveFailures: -2,
		OpenBase:            "not-a-duration",
		OpenMax:             "-5s",
		HalfOpenInterval:    "0s",
	}
	if got := r.ConsecutiveFailuresOrDefault(); got != 3 {
		t.Errorf("ConsecutiveFailuresOrDefault() = %d, want 3", got)
	}
	if got := r.OpenBaseOrDefault(); got != time.Second {
		t.Errorf("OpenBaseOrDefault() = %v, want 1s", got)
	}
	if got := r.OpenMaxOrDefault(); got != 60*time.Second {
		t.Errorf("OpenMaxOrDefault() = %v, want 60s", got)
	}
	if got := r.HalfOpenIntervalOrDefault(); got != 15*time.Second {
		t.Errorf("HalfOpenIntervalOrDefault() = %v, want 15s", got)
	}
}

func TestBeadsResilienceDurationsValidated(t *testing.T) {
	cfg := &City{}
	cfg.Beads.Resilience.OpenBase = "5mins"
	warnings := ValidateDurations(cfg, "city.toml")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "[beads.resilience]") && strings.Contains(w, "open_base") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ValidateDurations warnings = %v, want a [beads.resilience] open_base warning", warnings)
	}
}

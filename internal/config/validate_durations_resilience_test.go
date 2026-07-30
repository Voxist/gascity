package config

import (
	"strings"
	"testing"
)

// TestValidateDurationsWarnsOnResilienceDurations pins that [beads.resilience]
// durations are actually validated. The accessors' doc comments promise a
// load-time warning for invalid values; without these checks a unit-less
// "60" was silently replaced by the default and the operator believed their
// setting had taken effect.
func TestValidateDurationsWarnsOnResilienceDurations(t *testing.T) {
	cfg := &City{}
	cfg.Beads.Resilience.OpenMax = "60" // missing unit
	w := ValidateDurations(cfg, "city.toml")
	found := false
	for _, s := range w {
		if strings.Contains(s, "open_max") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning for open_max=%q; got %v", cfg.Beads.Resilience.OpenMax, w)
	}
}

package config

import (
	"strings"
	"testing"
)

// TestRoutedPoolRankProbeCommand pins the ADR-0076 D1 rank probe's shape: the
// SAME routed predicate as the work tier's routed leg (routing key, unassigned,
// epic/hold exclusions) but asking for the single best-by-priority row, so a
// store's advertised rank is a property of its best available work rather than
// of its oldest-first 20-row window.
func TestRoutedPoolRankProbeCommand(t *testing.T) {
	got := routedPoolRankProbeCommand(QueryTopology{}, "rig/worker")
	for _, want := range []string{
		"--sort priority --limit=1",
		`--metadata-field "gc.routed_to=$target"`,
		" --unassigned",
		` --exclude-label "hold:mayor"`,
		"bd ready",
		`probe_pool_rank "$1"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rank probe = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "migration") {
		t.Fatalf("rank probe must not carry the work tier's migration fallbacks: %q", got)
	}
	// The work tier's window must be UNCHANGED — the probe is additive, so
	// ADR-0035's anti-starvation ordering within a store survives (C2).
	work := routedPoolWorkQueryCommand(QueryTopology{}, "rig/worker")
	if !strings.Contains(work, "--sort oldest --limit=20") {
		t.Fatalf("work tier window changed: %q", work)
	}
}

// TestEffectiveRoutedRankProbeQueryForLegacyTarget mirrors
// buildRoutedPoolQuery's legacy workflow-control handling: a control dispatcher
// probes BOTH route spellings, in the same order the work tier would.
func TestEffectiveRoutedRankProbeQueryForLegacyTarget(t *testing.T) {
	a := &Agent{Name: "worker", PoolName: "rig/control-dispatcher"}
	got := a.EffectiveRoutedRankProbeQueryFor(QueryTopology{})
	if !strings.Contains(got, `probe_pool_rank "$1"`) || !strings.Contains(got, `probe_pool_rank "$2"`) {
		t.Fatalf("legacy-target probe must ask both route spellings: %q", got)
	}
}

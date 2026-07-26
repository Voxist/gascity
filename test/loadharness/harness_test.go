//go:build loadharness

package loadharness

import (
	"testing"
	"time"
)

// TestCityLoad is the single entry point that runs every deterministic
// scenario and prints a metrics table. It is infrastructure to PROVE Phase 2
// changes: it MEASURES the amplifier metrics and asserts only structural
// invariants (no silent-empty under injected fault; fan-out is exactly
// scopes×sessions; faults are observed as degraded). It does NOT assert the
// plan's target thresholds — those are Phase 2/3 cutover gates measured
// against this baseline.
//
// Run it with:
//
//	go test -tags loadharness -run TestCityLoad ./test/loadharness/ -v
func TestCityLoad(t *testing.T) {
	results := make([]ScenarioResult, 0, len(scenarioTable))
	for _, sc := range scenarioTable {
		results = append(results, runScenario(sc))
	}

	// Print the metrics table.
	t.Log("\n" + metricsTableHeader())
	for _, r := range results {
		t.Log(r.row())
	}
	t.Log("")
	t.Log("scenario notes:")
	for _, r := range results {
		t.Logf("  %-26s degraded_ticks=%-3d store_ops=%-7d %s",
			r.Name, r.DegradedTicks, r.StoreOps, r.Notes)
	}

	// Structural invariants — these are the harness's real guarantees, not
	// throughput thresholds.
	byName := make(map[string]ScenarioResult, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	// 1) Fan-out is exactly scopes × open sessions (the scaling-bomb metric).
	for _, r := range results {
		if want := r.Scopes * r.OpenSessions; r.ReadyFanoutPerTick != want {
			t.Errorf("%s: ReadyFanoutPerTick = %d, want scopes*sessions = %d",
				r.Name, r.ReadyFanoutPerTick, want)
		}
	}

	// 2) The amplifier-100-sessions scenario must demonstrate the 800-fan-out
	//    reference point the plan names (8 stores × 100 sessions).
	if amp := byName["amplifier-100-sessions"]; amp.ReadyFanoutPerTick != 800 {
		t.Errorf("amplifier-100-sessions: fan-out = %d, want 800 (8 scopes x 100 sessions)",
			amp.ReadyFanoutPerTick)
	}

	// 3) Every fault-injecting scenario must observe degradation and must NOT
	//    render silent-empty. This is the typed-unavailable contract the
	//    harness exists to protect across cutover.
	for _, name := range []string{"poison-repro", "server-resolution-failed", "type-rejected-poison"} {
		r := byName[name]
		if r.SilentEmpty {
			t.Errorf("%s: rendered silent-empty; a fault must surface as typed-degraded", name)
		}
		if r.DegradedTicks == 0 {
			t.Errorf("%s: no degraded ticks observed; fault was not detected", name)
		}
	}

	// 4) The no_history-fallback scope must keep its rows readable (non-empty),
	//    never silent-empty.
	if r := byName["no_history-fallback"]; r.SilentEmpty {
		t.Errorf("no_history-fallback: rendered silent-empty; no-history rows must stay readable")
	}

	// 5) Idle baseline still spawns (the controller-floor amplifier): the plan
	//    measures ≥108 spawns/min idle. We assert only that the rate is
	//    measurable and positive — the number itself is the baseline, not a
	//    pass/fail gate.
	if r := byName["idle-baseline"]; r.SpawnsPerMinute() <= 0 {
		t.Errorf("idle-baseline: spawns/min = %.1f, want measurable positive rate", r.SpawnsPerMinute())
	}
}

// TestCityLoadDeterministic asserts the harness is reproducible: two runs of
// the same scenario produce identical measured metrics (no wall-clock
// randomness leaks into the numbers). Latency is modeled from the fixed cost
// table, so it must match exactly.
func TestCityLoadDeterministic(t *testing.T) {
	sc := scenarioTable[1] // amplifier-100-sessions
	a := runScenario(sc)
	b := runScenario(sc)

	if a.Spawns != b.Spawns {
		t.Errorf("non-deterministic spawns: %d != %d", a.Spawns, b.Spawns)
	}
	if a.StoreOps != b.StoreOps {
		t.Errorf("non-deterministic store ops: %d != %d", a.StoreOps, b.StoreOps)
	}
	if a.TickP95 != b.TickP95 {
		t.Errorf("non-deterministic tick p95: %v != %v", a.TickP95, b.TickP95)
	}
	if a.ReadyFanoutPerTick != b.ReadyFanoutPerTick {
		t.Errorf("non-deterministic fan-out: %d != %d", a.ReadyFanoutPerTick, b.ReadyFanoutPerTick)
	}
}

// TestSpawnsScaleWithSessions verifies the amplifier model: spawn count grows
// with (scopes × open sessions). It is the measured proof that the fan-out is
// the scaling bomb the plan names, and the lever a native-store cutover must
// flatten.
func TestSpawnsScaleWithSessions(t *testing.T) {
	base := runScenario(scenario{name: "x", scopes: 4, openSessions: 0, ticks: 1, seedReady: 1})
	more := runScenario(scenario{name: "x", scopes: 4, openSessions: 25, ticks: 1, seedReady: 1})
	if more.Spawns <= base.Spawns {
		t.Errorf("spawns did not grow with sessions: base=%d more=%d", base.Spawns, more.Spawns)
	}
	// With 4 scopes × 25 sessions, the Ready fan-out alone adds 100 spawns.
	if delta := more.Spawns - base.Spawns; delta < 100 {
		t.Errorf("ready fan-out under-counted: delta=%d, want >=100 (4x25)", delta)
	}
}

// TestLatencyPercentiles exercises the pure percentile math deterministically.
func TestLatencyPercentiles(t *testing.T) {
	var d LatencyDistribution
	for i := 1; i <= 100; i++ {
		d.Record(time.Duration(i) * time.Millisecond)
	}
	if got := d.Percentile(50); got != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", got)
	}
	if got := d.Percentile(95); got != 95*time.Millisecond {
		t.Errorf("p95 = %v, want 95ms", got)
	}
	if got := d.Max(); got != 100*time.Millisecond {
		t.Errorf("max = %v, want 100ms", got)
	}
	var empty LatencyDistribution
	if got := empty.Percentile(95); got != 0 {
		t.Errorf("empty p95 = %v, want 0", got)
	}
}

// TestSpawnCounterPerMinute exercises the rate normalization deterministically.
func TestSpawnCounterPerMinute(t *testing.T) {
	var c SpawnCounter
	c.Add(120)
	if got := c.PerMinute(2 * time.Minute); got != 60 {
		t.Errorf("per-minute = %.1f, want 60", got)
	}
	if got := c.PerMinute(0); got != 0 {
		t.Errorf("zero-elapsed per-minute = %.1f, want 0 (no division trap)", got)
	}
}

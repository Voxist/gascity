//go:build loadharness

// Package loadharness is a build-tagged, deterministic load and scenario
// harness that MEASURES the city-scale amplifier metrics named in
// engdocs/contributors/city-scale-architecture-plan.md Phase 2. It exists to
// give later cutover steps a re-runnable baseline; it deliberately does not
// assert pass/fail against the plan's target thresholds (those are Phase 2/3
// gates). It measures and prints, and it is deterministic so successive runs
// are comparable.
//
// The harness models the controller's store access shape — CachingStore over a
// per-scope subprocess runner (the "subprocess amplifier") — and the
// per-assignee live Ready fan-out, both described in the plan's Thesis. It
// replays the named incident classes against in-memory fakes (no real Dolt,
// no real proxy, no wall-clock randomness) and records:
//
//   - spawns/min of the bd subprocess runner at idle and under a synthetic
//     open-session count,
//   - controller tick latency distribution (p50/p95/max),
//   - Ready fan-out spawn count per tick as a function of
//     (scopes × open sessions).
//
// See README.md in this directory for how to run it.
package loadharness

import (
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// SpawnCounter counts simulated bd-subprocess spawns. In production the
// controller's store access is CachingStore(BdStore): one bd subprocess per
// store operation, per scope, per tick (cmd/gc/bd_env.go
// bdCommandRunnerWithManagedRetryErr). This counter stands in for that process
// table so the harness can MEASURE the amplifier without spawning real
// processes. Safe for concurrent use.
type SpawnCounter struct {
	total atomic.Int64
}

// Inc records one simulated subprocess spawn.
func (c *SpawnCounter) Inc() {
	c.total.Add(1)
}

// Add records n simulated subprocess spawns.
func (c *SpawnCounter) Add(n int64) {
	c.total.Add(n)
}

// Total returns the cumulative spawn count.
func (c *SpawnCounter) Total() int64 {
	return c.total.Load()
}

// PerMinute returns the spawn rate normalized to one minute given the simulated
// elapsed duration. A zero or negative duration returns 0 to avoid a division
// trap; callers should pass the scenario's simulated wall time.
func (c *SpawnCounter) PerMinute(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(c.total.Load()) / elapsed.Minutes()
}

// LatencyDistribution accumulates simulated per-tick latencies and reports
// percentiles. It is not safe for concurrent use; record ticks from a single
// goroutine (the harness drives ticks serially by design — the metric of
// interest is per-tick cost, not parallelism).
type LatencyDistribution struct {
	samples []time.Duration
}

// Record appends one latency sample.
func (d *LatencyDistribution) Record(sample time.Duration) {
	d.samples = append(d.samples, sample)
}

// Count returns the number of recorded samples.
func (d *LatencyDistribution) Count() int {
	return len(d.samples)
}

// Percentile returns the latency at the given percentile in [0,100] using
// nearest-rank on a sorted copy. An empty distribution returns 0.
func (d *LatencyDistribution) Percentile(p float64) time.Duration {
	if len(d.samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(d.samples))
	copy(sorted, d.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// Max returns the largest recorded sample, or 0 if empty.
func (d *LatencyDistribution) Max() time.Duration {
	var max time.Duration
	for _, s := range d.samples {
		if s > max {
			max = s
		}
	}
	return max
}

// ScenarioResult is the measured outcome of one harness scenario. It is a pure
// data record: the harness prints it and asserts only structural invariants
// (e.g. "no silent-empty"), never the plan's target thresholds.
type ScenarioResult struct {
	// Name is the scenario identifier, e.g. "poison-repro".
	Name string
	// Scopes is the number of simulated stores (rigs + hq) exercised.
	Scopes int
	// OpenSessions is the synthetic open-session count driving fan-out.
	OpenSessions int
	// Ticks is the number of controller ticks simulated.
	Ticks int
	// Spawns is the cumulative simulated subprocess spawn count.
	Spawns int64
	// SimElapsed is the simulated wall time the scenario represents.
	SimElapsed time.Duration
	// TickP50, TickP95, TickMax summarize the per-tick latency distribution.
	TickP50 time.Duration
	TickP95 time.Duration
	TickMax time.Duration
	// ReadyFanoutPerTick is the measured Ready spawn count for a single tick
	// (scopes × open sessions in the BdStore fan-out model). It is the
	// scaling-bomb metric the plan names.
	ReadyFanoutPerTick int
	// StoreOps is the cumulative simulated store operation count.
	StoreOps int64
	// DegradedTicks counts ticks where a scope was observed degraded
	// (breaker-open / write-rejected) rather than silently empty.
	DegradedTicks int
	// SilentEmpty is true if the scenario produced a "no work" rendering that
	// was indistinguishable from store-unreachable — the anti-pattern the
	// plan's Phase 1 typed-unavailable work eliminates. The harness asserts
	// this stays false where a fault was injected.
	SilentEmpty bool
	// Notes carries scenario-specific observations for the printed table.
	Notes string
}

// SpawnsPerMinute returns the scenario's spawn rate normalized to a minute.
func (r ScenarioResult) SpawnsPerMinute() float64 {
	if r.SimElapsed <= 0 {
		return 0
	}
	return float64(r.Spawns) / r.SimElapsed.Minutes()
}

// header returns the fixed-width metrics-table header row.
func metricsTableHeader() string {
	return fmt.Sprintf("%-26s %6s %8s %6s %9s %10s %9s %9s %9s %9s",
		"SCENARIO", "SCOPES", "SESSIONS", "TICKS",
		"SPAWNS", "SPAWNS/MIN", "FANOUT", "TICK_P50", "TICK_P95", "TICK_MAX")
}

// row renders one ScenarioResult as a fixed-width metrics-table row.
func (r ScenarioResult) row() string {
	return fmt.Sprintf("%-26s %6d %8d %6d %9d %10.1f %9d %9s %9s %9s",
		r.Name, r.Scopes, r.OpenSessions, r.Ticks,
		r.Spawns, r.SpawnsPerMinute(), r.ReadyFanoutPerTick,
		shortDur(r.TickP50), shortDur(r.TickP95), shortDur(r.TickMax))
}

// shortDur renders a duration compactly for the metrics table.
func shortDur(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fus", float64(d.Microseconds()))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Milliseconds()))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

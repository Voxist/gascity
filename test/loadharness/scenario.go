//go:build loadharness

package loadharness

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// scenario is a fixed, deterministic load case. Every field is seeded from the
// scenarioTable below — there is no wall-clock randomness — so the harness is
// re-runnable and successive runs are comparable.
type scenario struct {
	// name identifies the scenario in the metrics table.
	name string
	// scopes is the number of simulated stores (rigs + hq).
	scopes int
	// openSessions is the synthetic open-session count per scope driving the
	// per-assignee Ready fan-out.
	openSessions int
	// ticks is the number of controller ticks to simulate.
	ticks int
	// scopeFaults assigns a scripted fault per scope index. A scope index not
	// present is healthy (faultNone).
	scopeFaults map[int]fault
	// mailRecipients, when > 0, drives the mail fan-out store-op count
	// (one create per recipient per tick).
	mailRecipients int
	// sseSubscribers, when > 0, drives the SSE subscriber-storm event pressure
	// (one event delivered per subscriber per tick).
	sseSubscribers int
	// seedReady is the number of ready work beads to seed into every healthy
	// scope before ticking, so Ready returns non-empty and the empty-vs-
	// unavailable distinction is observable.
	seedReady int
	// noHistory, when true, seeds the ready beads with NoHistory set — the
	// fallback-with-no_history case (plan 2.1(c)/2.8 ordering).
	noHistory bool
	// notes carries a human-readable observation for the table.
	notes string
}

// scenarioTable is the canonical, deterministic set of incident-class load
// cases the harness replays. It mirrors the scenarios the plan names in
// Phase 2 (2.1) and the harness brief: poison-repro, server-resolution-failed,
// no_history-fallback, mail-fanout, SSE subscriber storm — plus baseline idle
// and a synthetic open-session amplifier case so the fan-out metric is
// measured as a function of (scopes × open sessions).
//
// The fleet uses 8 scopes (7 rigs + hq) and 100 open sessions as the plan's
// "all rigs active" reference point.
var scenarioTable = []scenario{
	{
		name:      "idle-baseline",
		scopes:    8,
		ticks:     60,
		seedReady: 0,
		notes:     "no open sessions; measures controller-floor spawns/min",
	},
	{
		name:         "amplifier-100-sessions",
		scopes:       8,
		openSessions: 100,
		ticks:        10,
		seedReady:    5,
		notes:        "scaling bomb: 8 stores x 100 sessions Ready fan-out/tick",
	},
	{
		name:         "poison-repro",
		scopes:       8,
		openSessions: 20,
		ticks:        10,
		seedReady:    5,
		// One scope poisoned with a transport fault (the incident-9 shape):
		// a connection breaker + storehealth probe SHOULD observe it, and the
		// scope must render DEGRADED, never silent-empty.
		scopeFaults: map[int]fault{0: faultTransport},
		notes:       "scope 0 transport-poisoned; breaker/probe must observe, not silent-empty",
	},
	{
		name:         "server-resolution-failed",
		scopes:       4,
		openSessions: 10,
		ticks:        10,
		seedReady:    5,
		// Endpoint resolution returns nothing on one scope: the factory must
		// fall back, never silently open an empty store and report zero work.
		scopeFaults: map[int]fault{0: faultResolutionFailed},
		notes:       "scope 0 endpoint unresolvable; must fall back, not silent-empty",
	},
	{
		name:         "type-rejected-poison",
		scopes:       4,
		openSessions: 10,
		ticks:        10,
		seedReady:    5,
		// Application-class write rejection (a74fefde8): reads pass, writes of
		// a required custom type are rejected. A transport-only breaker would
		// MISS it; the write-path conformance probe observes it as degraded.
		scopeFaults: map[int]fault{0: faultTypeRejected},
		notes:       "scope 0 rejects custom-type writes; write-probe must observe (transport breaker misses)",
	},
	{
		name:         "no_history-fallback",
		scopes:       4,
		openSessions: 10,
		ticks:        10,
		seedReady:    5,
		noHistory:    true,
		notes:        "scope opened with no-history beads; reads must stay non-empty",
	},
	{
		name:           "mail-fanout",
		scopes:         4,
		openSessions:   10,
		ticks:          10,
		seedReady:      2,
		mailRecipients: 50,
		notes:          "50 recipients/tick; measures store ops under mail fan-out",
	},
	{
		name:           "sse-subscriber-storm",
		scopes:         4,
		openSessions:   10,
		ticks:          10,
		seedReady:      2,
		sseSubscribers: 200,
		notes:          "200 SSE subscribers/tick; measures event-bus delivery pressure",
	},
}

// tickInterval is the simulated controller-tick spacing (the plan's 10s
// patrol). Used only to convert tick counts into the scenario's simulated wall
// time for spawns/min — it is a model constant, not a sleep.
const tickInterval = 10 * time.Second

// runScenario executes one scenario deterministically and returns its measured
// result. It builds one amplifying store per scope, seeds ready work, and
// drives the configured number of controller ticks, charging the per-assignee
// Ready fan-out and any mail/SSE load. It records each tick's latency and
// classifies faulted scopes as degraded (typed) rather than silently empty.
func runScenario(sc scenario) ScenarioResult {
	spawns := &SpawnCounter{}
	var storeOps int64
	bus := events.NewFake()

	stores := make([]*ampStore, sc.scopes)
	for i := range stores {
		f := sc.scopeFaults[i]
		stores[i] = newAmpStore(fmt.Sprintf("scope-%d", i), f, sc.openSessions, spawns, &storeOps)
		seedScope(stores[i], sc)
	}

	var latency LatencyDistribution
	var degradedTicks int
	silentEmpty := false
	hadFault := len(sc.scopeFaults) > 0

	for t := 0; t < sc.ticks; t++ {
		tickNanos, tickDegraded, tickSilent := runTick(stores, sc, bus)
		latency.Record(tickNanos)
		if tickDegraded {
			degradedTicks++
		}
		if tickSilent {
			silentEmpty = true
		}
	}

	// Ready fan-out per tick in the BdStore model: one Ready subprocess per
	// open session per scope (the per-assignee fan-out). Faulted scopes still
	// attempt their fan-out before tripping, so the structural count is
	// scopes × open sessions.
	fanout := sc.scopes * sc.openSessions

	res := ScenarioResult{
		Name:               sc.name,
		Scopes:             sc.scopes,
		OpenSessions:       sc.openSessions,
		Ticks:              sc.ticks,
		Spawns:             spawns.Total(),
		SimElapsed:         time.Duration(sc.ticks) * tickInterval,
		TickP50:            latency.Percentile(50),
		TickP95:            latency.Percentile(95),
		TickMax:            latency.Max(),
		ReadyFanoutPerTick: fanout,
		StoreOps:           storeOps,
		DegradedTicks:      degradedTicks,
		SilentEmpty:        silentEmpty,
		Notes:              sc.notes,
	}
	// A scenario that injected a fault but never observed degradation rendered
	// silent-empty by construction — the anti-pattern the harness exists to
	// catch. Surface it so the assertion in TestCityLoad fails loudly.
	if hadFault && degradedTicks == 0 {
		res.SilentEmpty = true
	}
	return res
}

// seedScope seeds a healthy scope with ready work so Ready returns non-empty
// and the empty-vs-unavailable distinction is observable. Faulted scopes are
// seeded too (the fault is applied at read/write time, not at seed time) except
// transport/resolution scopes, whose seed writes would be rejected — those are
// seeded directly through the inner store to model pre-existing rows.
func seedScope(s *ampStore, sc scenario) {
	for i := 0; i < sc.seedReady; i++ {
		b := beads.Bead{Title: fmt.Sprintf("work-%d", i), Type: "task"}
		if sc.noHistory {
			b.NoHistory = true
		}
		// Seed via the inner store so a faulted scope still has rows to read;
		// this models work that existed before the fault began.
		if _, err := s.inner.Create(b); err != nil {
			panic(fmt.Sprintf("loadharness: seeding scope %s: %v", s.scope, err))
		}
	}
}

// runTick simulates one controller tick across all scopes. It returns the
// tick's total simulated latency, whether any scope was observed degraded, and
// whether any faulted scope rendered as silent-empty (a fault that produced
// "no work" indistinguishable from store-unreachable). The classification is
// the typed-unavailable contract the plan's Phase 1 establishes, modeled here
// so the harness can prove later code preserves it.
func runTick(stores []*ampStore, sc scenario, bus *events.Fake) (time.Duration, bool, bool) {
	degraded := false
	silent := false

	for _, s := range stores {
		// 1) Endpoint resolution probe (factory open path). A resolution
		//    failure must surface as unavailable, never an empty store.
		if err := s.resolveEndpoint(); err != nil {
			degraded = true
			bus.Record(events.Event{Type: "store.degraded", Subject: s.scope, Message: "endpoint unresolvable"})
			// Factory falls back rather than rendering work; not silent-empty
			// because we emitted a typed degraded signal.
			continue
		}

		// 2) Per-assignee live Ready fan-out: one Ready per open session.
		//    This is the amplifier the plan's 2.6 collapses to one Ready/store.
		var readyErr error
		for _, assignee := range s.fleet.assignees() {
			if _, err := s.Ready(beads.ReadyQuery{Assignee: assignee}); err != nil {
				readyErr = err
				break
			}
		}
		if readyErr != nil {
			degraded = true
			bus.Record(events.Event{Type: "store.degraded", Subject: s.scope, Message: readyErr.Error()})
			continue
		}

		// 3) Write-path conformance probe: create+close one ephemeral bead of
		//    a required custom type. A type-rejection scope is observed here —
		//    a transport-only breaker would have passed steps 1–2.
		probe := beads.Bead{Title: "conformance-probe", Type: probeCustomType}
		created, err := s.create(probe)
		if err != nil {
			degraded = true
			bus.Record(events.Event{Type: "store.degraded", Subject: s.scope, Message: err.Error()})
			continue
		}
		if err := s.closeBead(created.ID); err != nil {
			degraded = true
			bus.Record(events.Event{Type: "store.degraded", Subject: s.scope, Message: err.Error()})
			continue
		}

		// 4) Mail fan-out: one create per recipient (mail is a bead).
		for r := 0; r < sc.mailRecipients; r++ {
			if _, err := s.create(beads.Bead{Title: fmt.Sprintf("mail-%d", r), Type: "message"}); err != nil {
				degraded = true
				break
			}
		}

		// 5) A healthy scope that produced no ready rows AND injected no fault
		//    is genuinely empty — not silent-empty. Silent-empty only applies
		//    when a fault was present but produced an empty/no-work rendering
		//    without a degraded signal; that path is unreachable here because
		//    every fault sets degraded above. The check below is defensive.
		rows, err := s.list(beads.ListQuery{Status: "open", AllowScan: true})
		if err == nil && len(rows) == 0 && s.fault != faultNone {
			silent = true
		}
	}

	// SSE subscriber storm: deliver one event per subscriber. Models event-bus
	// delivery pressure independent of the store amplifier.
	for i := 0; i < sc.sseSubscribers; i++ {
		bus.Record(events.Event{Type: "controller.tick_completed", Subject: fmt.Sprintf("sub-%d", i)})
	}

	// Sum each scope's accumulated simulated latency for this tick.
	var total time.Duration
	for _, s := range stores {
		total += s.drainSimNanos()
	}
	return total, degraded, silent
}

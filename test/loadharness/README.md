# City-Scale Load / Scenario Harness (Phase 2.1)

A reproducible, build-tagged harness that **measures** the city-scale amplifier
metrics named in
[`engdocs/contributors/city-scale-architecture-plan.md`](../../engdocs/contributors/city-scale-architecture-plan.md)
Phase 2. It is the "harness FIRST" gate that gives later cutover steps a
re-runnable baseline.

It does **not** assert the plan's target thresholds (those are the Phase 2/3
cutover gates). It MEASURES the shape of the load — and asserts only the
structural invariants a cutover must preserve (no silent-empty under fault;
fan-out is exactly `scopes × sessions`; injected faults surface as typed
degraded signals).

## What it measures

- **spawns/min of the bd subprocess runner** at idle and under a synthetic
  open-session count — the "subprocess amplifier" (`CachingStore(BdStore)`,
  one bd subprocess per store op per scope per tick).
- **controller tick latency distribution** (p50 / p95 / max), modeled
  deterministically from a fixed per-op cost table.
- **Ready fan-out spawn count per tick** as a function of
  `(scopes × open sessions)` — the per-assignee live `Ready` fan-out that is
  the scaling bomb for "all rigs active" (8 stores × 100 sessions ≈ 800
  Ready spawns/tick).

## How to run

```bash
# The single entry point — runs every scenario and prints a metrics table:
go test -tags loadharness -run TestCityLoad ./test/loadharness/ -v

# The full harness suite (entry + determinism + scaling + metric-math tests):
go test -tags loadharness ./test/loadharness/ -v
```

The `loadharness` build tag keeps this code out of the default build and the
normal `go test ./...` sweep; it is opt-in infrastructure.

## Determinism

The harness is seeded entirely from the fixed `scenarioTable` in `scenario.go`.
There is **no wall-clock randomness**: latency is charged from the fixed
`opCost` table in `store.go`, and `TestCityLoadDeterministic` asserts two runs
of the same scenario produce identical metrics. Successive runs are therefore
directly comparable across code changes.

## Scenarios

All scenarios run against in-memory fakes — `beads.MemStore` for bead
bookkeeping, `events.Fake` for the event bus, and the `runtime.Fake` provider
shape — so no real Dolt, proxy, or process is required. The incident-class
conditions (proxy poison, endpoint-unresolvable, custom-type write rejection)
are simulated at the harness layer (`store.go` `fault`), so the harness does
**not** depend on the sibling resilience / storehealth branches compiling here.

| Scenario | Models |
|---|---|
| `idle-baseline` | controller-floor spawns/min with zero open sessions |
| `amplifier-100-sessions` | the 8×100 = 800 Ready fan-out scaling bomb |
| `poison-repro` | a scope whose proxy is transport-poisoned (incident 9); a breaker / store-health probe must observe it — never silent-empty |
| `server-resolution-failed` | endpoint resolution returns nothing; the factory must fall back, never silently open an empty store |
| `type-rejected-poison` | a scope that REJECTS a required custom bead type (the a74fefde8 write rejection); a transport-only breaker MISSES it, the write-path conformance probe observes it |
| `no_history-fallback` | a scope opened with `no_history` beads; reads must stay non-empty |
| `mail-fanout` | N recipients/tick; measures store ops under mail fan-out (mail is a bead) |
| `sse-subscriber-storm` | M subscribers/tick; measures event-bus delivery pressure |

## Relationship to the full `test/loadtest/` (plan item 2.1)

Plan item 2.1 also describes a heavier `//go:build loadtest` integration harness
that flood-tests **real** ephemeral Dolt + per-scope proxies on CI. That is a
separate, integration-tagged artifact. This `loadharness` package is the pure,
deterministic, in-memory measurement layer: it runs anywhere `go test` runs (no
Dolt, no ICU, no network) and produces the baseline numbers the Dolt-backed
flood gates are compared against. Anything that needs real Dolt behavior is
out of scope here and belongs behind the separate `loadtest` integration tag.

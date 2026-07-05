# vp-cixi.6 — Drop the open-work gate for gate-less cooldown probes

Bead: vp-cixi.6 (child of EPIC vp-cixi). Root cause (GAP D from vp-cixi.5):
`provider-health-probe` is a pure cooldown probe that tracks NO beads, yet the
dispatcher runs two open-work gates for it per tick (`hasOpenTracking` then
`hasOpenWork`), each issuing `bd list` / `bd query` reads against Dolt and
bounded by `orderGateTimeout` (8s). On store slowness the gate times out and
`gateFailClosed` SKIPS the order every cycle → the provider-health cache goes
stale → fail-closed provider health → failover can't pick claude2/anything.
Confirmed live: 60–90+min gaps between probe runs despite a 10m interval.

A/B/C (PR #357) shipped a mitigation (hysteresis + wider TTL) that absorbs the
skips. This plan is the deeper fix in gc-core (gascity): let an order OPT OUT
of the open-work gates entirely, since they are meaningless for orders that
consume no bead work. Same class as the decision-sweep rig-enum starvation.

## Approach

Add an order-level opt-out flag `no_work_gate` (Go `Order.NoWorkGate`). When
`true`, the dispatcher skips BOTH open-work gates (`hasOpenTracking` and
`hasOpenWork`) for that order — no `gateOpenWorkBounded` call, no Dolt reads,
no fail-closed skip, no gate-timeout backoff. The order still respects its own
trigger (cooldown interval) and per-order exec timeout; single-flight for these
orders is naturally bounded by the cooldown interval + the synchronous
tracking-bead the dispatcher creates before launch (which still happens).

Why a new flag, not reusing `Idempotent`:
- `Idempotent` changes semantics to fail-OPEN on timeout (may double-dispatch).
  A gate-less probe should not even *enter* the gate — it must not depend on a
  Dolt read completing inside 8s, and it should never emit
  `order.gate_timeout_fail_open`. The two properties ("safe to re-run" vs
  "consumes no bead work") are distinct; conflating them would make a probe's
  dispatch contingent on store health, which is exactly the bug.

## Micro-tasks (TDD, red/green/refactor/commit-on-green)

### T-001 — Order model: add `NoWorkGate` field + TOML decode + validation guard
- **acceptance**: `TestOrderNoWorkGateParsed` — an order TOML with
  `no_work_gate = true` decodes to `Order.NoWorkGate == true`; default is
  `false`. Also assert `Validate` accepts it (no extra constraint).
- **files**: `internal/orders/order.go` (struct field + `orderDecode` +
  `normalized()`), test in `internal/orders/order_test.go`.
- commit: `feat(orders): T-001 add Order.NoWorkGate opt-out flag — green at TestOrderNoWorkGateParsed`

### T-002 — Dispatcher: skip both gates when `NoWorkGate`
- **acceptance**: `TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay` —
  with `orderGateTimeout` shortened and a store whose gate queries sleep past
  it, an order with `NoWorkGate: true` STILL dispatches (creates a tracking
  bead) and records ZERO gate query calls; a plain order is skipped as before.
- **files**: `cmd/gc/order_dispatch.go` (guard the two `gateOpenWorkBounded`
  call sites + the `gateBackoffActive` short-circuit so backoff is irrelevant
  when the gate never runs).
- commit: `fix(dispatch): T-002 skip open-work gates for NoWorkGate orders — green at TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay`

### T-003 — Order TOML: set `no_work_gate = true` on provider-health-probe
- **acceptance**: `TestProviderHealthProbeOrderOptsOutOfWorkGate` — the
  shipped `packs/.../provider-health-probe.toml` parses with
  `NoWorkGate == true`. (Source-of-truth TOML lives in voxist-platform pack;
  this task adds the flag there + a parsing test in gascity fixtures if a copy
  is vendored, otherwise the test loads the pack file directly.)
- **files**: `packs/voxist-city/orders/provider-health-probe.toml`
  (voxist-platform rig), test in gascity.
- commit: `feat(orders): T-003 provider-health-probe opts out of work gate — green at TestProviderHealthProbeOrderOptsOutOfWorkGate`

### T-004 — Docs: order-author guide note for `no_work_gate`
- **acceptance**: a docs note exists describing when to set `no_work_gate`
  (pure probes/sweeps that track no beads) and the warning that it disables
  single-flight protection, so the order must be self-idempotent or
  interval-bounded.
- **files**: `docs/guides/orders.md` (or nearest order-author reference).
- commit: `docs(orders): T-004 document no_work_gate opt-out`

## Status

- [ ] T-001
- [ ] T-002
- [ ] T-003
- [ ] T-004

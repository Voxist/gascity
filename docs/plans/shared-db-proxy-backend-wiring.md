# Wire gascity to select the shared db-proxy backend (N+1 → 1) — Plan v0.1

**Status:** Draft · **HELD** (blocked on a beads release including `be-pen9` being
pinned in gascity) · **Date:** 2026-06-10 · **Author:** voxist.planner ·
**Rig:** gascity · **Bead:** ga-mozik · **Base:** `feat/beads-proxied-pooling`

## Precondition (gate — DO NOT START code until green)

`be-pen9` (the beads-side `local-shared-server` backend + `SharedProxyRootDir()`
+ `BEADS_SHARED_PROXY` opt-in) is **merged** (beads PR #4, T-001..T-008 green)
but **not yet pinned** in gascity's `bd`. Verified 2026-06-10 11:07 CEST against
`/opt/homebrew/bin/bd` (1.0.5, `121a2c2de`):

```
strings $(which bd) | grep -c BEADS_SHARED_PROXY     # => 0   (need > 0)
go tool nm $(which bd) | grep -c SharedProxyRootDir  # => 0   (need > 0)
strings $(which bd) | grep -c BEADS_PROXY_POOL_SIZE  # => 1   (control: grep sound)
```

The pinned binary predates `be-pen9`. The **code** micro-tasks (T-001..T-004,
T-006) are pure gascity env-projection wiring + unit/testscript assertions and do
**not** need a be-pen9 `bd` to be written or to pass (they assert what env gascity
*projects*, not what `bd` *does* with it). Only **T-005** (the runtime
`pgrep` collapse proof) requires the pin. The executor runs the whole table in
one session **once the pin lands**; until then this bead stays held by the
planner. Re-gate: `strings $(which bd) | grep -c BEADS_SHARED_PROXY` must be > 0.

## Context

Connection-pooling Phase 2 wiring already lives on `feat/beads-proxied-pooling`:
every proxied scope (HQ + N rigs) runs `bd` in proxied-server mode through the
pooling db-proxy, but each scope spawns its **own** `db-proxy-child` (HQ + N rigs
⇒ N+1 children, ~1 GB RAM measured on portharbour). `be-pen9` landed the beads
mechanism to collapse them onto **one** shared child: the parent's spawn-or-reuse
is keyed by **proxy rootDir**, and the pool is keyed by `(capabilities, database)`,
so a single child multiplexes every scope's database. Pointing every proxied
scope at one shared rootDir is the whole collapse.

`be-pen9` exposes that as an **opt-in** (OFF by default at the bd level):

- `BEADS_SHARED_PROXY` truthy ⇒ each proxied scope resolves its proxy rootDir to
  `doltserver.SharedProxyRootDir()` (`~/.beads/shared-server/proxy/`, override
  `BEADS_SHARED_PROXY_ROOT_PATH`) instead of its per-scope `.beads/proxieddb`.
- The `local-shared-server` backend fronts the managed dolt via the same
  external-server path, carries `--external-*` across the fork, and participates
  in the upstream-ID guard (a shared proxy pointed at the wrong managed dolt is
  rejected, never silently reused). Same managed-dolt host/port ⇒ same upstream
  ID ⇒ scopes share one child.

This bead is the **gascity production wiring** that turns the opt-in on for
proxied scopes: project `BEADS_SHARED_PROXY=1` into every proxied `bd`
invocation (agent **and** controller), so all scopes resolve to one rootDir and
one child.

### The real anchor (doc/impl drift — read this before touching `bd_env.go`)

`docs/CONNECTION_POOLING_DEPLOYMENT.md` §3/§4 (beads repo) says the pool-size
injection lives in `cmd/gc/bd_env.go` (`applyCanonicalDoltTargetEnv`). **It does
not** — that function only projects `GC_DOLT_HOST/PORT`. The pool-size injection
was implemented in a dedicated, proxied-gated overlay:

- `cmd/gc/beads_proxied_overlay.go:90` — `applyProxiedPoolEnv(env, cityPath)` is
  the single chokepoint. When the city opts into proxied mode and `bd` supports
  it, it sets (lines 98–103): `GC_BEADS_PROXIED=1`, `GC_BEADS_PROXY_POOL_SIZE=n`,
  `BEADS_PROXY_POOL_SIZE=n`, and the idle-timeout pair. `GC_BEADS_*` are read by
  `gc-beads-bd.sh` (agent path); `BEADS_PROXY_POOL_SIZE` is read by Go-launched
  `bd` directly (controller path). **`BEADS_SHARED_PROXY` belongs here**,
  alongside the pool-size keys — one edit covers both paths.
- `cmd/gc/beads_proxied_overlay.go:124` — `resolveProxiedGate(cityPath)` returns
  `(active, poolSize, idle)` from a 5s `city.toml`-mtime cache (`entry` struct
  ~line 113). The new shared-proxy gate fields ride this same cache.

## Constraints

- **HARD — default OFF; never auto-enable on live portharbour.** The collapse
  must be a **separate, default-false** gate (not implied by `proxied`). Once a
  be-pen9 `bd` is pinned, an always-on shared flag would collapse portharbour's
  18 live rigs onto one child with **zero** throwaway validation — exactly the
  failure `be-pen9` forbids ("validate on a THROWAWAY city, never live
  portharbour"). The gate defaults off; the throwaway validation (T-005) is the
  only place it is turned on until a deliberate, separately-tracked ops rollout.
- **HARD — validate on a THROWAWAY proxied city, never live portharbour** (same
  discipline as `be-pen9` T-007 and ga-bgub9 T-004).
- **Opt-in / no regression.** `server`, `doltlite`, `postgres`, and
  non-shared proxied scopes are byte-for-byte unchanged. `applyProxiedPoolEnv`
  already no-ops when the gate is off; the shared key is added inside that same
  `active` branch.
- Dolt, its versioning, and `dolt remote` sync are untouched — this is env
  projection in front of the managed dolt.
- Raw `go test`/`go vet` need the icu4c CGO flags
  (`-I/-L $(brew --prefix icu4c)`); use `rtk proxy` for clean output; `cmd/gc`
  is too big for one `go test` — scope to the touched packages. See the
  [gascity dev loop] memory.

## Proposed approach

Two surgical edits behind one new default-OFF gate, plus a runtime proof:

1. **Gate** — add `shared_proxy` to `[beads]` in `config` (mirror the existing
   `Proxied`/`ProxiedEnabled()` pair) and thread a `shared bool` through
   `resolveProxiedGate` / its cache `entry`.
2. **Project** — in `applyProxiedPoolEnv`, when the gate is on, set
   `BEADS_SHARED_PROXY=1` (Go-launched/controller `bd`) and `GC_BEADS_SHARED_PROXY=1`
   (for the script to forward). Single edit, both paths.
3. **Forward** — in `gc-beads-bd.sh` `run_bd_pinned` proxied branch (≈ line 2230,
   beside `export BEADS_PROXY_POOL_SIZE="${GC_BEADS_PROXY_POOL_SIZE:-4}"`), add
   `export BEADS_SHARED_PROXY="${GC_BEADS_SHARED_PROXY:-}"` (empty default ⇒ the
   Go overlay is the single source of truth for the gate).
4. **Prove** — throwaway proxied two-scope city with the gate on ⇒ exactly one
   `db-proxy-child` serving both stores (gated on the be-pen9 pin).

## Micro-tasks

> One bead, one PR. The executor consumes the whole table in a single session
> **once the precondition gate is green**. First task is the failing test (TDD,
> architecture §10). `est` is minutes.

| id | description | acceptance | est | slings |
|---|---|---|---|---|
| T-001 | Write the failing test: extend `TestApplyProxiedPoolEnvInjectsOnlyWhenGateOnAndBDCapable` to cover the new shared-proxy gate. | `cmd/gc/beads_proxied_overlay_test.go`: with `[beads] proxied=true, shared_proxy=true` (+ bd capable), `applyProxiedPoolEnv` env has `BEADS_SHARED_PROXY=="1"` **and** `GC_BEADS_SHARED_PROXY=="1"`; with `shared_proxy=false`/absent or `proxied=false`, **neither** key is set — RED. | 5 | — |
| T-002 | Add the default-OFF `shared_proxy` gate: `config.Beads.SharedProxyEnabled()` mirroring `ProxiedEnabled()` (`[beads] shared_proxy`, default false); thread `shared bool` through `resolveProxiedGate` + its cache `entry`; in `applyProxiedPoolEnv` set `BEADS_SHARED_PROXY=1` + `GC_BEADS_SHARED_PROXY=1` inside the existing `active` branch only when `shared`. | T-001 green; `go test ./cmd/gc/ -run ProxiedPoolEnv` passes; `server`/`doltlite`/non-shared-proxied cases still assert the keys absent (no regression). | 5 | — |
| T-003 | Write the failing testscript: the script's proxied branch forwards `GC_BEADS_SHARED_PROXY` → `BEADS_SHARED_PROXY`. | New/extended case under `cmd/gc/bd_testscript_test.go` (or a `run_bd_pinned`-exercising testscript): with `GC_BEADS_PROXIED=1 GC_BEADS_SHARED_PROXY=1`, the captured `bd` env contains `BEADS_SHARED_PROXY=1`; without `GC_BEADS_SHARED_PROXY`, it is unset — RED. | 4 | — |
| T-004 | Wire `examples/bd/assets/scripts/gc-beads-bd.sh`: in `run_bd_pinned` proxied branch (≈ line 2230) add `export BEADS_SHARED_PROXY="${GC_BEADS_SHARED_PROXY:-}"` beside the pool-size export; ensure the direct-server branch and `unset` cleanup (≈ line 2286) leave it unexported. | T-003 green; `gc_beads_bd_lint_test.go` still passes (shellcheck/lint clean). | 4 | — |
| T-005 | **[GATED on be-pen9 pin]** Runtime proof on a THROWAWAY proxied two-scope city with `shared_proxy=true`: assert exactly one shared child serves both stores. NOT run against live portharbour. | Precondition first: `strings $(which bd) \| grep -c BEADS_SHARED_PROXY` > 0. Then on the throwaway city: `pgrep -f 'db-proxy-child' \| wc -l` == 1 with `bd list` succeeding against **both** scopes; before/after child counts pasted into the bead. | 5 | — |
| T-006 | Build/vet/test green (icu4c CGO flags) for the touched packages; confirm no projection regression. | `go build ./cmd/gc/... && go vet ./cmd/gc/... && go test ./cmd/gc/ -run 'ProxiedPoolEnv\|Proxied\|BdTestscript'` green with `CGO_CFLAGS/CGO_LDFLAGS` set to `$(brew --prefix icu4c)`. | 5 | — |

## GDPR data-flow impact

### Data added / removed / relocated
None. This changes **how many proxy processes** front the same managed dolt and
**which rootDir** they share — not what data is stored or where. Beads issue data
and the dolt store are untouched; no personal data is read, written, or moved.

### New cross-border transfers (or "none")
None. The shared proxy listens on loopback (`127.0.0.1`) and fronts the same
local managed dolt; no new network egress, no new region.

### Audit-log changes (or "none")
None. One shared `proxy.log` instead of N per-scope logs is fewer files, not
different content; dolt audit/versioning is unchanged.

## MDR Class I traceability

Not applicable — not a clinical path. This is gascity/beads orchestration
infrastructure; no `voxmemo` clinical-documentation data crosses it and no
chain-of-evidence metadata is involved. The heading is retained so an auditor
sees the explicit consideration.

## Acceptance criteria

- New default-OFF `[beads] shared_proxy` gate; OFF ⇒ env projection is
  byte-for-byte unchanged for every scope type (no regression).
- Gate ON + proxied + be-pen9-capable `bd` ⇒ `BEADS_SHARED_PROXY=1` projected
  into both the agent (`gc-beads-bd.sh`) and controller (Go-launched) `bd` env.
- Throwaway proxied multi-scope city with the gate ON ⇒ `db-proxy-child` count
  collapses from N+1 to **1**, both stores queryable (T-005, gated on the pin).
- Live portharbour is **not** auto-enabled by this change (gate defaults OFF).
- `go build` + `go vet` + touched-package tests green (T-006).

## Rollback plan

1. **Git-level.** Revert the ga-mozik PR. Because the gate defaults OFF and is
   the only thing that projects `BEADS_SHARED_PROXY`, reverting removes the
   *option*; all scopes keep their current per-scope proxied behavior.
2. **Data-level.** None — no schema change, no migration. If a shared child is
   live on the throwaway, `pkill -f db-proxy-child` and scopes respawn per-scope
   proxies under the prior config. Dolt data and `dolt remote` sync are never
   touched.
3. **Decision criteria.** Trigger rollback if, on the throwaway city, any of:
   (a) a scope reads another scope's store (cross-store bleed); (b) the dolt
   `Connections` counter is not bounded; (c) the single shared proxy crashing
   takes down all scopes without clean spawn-or-reuse recovery.

## Open questions

- **(executor/reviewer) Gate field name + shape.** `[beads] shared_proxy = true`
  with `config.Beads.SharedProxyEnabled()` mirroring `ProxiedEnabled()` is the
  recommendation (default false). Confirm the field name is consistent with the
  existing `proxied` / `proxy_pool_size` keys in the `config` package.
- **(executor/reviewer) Script default for `GC_BEADS_SHARED_PROXY`.** Recommend
  empty default (`${GC_BEADS_SHARED_PROXY:-}`) so the Go overlay is the single
  source of truth and the gate cannot be turned on by the script alone. Confirm
  no other caller relies on a `1` default.
- **(executor/reviewer) Does `BEADS_SHARED_PROXY` need to be unset on the
  direct-server path?** The overlay only sets it when `active && shared`, so it
  should never leak to a direct scope; verify the `unset` block (≈
  `gc-beads-bd.sh:2286) and the Go non-proxied path both leave it unexported.
- **(architect) Production rollout of the live-portharbour flip is OUT OF SCOPE
  here.** Turning `shared_proxy` ON for live portharbour (18 rigs → 1 shared
  child = a fleet-wide SPOF for `bd`) is a separate, deliberate ops decision
  after the throwaway proof and after the be-pen9 pin. `be-pen9` judged the
  existing stale-pidfile + child-flock spawn-or-reuse recovery sufficient for
  the shared topology; the production blast-radius sign-off rides that decision,
  not this wiring bead. File a follow-up `ga-` bead for the live flip; route to
  `voxist.platform-architect` for the go/no-go.

## Status

Plan written; **held** by planner pending the be-pen9 pin (see Precondition).
When `strings $(which bd) | grep -c BEADS_SHARED_PROXY` > 0, re-route ga-mozik to
`gascity/voxist.executor` (base `feat/beads-proxied-pooling`) and the executor
runs T-001..T-006 in one session / one PR.

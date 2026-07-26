# City-Scale Architecture Plan

**Mission:** take voxist-city from 10 days of whack-a-mole to all 8 rigs active
simultaneously.

**Measured baseline (2026-06-10):** controller ticks 90–165s against a 10s
patrol; ≥108 bd subprocess spawns/min at an *idle* fleet; gc 48% CPU asleep;
hq = 19,520 rows of which 12,805 are closed session corpses (97% of a 2.9GB
data dir); 2,618 TIME_WAIT sockets on the dolt port; swap ~98% used at near-idle;
one provider account already 429'd; dolt sql-server listening on a **wildcard**
interface; 19/19 incidents human-detected, 0 auto-remediated.

**Provenance:** produced by a 12-agent analysis (incident archaeology, store
hot-path code audit, live capacity profile, scaling math, asset inventory →
3 independent designs → synthesis) and then hardened against **3 adversarial
reviews** (incident-coverage, technical feasibility, completeness) that
produced 32 findings — all folded into the items below. Residual accepted
risks are listed at the end.

---

## Thesis

Three walls bind first, in order:

1. **The subprocess amplifier.** The controller's store access is
   `CachingStore(BdStore)` — one `bd` subprocess per store operation, per
   scope, per tick (`cmd/gc/bd_env.go` `bdCommandRunnerWithManagedRetryErr`).
   Every bd/proxy/port-file defect therefore becomes a fleet-wide dispatch
   outage: 10 of 19 incidents trace to this single design. The per-assignee
   live `Ready` fan-out (`build_desired_state.go:1575`) scales it as
   **8 stores × open sessions ≈ 800 subprocesses/tick at 100 idle sessions** —
   the scaling bomb for "all rigs active".
2. **Self-amplifying failure.** The reconciler has a failure backoff
   (#2223, 1/min after 5 failures), but the **cache read fallthrough**
   (`caching_store_reads.go:94`, `Get:344`) and **per-operation subprocess
   spawn** have none: during the HQ wedge every tick's reads spawned bd procs
   that each hung 10s on the poisoned proxy — 83% CPU spin plus a connect
   storm that re-poisoned every fresh proxy. Failure is self-sustaining by
   construction.
3. **Silent-empty + zero detection.** Hook `[]`, dispatcher skip, claim
   no-op, gate SKIP all render as "no work". There is no typed distinction
   between *empty* and *store unreachable* on gc consumer paths, and the
   event bus auto-detected 0 of 19 incidents.

The plan: **Phase 0** makes overload impossible and the proxy stance explicit;
**Phase 1** makes failure non-amplifying, typed, and auto-detected; **Phase 2**
kills the amplifier (native in-process store, data-gated, canaried);
**Phase 3** scales out and retires the stopgaps. Every code item is
threshold-mechanical (ZFC-clean), controller-driven (SDK self-sufficiency),
composes an in-flight asset where one exists, and is tagged with its upstream
owner.

## Decisions and rejected alternatives

| Decision | Resolution | Rejected, and why |
|---|---|---|
| **Interim `proxied=true` vs rollback** | **Stay on `proxied=true`** (0.1) with a pre-agreed fallback trigger | Rollback reinstates the pre-pooling wall (71 conns/s churn, Dolt >100% CPU — why pooling was deployed) exactly when we need MORE headroom. Tolerable only at today's near-idle fleet, which defeats the mission. |
| Native-store timing | The structural lever, but sequenced **behind** the breaker + load harness | "Phase 2 immediately": a native-canary failure without a breaker connect-storms the dolt server directly — worse blast radius than today. |
| `dolt_mode_safe` gate | **Stays Fail until the `custom_types_registered` preflight passes per scope** — flip gated on *data*, never on mode; factory auto-falls-back to BdStore | Any direct Pass flip (the a74fefde8 zero-session landmine). Settled decision respected. |
| Per-rig dolt servers | **Rejected** | Dolt has headroom (≤30/256 conns, 10.6 writes/s fine); 8 servers multiplies deploy surface in our #2 failure class (binary/deploy hygiene). The hotspot is data + the tier in front, not the SQL server. |
| HQ control-plane server split | Phase 3, **conditional**, entry-gated on post-diet measurements | Splitting before the data diet + amplifier kill adds ops surface before fixing the cause. |
| Shared proxy (cstar/beads #4) | Phase 3 ops cleanup, **gated on the proxy-poison root cause** (or a written risk acceptance) | Collapsing 8→1 concentrates an *unknown-trigger* failure across all agent CLI. |
| Proxy poison handling | Breaker quarantine AND patrol auto-reap **with pre-reap forensics**, plus a standing root-cause item | Reap-only destroys the evidence each cycle and makes the trigger permanently un-diagnosable. |
| Ready fan-out fix | One `Ready` per store + in-memory assignee filter, aligned with upstream #3218 | Hand-rolling a parallel mechanism upstream is already building. |

---

## Provider Governor — quota-aware, capability-aware multi-account scheduling

**Why (corrects the original "partition"):** the two Claude accounts are
**alternating** capacity, not parallel. Running `claude`+`claude2` at once
doubles burn and co-exhausts both 5h windows *and* the weekly cap →
simultaneous fleet-wide blackout. There is no single knob: there are **four
signals** against **three constraints** plus a **quality** axis —

| Constraint | Signal | Right response | Strategy that attacks it |
|---|---|---|---|
| per-minute **rate** (load) | transient 429, recovers s–min | backoff, **stay on the account** | request-rate smoothing |
| **5-hour** rolling window | `five_hour` maxed up to 5h | **flip to the sibling Claude account** | **alternation** |
| **7-day** "weekly" cap | `seven_day` maxed for days | nothing recovers it short-term | **tiering** (fewer Claude tokens/work) |
| global **outage** | vendor down | **cascade to zai/others** | multi-vendor fallback |
| (cross-cut) **quality** | GLM/Qwen < Claude on hard work | reserve Claude for work that needs it | capability-aware routing |

### Validated capture (live, 2026-06-10)

Claude Code exposes authoritative subscription usage at
**`GET https://api.anthropic.com/api/oauth/usage`** (header
`anthropic-beta: oauth-2025-04-20`; internal fn `fetchUtilization`). Live shape:

```json
{ "five_hour":        { "utilization": 42.0, "resets_at": "2026-06-10T02:40:00Z" },
  "seven_day":        { "utilization":  8.0, "resets_at": "2026-06-15T20:00:00Z" },
  "seven_day_opus":   null,
  "seven_day_sonnet": { "utilization":  0.0, "resets_at": null },
  "extra_usage":      { "is_enabled": false, "used_credits": null, ... } }
```

`utilization` = %-of-cap used per bucket; `remaining = 100 − utilization`;
`resets_at` = exact window-reset time. **Separate weekly buckets for Opus and
Sonnet** are a model-tier lever (degrade Opus→Sonnet on the same account before
switching accounts). `extra_usage` = overage credits.

**Auth constraint (load-bearing finding):** the agents' setup-tokens
(`sk-ant-oat…`, scope `user:inference`) are **403-rejected** —
`OAuth token does not meet scope requirement user:profile`. Only a full OAuth
*login* credential carries `user:profile` (verified: agent setup-token 403s,
the keychain `Claude Code-credentials` login returns the JSON above). So the
governor needs a **per-account monitoring credential**: one `claude` OAuth
login per Claude account into a dedicated `~/.gc/monitor-<acct>/` config dir
(full login, not setup-token), which the poller refreshes (`refreshOAuth`).
This is a one-time auth setup, **separate from the agents' inference auth**.

### Governor design (controller-driven, ZFC-clean — measured state, not heuristics)

- **Quota poller** (controller-side): holds each account's monitoring
  credential; polls `/api/oauth/usage` every ~60–120s per Claude account
  (cheap — usage metadata, **zero model tokens**); emits typed
  `provider.quota_observed{account, five_hour_util, seven_day_util,
  seven_day_opus_util, seven_day_sonnet_util, extra_usage, resets_at}`.
- **Reactive floor** (no extra auth, always works): the existing
  session-output scan classifies the CLI's limit messages (`session limit` /
  `weekly limit` / `Opus limit` / `Sonnet limit` + "resets at …") into typed
  `provider.window_exhausted{account, type, reset}` — the backstop between
  polls or if a monitoring credential is unavailable.
- **Decision policy (declarative config; governor resolves
  `provider = f(tier, measured state, policy)` at session (re)spawn):**
  - **Tiering** (attacks the *weekly* cap): per-template `tier` —
    `claude-required` vs `overflow-ok`; `overflow-ok` → zai/dashscope/openrouter
    always → stretches `seven_day` ~`1/(1−overflow_fraction)`.
  - **Alternation** (attacks the *5h* window): `claude`+`claude2` = one logical
    pool, ONE active account; serve `claude-required` from the account with
    lower `max(five_hour_util, weighted seven_day_util)`; flip to the sibling
    when active crosses a threshold (e.g. `five_hour_util > 85`); **never both
    flat-out** unless demand exceeds one account's sustainable rate.
  - **Model-tier degrade** (new lever): `seven_day_opus` near cap but
    `seven_day_sonnet` has headroom ⇒ route `claude-required` to a Sonnet model
    on the same account before switching accounts.
  - **Cascade** (last resort): `claude-required` → overflow pool only when
    BOTH Claude accounts are window/quota-dark or Claude is globally down —
    degrade quality rather than stop.
  - **Load smoothing** (the original 429, *separate from quota*): cap
    requests/min per account below the rate limit + stagger spawns; a
    `provider.rate_limited` signal ⇒ backoff on the SAME account, **not** a flip.
- **Mechanism:** operates at the **session lifecycle** (a running `claude`
  session's provider env is fixed at spawn — "switching" a live agent = recycle
  it). `overflow-ok` agents spawn on the vendor pool and stay; `claude-required`
  agents recycle onto the sibling account **at natural boundaries** (idle/turn
  end, not mid-task) when the poller or a `window_exhausted` event marks the
  active account dark; a maxed account's `resets_at` schedules its automatic
  re-activation.

### Phase placement

P0.3 = the **tiering** config (immediate weekly-cap relief, outside the managed
block). P1 = quota poller + typed `provider.*` signals + request smoothing.
P2 = the alternation/model-degrade governor + recycle-on-dark at boundaries.
P3.2 = cascade + cross-vendor failover as the governor's last arm.
**Open setup task:** provision the per-account `user:profile` monitoring
credential (one OAuth login per Claude account) — the proactive path depends on
it; the reactive floor works without it.

### Success metrics this unlocks (data-tunable, not guessed)

Per-account `five_hour_util` / `seven_day_util` gauges; Claude-token-spend/hour
**by tier**; %-of-agent-hours on overflow (the tiering lever); time-to-both-
accounts-dark (target: never); fleet-wide-429 (target 0).

---

## Phase 0 — Tactical stabilization (days; config + ops, plus ONE idempotent data write in 0.6)

**0.1 — Proxied stance: KEEP `proxied=true`, with an honest tripwire and a
pre-agreed fallback.** The city is healthy on it; v2 S1–S5 closed the
leak→wedge→panic chain; rollback re-creates the conn-churn wall. The HQ wedge
(incident 9) recurring ≈11h is real, so bound it:
(a) document `gc stop && gc start` as the known-good ≤5-min remediation in the
runbook (1.11) — that is the *honest* Phase-0 MTTR;
(b) **external tripwire, NOT a city order**: a launchd interval job (or
supervisor-side loop independent of the tick/store path) runs the two-probe
matrix — proxied `bd list --limit 1` per scope; direct `SELECT 1` against the
dolt listener discovered from the **process table** (never the port file) —
and on A-fail∧B-ok alerts via the extmsg emitter binary directly. *Rationale:
order dispatch rides the controller tick and the beads store — exactly the
machinery incidents 5 and 9 degrade; an in-band sentinel cannot detect the
failure that disables it.* The order variant may exist as a secondary signal
only.
(c) **Fallback trigger, pre-agreed:** wedge frequency >1/48h before 1.2+1.5
land ⇒ flip `proxied=false` city-wide as a temporary, time-boxed regression to
the known CPU-churn state.
*Owner: city config + launchd. Effort S. Bounds 9's MTTR to ≤5min human
runbook (alert ≤ probe interval) until P1.5 automates it.*

**0.2 — Capacity envelope, RAM-derived with a fairness gate.**
Measure RSS/session on live agents first; set
`[workspace] max_active_sessions = min(3/core, (RAM − dev-load reservation −
dolt/proxy/system budget) / RSS-per-session)` — on this box ≈48 only if the
RSS math supports it — and per-rig `max_active_sessions = 10`.
**Reduce pack pool floors so the floor sum ≤ the city cap** (config-only,
reversible): a 48-slot cap against ~263 of unchanged floors makes slot
allocation reconciler-order-dependent and manufactures a *new* "rig has
waiting work but cannot spawn" starvation mode (incident-14-symptom) before
P3.1 elasticity lands.
*Gate: under synthetic backlog, active sessions pin at cap, swap <25%, and
**every rig with ready work holds ≥1 active session within one tick**.
Rollback: unset keys / restore floors.*

**0.3 — Capability tiering (the Phase-0 slice of the Provider Governor).**
*Static rig→account "partition" was rejected (see the Provider Governor
section): the two Claude accounts are **alternating**, not parallel, capacity —
running both at once co-exhausts their 5h windows + the weekly cap →
simultaneous fleet-wide blackout.* The Phase-0, config-only action is the
**tiering** half: label each agent *template* with a `tier` and route the
`overflow-ok` tier (librarian, routing/dispatch, status/doc, mechanical chores,
first-pass triage) onto the zai/dashscope/openrouter pool **all the time** —
written **outside the `gc-provider-switch` managed block** (verified: that
block is rewritten fleet-wide by the switch tool and would clobber hand edits)
+ a doctor check that the managed block never pins all `claude-required`
templates to one account. This immediately stretches the Claude weekly cap by
the overflow fraction. The alternation + quota-poller machinery lands in P1/P2.
*Gate: `overflow-ok` agents show `gc.provider` ∈ {zai,dashscope,openrouter};
Claude token-spend/hour drops by ~the overflow fraction. Contains incident 10;
full retirement P3.2.*

**0.4 — MCP isolation: root-cause, then repair.** The
`~/.gc/agent-*/plugins → ~/.claude/plugins` symlink is **live again** — it
came back after the original incident-11 fix, so something (gc config-dir
scaffolding, provider bootstrap, or a login flow) re-creates it. Identify the
creator first (read-only: grep gc bootstrap/provider code; file birthtime vs
gc start times), fix the creator, then replace symlinks with empty dirs.
*Gate: city-agent MCP proc count = 0 and stays 0 across a supervisor restart.*

**0.5 — FS/disk/network hygiene + security posture.**
(a) Spotlight-exclude `.beads`/dolt/proxieddb dirs; purge the 49GB
`.beads/backup`; `proxy_pool_size` 4→2 (deferred-S6 guidance for ≤8 scopes).
(b) **Bind the dolt sql-server and every db-proxy-child to 127.0.0.1
explicitly** — the production work ledger currently listens on a wildcard
interface and is LAN-reachable with permissive auth.
(c) Verify secret files (`.gc/secrets.env`, controller token) are mode 0600.
*Gate: fseventsd <10% CPU under bd churn; backend conns ≤20; `lsof` shows no
gc/dolt/proxy listener on a non-loopback interface.*

**0.6 — Operator visibility + data preconditions.** Commit/clean the dirty
voxist-platform pack worktree (un-breaks `gc status`, stops re-expansion
thrash — incident 13's standing tax); **backfill `step` into the 7
`custom_types` tables missing it** (idempotent data write — the one non-config
item in this phase); audit `.beads/hooks/on_*` across all 8 scopes with a
written event-coverage note (vp-tz6r precondition).
*Gate: `gc status` green; `SELECT name FROM <db>.custom_types ⊇
RequiredCustomTypes` for all 8 DBs.*

**0.7 — Provider quota budget, from LIVE data (not a paper estimate).**
Poll `/api/oauth/usage` per account (the validated capture) to read real
`five_hour`/`seven_day` utilization vs the 0.2 session cap and the 0.3 tier
split; size how many `claude-required` always-on sessions the alternating
Claude pool sustains without weekly-cap exhaustion, and which templates MUST be
`overflow-ok` to stay inside it. This both sizes the cap and seeds the Provider
Governor's thresholds; output feeds the P3 capacity drill's quota-budget pass.

---

## Phase 1 — Failure becomes non-amplifying, typed, and auto-detected (weeks 1–3)

Split into **1a (week 1–2: 1.0–1.4)** and **1b (week 2–3: 1.5–1.11)**; the
phase carried too many M items for one window, and 1.5/1.6 are the likely
slips (both gate the exit drill).

| # | Item | Repo / area | Size | Owner | Retires |
|---|---|---|---|---|---|
| 1.0 | **Staging city provisioning**: persistent staging city on this Mac (own directory, dedicated dolt port, own `tmux -L` socket, hard session caps, isolation budget) + synthetic backlog generator. Precondition for every drill and cutover below. Enumerate which gates MUST run on macOS (wedge drill, fseventsd/Spotlight, launchd provenance kill-test, swap gate) vs which CI covers (flood, conformance) | ops + scripts | M | fork-local | makes the drills real |
| 1.1 | **Fork resync** onto upstream gascity main (≥91e64b9a1): #3258/#3253 hydration-free bounded reads, #3211 100× parallel status counts, #3252 fork-rate doctor watch, **#3219 preflight-degrade (BEFORE any local gate work)**, #3251 no auto-backup, #3263; dolt 2.1.4 — **including the fork-verify.yml `DOLT_VERSION` pin bump (TestDoltVersionPins enforces it)** | gascity fork/main | M | fork (consume upstream) | cheapest standalone perf win |
| 1.2 | **Breaker package** `internal/resilience/`: registry keyed (scope, opClass); trip on 3 consecutive **transport-class** failures; full-jitter exponential open-state 1s→60s; half-open single probe/15s. Wired at the three chokepoints: `bdCommandRunnerWithManagedRetryErr` (open ⇒ `ErrStoreUnavailable`, zero subprocess), `caching_store_reads.go:94`/`Get:344` (open ⇒ last-good cache tagged degraded), reconcile loop (open ⇒ skip cycle, emit once). **Plus: (a) an architectural guard test (style of `TestGCNonTestFilesStayOnWorkerBoundary`) forbidding non-test `sql.Open`/direct store-dial in `cmd/gc` outside the breaker-wrapped registry — and therefore pull the 4 raw `sql.Open` sites (2× `convoy_sql.go`, `dolt_project_id.go:518` = the 2,618-TIME_WAIT hotspot, `dolt_sql_health.go`) into this phase via the vp-kxbh `internal/doltpool/` registry (it already has a pool.go skeleton); (b) a small upstreamable bd change: a typed machine-readable error envelope (reserved exit-code contract distinguishing transport vs application failures) — until it ships, the existing string table (`bdTransportRetryableError`) is documented and tested as an explicit compatibility surface.** Config `[beads.resilience]`; semantics aligned with beads-lib `dolt/circuit.go` | gascity + small beads PR | M+M | gastownhall/gascity + steveyegge/beads | **9** as a city-KO class; bounds 7-residue; structural not enumerated coverage |
| 1.3 | **Typed unavailable ≠ empty (R-INV):** `beads.ErrStoreUnavailable` propagated to `gc hook` (exit 2 + stderr token ≠ exit-1 no-work), order dispatch (`order.skipped_store_unavailable` event, never silent SKIP), `buildDesiredState` (freeze prior desired state, never zero-demand on error), CachingStore (`Degraded: true` in BeadsDiagnostic). Scope quarantine: breaker-open scope skips that rig's pools this tick; others proceed. Per-scope `scope_reconcile_budget` (5s) bounds tick duration. Also: typed `order.gate_timeout_fail_open` counter event (every fail-open counted — rising count is the early warning) | gascity | M | gastownhall/gascity | silent rendering of 1,2,3,4,5,12 → 1-line diagnoses; blast radius = one rig |
| 1.4 | **Deploy provenance, machine-derived (not human-maintained):** `make install` writes a build manifest (commit SHA + repo path); supervisor **on startup** asserts running `vcs.revision` is ancestor-or-equal of the source repo's `fork/main` HEAD (`git merge-base --is-ancestor`, read-only) and **hard-fails on mismatch**; `bd --version` vs `[beads] expected_build`; post-install contract probe = `bd context` from the city root **plus each un-migrated bd subcommand that agent packs actually use** (enumerate by grepping pack templates) against a scratch scope. *The original "running == on-disk" check passes when both are stale — incident 5's actual shape — so lineage assertion is mandatory, not optional.* | gascity doctor + Makefile | M | gastownhall/gascity | **1, 5**, incident-2 landmine, brew-clobber class |
| 1.5 | **Store Health Patrol** `internal/storehealth/`: controller-internal, per scope/30s, two-probe matrix where **probe A forces a fresh backend connection** (the HQ poison hit *new* opens only — a pooled ride-along would miss it). A-fail∧B-ok ×3 ⇒ **capture forensics first** (SIGQUIT goroutine dump of the proxy child, `lsof` of its conns, last-N log lines → `.gc/trace/quarantine/` per the reconciler-debugging workflow), then reap via the existing vp-w7tc lifecycle; trip breaker until A passes. A-ok∧B-fail ⇒ breaker + alert, never auto-kill the sql-server. Max 1 reap/scope/10min; second poison in window ⇒ alert + keep forensics. **Plus a cheap write-path conformance probe per scope** (create+delete one ephemeral bead of each `RequiredCustomType`): persistent write rejections are application-class — invisible to a transport-only breaker — and feed the same `store.degraded` quarantine path (closes the post-cutover a74fefde8-recurrence hole) | gascity new pkg + supervisor wiring | M | gastownhall/gascity | **9** detection ≤90s, remediation ≤120s, *and evidence preserved for root cause*; port-lie class harmless |
| 1.6 | **Session-bead hygiene:** land vp-g0z1 (`reapPhantomSessionBeads`, green TDD start) + idempotent (deduped) session-bead creation per pool slot (200–415 dupes observed) | gascity session lifecycle | M | gastownhall/gascity | **6** |
| 1.7 | **Port-file de-authority, fully:** all gc consumers resolve the dolt endpoint from the managed-server live handle → process table, *full stop* (`proxied_server_client_info.json` is itself a status file — demoted alongside, same doctor flag). **Root-cause the surviving port-file writer** (the file lies *today*: a writer outlived the pooling fix). Consistency check is hard-red at zero tolerated mismatches; schedule actual file removal once bd's direct-mode discovery resolves endpoints another way | gascity `bd_env.go` + consumers | S-M | gastownhall/gascity | **4** + the 5-amplifier; enforces our own no-status-files principle |
| 1.8 | **Proxy log throttle + lumberjack rotation (50MB×3)** — deferred S3d, per vp-rnq0 | beads | S | cstar #5 lineage | **8** |
| 1.9 | **Typed events + heartbeat + scheduled doctor:** register `store.degraded/recovered/probe_failed`, `proxy.reaped`, `breaker.state_changed`, `controller.tick_completed` (emitted at a patrol multiple or on threshold breach — not every tick), `order.skipped_store_unavailable`, `order.gate_timeout_fail_open`, `session.provider_throttled` (from the existing session-output scan — minimal provider-health signal now, not Phase 3), `doctor.alert`. **The supervisor evaluates the cheap doctor subset (provenance, agent_config_isolation, port-consistency, tick-age, listener_bound_localhost, secret_file_modes, S6 ceiling `scopes×(pool+1) ≤ 0.8×@@max_connections`) on startup and on a fixed interval** — without this, every doctor-based "retirement" is detection-at-human-cadence, the exact vigilance model that produced incidents 5 and 11. Subprocess admission semaphore (`max_inflight_per_scope=4`, global 16) + `gc_bd_inflight` gauge. Event-bus hygiene: explicit `archiveRetainAge` (30d); add reconciler-trace (878MB today) and sweep logs to the rotation budget | gascity events/doctor/supervisor | M | gastownhall/gascity | event bus becomes an actual detector (caught 0/19); cements 11; closes the doctor-cadence hole |
| 1.10 | **Flat-membership O(rigs) gate evaluation** (the deferred vc-6qh1 #1' design). The shipped fail-open means gate timeouts silently PASS gates, and O(tree) cost grows with exactly the load this plan adds — fail-open frequency would *rise* with the mission. (#3263 upstream is only a map-race guard; no flat-membership commit exists anywhere yet) | gascity orders | M | gastownhall/gascity | **12** (the real fix; 1.3's counter is the tripwire) |
| 1.11 | **Runbooks + second operator:** `voxist-city/docs/runbooks/` — wedge remediation, deploy+cutover checklist (with provenance verification), drill procedures, the 0.1 fallback trigger, canary rollback — each PR-reviewed by a second teammate. Provider auth moved to service accounts/shared secret storage (not personal OAuth). Contribution model documented (who pushes fork/main, who deploys, escalation). *Exit gate: a second operator executes the wedge remediation end-to-end unaided on staging* | docs + ops | M | fork/Voxist org | bus factor 1 → 2+; the org move becomes real |

**Phase 1 exit gates (vs baseline):** wedge drill on the staging city — until
the poison reproducer exists (see 2.1), inject the fault with SIGSTOP on the
proxy child, and say so — ⇒ exactly one rig DEGRADED in `gc status`, gc CPU
<20% **under read-storm load against the tripped scope** (the path that
actually pinned CPU in incident 9), auto-reap ≤3 probe cycles with forensics
captured; idle bd spawns/min (measured by the same ps-sampling script as the
baseline — no in-band counter exists until 2.9) shows no regression and p95
tick <30s [the ≥50%-drop expectation from the resync is advisory, not a hard
gate]; each of the 6 known idle-fleet signatures yields a distinct observable
(table test); stale-binary kill-test goes supervisor-red in one run.
**Rollback levers:** breaker/quarantine behind `[beads.resilience]` (disable =
today's behavior); patrol reap behind a flag; resync revertible by branch.

---

## Phase 2 — Kill the subprocess amplifier (weeks 3–6)

The structural step-change: the controller moves in-process. Strictly gated;
every step's rollback is config-only because the factory auto-falls-back to
BdStore — **with the identity assertion below, so fallback can never silently
mean "wrong database".**

| # | Item | Repo / area | Size | Owner |
|---|---|---|---|---|
| 2.1 | **Load-test harness FIRST** `test/loadtest/` (`//go:build loadtest`): ephemeral dolt + 8 scopes with full custom-type registration + per-scope proxies; flood 200 concurrent bd ops/120s asserting zero nil-store panics, port-file content unchanged, proxy count == scopes, backend conns ≤0.8×max, p95 op <2s, log growth <10MB, no breaker stuck open. Controller soak: 260 synthetic session beads ⇒ tick p95 <2× patrol. **Added scenarios from verification:** (a) *poison-repro*: concurrent new-DB opens against a fresh proxy (the observed incident-9 signature) — drives the vp-2rfp root cause; (b) *server-resolution-failed*: managed server down at open ⇒ factory must fall back to BdStore, NEVER silently open an empty embedded DB; (c) *BdStore-fallback-with-no_history beads* (see 2.8 ordering); (d) *mail fan-out* (N agents × M messages/min across scopes); (e) *N concurrent dashboard/SSE clients during flood* ⇒ tick p95 unaffected, API p95 <500ms. CI runs on Linux (timing gates = 3-run-median trend gates, not single-run); the macOS-only factors (fseventsd, swap, launchd) are staging-city drill gates, not CI gates | gascity + fork CI | L | fork-local CI; flood scenarios upstreamable to beads |
| 2.2 | **beads open-path fix:** route `IsDoltProxiedServerMode()` → `dolt.NewFromConfig` (server-mode pooled `*sql.DB` + built-in breaker) in `beads_cgo.go`/`beads_nocgo.go`. *The true mechanism behind "invalid issue type" is an embedded-DB misroute (verified: `IsDoltServerMode()` returns false for proxied-server ⇒ `OpenBestAvailable` falls through to embeddeddolt ⇒ creates a fresh typeless DB), not the type system.* **Propose upstream — never a Phase-2 blocker** (it is a semantics decision upstream may answer differently: error vs proxy-aware routing — offer both shapes in the PR); 2.3 achieves the same gc-side with zero beads dependency | beads | S | PR to steveyegge/beads; carry on bfork meanwhile |
| 2.3 | **gc canary lever (no beads release needed):** when `[beads] native_store` enabled, project `BEADS_DOLT_SERVER_MODE/HOST/PORT` (keys already in `nativeDoltOpenEnvKeys`) from gc's managed-server live handle — never the port file; error loudly when unresolvable. **Plus the load-bearing safety fix: a post-open identity assertion in `newNativeDoltStoreAt`** — compare the opened storage's `project_id`/`issue_prefix` against `.beads/metadata.json`; mismatch or empty ⇒ close + `ErrStoreUnavailable` ⇒ BdStore fallback. *Without this there is a silent wrong-database mode: empty env projection or a metadata load failure makes cgo `OpenBestAvailable` create/open an empty embedded DB that passes every existing gate and reports NativeDoltStore — resurrecting the zero-session class without its signature.* | gascity factory | M | gastownhall/gascity |
| 2.4 | **`custom_types_registered` preflight + gate rewrite:** new check verifies every `RequiredCustomTypes` resolves for the scope (custom_types ∪ types.custom ∪ config.yaml, one pooled SELECT). `checkDoltModeSafe` proxied-server branch: **Pass iff native_store="auto" ∧ custom_types_registered ∧ endpoint resolvable; else today's Fail.** TDD first: failing conformance test creating session/step/convergence beads through `NativeDoltStore` against a server-mode fixture (the a74fefde8 class reproduced in CI before any gate code changes) **+ the 2.1(b) server-resolution-failed scenario** | gascity `contract/` | M | gastownhall/gascity (composes with #3219) |
| 2.5 | **Canary rollout** (staging-city green is a precondition for every lever): deploy `native_store="off"` (behavior identical; provenance verifies lineage) → **vr** (smallest rig) 48h: BeadsDiagnostic shows NativeDoltStore, zero "invalid issue type", pool counts >0, no `session.work_query_failed` → remaining rigs one at a time → **hq LAST** → default "auto" one release later | city config | S/step | fork → upstream proposal |
| 2.6 | **Ready fan-out collapse:** `liveReadyForControllerDemandQuery` becomes one `Ready(TierBoth)` per store + in-memory assignee filter (the per-assignee fan-out — 800 subprocesses/tick at 100 idle sessions — is a BdStore artifact); align with upstream #3218 | gascity `build_desired_state.go` | M | gastownhall/gascity |
| 2.7 | **Pooled-connection completion:** whatever of the 4 raw `sql.Open` sites was not already migrated in 1.2's guard-test work; NativeDoltStore shares the `internal/doltpool/` registry (one pool per endpoint). Budget ~61 of 256 conns; S6 supervisor-doctor gate enforces the 0.8× ceiling | gascity | S-M | gastownhall/gascity |
| 2.8 | **HQ data diet — respecified after verification:** (a) NEW session beads created `no_history=true` behind `[session] bead_no_history` — **hard-ordered strictly after fleet-wide 2.5 completion** (bd 1.0.x cannot fully surface no-history rows: a stress-time BdStore fallback with the diet on would make session beads partially invisible — a new zero-session route; alternatively the key self-disables whenever the scope's diagnostic store ≠ NativeDoltStore); (b) retention order **DELETES** closed session beads >7d (the corpses are *already closed* — 12,805 of them; "closing" them is a no-op); (c) **a one-time, windowed hq history-compaction step** (dolt history squash/rebuild, drilled on staging first) — `CALL DOLT_GC` only collects chunks unreachable from commits, so deletes alone cannot reclaim the ~2.4GB; this unplanned-in-v1 step is what actually reaches the <500MB gate; (d) wire (or raw-SQL bypass) gc's currently observe-only DOLT_GC maintenance path; (e) **generalize retention to a type-aware order** (messages closed >Nd, molecules/steps with closed roots, convoy debris) — mail is a bead, and P3.1 increases message traffic onto this same store | gascity lifecycle + pack orders + one-time migration | L | gastownhall/gascity + city config |
| 2.9 | **Metrics closure:** per-scope `store_op_latency_ms`/`errors`/`breaker_state`/`spawn_total` via BeadsDiagnostic + Huma-typed `/api/stores/health`; `api_request_duration` + `sse_subscriber_count`; finish beads S3f proxy stats (`PoolWaiters`, `PoolSaturatedCount`); verify the dashboard read path rides CachingStore (degraded-tagged last-good under breaker-open), never per-refresh subprocesses | gascity + beads | M | gascity upstream; S3f on cstar #5 line |
| 2.10 | Pack re-expansion dirt-tolerant content hash (incident-13 root fix) | gascity import cache | M | gastownhall/gascity |

**What remains of the proxy tier:** agent-CLI-only, on `proxied=true` with
cstar pooling + S1–S5. The controller no longer has the proxy in its
dependency chain — incident 9's blast radius shrinks from city-KO to "some
agent CLI ops retry; patrol reaps with forensics."

**Phase 2 exit gates:** idle bd spawns ≤10/min (from ≥108, now measurable
in-band via `spawn_total`); projected 8-rigs-active spawns ≤120/min (from
500–1500); reconcile p95 <100ms/store (from 0.5–2s); full tick <10s at current
fleet; hq ready latency 1–5ms (from 234ms); gc idle CPU <10% (from 48%);
TIME_WAIT <100 (from 2,618); hq data dir <500MB *post-compaction*;
**merge-pipeline drain rate measured (PRs merged/day at elevated load)** —
phases 0–2 raise PR volume into a pipeline whose fixes are Phase 3; if drain
rate degrades, pull 3.3 (M2 delivery-warden) forward.
**Rollback:** flip `native_store="off"` per scope (no restart); preflight
failure self-falls-back to BdStore; the identity assertion guarantees fallback
is never silently-wrong-database; never to a74fefde8 because the gate proves
type acceptance on real data before flipping.
**Retires:** 2 (gate data-proven + CI conformance + write-path probe),
3 (controller off bd CLI), 7 (breaker + off-proxy), 13.

---

## Phase 3 — Scale out, retire stopgaps, conditional topology (month 2+)

| # | Item | Size | Owner | Retires |
|---|---|---|---|---|
| 3.1 | **Elasticity stack:** land feat/scale-from-zero (7e72f8ac0) + cold-wake PR #3175 + consolidate the 3 cross-store delivery branches into ONE landable series (cross-store claim-write, route-guard exemption, read→claim pipeline test) + demand-driven controller (vp-p3c6); then set rig pool floors to 0 (superseding the 0.2 floor reduction). Routing validates target-store visibility at route time. The demand controller **consumes the THROTTLED provider state as backpressure** (not just failover). Retires the idle-pool-nudger stopgap | L | gastownhall/gascity (one series) | **14** |
| 3.2 | **Provider Governor — cascade/failover arm** (composes vp-m01x jittered backoff/Retry-After + vp-546v): failover targets **sessions**, not in_progress beads (fixes the startup-429 chicken-and-egg). Consumes the P1 quota poller (`five_hour`/`seven_day` utilization + `resets_at`) + typed `provider.*` signals so failover fires on the right cause; degrade-quality cascade only when BOTH Claude accounts dark. The alternation + tiering (P0.3/P2) keep this arm rarely-needed | M | gastownhall/gascity | **10** |
| 3.3 | **Delivery pipeline:** vp-krai M2 delivery-warden (upstream #3203) + M7 doctor PR-delivery view (vp-fqoo). *Pull forward into Phase 2 if its exit-gate drain-rate measurement degrades* | M | gastownhall/gascity | **15** |
| 3.4 | **Shared proxy** (cstar #4 + selection bead ga-mozik): 8 children → 1, ~1GB RAM back. **Entry-gated on: proxy-poison trigger root-caused (from 1.5/2.1 forensics + repro) OR a written risk acceptance** — consolidating an unknown-trigger failure across all agent CLI is otherwise a new single point of failure | M | cstar line + gascity | shrinks 7/9 surface |
| 3.5 | **Idle-fleet classifier:** vp-evof Go port (`gc beads state`) + `checks_idle_fleet` supervisor-doctor check composing breaker/provider/hook/phantom signals into a NAMED diagnosis in `gc status` | M | gastownhall/gascity | archaeology cost of every future "agents idle" |
| 3.6 | **Conditional hq server split:** ENTER only if, after the 2.8 diet + 2.5 cutover, hq still >50% of dolt CPU or any hq wedge recurs. PASS gate: hq outage drill leaves rig agents claiming/closing rig beads | M/L | fork-local first | residual 9 blast radius |
| 3.7 | **Upstream closure:** gate rewrite + breaker + patrol proposed to gastownhall; cstar #5 (S1–S5) and #2/#3 (canonical SEC-003) merged so bd rebuilds stop re-carrying 17 commits; **the enumerated un-migrated bd proxied subcommands** (from 1.4's pack-template audit) contributed as concrete PRs to steveyegge/beads — not open-ended | S–M | upstream | **1**/**3** regression-by-rebuild class |
| 3.8 | **Agent privilege boundary:** dedicated unix user (or sandbox-exec profile) per session pool — agents currently execute arbitrary code as the operator with read access to all secrets and the home dir. Also the prerequisite for any multi-host story | M/L | fork → upstream design | security class beyond SEC-003 |
| 3.9 | **Single-box decision point:** the quarterly capacity drill (below) measures the true 8-rig envelope (RAM-per-session × cap + dolt + proxies + dev load vs 128GB; swap; load). If the envelope exceeds the box ⇒ pilot ONE rig on a remote runtime provider (the k8s/exec providers already exist behind the Session surface) — depends on 0.5(b)/3.8 listener+privilege work | decision + M pilot | fork | the single-box dead end, answered with data |

**Phase 3 exit gates:** quarterly capacity drill — flood all rigs with
synthetic backlog ⇒ active sessions pin at cap, no swap growth, p95 tick <30s,
zero fleet-wide 429 **and per-account quota utilization <80%**; idle rig = 0
sessions and routed work to a cold rig spawns+claims <2min with the nudger
order deleted; fork delta ≤ pooling + CI files.

---

## Whac-A-Mole Retirement Table (post-verification honesty)

| # | Incident class | Contained today | Permanently retired by |
|---|---|---|---|
| 1 | SEC-003 bd-context hard-fail | fixed | **P1.4** deploy contract probe (supervisor-enforced) + **P3.7** canonical lineage |
| 2 | dolt_mode_safe Pass → zero sessions | Fail gate | **P2.4** data-gated flip + CI conformance + **P1.5** write-path probe (runtime recurrence) |
| 3 | bd proxied nil-store panics | v2 S2 guard | **P2** controller off bd CLI + **P3.7** enumerated migrations (agent-side until then: contract-probed at deploy) |
| 4 | dolt-server.port clobber | bd gates | **P1.7** de-authority **+ root-cause of the surviving writer** |
| 5 | Stale gc binary → dispatcher skip | manual vigilance | **P1.4** machine-derived lineage assertion, supervisor-enforced (running==on-disk alone passes incident 5's actual shape) |
| 6 | Start-pending bead jam | manual purge | **P1.6** reaper + idempotent creation |
| 7 | Proxy wedge + conn leak | v2 S1–S5 | **P1.2** breaker + **P2** controller off proxy (+3.4) |
| 8 | proxy.log flood | manual truncation | **P1.8** rotation/throttle + P0.5 exclusion (+ reconciler-trace in the same budget) |
| 9 | HQ wedge, no-backoff city KO | gc stop/start | **CONTAINED, not retired**: P1.2+1.5 auto-heal ≤2min with forensics; P2 removes controller exposure; **root cause stays an open standing item** (2.1 poison-repro + 1.5 quarantine artifacts feed vp-2rfp); P3.6 contingency |
| 10 | Provider 429 fleet idle | **P0.3** capability tiering (overflow off Claude) | **Provider Governor**: P1 quota poller (`/api/oauth/usage` five_hour+seven_day) + typed signals, P2 alternation/model-degrade, P3.2 cascade/failover. *Distinguishes load-429 (backoff) from window-exhaustion (flip account) from weekly-cap (tier harder) from outage (cascade) — today all flatten to "healthy"* |
| 11 | MCP fleet bloat | **P0.4** (creator root-caused first) | **P1.9** isolation check at supervisor cadence |
| 12 | Order-gate O(tree) hang | fail-open (silently passes gates!) | **P1.10** flat-membership evaluation + P1.3 fail-open counter |
| 13 | Pack re-expansion thrash | **P0.6** hygiene | **P2.10** dirt-tolerant hash |
| 14 | Cross-store dead-drops / nudger | nudger order + **P0.2 fairness gate** (the cap itself could manufacture this) | **P3.1** elasticity + delivery series (nudger deleted) |
| 15 | Review-gate timeouts / orphaned PRs | M1 shipped | **P3.3** M2+M7 (pull forward if the Phase-2 drain-rate gate degrades) |

---

## Success-metrics dashboard (watched weekly; sources: `/api/stores/health`, supervisor doctor, events.jsonl)

| # | Metric | Baseline | Target |
|---|---|---|---|
| 1 | bd subprocess spawns/min, idle fleet | ≥108 | ≤10 (P2) |
| 2 | Controller tick p95 / heartbeat age | 90–165s vs 10s patrol | <30s (P1), <10s (P2) |
| 3 | gc supervisor idle CPU | 48% | <10% |
| 4 | Store incidents auto-detected/auto-remediated, **with forensics captured** | 0% auto | 100% of known signatures named in `gc status`; 0 human store interventions/month; every reap leaves a quarantine artifact |
| 5 | Fleet-wide provider 429s / per-account quota utilization | ≥1 fleet-wide / unmeasured | 0 fleet-wide; any 429 ≤2 rigs; utilization <80%/account |
| 6 | Active sessions vs RAM-derived cap; swap | uncapped (263 ceiling); swap ~98% | pinned ≤cap; swap <25% |
| 7 | hq data dir / closed-session corpses / open message beads | 2.9GB / 12,805 / unmeasured | <500MB post-compaction / <2k / bounded by retention |
| 8 | Dolt Threads_connected + TIME_WAIT | 18–30 / 2,618 | ≤61 steady, ≤205 hard (0.8×256) / <100 |
| 9 | Deploys blocked by provenance (supervisor-enforced) | gate absent | gate enforced at startup; 0 stale-binary incidents |
| 10 | proxy.log + trace growth; fseventsd CPU | 1.38GB precedent; 878MB traces; 120% | <50MB/day bounded; <10% |
| 11 | `order.gate_timeout_fail_open` count | uncounted (silent) | 0 steady-state; any rise alerts |
| 12 | Merge-pipeline drain rate (PRs merged/day under load) | unmeasured | no degradation as fleet throughput rises |

**Standing drills:** wedge drill (staging, SIGSTOP-injection until the
poison repro lands) after every gc roll; capacity drill quarterly (incl. the
quota-budget pass); load harness red blocks every cutover; second-operator
runbook drill once per quarter.

---

## Residual accepted risks (explicit, from adversarial review)

1. **HQ-proxy poison trigger remains unknown** until the 1.5 forensics + 2.1
   repro converge on vp-2rfp. Until then incident 9 is *auto-healed chronic
   degradation* (~minutes of one-scope agent-CLI latency every ~11h), not
   retired. P3.4 consolidation is blocked on this.
2. **Linux CI vs macOS prod**: flood/conformance gates run on CI; the
   platform-class behaviors (fseventsd, swap, launchd) are covered only by
   staging-city drills on the same Mac — a shared-fate residual until 3.9.
3. **bd error classification** at the 1.2 chokepoint rides a tested string
   table until the typed-error-envelope bd change ships upstream.
4. **Co-located dev workload** shares the box; the 0.2 RAM reservation is an
   estimate revisited at each capacity drill.

**The plan's invariant:** when anything in this city breaks again, it is
detected in ≤90s by the supervisor (not a human), degrades one scope instead
of spinning, names itself in `gc status`, leaves forensic evidence, and the
fleet is provably running the binary that contains the fix.

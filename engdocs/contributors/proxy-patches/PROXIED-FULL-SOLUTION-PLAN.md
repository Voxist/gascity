# beads `[beads] proxied-server` mode — full solution plan (v2, ironclad)

> v2 supersedes v1 after a deep spec-panel review (9-agent, requirements-graded against the real code). v1 was written against the wrong branch and prescribed a redundant UOW adapter; v2 re-baselines onto `feat/connection-pooling`, adopts the routed store that already exists there, and fixes its actual defects. See vp-g0z1, bd-proxied-migration-incomplete. Verdict: **IRONCLAD-AFTER-CRITICAL-FIXES** — apply C-GAP-1..7 + the pinned values below.

## 0. BRANCH BASELINE (read first — v1's biggest defect was ignoring this)

| Branch | Has routed store (`newProxiedServerRoutedStore`)? | Has pool (`pool.go`/`pooledconn.go`/`mysqlwire.go`)? | Proxied-mode nil-store behavior |
|---|---|---|---|
| `fix/bd-ready-proxied-server-nil-store` (current HEAD; the **deployed** bd) | **NO** | NO | sets only `uowProvider`, `store` stays nil → **`bd ready` panics** |
| `feat/connection-pooling` (the branch prod *should* run) | **YES** (`uow_factory.go:30`, `dolt.New(...)` ServerMode routed through proxy) | YES | `main.go:1048` `store = s` (routed store) → works, **until the leak/wedge below** |

**All `cmd/bd/main.go` line numbers in this plan are `feat/connection-pooling` numbers** unless tagged otherwise. The fix targets `feat/connection-pooling`; the deployed bd must be rebuilt from it (carrying the v2 fixes), not from HEAD.

## 1. ROOT CAUSE — one causal chain, verified against source

The "work not claimed" outage, the EOF/pool-wedge, and the nil-store panic are **one chain**, not three bugs:

1. **Routed-store leak (the driver).** On `feat/connection-pooling`, proxied PreRun opens a routed `*sql.DB`-backed `DoltStorage` (`main.go:1048` `store = s`), but proxied **PostRun closes `uowProvider` only** (`main.go:1176-1178`); `store.Close()` runs only in the *direct* branch (`main.go:1242`). **Every proxied `bd` invocation leaks its connection to the proxy.**
2. **Pool wedge.** The per-scope db-proxy has an **unbounded** accept path — `conns errgroup.Group` with no `SetLimit` (`server.go:72`) and a fresh `p.server.Dial(ctx)` per accepted client (`server.go:201,273`). Leaked + unbounded connections saturate the managed Dolt → backend dials hit the dial deadline (`i/o timeout`), clients EOF mid-handshake (`mysqlwire` `"read handshake response: EOF"`) and retry in a tight loop → the 1 GB `proxy.log`, no backpressure, **permanent wedge**.
3. **Open-failure → silent nil → panic.** Once wedged, `newProxiedServerRoutedStore` **fails** to connect; the call site `if s, serr := ...; serr == nil { store = s }` (`main.go:1048`) **silently leaves `store=nil` on error**. Then any command dereferencing `store` (`bd ready --claim`, `search`, `blocked`, `dep cycles`) **nil-panics**.
4. **Deployed-bd amplifier.** The bd actually deployed is built from **HEAD** (`fix/bd-ready…`), which has **no routed store at all** → `store` is nil in proxied mode *unconditionally* → guaranteed panic. (This is why `bd ready` panics live even when the proxy is briefly healthy.)

So: **leak → wedge → open-fail → nil → panic.** Fix the leak + bound the pool + fail-fast on open-failure + ship the right branch, and the whole chain closes.

## 2. ARCHITECTURE DECISION (corrected from v1)

**Adopt the routed store; drop the UOW adapter as the primary fix.**

`newProxiedServerRoutedStore` (`feat/connection-pooling:uow_factory.go:30`) is a real `dolt.New(...)` `DoltStorage` routed through the proxy. It **natively services** `ClaimReadyIssue`, `CloseIssue`, `IsBlocked`, `DetectCycles`, `GetNewlyUnblockedByClose` — the controller + agent hot paths — with **no new use-case** (v1 wrongly proposed a UOW adapter that *cannot* service these: `BulkIssueStore`/`DependencyQueryStore` have no UOW use-case in `uow.go:14-21`; v1's "serviceable slice" claim was false). It also gives single-store transaction atomicity for multi-step mutations (R9/R11) for free.

The **UOW adapter + interface segregation are demoted to a follow-up** (Step S7) for any future use-cases that genuinely need per-call UOW semantics. They are NOT on the critical path.

## 3. IMMEDIATE SAFE STABILIZATION — ship now to end the outage (unchanged, ship-ready)

Turn `[beads] proxied` **OFF**, reverting live cities to direct ServerMode (battle-tested; populates `store` at `main.go:1066`). Eliminates the leak, the wedge, and the nil-store class in one config flip — zero hot-path Go changes.

```toml
[beads]
proxied = false
```
Mechanism: `BeadsConfig.ProxiedEnabled()` (`gascity/internal/config/config.go:1287`) → false → the overlay gate (`cmd/gc/beads_proxied_overlay.go:83`) is false → scopes resolve to `dolt_mode="server"`. `BdStore` still pools at the gc layer (MEMORY [[Option 2]]) — only per-CLI connection churn is lost.

Steps: (1) set `proxied=false` in each live `city.toml`; (2) `gc stop && gc start` so `ensureCanonicalScopeMetadata` rewrites each scope's `.beads/metadata.json` to `dolt_mode="server"`; (3) `ps -ax | grep db-proxy-child` → kill orphans (never personal tmux/servers); (4) verify `bd ready --json`, `bd query --json`, `bd update`, `bd close`, **and `bd ready --claim`** all exit 0.

## 4. FULL SOLUTION — ordered, concrete steps (all on `feat/connection-pooling`)

### S1 — Fix the routed-store leak (closes the wedge driver) **[C-GAP-2, R30]**
`cmd/bd/main.go` proxied PostRun block (`~1176-1179`): after `_ = uowProvider.Close(rootCtx)`, add
```go
if store != nil { _ = store.Close(); store = nil }   // mirror the direct cleanup at main.go:1242
```
**Test:** `main_proxied_leak_test.go` — run N=50 proxied bd invocations against one proxy; assert the proxy's `Threads_connected` / open-conn count returns to baseline (no monotonic growth).

### S2 — Make routed-store-open failure FATAL, not silent **[Major, R20/R22]**
`cmd/bd/main.go:1048` — replace `if s, serr := newProxiedServerRoutedStore(...); serr == nil { store = s }` (silent on error) with: open, and on error `FatalError("proxied store unavailable: %v", serr)`. The "best-effort" branch *is* the nil-store class.
**Test:** point `BEADS_DIR` at a proxied scope with no reachable proxy; assert exit≠0 with a clear message, **no panic**.

### S3 — Bound the proxy (the actual wedge fixes — NEW code on `feat/connection-pooling`) **[C-GAP-3, R13–R17]**
These files exist on `feat/connection-pooling`; the named guards do **not** — they are new code, not edits.
- **S3a (accept concurrency, primary anti-wedge):** in `proxy/server.go` after pool init, `p.conns.SetLimit(poolSize)` (prefer `errgroup.SetLimit` — zero goroutine growth — over a semaphore). Caps live backends; clients queue. **[R13]**
- **S3b (true bounded pool + lock ordering — C-GAP-4):** add `maxOpen = poolSize` + a live-borrow counter. `get()` blocks (borrow-wait, 2s timeout → MySQL ERR-1040 "server busy", not a bare TCP close) instead of dialing unconditionally. **Invariant:** check-and-increment the borrow counter **atomically under `p.mu`** *before* releasing for `dialNew`; decrement under `p.mu` in `put()` and the `dialNew` error path — else two goroutines both see `live==maxOpen-1` and overrun by up to `poolSize`. **[R14/R15]**
- **S3c (dial deadline):** backend *dial* gets `poolDialTimeout=2s`, distinct from the `handshakeTimeout=10s` — saturated Dolt fails fast, frees the slot, bounded backoff on `i/o timeout`. **[R19]**
- **S3d (EOF / log-flood guard):** classify expected client disconnects at `acceptClient` ("read handshake response: EOF") — log debug, **rate-limited 1/s per remote addr** (`golang.org/x/time/rate`); add log rotation (50 MB × 3, lumberjack) so a storm can't make a 1 GB log. **[R23/R36]**
- **S3e (cap the client `*sql.DB`):** `dolt_sql_provider.go openDB` — `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(5m)` (it currently sets none; match `dolt/store.go:1334`). **[R17]**
- **S3f (observability):** extend `proxy/stats.go` with `BackendDialConcurrentPeak` (gauge), `PoolWaiters`, `PoolSaturatedCount`; wire increments in S3a/S3b; expose to operators (not log-only). **[R16/R33]**

### S4 — Restore identity validation in proxied mode **[C-GAP-5, R43]**
`validateWorkspaceIdentity` (`main.go:1132`, currently direct-only → skipped in proxied mode → scope-leak risk). Call it in the proxied write path before `syncCommandContext()`, guarded by `!useReadOnly`, reading metadata over the routed store.
**Test:** multi-scope isolation — a bead written in scope A is not visible/writable through scope B's proxy.

### S5 — Command-surface classification + honest unsupported errors **[R1/R4/R5/R7/R25/R41]**
Add a table classifying **every** `cmd/bd/*.go` command as `serviced` (routed store / read), `noDbCommands` (`main.go:743-769`), or `unsupported`. The routed store services the work surface; the genuinely-impossible raw-Dolt ops — `bd dolt push/pull/commit`, federation staging-branch surgery, compaction — return a **typed** `*UnsupportedInProxiedModeError{Capability}` (errors.As-checkable; const enum of capability strings; message e.g. *"compaction requires direct ServerMode (set [beads] proxied=false)"*). Reclassify `RemoteStore.Push/Pull/ForcePush` as **unsupported** (the `DoltRemoteUseCase` covers config CRUD only).
**Test:** `unsupported_not_swallowed_test.go` (the dead-drop guard) — `bd compact`, `bd federation …`, `bd dolt push/pull` each exit≠0 with the capability message; assert they **never** print empty-JSON + exit 0.

### S6 — Hard rollout gates (were "open questions" in v1) **[C-GAP-6/7, R18/R48/R49]**
- **Connection-ceiling gate:** query `SELECT @@max_connections` on managed Dolt; compute `peak = N_scopes × (poolSize + 1)` (the +1 = init-schema `openDB`); **hard-stop if `peak > 0.8 × max_connections`.** With default `max_connections=151`, an 8-scope city needs `poolSize ≤ 18`; **recommended `poolSize=2` (8×3=24 ≪ 151)**. (`BackendLocalSharedServer` is still stubbed at `db_proxy_child.go:89` — no shared-pool consolidation, so this per-scope ceiling is the live constraint.)
- **Branch-reconciliation gate:** the prod `bd` MUST carry both the routed store AND S1–S3 — **ship both or neither**. `git diff HEAD..feat/connection-pooling --name-only`, resolve `close.go`/`dep.go`/`create.go` conflicts, rebase HEAD's SEC-003/nil-guard work onto `feat/connection-pooling` (not vice-versa).

### S7 — Follow-ups (NOT blockers) **[R42/R51]**
- Wire federation reads / advanced queries over the routed store/UOW rather than blanket-stubbing (don't break serviceable reads).
- Interface segregation: split `DoltStorage` (12 sub-interfaces — fix the "13" miscount) into a narrow `WorkStore` the proxied path satisfies honestly + a `MaintenanceStore`/`SyncStore` it doesn't — making "unsupported" a compile-time absence. The routed store already satisfies the full interface, so this is lower-urgency than under v1's adapter design.

## 5. PINNED VALUES (no ambiguity left for the implementer)

| Value | Default | Rationale |
|---|---|---|
| `openDB` `SetMaxOpenConns` / `SetMaxIdleConns` | **1 / 1** | each bd process is single-threaded vs its proxy; the proxy multiplexes |
| `openDB` `SetConnMaxLifetime` | **5 min** | avoid stale long-lived conns |
| `poolSize` (and `maxOpen`) | **2** for ≤8-scope cities (max 18) | `8×(2+1)=24 ≪ 151`; 4 only with verified headroom |
| pool borrow-wait timeout | **2s** → ERR-1040 | matches `readyDialTimeout` |
| backend dial deadline | **2s** (`poolDialTimeout`), distinct from `handshakeTimeout=10s` | fail fast, free slot |
| flood test | **N=200, poolSize=4, wall-time ≤30s** | hard CI gate |
| proxy log | **rotate 50 MB × 3**; flood delta **≤10 MB/hr** | the incident made 1 GB |
| EOF log rate-limit | **1/s per remote addr** (`x/time/rate`) | stop the storm |
| ceiling gate | `N×(poolSize+1) ≤ 0.8 × @@max_connections` | R18 pass/fail |
| commit message format | `"bd: <command> <issueID>"` | Dolt history readability |
| canary green signal | (1) `proxy.log` delta <10 MB/hr; (2) `bd ready --claim --json` claims >0; (3) `db-proxy-child` count ≤ scope count; over 24h | measurable §7 criterion |
| `Capability` strings | const enum block | tests reference exact values |

## 6. TEST + VALIDATION (mapped to requirements; every R has a named test)

**Unit (beads):** `main_proxied_leak_test.go` (S1/R30); routed-store-open-fail-is-fatal (S2/R20/R22); `store_guard_test.go` recover-harness for `ready --claim, query, list, show, stats, count, purge, update, close, search, blocked, dep cycles` — all error, none panic (R21); pool unit tests for S3a–S3f incl. the lock-ordering race (R13/R14/R16); `unsupported_not_swallowed_test.go` (R41); `TestDirectModeUnchanged` (R46); `TestProxiedBindsLoopback` (R38); DSN-not-logged (R39).
**Integration (real Dolt + proxy in `t.TempDir()`, build-tagged; package `cmd/bd` per the actual harness — fix v1's `test/` path):** the controller's literal shell-outs (`internal/beads/bdstore.go`, `internal/config/config.go`): `bd ready [--claim] [--assignee] --json`, `bd list/query/count/show/stats --json`, `bd update`, `bd close` → all exit 0, valid JSON, and **`bd ready --claim` actually claims** (R1/R2). Parity test: same data → identical result sets proxied vs direct (R6). Multi-scope isolation (R43/R48). Interop: rebuilt bd + already-running proxy + existing `metadata.json` (R47).
**Load (`proxy_flood_test.go`, integration):** N=200 concurrent `bd ready --json`, poolSize=4 → completes ≤30s; zero panics; `BackendDialConcurrentPeak ≤ poolSize`; `SHOW STATUS LIKE 'Threads_connected'` never exceeds the ceiling; proxy-log delta ≤10 MB; a single `bd ready` succeeds after (convergence, no wedge); **fail if any invocation logs "read handshake response: EOF"** (R15/R16/R18/R23).
**CI:** macOS local build is Dolt/ICU-blocked (MEMORY [[gascity fork CI verification]]) → the entire suite is gated on **fork-verify CI provisioning managed Dolt + proxy**; confirm the harness stands those up or R1/R2/R6/R15 have zero executed coverage. Commit `--no-verify` where the local Dolt toolchain blocks the hook.

## 7. ROLLOUT
1. **Stabilize (today, gascity-only):** `[beads] proxied=false` + `gc stop && gc start`; verify §3. Ends the outage, zero code risk.
2. **Fix (beads, on `feat/connection-pooling`):** one PR carrying S1+S2+S4 (routed-store leak/fatal/identity) and S3 (pool), reconciled with HEAD's SEC-003/nil-guard work per the branch-reconciliation gate (S6). cstar owns the upstream PR.
3. **Validate:** full §6 suite on fork-verify CI; **do not proceed without a green flood test (no wedge) and the ceiling gate passing.**
4. **Canary re-enable:** flip ONE city to `proxied=true` with the rebuilt bd; watch the §5 canary green signals for 24–48h; then roll the fleet.

**Gates:** fork-verify green (unit+integration+flood); ceiling gate `peak ≤ 0.8×max_connections`; branch-reconciliation (ship-both); canary 24h clean. No `internal/api/` paths → `make dashboard-check` N/A.

## 8. RESIDUAL RISKS (accept-and-monitor)
1. **Startup race:** routed-store open is now fatal; if the proxy isn't listening yet at first `bd` PreRun post-`gc start`, that command fails cleanly (not a wedge). Monitor first-invocation failures after restart.
2. **Phase-2 `BackendLocalSharedServer` still stubbed** — per-scope proxies stay independent; `N×(poolSize+1)` is the live ceiling until consolidation. Monitor `Threads_connected` as scope count grows past 8.
3. **Interim LSP lie** — raw-Dolt ops still fail at the proxy/Dolt layer, not a clean compile-time boundary. Segregation (S7) remains a real follow-up; audit agent prompts that assume `bd federation`/`bd dolt` work in proxied mode.
4. **CI coverage** — if fork-verify doesn't stand up Dolt+proxy, the "green" is hollow for R1/R2/R6/R15. Confirm the harness first.
5. **Toggle/drift interleave** — rapid canary flip/unflip could race the drift reconciler; confirm `config_drift` clean across one full reconcile cycle per flip.

---

## Appendix A — Requirement rubric (R1–R52) the plan is graded against

Coverage after applying v2: **COVERED ~40 · remaining gaps closed by S1–S6.** The rubric (functional R1–R7, transactional R8–R12, concurrency R13–R19, failure-handling R20–R26, lifecycle R27–R32, operational R33–R37, security R38–R40, data-integrity R41–R45, compatibility R46–R50, interface-integrity R51–R52) is preserved in the workflow output (`wutgsmcuq`) and the coverage matrix in the review chair verdict. Key once-MISSING items now addressed: R2 (routed store services claim), R9 (single-store atomicity), R13/R14 (pool bounding), R17 (sql.DB caps), R18/R48 (ceiling gate), R30 (leak fix), R43 (identity), R49 (branch reconciliation). Remaining PARTIAL→follow-up: R42/R51 (federation-over-UOW, interface segregation — S7).

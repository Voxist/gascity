# vp-e0hv — `gc status` 25-30s StatusView build: implementation plan

## Problem (verified)

`GET /v0/city/{name}/status` (the supervisor's full StatusView) takes **~28-30s on every
cold call**; the client times out, so `gc status` falls back to the local tmux probe → the
50 ms bound trips → `"runtime status probe timed out; using partial status"` + sessions
render dead.

Measured live (voxist-city, supervisor PID 1299):

| Request | Time |
|---|---|
| `…/status` (full) | 28-30 s, every call |
| `…/status?lite=true` | 0.81 s |
| `…/status` again within 2 s | 0.0015 s (time-bucket cache, TTL 2 s) |

The ~28 s lives entirely in the **full-only blocks** of `buildStatusBody`
(`internal/api/handler_status.go`): `statusWorkCounts`, `countSessions`, `cachedStoreHealth`.
Eliminated by measurement: `WalkSize` (0.03 s), bead scans (store is 51 beads), `countSessions`
(in-memory), agent loop / mail / named-sessions (run in `?lite` too). The cost is in **un-cancellable
sub-queries whose 1 s budget does not actually cancel** — `Store.List` takes no `context`
(comment, `handler_status.go:617-621`), so the 1 s timeout *abandons a goroutine* that keeps
running and holding a Dolt connection; and `storehealth.LastMaintenance` does an unbounded
`events.Provider.List` over a 1.0 G event backing. The 30 s `storeHealth` sub-cache is defeated
because each build (~28 s) ≈ the TTL.

## Fixes (this plan covers 1-4; #1 implemented here)

---

### Fix 1 — Warm, background-built StatusView (the cure) — IMPLEMENTED

**Goal:** the request path must never run a synchronous O(store) build. Serve the last
background-built body immediately (stale-while-revalidate); rebuild off the request path.

**Design (dedicated warm cache, lazy refresh — no long-lived goroutine):**

- `Server` gains dedicated warm-cache fields (mirrors the existing `storeHealth*` field
  pattern; the shared `responseCache` is unsuitable — its 2 s expiry + 256-entry eviction
  would drop the warm body):
  ```go
  statusWarmMu   sync.Mutex
  statusWarmFull *statusWarmEntry      // full body
  statusWarmLite *statusWarmEntry      // ?lite body
  statusBuildSF  singleflight.Group    // single-flights the (re)build per variant
  ```
- New `internal/api/status_warm.go`: `statusWarmEntry{body, builtAt}`, tunables
  (`statusWarmServeMaxAge = 5m`, `statusWarmRefreshAfter = 5s`, `statusWarmBuildTimeout = 60s`),
  and helpers `warmStatusBody` / `setWarmStatusBody` / `buildAndStoreStatus` /
  `refreshStatusBodyAsync`.
- `buildAndStoreStatus(lite)` is the single (re)build entry point, single-flighted per variant
  via `statusBuildSF.DoChan` so a burst of cold requests — or a cold request racing a refresh —
  shares ONE build. It builds with a dedicated 60 s ctx (not the 30 s `backgroundCtx`), stores
  the warm entry, and `storeResponse`s it so the ≤2 s exact-bucket fast path keeps working. Two
  hardening guards (added after self-review): the build closure `recover()`s so a panic in the
  agent/rig fan-out can't crash the supervisor via a background goroutine; and the caller
  `select`s the `DoChan` result against `statusWarmBuildTimeout` and `Forget`s the key on
  timeout, so a build wedged on an uncancellable read can't poison the singleflight key and
  hang every future request (it serves the last warm body instead).
- `refreshStatusBodyAsync(lite)` runs `buildAndStoreStatus` off the request path (fire-and-forget
  via `statusBuildAsyncHook`, overridable in tests).
- `humaHandleStatus` (non-blocking path) becomes:
  1. exact time-bucket cache hit → serve (unchanged ≤2 s burst path);
  2. warm entry within `statusWarmServeMaxAge` → **serve it now**, and if older than
     `statusWarmRefreshAfter` kick `refreshStatusBodyAsync` (don't wait);
  3. otherwise (cold start / long idle) → **one single-flighted synchronous build**, then serve
     and warm. Only this first build after a (re)start or >5 m idle is synchronous.
  Blocking/strict-freshness callers (`?index=&wait=`) keep their own synchronous build (they
  asked to wait on an event).

**Behavior change:** the very first `/status` after a (re)start or >5 m idle still pays the
~28 s build synchronously (single-flighted, so concurrent cold requests share it and a wedged
build is capped at `statusWarmBuildTimeout`, not infinite). Every subsequent request — the
common case on an actively-polled fleet — is served from the warm entry in <1 ms. (An earlier
draft of this plan proposed a `statusWarming503` cold-start fallback that returns immediately;
that was dropped in favor of the synchronous cold build to preserve the always-200 contract and
avoid reworking the handler's cache tests. There is no `statusWarming503` in the shipped code.)

**Tests:** unit tests for the warm-cache helpers (round-trip, warm-serve-without-rebuild,
aged-body triggers background refresh); the TTL-expiry cache test was rewritten to the warm
contract (`TestHandleStatusWarmCacheServesAcrossBucketExpiry`); existing `handler_status` /
`city_status` tests stay green. The two hardening guards added in the self-review
(panic `recover()`, `DoChan`+`Forget` wedge escape) each have a dedicated regression test
(`TestBuildAndStoreStatusRecoversFromBuildPanic`, `TestBuildAndStoreStatusEscapesWedgedBuild`,
added in the rework pass responding to PR #74's second review) that inject a
panicking/blocking `storeHealthComputer` and were verified to fail against the
pre-hardening commit (`8f3d1f5a86`) before confirming green against the fix.

**Risk:** low — additive, isolated to the status read path; the 503-fallback contract already
exists (`cacheLiveOr503`).

---

### Fix 2 — Make the 1 s budget real (plumb `context` through reads)

**Goal:** the per-store 1 s timeout must *cancel* the work, not abandon a goroutine that keeps
holding a Dolt connection (the pile-up that compounds latency under load).

**Steps:**
- Add `ListCtx(ctx, ListQuery)` (or extend `List`) to the `beads.Store` interface and thread
  `ctx` through `DoltliteReadStore.List` / `.Count` and `CachingStore` so a cancelled ctx aborts
  the SQL and releases the connection. Keep `List` as a ctx-less shim that calls `ListCtx(context.Background(), …)` for existing callers.
- Replace `statusListStoreWithTimeout` (goroutine + `time.After`, abandons) with a real
  `ListCtx(ctxWithTimeout, …)`.
- Thread `ctx` into `events.Provider.List` likewise (used by `LastMaintenance`).

**Risk:** medium — touches the `beads.Store` interface and every implementation; needs care to
keep the abandoning fallback for stores that still can't honor ctx. Land behind Fix 1 (which
removes the request-path urgency).

---

### Fix 3 — Fix the two unbounded blocks specifically

- **`storehealth.LastMaintenance`**: stop scanning the full event log. Either (a) query the
  event provider for the last-N of `StoreMaintenance{Done,Failed}` via an indexed/tailing read,
  or (b) have the maintenance runner record `lastAt/lastStatus` into a tiny dedicated state file
  that `computeStoreHealth` reads in O(1).
- **Work counts**: ensure the `ready` bucket (a derived status) doesn't force a full
  abandon-scan. Either compute work counts from the in-memory `CachingStore` snapshot
  unconditionally for the status overview (accept slight staleness — it's an overview), or make
  the doltlite counter answer `ready` via an indexed query. Pair with Fix 2 so any residual
  delegation is ctx-bounded.

**Risk:** low-medium, localized. Mostly subsumed by Fix 1 for the user-visible symptom; do it to
keep the background build itself fast and cheap.

---

### Fix 4 — Expose `/debug/pprof` on the supervisor (diagnosability) — ALREADY IMPLEMENTED, enable it

Black-box analysis could not split the final two candidates because the live supervisor exposed
no pprof. It turns out the code is **already present**: `api.StartPprof` (`internal/api/supervisor.go:248`)
registers `net/http/pprof` handlers on a **dedicated localhost-only mux** (`127.0.0.1:6060` by
default) and is called from the supervisor run path (`cmd/gc/cmd_supervisor.go:1265`) — gated on
`GC_PPROF=1`, off by default. The earlier 404 was because (a) `GC_PPROF` was unset on the live
supervisor and (b) pprof binds `:6060`, not the API port `:8372`.

So Fix 4 needs **no code** — just enable it: set `GC_PPROF=1` in the supervisor's launchd
environment (added during this deploy) so a future slow build is a one-line goroutine dump:
`curl 127.0.0.1:6060/debug/pprof/goroutine?debug=2`. (Note: the plist `EnvironmentVariables` are
regenerated by `gc` on import/upgrade, so for a durable default this should move into the plist
source asset / `zai-setenv`-style bootstrap rather than a manual plist edit.)

**Risk:** low — gated, localhost-only, off by default.

---

## Sequence

1. **Fix 1** (this PR) — kills the user-visible symptom with low risk.
2. **Fix 4** — cheap, unblocks precise diagnosis of the residual build cost.
3. **Fix 3** — make the background build itself fast (informed by #4's profile).
4. **Fix 2** — the deeper correctness fix (ctx-cancellable reads); largest blast radius, do last.

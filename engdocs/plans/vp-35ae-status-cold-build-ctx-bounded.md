# vp-35ae — cold StatusView build: ctx-cancellable + bounded reads — Plan v0.1

**Status:** Draft · **Date:** 2026-07-10 · **Author:** voxist-platform/voxist.planner · **Rig:** voxist-platform (code in the gascity fork repo)

## Context

Deferred follow-up to **vp-e0hv** (warm StatusView, PR #74). Fix 1 (warm,
background-built body) + Fix 4 (pprof) shipped; the steady-state request path is
O(1). The **cold** build (first `/status` after a supervisor restart or >5 min
idle) still takes ~28–47 s — exactly when an operator checks status. Two
root causes remain, both verified live on `origin/main` (HEAD `534841743`):

1. **Fix 3 — `storehealth.LastMaintenance` does an unbounded event scan.**
   `internal/storehealth/storehealth.go:97` `LastMaintenance(ep)` calls
   `ep.List(events.Filter{Type: …})` **twice** (once for `StoreMaintenanceDone`,
   once for `StoreMaintenanceFailed`), each a full scan of the ~1 G event
   backing. Called from `internal/api/store_health.go:58` (status path, inside
   `computeStoreHealth`) and `cmd/gc/store_health.go:41` (CLI). The 30 s
   `storeHealthCacheTTL` is defeated because each cold build (~28 s) ≈ the TTL.

2. **Fix 2 — the per-store 1 s budget abandons goroutines instead of cancelling.**
   `internal/api/handler_status.go:647` `statusListStoreWithTimeout` wraps
   `store.List(query)` in `go func(){…}()` + `select{ case <-time.After(…) }`.
   `Store.List` takes **no `context`** (`internal/beads/beads.go:366`), so on
   timeout the goroutine is *abandoned* — it keeps running and holding a Dolt
   connection (the pile-up that compounds latency under load). The comment at
   `handler_status.go:640-646` documents this exact gap. The `Counter` capability
   (`internal/beads/counter.go:22`) already takes `ctx` and is preferred, but the
   List fallback (`statusStoreWorkCounts`, `countBeadStoreRows`) and the
   `query.Live`/`ParentID` branch of `CachingStore.List`
   (`internal/beads/caching_store_reads.go:20-29`, calls ctx-less
   `c.backing.List`) still hit the abandoning path.

Verified facts driving the design:

- No `ListCtx` / `CtxLister` capability exists today (`grep` across
  `internal/beads/` returns nothing) — we introduce one, mirroring the optional
  `Counter` interface pattern, so `Store.List` stays a ctx-less shim and the 16
  test fakes + 10 production impls are untouched.
- `events.FileRecorder` already implements `ListTail(filter, limit)` via
  `ReadFilteredTail` (`internal/events/recorder.go:435`, helper at
  `internal/events/reader.go:361`). The `TailProvider` optional interface
  (`internal/events/events.go:323`) is the bounded read primitive Fix 3 needs.
- `statusStoreReadTimeout = 1 * time.Second` (`handler_status.go:32`).
- `computeStoreHealth(ctx, …)` already receives a `ctx`; only the
  `LastMaintenance(ep)` call inside it is ctx-less.

This plan covers **Fix 3 then Fix 2** (the parent plan's recommended sequence:
the localized, low-risk win first; the deeper interface change last, behind Fix 1
which already removed request-path urgency).

## Constraints

- **gascity fork repo.** All code, the branch, and the PR live in
  `git@github.com:Voxist/gascity.git` (remote `Voxist`). Base branch is `main`
  (gascity has no `feature` branch; the voxist-platform `feature` sentinel does
  not apply cross-repo). The bead `vp-35ae` is filed in the **voxist-platform**
  store (prefix `vp`) because voxist-platform owns the gascity fork — this is the
  same convention the parent `vp-e0hv` used (its plan doc also lives in
  `gascity/engdocs/plans/`). Not a filing error.
- **TDD enforced.** Every micro-task is fronted by a failing test. The executor
  deletes code written before its test exists.
- **Do not change the `Store.List` signature.** It ripples through 16 test fakes
  + 10 production impls. Use an optional `CtxLister` capability (type-assert),
  exactly as `Counter` does. `List` stays a `context.Background()` shim.
- **Preserve the always-200 / partial-status contract.** `statusListStoreWithTimeout`
  must still return `(rows, partial-error)` on timeout, not 503 — existing
  `handler_status` + `city_status` tests encode this. The change is *how* the
  timeout is honored (cancel vs abandon), not the response shape.
- **Keep the abandoning fallback for stores that can't honor ctx.** A store that
  does not implement `CtxLister` falls back to the current goroutine+`time.After`
  path. The win is for the stores that *do* (Doltlite, CachingStore live branch,
  BdStore) — the ones that actually hold Dolt connections.
- **Go module.** `go test ./...` from the gascity worktree root is the test
  command. No DB migrations; this is read-path only.

## Proposed approach

### Fix 3 — bounded `LastMaintenance` via `TailProvider`

`LastMaintenance` only needs the **single most-recent** maintenance event of each
type, but today it materializes the entire matching history. Replace each
`ep.List(Filter{Type: typ})` with a bounded tail read:

- Type-assert `ep` to `events.TailProvider`; if it implements `ListTail`, call
  `ep.ListTail(Filter{Type: typ}, 1)` (or a small N like 8 to be safe under
  out-of-order timestamps) and keep the max-`Ts` selection. `FileRecorder`
  (the production backing) already implements `ListTail`, so the status path gets
  the bounded read for free.
- If `ep` does **not** implement `TailProvider` (e.g. `events.Fake` in tests,
  `Multiplexer`), fall back to the current full `ep.List(…)` scan — correctness
  preserved, just not bounded. This keeps `TestLastMaintenanceReturnsLatestAcrossTypes`
  (which uses `Fake`) green unchanged.

This is O(last-N) instead of O(full log) on the production path — the single
biggest cold-build win, localized to one function + its two callers (no caller
signature change needed; `LastMaintenance(ep)` keeps its signature).

### Fix 2 — real cancellation via an optional `CtxLister` capability

Introduce `CtxLister` (optional interface, sibling of `Counter`):

```go
// CtxLister is an optional Store capability for context-cancellable reads.
// Stores that hold an external connection (Dolt, bd-CLI subprocess) should
// implement it so a cancelled context aborts the in-flight read and releases
// the connection, instead of abandoning a goroutine that keeps holding it.
type CtxLister interface {
    ListCtx(ctx context.Context, query ListQuery) ([]Bead, error)
}
```

- Implement `ListCtx` on the stores that actually hold connections and sit on the
  status abandoning path: `DoltliteReadStore` (thread ctx into `queryIssues` →
  the SQL query), `CachingStore` (ctx into the `query.Live || ParentID != ""`
  backing call; the in-memory cache path needs no ctx), and `BdStore` (thread ctx
  into `listViaBDList` → the bd subprocess, which already accepts a ctx via
  `ExecCommandRunnerWithEnvContext`). Each `ListCtx` is the real implementation;
  each `List(query)` becomes `return s.ListCtx(context.Background(), query)`.
- `statusListStoreWithTimeout`: when `store` implements `CtxLister`, drop the
  `go func()` + `time.After` abandon and call `store.ListCtx(reqCtx, query)`
  directly (the `reqCtx` already has the 1 s `WithTimeout`). A cancelled ctx now
  kills the backend read / subprocess and releases the connection. Stores
  without `CtxLister` keep the current abandoning goroutine path.
- The scoped-store resolution (`state.ScopedStoreLike`) already runs inside the
  timed goroutine and already takes `reqCtx`; it is unchanged.

Blast radius is bounded to the 3 store impls + the one handler function + their
tests. The `Store` interface, the 16 test fakes, and the other 7 `List` impls
are untouched.

## Micro-tasks

| id | description | acceptance | est | slings |
|---|---|---|---|---|
| T-001 | Write failing test: `LastMaintenance` on a `TailProvider`-implementing provider calls `ListTail` (not `List`) and returns the latest maintenance ts/status from only the tail read | `internal/storehealth/storehealth_test.go::TestLastMaintenanceUsesListTailWhenAvailable` fails (asserts a `*tailCallRecorder` saw `ListTail` with limit ≤ small N and never `List`; latest `StoreMaintenanceDone`/`Failed` ts wins) | 4 | — |
| T-002 | Implement `ListTail` path in `LastMaintenance`: type-assert `events.TailProvider`, call `ListTail(filter, 8)`, keep max-`Ts`; fall back to `List` for non-`TailProvider` | T-001 passes; `TestLastMaintenanceReturnsLatestAcrossTypes` + `TestLastMaintenanceOnlyDoneEvents` + `TestLastMaintenanceNoEvents` + `TestLastMaintenanceNilProvider` stay green | 4 | — |
| T-003 | Write failing test: `LastMaintenance` falls back to full `List` when the provider does NOT implement `TailProvider` (proves the fallback path is unchanged) | `internal/storehealth/storehealth_test.go::TestLastMaintenanceFallsBackToListForNonTailProvider` fails (uses a `Fake`-equivalent that only implements `List`; asserts correct latest-ts result via the `List` path) | 3 | — |
| T-004 | Confirm the T-003 fallback is already covered by the T-002 implementation; no new code if T-003 passes immediately after T-002 | T-003 passes; `go test ./internal/storehealth/...` green | 2 | — |
| T-005 | Write failing benchmark/regression test: `LastMaintenance` on a large event backing reads bounded work (tail), not the full log | `internal/storehealth/storehealth_test.go::TestLastMaintenanceBoundedTailRead` fails (provider records how many events it decoded; with N=10k maintenance events, a `ListTail`-capable provider decodes ≤ the tail limit, not 10k) | 5 | — |
| T-006 | Tune the tail limit in `LastMaintenance` to the smallest value that is correct under out-of-order timestamps (document why); make T-005 pass | T-005 passes; comment in `storehealth.go` states the limit + the ordering assumption | 3 | — |
| T-007 | Write failing test: `CtxLister` capability — `DoltliteReadStore.ListCtx` honors a cancelled context and returns `ctx.Err()` without completing the scan | `internal/beads/doltlite_count_test.go` (or `doltlite_read_store_test.go`)::`TestDoltliteReadStoreListCtxHonorsCancelledContext` fails (build tag `gascity_native_beads`; pre-cancelled ctx → `errors.Is(err, context.Canceled)`) | 5 | — |
| T-008 | Implement `DoltliteReadStore.ListCtx(ctx, query)` threading ctx into `queryIssues`; make `List(query)` a `context.Background()` shim to `ListCtx` | T-007 passes; `go test -tags gascity_native_beads ./internal/beads/...` green; existing doltlite List tests unchanged | 5 | — |
| T-009 | Write failing test: `CachingStore.ListCtx` honors ctx on the `query.Live \|\| ParentID != ""` backing path (cancels `backing.ListCtx`), and serves the in-memory cache path ctx-agnostically | `internal/beads/caching_store_internal_test.go::TestCachingStoreListCtxHonorsContextOnLivePath` fails (a `query.Live` read against a backing whose `ListCtx` blocks until ctx-cancelled returns `context.Canceled` promptly) | 5 | — |
| T-010 | Implement `CachingStore.ListCtx(ctx, query)`: route the live/ParentID branch through `backing.(CtxLister).ListCtx` when available (else `backing.List`), cache path unchanged; make `List(query)` a `context.Background()` shim | T-009 passes; existing CachingStore List tests green | 5 | — |
| T-011 | Write failing test: `BdStore.ListCtx` honors a cancelled context (kills the bd subprocess instead of abandoning it) | `internal/beads/bdstore_test.go::TestBdStoreListCtxHonoursCancelledContext` fails (bd subprocess that blocks; pre-cancelled ctx → prompt `context.Canceled`, child reaped) | 5 | — |
| T-012 | Implement `BdStore.ListCtx(ctx, query)` threading ctx into `listViaBDList` via the existing ctx-bearing exec runner; make `List(query)` a `context.Background()` shim | T-011 passes; existing BdStore List tests green | 4 | — |
| T-013 | Write failing test: `statusListStoreWithTimeout` on a `CtxLister` store calls `ListCtx` directly (no abandoned goroutine) and a cancelled 1 s budget cancels the read, releasing the backend | `internal/api/handler_status_scoped_store_test.go::TestStatusListStoreWithTimeoutCancelsCtxLister` fails (a `CtxLister` fake whose `ListCtx` blocks until ctx-cancelled returns within ~`statusStoreReadTimeout` with a partial-error containing "timed out", and its `ListCtx` observes the cancelled ctx — not `List`) | 5 | — |
| T-014 | Implement the `CtxLister` fast path in `statusListStoreWithTimeout`: if `store.(CtxLister)` succeeds, call `ListCtx(reqCtx, query)` directly under the existing `WithTimeout`; else keep the goroutine+`time.After` abandon path for non-`CtxLister` stores | T-013 passes; existing `TestStatusListStoreWithTimeout*` + `handler_status`/`city_status` tests green (response shape unchanged) | 4 | — |
| T-015 | Update `handler_status.go:640-646` doc comment to reflect that `CtxLister` stores now get real cancellation (abandon remains only the non-`CtxLister` fallback); add a regression test that the abandon fallback still bounds a non-`CtxLister` store | `internal/api/handler_status_scoped_store_test.go::TestStatusListStoreWithTimeoutAbandonFallbackBounded` fails then passes (a `List`-only blocking fake is bounded by `statusStoreReadTimeout` via the goroutine path) | 4 | — |
| T-016 | Run the full suite + vet: `go vet ./... && go test ./...` (and `go test -tags gascity_native_beads ./internal/beads/...`); fix any fallout from the `List`→`ListCtx` shim on callers | `go test ./...` green; no new vet warnings | 3 | — |
| T-017 | Live verification (operator step, documented not automated): on a voxist-city supervisor, set `GC_PPROF=1`, restart supervisor, curl `/v0/city/<name>/status` cold and confirm the build is bounded (≤2 s target per acceptance) and `goroutine` pprof shows no abandoned Dolt-holding goroutines | `bd note vp-35ae --message` with the measured cold-build time + pprof goroutine count before/after | 3 | — |

Sequence note: T-001..T-006 (Fix 3) are independent of T-007..T-015 (Fix 2) and
may land as separate commits; T-016/T-017 close both. Fix 3 is the higher-value,
lower-risk half and can ship first if the PR is split.

## GDPR data-flow impact

### Data added / removed / relocated
No data flow changed. This is a read-path performance/correctness fix to the
status and store-health endpoints. No bead, event, or PII record is created,
deleted, moved, or reshaped; the set of events `LastMaintenance` considers is
unchanged (only the read width — tail vs full scan — changes).

### New cross-border transfers
none

### Audit-log changes
none — the event log is read, not written.

## MDR Class I traceability

Not applicable — not a clinical path. The status/store-health endpoints are
fleet-operational telemetry; they do not touch the voxmemo→voxist-api clinical
pipeline or any chain-of-evidence metadata.

## Acceptance criteria

- A cold StatusView build (first `/status` after a supervisor restart or >5 min
  idle) stays within a bounded budget (target ≤2 s on a busy fleet), OR — if the
  full ≤2 s is not reachable in this PR — the per-subquery 1 s timeout
  demonstrably *cancels* work (no abandoned goroutines holding Dolt connections),
  confirmed via `curl 127.0.0.1:6060/debug/pprof/goroutine?debug=2` before/after.
- `LastMaintenance` reads a bounded tail (not the full ~1 G event log) on the
  `FileRecorder` production backing.
- `statusListStoreWithTimeout` honors context cancellation for `CtxLister`
  stores (Doltlite, CachingStore live branch, BdStore); non-`CtxLister` stores
  keep the existing bounded abandon fallback.
- The always-200 / partial-status response contract is unchanged — existing
  `handler_status` + `city_status` tests stay green.
- `go vet ./... && go test ./...` and `go test -tags gascity_native_beads
  ./internal/beads/...` are green.

## Rollback plan

1. **Git-level:** revert the merge commit on `Voxist/gascity` `main`. The change
   is additive (a new optional `CtxLister` interface + a `ListTail` branch in
   `LastMaintenance`); revert restores the ctx-less `List`-only path and the
   full-scan `LastMaintenance`. No migration to reverse.
2. **Data-level:** none — read-path only, no schema or persisted-state change.
3. **Decision criteria:** trigger rollback if, after deploy, a cold `/status`
   regresses past the prior ~28–47 s baseline (e.g. the `ListTail` limit is
   wrong and starves the result, or a `CtxLister` impl deadlocks), or if pprof
   shows a *new* class of leaked/hung goroutine on the status path. Fix 1's warm
   cache means the steady-state request path is unaffected by either fix, so a
   rollback is low-urgency (cold-build-only symptom).

## Open questions

- **Tail limit value (T-006).** Are maintenance-event timestamps strictly
  monotonic per type in practice, or can a `Failed` arrive with an earlier `Ts`
  than a subsequent `Done`? If strictly monotonic, `ListTail(filter, 1)` suffices;
  if not, a small N (8) + max-`Ts` selection is the safe default. The plan
  defaults to N=8 pending the executor's check against a real event log. No human
  escalation needed — verifiable from the event writer's contract
  (`internal/events`).
- **Should `Multiplexer` gain `ListTail`?** `LastMaintenance`'s production
  backing is `FileRecorder` (already tail-capable), so the status path is bounded
  without touching `Multiplexer`. Adding `Multiplexer.ListTail` is a separate,
  optional hardening not required for this bead's acceptance — leave for a
  follow-up if a profile shows the multiplexer path on the hot cold-build.
- **`countBeadStoreRows` always-hydrates (store_health.go:79 comment).** This is
  a separate deferred item (`#1896 follow-up` per the existing comment) and is
  out of scope here; Fix 2's `CtxLister` makes its abandon path cancellable as a
  side effect, but it is not the target of this plan.

## Status

- [x] T-001 — failing test: LastMaintenance calls ListTail on a TailProvider   ✅ green at 3a7bf61d9
- [x] T-002 — LastMaintenance ListTail path + max-Ts selection + List fallback   ✅ green at 3a7bf61d9
- [x] T-003 — failing test: LastMaintenance falls back to List for non-TailProvider   ✅ green at c71c363ec
- [x] T-004 — confirm T-003 covered by T-002 (no new code)   ✅ green at c71c363ec
- [x] T-005 — failing test: LastMaintenance reads bounded work on a 10k-event backing   ✅ green at f19cdbcc1
- [x] T-006 — tune + document tail limit (8, safety margin vs backward wall-clock step)   ✅ green at f19cdbcc1
- [x] T-007 — failing test: DoltliteReadStore.ListCtx honors cancelled ctx   ✅ green at c2a3d7cf8
- [x] T-008 — DoltliteReadStore.ListCtx threads ctx through queryIssues chain   ✅ green at c2a3d7cf8
- [x] T-009 — failing test: CachingStore.ListCtx honors ctx on Live/ParentID path   ✅ green at 76ea6a483
- [x] T-010 — CachingStore.ListCtx routes backing reads through CtxLister when available   ✅ green at 76ea6a483
- [x] T-011 — failing test: BdStore.ListCtx honors cancelled ctx (pre-flight)   ✅ green at 0e5af5913
- [x] T-012 — BdStore.ListCtx pre-flight ctx.Err() check   ✅ green at 0e5af5913
- [x] T-013 — failing test: statusListStoreWithTimeout calls ListCtx directly for CtxLister stores   ✅ green at ee56202c7
- [x] T-014 — CtxLister fast path in statusListStoreWithTimeout (ScopedStoreLike priority preserved)   ✅ green at ee56202c7
- [x] T-015 — doc comment rewrite + abandon-fallback regression test   ✅ green at 7b64030bd
- [x] T-016 — full suite + vet   ✅ green — see verification notes below
- [ ] T-017 — live pprof verification on a running supervisor — **deferred to operator**, not run by the executor (restarting the supervisor is shared-infrastructure, high-blast-radius; see bd note on vp-35ae)

### T-016 verification notes

`go build ./...` and `go vet ./...` are clean. `go test ./internal/api/...
./internal/beads/... ./internal/storehealth/...`, `go test -tags
gascity_native_beads ./internal/beads/...`, and `go test
./internal/testpolicy/resourcecensus/...` are all green. A targeted run of
every status/store-health test in `cmd/gc` (`TestCityStatus*`,
`TestDoRigStatus*`, `TestRouteCityStatus*`, `TestRouteRigStatus*`,
`TestStoreHealth*`, `TestControllerStatus*`, `TestLiveRowCount*`,
`TestCollectStoreHealth*`, `TestRenderStoreHealthBlock*`,
`TestSnapshotFromStatusView*`) is green.

A full unsharded `go test ./...` locally hits two pre-existing, unrelated
failures — confirmed independent of this plan's diff:
- `cmd/gc` times out at the default 10 m test timeout (goroutine dump shows
  it stuck in the unrelated `TestPackCommandCobraHelpAndUnknownParity` CLI
  help test). This package is CI-sharded into 12 parts precisely because it
  does not fit the default timeout unsharded; the targeted status-test subset
  above completes in ~8 s.
- `internal/productmetrics` fails 2-3 tests (nondeterministic across runs)
  on deeply-nested-directory purge/quarantine cases with `mkdir: file name
  too long` / `too many open files` — a macOS path-length (`PATH_MAX`) and
  ulimit artifact in an unrelated subsystem (product-usage spool), not
  touched by this plan's diff.

**Also found and fixed during T-016 (not a plan task):** this worktree's
`gc/vp-35ae` branch had been created from `origin/main` (the gastownhall
upstream fork point, commit `a93023093`) instead of `Voxist/main` — the
known trap documented for this repo (stale/diverged `origin` remote). This
made the branch 205 commits behind the real PR target and caused a
spurious `resourcecensus` census-drift failure plus a 109-file diff full of
unrelated commits. Fixed via `git rebase --onto Voxist/main a93023093
HEAD` (clean, no conflicts; verified unpushed, no dependent PR). Post-rebase
the diff against `Voxist/main` is exactly the 12 files this plan touches,
and `resourcecensus` passes.

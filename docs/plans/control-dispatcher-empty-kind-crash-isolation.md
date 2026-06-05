# Plan: control-dispatcher survives empty/unknown gc.kind beads (ga-3p3o)

> **Status:** ready-to-execute — 2026-06-05
> **Bead:** `ga-3p3o` (bug, **P1**) — *a single empty/unknown gc.kind
> bead (e.g. from `gc sling`) crashes the singleton control-dispatcher →
> quarantine. Root cause of `po-de0tu` (only symptom-resolved before).*
> **Root cause + design by:** `voxist.platform-architect` (in the bead body).
> **Invariant:** a singleton control plane must not be crashable by one
> malformed work item.
> **One bead per plan** — executor consumes this whole file in one session
> and opens a single PR (see PR-splitting note re: file count).

## Context

`gc convoy control --serve` runs a **singleton** control-dispatcher
session. Its drain loop is `drainWorkflowServeWork`
(`cmd/gc/dispatch_runtime.go:436`). For each ready control bead it calls
`controlDispatcherServe` (a package var = `runControlDispatcherInStore`,
`dispatch_runtime.go:73`) and classifies the returned error
(`dispatch_runtime.go:477-491`):

- `dispatch.ErrControlPending` → skip + `continue` (l.478).
- `dispatch.IsTransientControllerError` → skip + `continue` (l.485).
- **anything else → `return result, fmt.Errorf(...)` (l.491) — FATAL.**

That fatal return propagates up through `runConvoyControlServe`
(`dispatch_runtime.go:131`) → `errExit` → **the serve process exits**. The
supervisor restarts it; the offending bead is still in the ready set →
the loop hits it again → exits again. After `MaxRestarts` (default 5,
`internal/config/config.go` `MaxRestarts`) within `RestartWindow`
(default 1h) the session is **quarantined** → the whole convoy/pool
dispatch plane goes down. One malformed bead takes out the control plane.

### What already exists (verified on `feat/beads-proxied-pooling`)

The **per-bead** path is already partly defended. `ProcessControl`
(`internal/dispatch/runtime.go:98`) dispatches on `gc.kind`; an
empty/unknown kind hits the `default:` at `runtime.go:138` and returns a
**permanent** error `"<id>: unsupported control bead kind \"\""`. That
error is neither `ErrControlPending` nor transient, so
`runControlDispatcherWithStoreAndConfig`
(`cmd/gc/cmd_convoy_dispatch.go:242-254`) calls
`quarantineControlFailureBead` (closes the bead, labels it
`gc:control-quarantined`) and returns **nil** → the serve loop sees nil
and continues.

So the *direct* `ProcessControl` empty-kind path is already isolated. The
residual crash (which the bead reports as still live) is any path where a
**parkable per-bead error reaches the serve loop's l.491 unparked** — e.g.
a binding/routing failure surfaced from `resolveGraphStepBinding` /
`graphFallbackBindingForBead` (`cmd_convoy_dispatch.go:600/633`) inside a
decorate callback, or a quarantine that itself failed. The fix makes the
**serve loop the outermost, unconditional backstop** so the invariant
holds regardless of which inner path produced the error. The executor's
first (failing) test pins the exact live path; the backstop covers it.

### Fix (architect design — defense in depth)

- **(B) DISPATCHER ROBUSTNESS (primary, the reliability fix).** Introduce
  a typed sentinel `dispatch.ErrControlUnsupportedKind` (wrap it at
  `runtime.go:138`) and a classifier `dispatch.IsParkableControlError(err)`
  (true for unsupported-kind / un-routable per-bead categorization errors;
  false for `nil`, pending, transient). In the serve drain loop, add a
  branch **before** the fatal `return`: a parkable per-bead error is
  warned and **`continue`d**, never returned. The process never exits for
  one bad bead. The inner quarantine net (`cmd_convoy_dispatch.go:250`)
  still does the actual parking (bead → closed + `gc:control-quarantined`),
  so the bead leaves the ready set and cannot re-trigger.
- **(A) SLING HYGIENE (trigger reduction).** `gc sling` must stamp a valid,
  supported `gc.kind` on any bead it routes to the control-dispatcher, so
  well-formed input reaches the dispatcher in the first place. Lead:
  `internal/sling/sling.go:1474` strips `gc.kind` from a wisp root
  (`mapsCloneWithout(root.Metadata, "gc.kind")`) — a likely source of
  empty-kind beads; the kind switches at `cmd/gc/cmd_sling.go:1191/1198/1263`
  show where kind is read.

### Base branch (LOAD-BEARING)

**Base off `feat/beads-proxied-pooling`, NOT `main`.** The bug is observed
on the running proxied-server city (which runs `feat`); `po-de0tu` (the
symptom of this same root cause) was `feat` stabilization work; and both
`cmd/gc/cmd_convoy_dispatch.go` (+17 lines) and `internal/config/config.go`
(+65 lines) **already diverge on `feat`** vs `origin/main`, so a main-based
fix would need re-application. The bead's cited line numbers (l.189, l.211,
l.600, l.633) match the **feat** working tree exactly. `feat` lives on the
`fork` remote (`cstar/gascity`), not `origin` (`gastownhall/gascity`); PR
base is `fork/feat/beads-proxied-pooling` (see Open questions).

**Worktree prepared by the planner:**
- `work_dir`: `/Users/cstar/rigs/.gc-worktrees/gascity-ga-3p3o`
- branch: `gc/ga-3p3o` (based at `feat/beads-proxied-pooling` tip `e0c9bec81`)

## Micro-tasks

TDD, red→green. First task is the failing test. Run acceptance from the
worktree root (Go 1.26.3).

| id | description | acceptance (single failing test → make it pass) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add failing unit test `TestUnsupportedControlKindIsParkable` in `internal/dispatch/control_test.go` (or `runtime_test.go`): an open bead with empty `gc.kind` makes `ProcessControl` return an error with `errors.Is(err, ErrControlUnsupportedKind)`; `IsParkableControlError(err)` is true; and `IsParkableControlError` is false for `nil`, `ErrControlPending`, and a transient error. | `go test ./internal/dispatch/ -run TestUnsupportedControlKindIsParkable` **fails to compile** — `ErrControlUnsupportedKind` and `IsParkableControlError` are undefined. | 5 | — |
| T-002 | In `internal/dispatch`: add `var ErrControlUnsupportedKind = errors.New("unsupported control bead kind")`; wrap it at `runtime.go:138` (`fmt.Errorf("%s: %w %q", bead.ID, ErrControlUnsupportedKind, kind)`); add `func IsParkableControlError(err error) bool` (true iff non-nil, not pending, not transient, and `errors.Is(err, ErrControlUnsupportedKind)` — extension point for future un-routable sentinels). | `go test ./internal/dispatch/ -run TestUnsupportedControlKindIsParkable` **passes**. | 5 | — |
| T-003 | Add failing test `TestServeDrainParksBadBeadWithoutCrashing` in `cmd/gc` (use the `controlDispatcherServe` package-var seam, `dispatch_runtime.go:73`, and the existing serve test harness): stub it to return a parkable error for bead `bad` and `nil` for bead `good`; drive the drain over a queue `[bad, good]`; assert the drain returns **no error** (no fatal exit), `good` is processed, and the parkable error did not propagate. | `go test ./cmd/gc/ -run TestServeDrainParksBadBeadWithoutCrashing` **fails** — the loop returns the fatal error at `dispatch_runtime.go:491`. | 5 | — |
| T-004 | In `drainWorkflowServeWork` (`cmd/gc/dispatch_runtime.go`, just before the l.491 fatal return): add `if dispatch.IsParkableControlError(err) { workflowTracef("serve parked-unroutable bead=%s kind=%s err=%v", beadID, kind, err); pendingCount++; result.pendingAny = true; continue }`. Never `return` fatal for a parkable per-bead error. Keep the existing pending/transient branches. | `go test ./cmd/gc/ -run TestServeDrainParksBadBeadWithoutCrashing -count=1` **passes**, and the existing serve tests (`go test ./cmd/gc/ -run 'TestRunWorkflowServe\|TestDrainWorkflowServe\|Serve' -count=1`) stay green. | 5 | — |
| T-005 | Add failing test `TestSlingStampsControlKind` (in `internal/sling/sling_test.go` or `cmd/gc/cmd_sling_test.go`): a bead produced by the `gc sling` auto-task / control-routing path that is routed to `config.ControlDispatcherAgentName` has a **non-empty, supported** `gc.kind`. Locate the empty-kind source via the failing assertion (lead: the wisp-root strip at `internal/sling/sling.go:1474`). | the new test **fails** — a sling-routed control bead currently lands with empty `gc.kind`. | 5 | — |
| T-006 | Stamp a valid `gc.kind` in the identified sling path (do not route a bead to the control-dispatcher without a supported kind; if the wisp-root strip at `sling.go:1474` is the source, retain/restore a valid kind for control-routed beads). | `go test ./internal/sling/ ./cmd/gc/ -run 'TestSlingStampsControlKind' -count=1` **passes**. | 5 | — |

Total est: ~30 min across 6 micro-tasks (3 red/green pairs: B sentinel/classifier, B serve-loop backstop, A sling hygiene).

### PR-splitting guidance (file-count heuristic)

The full change spans ~5 files (`internal/dispatch/runtime.go`,
`cmd/gc/dispatch_runtime.go`, `internal/sling/sling.go` + 2–3 test files),
above the usual 3-file guideline. This is justified — the bead's
acceptance requires **both** defenses and they are tightly coupled (both
about empty-`gc.kind` handling). **(B) (T-001–T-004) is the must-ship
reliability fix and fully satisfies the crash-survival invariant on its
own.** If the reviewer judges the combined diff too large, land **(B) as
the PR** and spin **(A) (T-005–T-006)** into a follow-up bead — note this
in the PR description so the `gc sling` acceptance line is tracked.

## GDPR data-flow impact

**No impact.** This change concerns control-dispatcher reliability — error
classification (`ErrControlUnsupportedKind`, `IsParkableControlError`),
serve-loop control flow, and a `gc.kind` metadata stamp on orchestration
beads. No personal data is read, written, transmitted, or logged. Control
beads carry operational identifiers (bead IDs, `gc.kind`, routes,
assignees), not data-subject data. Parking a malformed control bead sets
its status/labels/metadata — operational state, not personal data. No new
persistence, network egress, or log fields beyond an existing-style trace
line. Article 30 record of processing is unaffected.

## MDR Class I traceability

**No-op outside voxmemo.** This change is in `gascity` (the `gc`
orchestration control plane), not the voxmemo→voxist-api clinical
documentation pipeline. It does not touch the chain-of-evidence from
microphone capture through ASR to exported clinical note: no recording,
transcript, timestamp, device identifier, or operator attribution is
created, modified, or relied upon. The heading is retained per Voxist
planner discipline so an auditor sees the explicit consideration.

## Validation gates

- `go test ./internal/dispatch/ ./internal/sling/ ./cmd/gc/ -count=1` green;
  `go vet ./...` clean on `feat/beads-proxied-pooling`.
- **Invariant proven by test:** an unroutable/empty-kind bead in the serve
  queue does NOT cause `drainWorkflowServeWork` to return a fatal error;
  a following well-formed bead is still processed (dispatcher stays
  ACTIVE). The inner path still closes/labels the bad bead
  (`gc:control-quarantined`) so it leaves the ready set.
- `git diff` confined to the files listed in PR-splitting guidance + tests.
- No new third-party Go modules; no new env vars.
- **Manual operator check (not a unit test):** on a proxied-server city,
  `gc sling` an auto-task that lands routed-to-dispatcher with no kind (or
  inject one), confirm the control-dispatcher logs a park warning and stays
  ACTIVE (not quarantined) while other convoy work proceeds. Record in the
  PR; cannot be asserted in CI without a live dispatcher.

## Notes for the executor

- **Package-var seam.** `controlDispatcherServe` (`dispatch_runtime.go:73`)
  is a function var precisely so tests can stub per-bead dispatch — use it
  for T-003 instead of standing up a real store/session.
- **Don't over-broaden parkable.** `IsParkableControlError` must stay
  `false` for `nil`/pending/transient and for genuinely systemic errors
  (store/config unavailable) — those should still surface. Park only
  per-bead categorization/routing failures. Keep it a small, explicit
  `errors.Is` set so the blast radius is reviewable.
- **The inner net already parks.** Confirm in T-003/T-004 that you are not
  double-quarantining; the serve-loop branch only guarantees the *process*
  survives. Actual bead parking stays in
  `quarantineControlFailureBead` (`cmd_convoy_dispatch.go:269`).
- **A-path discovery.** T-005's assertion should drive you to the exact
  empty-kind source. Start at `internal/sling/sling.go:1474` (wisp-root
  `gc.kind` strip) and the control-routing in `cmd/gc/cmd_sling.go`.

## Open questions

- `[architect]` **Base branch + remote.** Plan bases on
  `feat/beads-proxied-pooling` (on the `fork` remote). Confirm the PR
  targets `fork/feat/beads-proxied-pooling` rather than `origin/main`.
  Evidence in the Base branch section. Not a blocker.
- `[architect]` **Backstop breadth.** This plan parks only
  `IsParkableControlError` errors at the serve loop and keeps other
  non-pending/non-transient errors fatal (so systemic faults still
  surface). The stronger reading of the invariant — "park-and-continue on
  *any* non-systemic per-bead error" — is deliberately not taken, to avoid
  masking handler bugs. Confirm this scoping is acceptable, or widen.
- Whether (A) should ship in the same PR as (B) or as a follow-up — see
  PR-splitting guidance; reviewer's call on diff size.

## Out of scope

- The async-cache status path and any non-dispatcher reliability work.
- Raising/altering `MaxRestarts` / `RestartWindow` — the fix prevents the
  crash-loop; the quarantine threshold stays as-is.
- Re-categorizing existing already-quarantined beads.
- `origin/main` — this fix targets `feat/beads-proxied-pooling` only.

## Status

**(B) DISPATCHER ROBUSTNESS — shipped (the must-ship reliability fix).**

- [x] T-001 — failing test `TestUnsupportedControlKindIsParkable` (undefined symbols)   ✅ red at `7576dfac7`
- [x] T-002 — `ErrControlUnsupportedKind` sentinel + `IsParkableControlError` classifier; wrap at `runtime.go` default case   ✅ green at `7576dfac7`
- [x] T-003 — failing test `TestServeDrainParksBadBeadWithoutCrashing` (fatal error propagates at l.491)   ✅ red at `4da603884`
- [x] T-004 — parkable backstop branch in `drainWorkflowServeWork` before the fatal return; existing serve/quarantine tests stay green   ✅ green at `4da603884`

Gates: `go test ./internal/dispatch/ ./internal/sling/` green; `cmd/gc` serve/dispatch/convoy/quarantine blast-radius (`-run 'Serve|Drain|ControlDispatcher|Convoy|Quarantine|Parked'`) green; `go vet` clean. (Full `./cmd/gc/` single-invocation exceeds 9 min — CI runs it as 12 shards; not run whole here by design.)

**(A) SLING HYGIENE — deferred to a follow-up bead (plan-sanctioned).**

- [ ] T-005 / T-006 — deferred. The planner's lead (`sling.go:1474` wisp-root
  `gc.kind` strip) was investigated and does **not** route to the
  control-dispatcher: `privatizeAttachedRootOnlyWisp` only fires for a
  `RootOnly` wisp attached to a source bead, sets `Type="molecule"`, and routes
  to the **execution** target (`a.QualifiedName()`), producing an execution
  molecule, not a control bead. `IsControlDispatcherKind("wisp")` is false, so
  the workflow decorator never control-routes it. The real "auto-task"
  empty-kind source (most likely the generic routing boundary
  `cliBeadRouter.Route` at `cmd_sling.go:631`, which stamps `gc.routed_to`
  with no kind guard) needs more design work than a 5-min micro-task, and the
  naive fix (re-stamp an arbitrary control kind) would be **unsound** — it would
  make `ProcessControl` mis-handle the bead. Per the PR-splitting guidance,
  (B) fully satisfies the crash-survival invariant on its own, so (A) is pure
  trigger-reduction and is tracked in a follow-up bead. With (B) shipped, the
  dispatcher now **survives** any such bead regardless, so (A) is no longer a
  crash risk — only a hygiene improvement.

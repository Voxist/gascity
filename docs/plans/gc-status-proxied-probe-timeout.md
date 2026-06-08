# Plan: gc status probe timeout proxied-server-aware (ga-3bwc)

> **Status:** ready-to-execute — 2026-06-05
> **Bead:** `ga-3bwc` (bug, P2) — *gc status probe 50ms timeout too
> tight for proxied-server → 'runtime status probe timed out' on every
> gc status.*
> **Root cause + design by:** `voxist.platform-architect` (in the bead body).
> **One bead per plan** — the executor consumes this whole file in one
> session and opens a single PR. No child beads.

## Context

`cmd/gc/status_provider.go:14` hard-codes
`statusProviderCallTimeout = 50 * time.Millisecond`. `boundedStatusCall`
(l.32–47) runs each underlying `runtime.Provider` status method in a
goroutine with that deadline; on timeout it fires
`statusProviderTimeoutWarning` once (`warnOnce`) — printing
`gc status: runtime status probe timed out; using partial status` to
stderr — and returns a fallback value.

Under the **proxied-server** bd runtime, the status call traverses the
bd-subprocess / dbproxy round-trip plus per-session/rig probing and
**routinely exceeds 50 ms**, so the warning fires on essentially every
`gc status` and the command degrades to partial status. 50 ms was right
for embedded/local Dolt; it is too tight for proxied-server. The symptom
is cosmetic (status is still usable via fallback) but noisy and degrades
status completeness. It is the residual of the runtime-stabilization work
(port-pin `po-50xd7` + controller un-wedge `po-de0tu`, both done).

### Fix approach (design option A, made proxied-aware)

Make the bounded-call deadline **per-provider** and **proxied-aware**
instead of a single global constant:

- Keep the embedded/default deadline at `50ms` — embedded calls return in
  microseconds and never approach it, so **embedded latency is unchanged**
  and the tight fallback bound is preserved.
- When `BeadsConfig.ProxiedEnabled()` is true, use a larger but still
  **bounded** deadline (`statusProviderProxiedCallTimeout = 1 * time.Second`;
  design's acceptable range is 500 ms–1 s). `gc status` therefore returns
  within ~1 s worst case even if a probe genuinely wedges — it never hangs.

The two construction sites that build the status provider
(`cmd/gc/providers.go:185 newStatusSessionProviderForCity` and
`:189 newStatusSessionProviderForCityWithSnapshot`) **already carry
`cfg *config.City`**, so they can select the deadline from
`cfg.Beads.ProxiedEnabled()` with no new plumbing. The established access
pattern is `cfg.Beads.ProxiedEnabled()` / `cfg.Beads.ProxyPoolSizeOrDefault()`
(see `cmd/gc/doctor_beads_proxied.go:31`, `cmd/gc/beads_proxied_overlay.go:82,140`).

This is a per-provider deadline field rather than mutating the package
global, so it is race-free and unit-testable, and the existing zero-arg
`newBoundedStatusProvider(base)` is preserved (defaults to the 50 ms
global) so current callers and tests are untouched.

### Base branch (LOAD-BEARING — read before branching)

**Base off `feat/beads-proxied-pooling`, NOT `main`.**

- The bug only manifests under the proxied-server runtime, which exists
  **only** on `feat/beads-proxied-pooling`.
- The proxied-aware fix reads `BeadsConfig.ProxiedEnabled()` /
  `ProxyPoolSizeOrDefault()`, and that config surface
  (`internal/config/config.go` `Proxied`, `ProxyPoolSize`, the accessors)
  exists **only** on `feat/beads-proxied-pooling` — it is **absent on
  origin/main** (verified: `grep` over `origin/main:internal/config/config.go`
  returns nothing).
- `cmd/gc/status_provider.go` is **byte-identical** on `origin/main`, the
  feat tip, and the working tree — so a main-based fix could only be a
  blanket `50ms→1s` bump that also degrades embedded ("snappy") behaviour
  and fixes the bug on a branch where it cannot even occur. Strictly worse.

`feat/beads-proxied-pooling` currently lives on the **`fork` remote**
(`git@github.com:cstar/gascity.git`) as `fork/feat/beads-proxied-pooling`,
not on `origin` (`gastownhall/gascity`). The PR base is therefore the feat
branch on the fork. See Open questions for the exact remote/PR-target
mechanics (executor/architect to confirm at push time).

**Worktree is already prepared by the planner:**
- `work_dir`: `/Users/cstar/rigs/.gc-worktrees/gascity-ga-3bwc`
- branch: `gc/ga-3bwc` (based at `feat/beads-proxied-pooling` tip `e0c9bec81`)

## Micro-tasks

TDD, red→green. The first task is the failing test. Run each acceptance
from the worktree root with the module's toolchain (Go 1.26.3).

| id | description | acceptance (single failing test → make it pass) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Add failing unit test `TestStatusProbeTimeoutSelectsProxiedBound` in `cmd/gc/status_provider_test.go`: assert the selector returns `statusProviderProxiedCallTimeout` for a `*config.City` with `Beads.Proxied=true`, and `statusProviderCallTimeout` (50 ms) for embedded (`Proxied` nil/false) and for `nil` cfg. | `go test ./cmd/gc/ -run TestStatusProbeTimeoutSelectsProxiedBound` **fails to compile** — `statusProbeTimeout` and `statusProviderProxiedCallTimeout` are undefined. | 4 | — |
| T-002 | In `cmd/gc/status_provider.go` add `statusProviderProxiedCallTimeout = 1 * time.Second` and `func statusProbeTimeout(cfg *config.City) time.Duration` (proxied → proxied bound; nil/embedded → `statusProviderCallTimeout`); add the `internal/config` import. | `go test ./cmd/gc/ -run TestStatusProbeTimeoutSelectsProxiedBound` **passes**. | 4 | — |
| T-003 | Add failing test `TestBoundedStatusProviderUsesPerProviderTimeout` reusing the existing `statusProbeProvider` fake: a provider built with a 10 ms deadline and `delay=50ms` returns the fallback and fires exactly one warning; one built with a 200 ms deadline and `delay=50ms` returns the real result and fires no warning. | `go test ./cmd/gc/ -run TestBoundedStatusProviderUsesPerProviderTimeout` **fails to compile** — `newBoundedStatusProviderWithTimeout` is undefined. | 5 | — |
| T-004 | In `status_provider.go`: add field `timeout time.Duration` to `statusProvider`; add `newBoundedStatusProviderWithTimeout(base runtime.Provider, d time.Duration)`; make `newBoundedStatusProvider(base)` delegate with `statusProviderCallTimeout`; change `boundedStatusCall` to read `p.timeout` (keep `<= 0 ⇒ unbounded`). Preserve the existing already-wrapped idempotency guard. | `go test ./cmd/gc/ -run 'TestBoundedStatusProviderUsesPerProviderTimeout\|TestStatusProviderTimeoutDoesNotStickAcrossCalls' -count=1` **passes** — new boundary test green AND the existing global-timeout test still green. | 5 | — |
| T-005 | Add failing test `TestStatusSessionProviderAppliesProxiedTimeout`: call `newStatusSessionProviderForCity(&config.City{Beads: config.BeadsConfig{Proxied: <ptr true>}}, t.TempDir())`, type-assert `*statusProvider`, assert `.timeout == statusProviderProxiedCallTimeout`; embedded cfg → `statusProviderCallTimeout`. | `go test ./cmd/gc/ -run TestStatusSessionProviderAppliesProxiedTimeout` **fails** — call site still hard-wires the 50 ms default. | 5 | — |
| T-006 | In `cmd/gc/providers.go` change both `newStatusSessionProviderForCity` and `newStatusSessionProviderForCityWithSnapshot` to wrap with `newBoundedStatusProviderWithTimeout(newSessionProviderFromContext(...), statusProbeTimeout(cfg))`. | `go test ./cmd/gc/ -run TestStatusSessionProviderAppliesProxiedTimeout` **passes**, and full `go test ./cmd/gc/ -count=1` is green. | 4 | — |

Total est: ~27 min across 6 micro-tasks (3 red/green pairs).

## GDPR data-flow impact

**No impact.** This change adjusts an in-process timeout (`time.Duration`)
governing how long `gc status` waits for the local runtime status-probe
(tmux / session provider) before returning partial status. No personal
data is read, written, transmitted, or logged by any changed path. The
bounded status methods (`IsRunning`, `IsAttached`, `ProcessAlive`,
`ObserveLiveness`, `Peek`, `GetMeta`, `ListRunning`, `GetLastActivity`,
`Pending`) operate on session/rig operational identifiers, not
data-subject data. The selector reads `BeadsConfig.ProxiedEnabled()` (a
bool) and, if pool-size scaling is later adopted, `ProxyPoolSizeOrDefault()`
(an int) — configuration values, not personal data. No new persistence, no
new network egress, no new log fields, no change to the Article 30 record
of processing.

## MDR Class I traceability

**No-op outside voxmemo.** This change is in `gascity` (the `gc`
orchestration CLI), not the voxmemo→voxist-api clinical documentation
pipeline. It does not touch the chain-of-evidence from microphone capture
through ASR to exported clinical note: no recording, transcript,
timestamp, device identifier, or operator attribution is created,
modified, or relied upon. The heading is retained per Voxist planner
discipline so an auditor sees the explicit consideration; there is no MDR
Class I traceability surface in the status-probe timeout path.

## Validation gates

- `go test ./cmd/gc/ -count=1` green; `go vet ./cmd/gc/` clean.
- `go build ./...` succeeds on `feat/beads-proxied-pooling`.
- Boundary behaviour proven by unit test on both sides of the deadline
  (call slower than deadline → fallback + exactly one warning; faster →
  real result, no warning) using the existing `statusProbeProvider` fake
  and `t.TempDir()` / no real tmux dependency.
- `git diff` confined to `cmd/gc/status_provider.go`,
  `cmd/gc/status_provider_test.go`, and `cmd/gc/providers.go`. No other
  files modified.
- **Manual operator check (not a unit test):** on a healthy proxied-server
  city, `gc status` no longer prints `runtime status probe timed out;
  using partial status` and returns COMPLETE status. Record the before/after
  in the PR description (cannot be asserted in CI without a live proxied
  runtime).

## Notes for the executor

- **Same-package tests.** `status_provider_test.go` is `package main`, so
  tests can read the unexported `statusProvider.timeout` field and the
  package-level constants directly — no exported getter needed.
- **Ordering in the existing test.** `TestStatusProviderTimeoutDoesNotStickAcrossCalls`
  sets the global `statusProviderCallTimeout = 10ms` *before* it constructs
  the provider. Make `newBoundedStatusProvider(base)` snapshot the current
  global into `p.timeout` at construction so that test stays green
  unchanged. Do not read the global inside `boundedStatusCall` anymore —
  read `p.timeout`.
- **T-005 construction weight (escape hatch).** `newStatusSessionProviderForCity`
  builds the default (tmux) session provider, which is construct-only (no
  running tmux needed). If `newSessionProviderFromContext` turns out to do
  heavy I/O or env reads that make the test flaky, fall back to asserting
  the seam directly: `newBoundedStatusProviderWithTimeout(runtime.NewFake(),
  statusProbeTimeout(proxiedCfg)).(*statusProvider).timeout ==
  statusProviderProxiedCallTimeout`, and treat the two-line call-site edit
  as covered by compilation + the existing `cmd_status_test.go` suite.
  Document the choice in the PR.
- **Bound value.** `1 * time.Second` is the default; the design's stated
  acceptable range is 500 ms–1 s. Keep it a named constant so it is easy to
  tune in review.

## Out of scope

- **Design option B** (serve status from a non-blocking async cache so the
  CLI never blocks). Larger change; option A is the minimal fix the bead asks
  for. File a follow-up bead if review wants the cache.
- **Pool-size scaling** of the proxied bound (scale with
  `ProxyPoolSizeOrDefault()`). The bead says "could" — optional refinement;
  a flat 1 s bound satisfies all acceptance criteria. See Open questions.
- Any change to the warning string or to `cmd_status.go` /
  `city_status_snapshot.go` output formatting.
- Touching `origin/main` — this fix targets `feat/beads-proxied-pooling`
  only (see Base branch).

## Open questions

- `[architect]` **Base branch confirmation.** Plan bases the fix on
  `feat/beads-proxied-pooling` (proxied config + the bug both live only
  there). Confirm this is the intended integration target rather than a
  blanket bump on `origin/main`. Rationale and evidence are in the Base
  branch section above; flagged for confirmation at PR review, not a
  blocker.
- `[architect]` **Remote / PR target.** `feat/beads-proxied-pooling` is on
  the `fork` remote (`cstar/gascity`), not `origin` (`gastownhall/gascity`).
  Confirm the executor should push `gc/ga-3bwc` to `fork` and open the PR
  against `fork/feat/beads-proxied-pooling`. (Executor-resolvable at push
  time; flagged because it diverges from the usual origin/main flow.)
- Proxied bound exact value (1 s vs 750 ms vs 500 ms) and whether to scale
  it with `ProxyPoolSizeOrDefault()` — reviewer-tunable; the named constant
  makes this a one-line change.

## Risks and unknowns

- **Deadline too low under load.** If 1 s proves too tight on a busy
  proxied city (large pool, many sessions), the warning could still fire
  occasionally. Mitigation: the value is a named constant; bump or adopt
  pool-size scaling. Not expected at the default pool size of 4.
- **Other callers of the global.** `newBoundedStatusProvider(base)` (zero
  arg) is retained and still defaults to the 50 ms global, so any
  non-`cfg` caller and the existing tests keep their current behaviour.
  Only the two `cfg`-bearing city status call sites change.
- **feat branch drift.** `feat/beads-proxied-pooling` is 28 ahead / 13
  behind `origin/main`. The fix touches files identical to main, so a
  later rebase of the feat branch onto main is low-risk for this change.

# Plan: gascity owns db-proxy lifecycle — apply Karel patch 0002 (ga-bgub9)

> **Status:** ready-to-execute — 2026-06-07
> **Bead:** `ga-bgub9` (feature, **P1**) — Phase 1 proxy-lifecycle
> ownership; pairs with bd Phase 0 `be-8nd`; part of the permanent
> `po-de0tu` fix.
> **Author:** Karel Bourgois (karel@voxist). **Architect-verified** clean.
> **Patch:** `/Users/cstar/rigs/_handoff/0002-gascity-phase1-proxy-lifecycle.patch`
> (8 files, +262/-17). **Planner pre-flight: `git apply --check` PASSES on
> `feat/beads-proxied-pooling`** (acceptance #1 already de-risked).
> **One bead per plan** — this is one atomic patch → one PR.

## Context

Any finite `proxy_idle_timeout` is starved by sparse controller probes →
spawn/serve/idle-die/respawn **churn** that never reaches warm-pool steady
state. The fix gives `gc` ownership of the db-proxy lifecycle:

1. **Never-idle default:** `proxy_idle_timeout` default `"10m"` → `"0"`
   (`internal/config/config.go`). The proxy stays warm for the city's
   life; gc owns start/stop.
2. **Reap on stop:** `reapProxiedChildrenForCity` (new
   `cmd/gc/beads_proxy_reap.go`) wired into
   `cmd/gc/dolt_stop_managed.go`'s `clearPublishedState` path so a genuine
   `gc stop` tears down the db-proxy-children. Discovery is by **live
   process table** (`ps -axww` matched on `bd db-proxy-child --root` under
   each scope `.beads`), **never by pidfile**; SIGTERM → 2s grace →
   SIGKILL. Together with `be-8nd` this makes `.beads/dolt-server.port`
   single-owner.

The patch is pure/testable: process-table discovery and the `--root`
parser are isolated, table-driven-tested functions
(`proxyChildRootArg`, the `ps`→PID lister, the scope-`.beads` enumerator).

### Base branch & merge target

- **Base:** `feat/beads-proxied-pooling` (our gascity fork pooling branch;
  same as `ga-3p3o`). Proxy mode exists only here.
- **Merge target: the FORK** (`cstar/gascity`), branch
  `feat/beads-proxied-pooling`. Upstream `gastownhall/gascity` has **no
  proxy mode** → this is UNMERGEABLE there. Do NOT open the PR against
  upstream/main.
- **Worktree prepared by the planner:**
  `work_dir` = `/Users/cstar/rigs/.gc-worktrees/gascity-ga-bgub9`,
  branch `gc/ga-bgub9` (based at `feat/beads-proxied-pooling` tip
  `e0c9bec81`).

## Micro-tasks

TDD red→green via staged patch application (the patch bundles test-first
tests by Karel; we split so the new tests are seen RED before the impl
lands). Run from the worktree root; raw `go test`/`go vet` need the icu4c
CGO flags (`-I/-L $(brew --prefix icu4c)`) per the gascity dev loop.

`P=/Users/cstar/rigs/_handoff/0002-gascity-phase1-proxy-lifecycle.patch`

| id | description | acceptance (single failing test → make it pass) | est_minutes | slings |
| --- | --- | --- | --- | --- |
| T-001 | Apply ONLY the new test file: `git apply --include='cmd/gc/beads_proxy_reap_test.go' "$P"`. Run the four new unit tests. | `go test ./cmd/gc/ -run 'TestProxiedScopeBeadsDirs\|TestProxyChildRootArg\|TestProxyChildPIDsFromPS\|TestReapProxiedChildrenForCity_NoopWhenNotProxied'` **fails to compile** — `reapProxiedChildrenForCity`, `proxyChildRootArg`, the ps-PID lister and scope enumerator are undefined. | 4 | — |
| T-002 | Apply the rest of the patch (impl + config default + overlay-test update + docs/schema): `git apply --exclude='cmd/gc/beads_proxy_reap_test.go' "$P"`. | `go test ./cmd/gc/ -run 'TestProxiedScopeBeadsDirs\|TestProxyChildRootArg\|TestProxyChildPIDsFromPS\|TestReapProxiedChildrenForCity_NoopWhenNotProxied' -count=1` **passes**, and the updated `cmd/gc/beads_proxied_overlay_test.go` (now expecting idle_timeout `0`, not `10m`) passes. | 5 | — |
| T-003 | Verify build, vet, and schema-regen idempotence on the fully-applied tree. | `go build ./cmd/gc` succeeds, `go vet ./cmd/gc/... ./internal/config/...` clean, and re-running the city-schema generator produces **no further diff** to `docs/schema/city-schema.{json,txt}` / `docs/reference/config.md` (the patch's schema changes match generated output). | 5 | — |
| T-004 | Behavior verification (integration / manual — needs a live proxied runtime; document results in the PR, not a CI unit test). | With `proxy_idle_timeout=0`: one `bd db-proxy-child` (stable pid) is reused across repeated calls (no respawn churn). After `gc stop`: **zero** surviving `db-proxy-child` processes whose `--root` is under the city/rig `.beads`. | 5 | — |

Total est: ~19 min. Files (all from the patch, atomic): `cmd/gc/beads_proxy_reap.go` (+test), `cmd/gc/dolt_stop_managed.go`, `internal/config/config.go`, `cmd/gc/beads_proxied_overlay_test.go`, `docs/reference/config.md`, `docs/schema/city-schema.{json,txt}`.

> **File count note:** 8 files exceeds the usual 3-file guideline, but this
> is one atomic, architect-verified upstream patch (3 of the 8 are
> generated docs/schema). Splitting it would break a cohesive,
> externally-authored change. One bead, one PR — intentional.

## GDPR data-flow impact

**No impact.** This change governs the lifecycle of a local database-proxy
subprocess (`bd db-proxy-child`) — when it stays warm and when `gc stop`
reaps it. No personal data is read, written, transmitted, or logged. The
process-table discovery matches on the `--root` flag (a local `.beads`
filesystem path), and the config change is a timeout default. No new
persistence, network egress, or log fields beyond an existing-style
"stopped N db-proxy-child process(es)" stderr line. Article 30 record of
processing unaffected.

## MDR Class I traceability

**No-op outside voxmemo.** This change is in `gascity` (the `gc`
orchestration runtime's db-proxy process management), not the
voxmemo→voxist-api clinical documentation pipeline. It does not touch the
chain-of-evidence from microphone capture through ASR to exported clinical
note. The heading is retained per Voxist planner discipline so an auditor
sees the explicit consideration.

## Validation gates

- `go build ./cmd/gc` + `go vet` clean; the four new unit tests +
  `beads_proxied_overlay_test.go` green; schema-regen idempotent (T-003).
- Behavior (T-004, manual on a proxied city): never-idle keeps one stable
  db-proxy-child; `gc stop` leaves zero db-proxy-children under the scope
  `.beads`.
- `git diff` confined to the 8 patch files. No new third-party Go modules.
- PR opened against **`fork` `feat/beads-proxied-pooling`** (NOT upstream).
- DoD = PR + CI-green + review; human does the merge (`be-8nd` pairs).

## Notes for the executor

- **Prefer the patch over cherry-pick.** The patch is verified to
  `git apply --check` clean on this branch. (Fallback only if it ever
  conflicts: cherry-pick `bourgois fork/main:fix/proxy-lifecycle-ownership`.)
- **Discovery is process-table, never pidfile.** Do not "improve" the reap
  to read a pidfile — the whole point (single-owner `.beads/dolt-server.port`,
  po-de0tu) is that gc finds children by `ps` match on
  `bd db-proxy-child --root <scope>/.beads`. Keep SIGTERM→2s→SIGKILL.
- **Proxied-off must be a no-op.** `TestReapProxiedChildrenForCity_NoopWhenNotProxied`
  guards that a non-proxied city reaps nothing — keep it green.
- **Schema is generated.** Don't hand-edit `city-schema.*`; if T-003 shows
  a diff after regen, the generator — not the committed file — is the
  source of truth (re-run it and commit its output).
- **Merge target.** Open the PR to the fork's `feat/beads-proxied-pooling`.
  Opening against upstream main will fail review (no proxy mode upstream).

## Open questions

- `be-8nd` (bd Phase 0, beads rig) is the paired half that makes
  `.beads/dolt-server.port` fully single-owner. It is a **different rig**
  (`be-` prefix) and out of scope here; this bead stands alone for CI but
  the *operational* single-owner guarantee needs both. Track the pairing;
  do not plan `be-8nd` here.

## Out of scope

- `be-8nd` / any beads-rig changes.
- Reworking the proxy pool sizing or the dbproxy itself.
- Upstream (`gastownhall`) — proxy mode does not exist there.

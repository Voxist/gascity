# WIP rescue patches, 2026-11-13

Uncommitted work saved from stale gascity worktrees during the 2026-11-13 worktree cleanup.
Each `<worktree>.diff` is `git diff HEAD --binary` (staged, unstaged, and untracked files via intent-to-add) taken in that worktree.
Apply one with `git apply <worktree>.diff` on a checkout of its base commit.

Excluded as tool leftovers: `.omc/`, `.claude/`, `.beads.gate.lock`, `cmd/gc/isolated-home/` (vp-mbxt test cache), and the `scripts/add-testenv-import.go` regeneration of `internal/testenv/` (delivery-setphase, vp-ueh0b).

The 11 worktrees under `worktrees/` are locked in place with these changes still uncommitted. agent-a0e6c810962b78b48 was removed after its script was saved here; its base is the unpushed merge on the local branch `rescue/agent-a0e6c810962b78b48-20261113`.

## vp-3lqh

- Worktree: `worktrees/vp-3lqh`
- Branch: `gc/vp-3lqh`
- Base commit: `6257b394c885ee4a94d90a93f68894634630364e`
- Patch: `rescue-patches/20261113/vp-3lqh.diff`
- Summary: One-line test fix: TestResolveEventsPath_NoSourceReturnsError sets GC_CEILING_DIRECTORIES to its temp cwd so city discovery cannot walk above it.
- Files:
  - `cmd/gc/cmd_analyze_reliability_test.go`

## vp-2rfp

- Worktree: `worktrees/vp-2rfp`
- Branch: `gc/vp-2rfp`
- Base commit: `0c94500983a49863304a91ee220d1ccf8cd552d8`
- Patch: `rescue-patches/20261113/vp-2rfp.diff`
- Summary: Reconcile backoff and circuit-breaker hardening: promoteLiveLocked resets circuitTripped so a second live-to-degraded transition logs the trip again; dolt cleanup-discovery tweak; plan doc v0.1.
- Files:
  - `cmd/gc/dolt_cleanup_discovery.go`
  - `engdocs/plans/vp-2rfp-reconcile-backoff-circuit-breaker.md`
  - `internal/beads/caching_store_reconcile.go`
  - `internal/beads/caching_store_reconcile_internal_test.go`

## vp-kxbh

- Worktree: `worktrees/vp-kxbh`
- Branch: `gc/vp-kxbh`
- Base commit: `6acae8d2a545aa5b50b56777a04546c82880c3e8`
- Patch: `rescue-patches/20261113/vp-kxbh.diff`
- Summary: One-line test fix: dolt_project_id_test opens the managed Dolt pool with managedDoltPassword().
- Files:
  - `cmd/gc/dolt_project_id_test.go`

## delivery-setphase

- Worktree: `worktrees/delivery-setphase`
- Branch: `feat/delivery-setphase-entered-at`
- Base commit: `ca4b888fb7a38d13fb702f27910fcf28ff4656c4`
- Patch: `rescue-patches/20261113/delivery-setphase.diff`
- Summary: Staged re-add of the internal/delivery package (phase state machine, close guard, store) on top of the upstream revert of PR #3177 (#3334).
- Files:
  - `internal/delivery/close_guard.go`
  - `internal/delivery/close_guard_test.go`
  - `internal/delivery/integration_test.go`
  - `internal/delivery/phase.go`
  - `internal/delivery/phase_test.go`
  - `internal/delivery/store.go`
  - `internal/delivery/store_test.go`
  - `internal/delivery/testenv_import_test.go`
  - `internal/delivery/validate_transition_test.go`

## vc-37gh

- Worktree: `worktrees/vc-37gh`
- Branch: `gc/vc-37gh`
- Base commit: `8de6b46df7971efc8b43efe2fab568d123478a38`
- Patch: `rescue-patches/20261113/vc-37gh.diff`
- Summary: ADR-0094 D6: route all six depsComplete write sites through one setter that records the last writer and a transition timestamp, plus a deps-complete gauge test.
- Files:
  - `internal/beads/caching_store.go`
  - `internal/beads/caching_store_deps_complete_gauge_internal_test.go`
  - `internal/beads/caching_store_events.go`
  - `internal/beads/caching_store_reconcile.go`
  - `internal/beads/caching_store_reconcile_census_test.go`

## vp-7itrh

- Worktree: `worktrees/vp-7itrh`
- Branch: `gc/vp-7itrh`
- Base commit: `88e65c49b1dcc8a77bec990e5a0dbc0204485472`
- Patch: `rescue-patches/20261113/vp-7itrh.diff`
- Summary: vp-7itrh DEFECT-2 contract test: gc session nudge --delivery queue returns 0 only when the message is durably queued, and fails loudly on an unresolvable target.
- Files:
  - `cmd/gc/cmd_session_nudge_queue_contract_test.go`

## vp-cz7o.21.1

- Worktree: `worktrees/vp-cz7o.21.1`
- Branch: `gc/vp-cz7o.21.1`
- Base commit: `06e3d1fe0f8a56584b8e0d91c3088397837a7d88`
- Patch: `rescue-patches/20261113/vp-cz7o.21.1.diff`
- Summary: City identity map: readCityIdentityMapEntry, and ensureProjectID restores a rig's project ID from the city identity map.
- Files:
  - `cmd/gc/city_identity_map.go`
  - `cmd/gc/city_identity_map_test.go`
  - `cmd/gc/dolt_project_id.go`
  - `cmd/gc/dolt_project_id_test.go`

## vp-gg023

- Worktree: `worktrees/vp-gg023`
- Branch: `vp-gg023`
- Base commit: `9d2f9ddbb24ecea61b8adec092e2d084c7d4526f`
- Patch: `rescue-patches/20261113/vp-gg023.diff`
- Summary: ADR-0089 worktree custody: record worktree custody on bead release, scope the bead-worktree reaper to the configured worktree roots, new events and bead metadata keys.
- Files:
  - `cmd/gc/assigned_work_residency_test.go`
  - `cmd/gc/bead_worktree_reaper.go`
  - `cmd/gc/bead_worktree_reaper_integration_test.go`
  - `cmd/gc/bead_worktree_reaper_scope_root_test.go`
  - `cmd/gc/city_runtime.go`
  - `cmd/gc/cmd_start.go`
  - `cmd/gc/cmd_start_test.go`
  - `cmd/gc/dead_assignee_repair_test.go`
  - `cmd/gc/execution_stalled_convergence_test.go`
  - `cmd/gc/pool_session_name.go`
  - `cmd/gc/pool_session_name_orphan_release_fanout_test.go`
  - `cmd/gc/pool_session_name_protected_wake_test.go`
  - `cmd/gc/pool_session_name_route_assignee_liveness_test.go`
  - `cmd/gc/pool_session_name_test.go`
  - `cmd/gc/pool_session_name_worktree_custody_test.go`
  - `cmd/gc/pool_session_release_binding_owner_test.go`
  - `cmd/gc/pool_session_release_owner_store_test.go`
  - `cmd/gc/pool_session_release_session_store_test.go`
  - `cmd/gc/pool_slot_unaliased_test.go`
  - `cmd/gc/session_reconciler.go`
  - `cmd/gc/split_topology_conformance_test.go`
  - `internal/beadmeta/keys.go`
  - `internal/events/events.go`
  - `internal/events/payloads.go`
  - `internal/git/git.go`

## vp-mbxt

- Worktree: `worktrees/vp-mbxt`
- Branch: `gc/vp-mbxt`
- Base commit: `dedaadc88eb031a15c50e9a384560b19014ba8d5`
- Patch: `rescue-patches/20261113/vp-mbxt.diff`
- Summary: CPU-runaway watchdog for managed Dolt sql-servers (default on, GC_DOLT_CPU_WATCHDOG=0 opts out), dolt scope-watchdog wiring, CHANGELOG entry, vp-petx bisect plan doc.
- Files:
  - `CHANGELOG.md`
  - `cmd/gc/dolt_cpu_watchdog.go`
  - `cmd/gc/dolt_cpu_watchdog_test.go`
  - `cmd/gc/dolt_scope_watchdog.go`
  - `engdocs/plans/vp-petx-bisect-serialization-failure.md`

## vp-ry9jm

- Worktree: `worktrees/vp-ry9jm`
- Branch: `gc/vp-ry9jm`
- Base commit: `eacd677348e6ec21ffa6dee54b3b1a102145c37d`
- Patch: `rescue-patches/20261113/vp-ry9jm.diff`
- Summary: gc doctor check that inspects supervisor launchd jobs and plists read-only, plus a supervisor lifecycle change.
- Files:
  - `cmd/gc/cmd_doctor_supervisor_launchd.go`
  - `cmd/gc/cmd_doctor_supervisor_launchd_test.go`
  - `cmd/gc/cmd_supervisor_lifecycle.go`

## vp-ueh0b

- Worktree: `worktrees/vp-ueh0b`
- Branch: `fix/gc-test-isolation-symlink`
- Base commit: `242b06f2f7bee6f05573721f8dd95f06ec6cb1e5`
- Patch: `rescue-patches/20261113/vp-ueh0b.diff`
- Summary: Test isolation: city discovery resolves symlinks before comparing with GC_CEILING_DIRECTORIES, cmd/gc tests set a ceiling at their temp dir, golangci allow-serial-runners.
- Files:
  - `.golangci.yml`
  - `cmd/gc/city_discovery.go`
  - `cmd/gc/main_test.go`
  - `cmd/gc/rig_anywhere_test.go`
  - `cmd/gc/testenv_test.go`

## agent-a0e6c810962b78b48

- Worktree: `.claude/worktrees/agent-a0e6c810962b78b48`
- Branch: `resync/origin-main-20260904`
- Base commit: `d193c16d60857fd746bd5ce25e42b1d2f2c6be02`
- Patch: `rescue-patches/20261113/agent-a0e6c810962b78b48.diff`
- Summary: Untracked helper scripts/check-resync-loss-upstream.py from the Sep 4 resync (resync/origin-main-20260904).
- Files:
  - `scripts/check-resync-loss-upstream.py`

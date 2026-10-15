# Fork-delta census, 2026-10-14 (ga-s69mk.1)

Phase 0 of the fork convergence epic ga-s69mk: what the Voxist/gascity and
Voxist/beads forks carry over upstream, classified by what should happen to
each piece. The gascity census is reproduced in full below; the beads census
is summarised in the last section.

File and line references are on the pre-resync `fork/main` (cbbf84bf0).
After the 2026-10-14 resync (aa0ae5671) locate code by symbol, not by line.
The Phase 2 rows marked **Delete** were removed by ga-s69mk.3.

## gascity

Refs (2026-10-14): `origin/main` = 9ef8fc783 (gastownhall/gascity), `fork/main` = cbbf84bf0 (Voxist/gascity),
merge-base = e9f7e957a (2026-09-25, last resync). Upstream has 843 non-merge commits since the merge-base.

Delta measured: `git diff origin/main...fork/main` = 745 files, **+88,944 / -4,346** (numstat; files with a
`-diff` attribute such as openapi.json, genclient, census.go are excluded from numstat and listed separately).
`git log origin/main..fork/main --no-merges` = 575 commits (839 including 264 merges).

### Summary

| Bucket | Features | +lines | % of delta |
|---|---:|---:|---:|
| A. ALREADY-UPSTREAM | 3 | 1,654 | 1.9% |
| B. SUPERSEDED | 4 | 8,976 | 10.1% |
| C. UPSTREAMABLE | 32 | 46,678 | 52.5% |
| D. FORK-OWNED | 19 | 30,448 | 34.2% |
| E. RESYNC-DEBT | 2 | 1,186 | 1.3% |
| **Total** | **60** | **88,942** | **100%** |

Coverage: 88,942 of 88,944 added lines (the two missing lines sit in deleted or binary dist files). Every added
line is assigned to exactly one feature.

Outside numstat, and all in bucket E (regenerate, never merge): `internal/api/openapi.json` (+2,678),
`internal/api/genclient/client_gen.go` (+1,331), dashboard TS generated client, `docs/reference/{cli,config}.md`,
`docs/reference/schema/*`, `cmd/gc/metrics_census_gen.go`, `internal/testpolicy/resourcecensus/census.go` (+220;
hand-written, merge it, never `--theirs`), `cmd/gc/productmetrics_command_census.json` (+87; hand-maintained
input, merge it), `internal/testenv/testdata/gc_env_read_baseline.golden`, `scripts/runtime-tmux-tests.manifest`,
and 21 renamed dashboard `dist/` bundles. These shrink automatically when the features behind them go away.

#### Method

1. **Line attribution.** `git blame --porcelain fork/main` was run on every changed file. Only lines that
   `git diff -U0 <merge-base> fork/main` reports as added were counted, so each count is a real delta line
   (88,942 attributed).
2. **Commit to feature.** All 575 fork commits and the merge commits were grouped by subject, bead id and
   file cluster (scripts kept at `.../scratchpad/census/bin/`).
3. **Resync churn.** 96 commits are resync repairs: `fix(resync)`, the `#137 round N` reviews, `merge-fix`,
   and merge-commit resolutions. They wrote 10,591 of the surviving delta lines. Most of those lines put back
   fork code that a merge had dropped, so they are credited to the feature that owns the file. Bucket E keeps
   only what exists purely because of merges: census and ratchet rows, cipolicy hashes, goldens, go.sum,
   test-resources.toml, and 204 lines where the fork still carries an older upstream revision.
4. **Upstream checks.** Each feature was checked three ways: distinctive symbols grepped on `origin/main`, the
   upstream log searched since June for bourgois-authored and adopted (`gc-adopt-pr-followup`) commits, and
   bourgois's open upstream PRs listed with `gh`.

Caveat: features split by file pattern (F01/F02, F08/F09, F12/F13, F14/F15, F20/F21, F23/F24, F25/F26, F33/F34)
have approximate boundaries, roughly ±5%.

---

### Feature table

Columns: id · feature · bucket · +lines · key files · key commits / beads · upstream evidence · action.

#### A. ALREADY-UPSTREAM (drops out on resync; take upstream's version on conflict)

| id | feature | +lines | key files | commits / beads | evidence | action |
|---|---|---:|---|---|---|---|
| F14 | bd fresh-bootstrap hardening (interrupted-bootstrap heal, version witness, never force-reinit on an unanswered probe, canonical server metadata before the first init) | 1,262 | `examples/bd/assets/scripts/gc-beads-bd.sh`, `cmd/gc/beads_provider_lifecycle_test.go` | 6c6fe2f62, 926f2b578, c59bf76c8, 0dfdaf732, c4dbafb3d, 4aa99c742, db36d8852, 45113b84d, cd73319b4, 39b4d2f76 | Fork PR #5926, adopted upstream as **4c7b434b7 (#7265, 2026-10-06)**: "carries #5926's two commits ... plus review fixes" | On resync take upstream's `gc-beads-bd.sh` and lifecycle hunks, then re-apply only F03's recover lock (ga-iv2l2) and F15 (ga-2hgoz). Run `git range-diff` against #7265 to confirm. |
| F09 | `gc hook` exits 2 when the store is unavailable (typed unavailable is not the same as empty) | 316 | `cmd/gc/cmd_hook_unavailable_test.go` (+ `cmd_hook.go` hunks) | e5bd1214a, 9c3333236 (restore) | **221673bb1 (#7748)** and **0a831ed72 (#7750)**: adopted from Voxist branches `up/ga-8p8ky-hook-exit2-*` | Take upstream wholesale. |
| F11 | Residual drift in already-upstreamed resilience registry and doltpool | 76 | `internal/doltpool/pool.go`, `internal/resilience/registry.go` | a365da4de, 45327df45, 622f442b5 | upstream d2130640c (#3318), 8ea0080bf (#3379), fae58763f (#3313) | Take upstream. The fork-only breaker-callback-deadlock fix is upstream PR #4839 (open). |

Earlier fork-first PRs that already landed upstream have no surviving delta: fsnotify bump (#5478), live
managed-Dolt port resolver (#3389), `doctor --fix` port re-pin (#3529), 127.0.0.1 bind (#3361), uuid self-heal
(#3514), rig init long timeout (#3555), import-cache gitignored (#3314), tmux liveness retention (#3311),
bd-backup-freshness (#3739), path normalizer (#4695), fork-verify workflow (#3674), and the flat-membership order
gate (#3115). They are listed here for completeness only.

#### B. SUPERSEDED (delete fork code, use upstream's)

| id | feature | +lines | key files | commits / beads | evidence | action |
|---|---|---:|---|---|---|---|
| F01 | Doctor `binary-divergence`: the supervisor's executed gc image vs the verified PATH gc (file identity, deleted-image detection) | 3,728 | `internal/doctor/checks_binary_divergence{,_test}.go` (3,170), `cmd/gc/supervisor_liveness_probe_test.go`, `cmd/gc/cmd_doctor_binary_divergence_test.go`, `internal/doctor/file_identity_*.go` | a505ce6c6, 55e6e4cad, 426125bdb, 7d34f5d3c, bb7e2c10e, 8abb5922b, 6fa352fdf | **3298e7bac (#6724, 2026-10-13)**: upstream `supervisor-binary` doctor check (`internal/doctor/checks_supervisor_binary.go`), plus `GC_BIN` fail-closed and service-file replacement | Delete the fork check and keep upstream's. First diff coverage for "same path, different build": if upstream misses that case, send that one case upstream. This is the largest single B deletion. |
| F18 | Fork `[beads] proxied` / `proxy_pool_size` / `proxy_idle_timeout` opt-in: metadata overlay, pool env, db-proxy reap on `gc stop`, `beads-proxied-capability` doctor check, proxy-patches docs | 1,769 | `cmd/gc/beads_proxied_overlay{,_test}.go`, `cmd/gc/beads_proxy_reap{,_test}.go`, `cmd/gc/doctor_beads_proxied.go`, `internal/config/config.go`, `internal/events/proxy_payloads.go`, `engdocs/contributors/proxy-patches/*` | b4b3c8998, 9b5e0668c, 117d2b9e9, 5b717602d, 86a8fb960, bbb7cdc23, 36373f98f, 544cf079e, 0bada1e4d; vp-3vx2 | Shipped upstream as #3177 (ac414569d), then **reverted upstream by ca4b888fb (#3334)**. Upstream replaced it with a bd-owned proxied-server: af7ad8a0f (#6273) proxied-local by default, `gc beads city migrate-proxied`, and `[beads] proxied_idle_timeout` (500c05f09/7bfac93cf/850201d51, #7097/#7102/#7107) | Delete in Phase 2 (ga-s69mk.3). See the Phase 2 inventory. |
| F19 | Store-health patrol: two-probe state machine, quarantine and forensics, `store.*` events, `[storehealth]` config | 1,324 | `cmd/gc/storehealth_patrol.go` (402), `internal/storehealth/{patrol,quarantine}{,_test}.go`, `internal/config/config.go` (StoreHealthConfig) | 2cf5ce93e, 1ec17b8bd, 77b11e702, d9f03de2b | Hard-gated on F18's `cfg.Beads.ProxiedEnabled()` (`storehealth_patrol.go:45`), and voxist-city runs `proxied = false`, so it is **inert in production**. Its target, the fork-owned db-proxy child, does not exist under upstream's bd-owned design. Upstream PRs #5945/#5946/#5947 are open. | Delete with F18 and close #5945–#5947. If a patrol is wanted under bd-owned proxied, rebuild it keyed on upstream's topology. |
| F28 | Native-store canary scope lever, post-open identity assertion, `custom_types_registered` preflight and cutover gate | 2,155 | `cmd/gc/bd_env.go`, `cmd/gc/native_store_identity.go`, `internal/beads/identity/*`, `internal/doctor/checks_custom_types_preflight.go`, `internal/beads/contract/custom_types_preflight*.go`, `cmd/gc/doctor_native_store_canary.go` | 14f7f134f, 8e2cc6849, f88c18e63, 5789360c7, 4c4854904, 3f10bb87b, cab39f961 | The canary is default-off, voxist-city does not set it, and the native store has been on fleet-wide since 00dbedf65 (vp-kpoi). Upstream has **64edbb1be (#7214)** "schema-based compat guard for the native-store preflight (G4)" and the upstream proxied-native lane | Retire the canary lever and the custom-types preflight. Keep the post-open identity assertion (~400 lines) only if G4 does not cover it; it is a reasonable small upstream PR. |

#### C. UPSTREAMABLE (generic SDK improvements upstream lacks; fork-first already merged)

| id | feature | +lines | key files | commits / beads | evidence (upstream status) | action |
|---|---|---:|---|---|---|---|
| F03 | Managed-Dolt recover gate: one guarded recover route, three-valued liveness, a controller-only lifecycle owner, session-env strip, recover lock and decline exit code, implicit-recovery gate on CLI (never revive a stopped city), recover events | 5,771 | `cmd/gc/dolt_recover_gate.go`, `cmd/gc/managed_dolt_implicit_recovery_gate.go`, `cmd/gc/beads_provider_recover_gate_test.go`, `cmd/gc/gc_beads_bd_recover_lock_test.go`, `cmd/gc/dolt_sql_health_probe_timeout_test.go`, `gc-beads-bd.sh` | 01fee79c5, a39c2b351, a4c75137d, 108aa7b66, 09c16cd4f, 2dee8cb4e, 24db1eec7; ga-amol9 ga-fjr5f ga-lc97s ga-iv2l2 ga-zfgsh ga-z3c6p ga-2598s ga-cflrh | No upstream equivalent (`recoverGate` absent). Nearby upstream work: 084b3e7cc (#6817, never start a stopped proxied store), f4196f0b5 (#6512) | Highest-value upstream series. It conflicts with F14's script, so land it after the resync. |
| F04 | Order dispatch when the store is unavailable: recover-on-unavailable single-flight shared with webhooks, D1/D2 deadlines, scope-watchdog-liveness doctor check | 3,499 | `cmd/gc/order_dispatch.go`, `cmd/gc/order_dispatch_store_unavailable_fallback_test.go`, `cmd/gc/doctor_dolt_watchdog_liveness.go`, `cmd/gc/webhook_recover_singleflight_test.go` | 668f783da, 8ea934199, 771078825, 11ebc23cd, 5b9279a3a, c5a61c07c, 12dcbd7bc; ga-3bwmf ga-w3bkx | none (upstream bff31d320 #3902 is the older fail-open-on-timeout piece) | Upstream it after F03. |
| F05 | Order-tracking marker sweep keyed by controller generation, respecting each order's timeout | 1,631 | `cmd/gc/order_dispatch.go`, `cmd/gc/order_tracking_sweep_deadline_test.go`, `cmd/gc/order_dispatch_generation_test.go` | c2950c848, ce3dd0501, 51a560968, 9f33c10e8; vp-qauad vp-3dyjq | none (`controllerGeneration` absent; ADR-0130) | Upstream it. Small and self-contained. |
| F07 | Dolt delivery window, boot drain, and verified-zero sync drain (off-box backlog by CLI push; read-timeout leak fix; cold-open wall classifier; orphaned push reaper) and the dolt-listener-deadline doctor check | 3,498 | `cmd/gc/dolt_delivery_window.go`, `cmd/gc/dolt_boot_drain.go`, `examples/bd/dolt/commands/sync/run.sh`, `examples/bd/dolt/sync_*_test.go`, `cmd/gc/doctor_dolt_listener_deadline.go` | 6511762ea, c5ebe825c, 7310d7790, 440747b38, 25374c1e6, 617a7ed3b, affe91f9e; vp-o52ia vp-p8tze vp-6hb8 vp-9ex9z vp-9v6f9 vc-ewyro ga-zf03v | none | Upstream it as one PR scoped to the dolt pack. The ADR-0064 docs need upstream framing. |
| F08 | CachingStore read resilience: availability gate, reality-first degraded reads (last-good only when the store gives no answer), ADR-0094 ready-projection invalidation, disowned-verdict side map, depsComplete gauge, flood guards | 3,537 | `internal/beads/caching_store{,_reads,_availability,_reconcile}.go` and about 10 `*_internal_test.go` | 7a34a0f30, df3459f88, 4476534ec, ce5fee14e, 706077a6c, 38d20cdc2; ga-2p81g vc-vlyk vc-u2n6 vc-493m3j ga-eyz5l ga-xqc6b | **Open upstream PRs #5962 (availability gate), #5963 (last-good reads), #5966** | Drive the open stack to merge. Conflict risk is high because `caching_store_*` changes heavily upstream. |
| F10 | Scope transport breaker at the bd chokepoint, `[beads.resilience]` config, store-warming degrade-not-stop, bounded tick reads, wedged-backend timeout as transport failure | 2,185 | `cmd/gc/bd_resilience.go`, `cmd/gc/order_tracking_sweep_breaker.go`, `internal/config/config.go`, `internal/beads/caching_store_unavailable_internal_test.go` | e017841b3, 181422712, 137de5741, 62ca7a573, d0c0c7408, 1ae8766f8; vc-ny00 vp-2rfp | **Open #5948 (config), #5961 (jitter seam), #4839 (callback deadlock)**. The registry itself is upstream (F11). | Drive the open PRs. |
| F12 | Supervisor-cadence doctor subset (tick age, slow ticks, S6 connection ceiling, agent-config isolation), unsampled controller heartbeat, host-load stream | 1,737 | `cmd/gc/supervisor_doctor.go`, `internal/supervisordoctor/doctor.go`, `cmd/gc/host_load.go`, `internal/events/storehealth_payloads.go` (breaker/tick/doctor events) | 5d1abe489, 0b88f7b76, 58b46f8e1; vp-qvqk | **Open #5953 (event payloads), #5956 (heartbeat)** | Upstream it. S6 must stop reading `ProxyPoolSizeOrDefault()` (see Phase 2). |
| F13 | bd subprocess admission semaphore and `gc_bd_inflight` gauge (fail-fast on saturation) | 577 | `cmd/gc/bd_admission.go`, `cmd/gc/bd_resilience.go` | cf2dcbce0, 8ec2b5e7a | **Open #5949** | Drive #5949. |
| F15 | bd bootstrap follow-ups after #5926: finish pending migrations after a forced reinit (ga-2hgoz), refuse ready-but-uninitialized (ga-fo5mg, ga-wuda3), BD_BIN witness (ga-2rkhi), integration repairs | 1,054 | `gc-beads-bd.sh`, `cmd/gc/gc_beads_bd_finish_schema_migration_after_forced_init_test.go`, `cmd/gc/beads_scope_ownership.go`, `test/integration/e2e_helpers_test.go` | e9dfaaf33, f39a0b9f1, f8b79d3ab, 0bad9197e, 13a6032f8 | Partly overlaps upstream a4ea8ee88 (#6709, verified-empty without force reinit) and 863c89b0e (#6658) | Rebase onto #7265 after the resync, then upstream the rest. |
| F20 | Provider credential resolver (`gc provider credentials`, replaces rotate-key) | 1,800 | `cmd/gc/provider_credentials*.go`, `cmd/gc/cmd_provider_credentials*.go`, `internal/processenv/provider_key*.go`, `cmd/gc/upstream_serving_env.go` | db361ee9a, 53570c9b3, dc936cd7e, 59d266675; ga-i86nb vp-n4xr | **Open #3525** (titled "gc provider credentials") | Drive #3525. Until it merges, keep it behind the provider boundary. |
| F23 | Reconciler safety: keep sessions whose provider cannot be resolved (ga-8a8fq), no drain from a partial build and never drain an attached session (ga-c8rck), scale_check probe store env (ga-7nwnh), ready fan-out collapse, suspension invalidates the demand snapshot | 2,083 | `cmd/gc/build_desired_state.go`, `cmd/gc/session_reconciler.go`, `cmd/gc/unresolved_provider_blast_radius_test.go`, `cmd/gc/build_failure_drain_guard_test.go`, `cmd/gc/scale_check_probe_env_test.go` | a9902849f, 674d0d417, 042939b06, 665b6724d, 75997e86b, c32847bfc | Not upstream. **Upstream is rewriting this layer** (controller v2: allocator, effectTx, drain arms; 50+ commits since the merge-base) | Port each fix as a regression test against the v2 controller. Otherwise it becomes B once legacy is removed. Treat as a high-conflict area. |
| F24 | Phantom session-bead reaper, `[session] pending_create_ttl`, idempotent pool-slot bead (vp-g0z1) | 406 | `cmd/gc/session_beads.go`, `cmd/gc/session_phantom_reaper_store_test.go` | 25f0e9855, 2dc2a152e, 1c30c70c1, 958e11c58 | Related upstream 7b0a60653 (#6958, phantom session keys) | Check whether #6958 covers it. If so, drop the fork code; if not, upstream. |
| F25 | Hook cross-store selection ranked on best candidate (ADR-0076, vp-d1kjk) and Tier-3 hybrid pool sort (vc-zv4y) | 973 | `cmd/gc/hook_cross_store.go`, `cmd/gc/cmd_hook.go` | 6b96152f2, 485678d45, c02e98128 | Cross-store base is upstream (#2877, #3119); the ranking is not | Upstream it. Note the upstream hook budget rework 515eb2b69 (#7601). |
| F26 | work_query origin-gate refusal reason surfaced through `gc hook` and agent scripts (vc-ozanp5) | 1,199 | `internal/config/workquery.go`, `internal/config/workquery_origin_gate_test.go`, `cmd/gc/agent_script_origin_gate_test.go` | 31a8fccd3, bee6b5ccc, 33181fb17, 2acff2699, 173c745d7 | Upstream has the gate (`poolDemandOriginGateScript`) but not the refusal prefix and plumbing | Upstream it. |
| F27 | Status performance: warm background StatusView with SWR, ctx-bounded cold build, supervisor API routing, configurable snapshot timeout (vp-e0hv, vp-8r9g, vp-35ae, vp-m84x) | 2,095 | `internal/api/status_warm*.go`, `internal/api/handler_status.go`, `cmd/gc/status_provider.go`, `engdocs/plans/vp-35ae-*.md`, `engdocs/plans/vp-e0hv-*.md` (437) | 191535a4f, c43553533, c7cbda3bb, 04c5ff4a3, e00b1e9b4 | none | Upstream the code and drop the 437 lines of `engdocs/plans/*` from the tree. |
| F29 | Events: bounded descending tail read across archives, degraded reads of corrupt archives, bounded order-firing and zero-cursor reads (vp-x7x8w, vc-89s, vp-8jig) | 1,781 | `internal/events/reader.go`, `internal/events/reader_{degraded,tail_archive}_test.go`, `internal/doctor/checks_order_firing*.go`, `internal/orders/triggers*.go` | 34a62b8a1, 920ba4c25, 3476b538f, 8a5a2b498, 2110b93dd | **Partial overlap**: upstream 2fc891fd9 (#6063, Since query from the tail), `LatestArchivedMatch`, aa66f2b60 (rotation) | Diff against #6063 and drop the duplicated parts. Upstream the archive-spanning tail and corrupt-archive degrade. |
| F30 | Config robustness: one bad `[[patches.agent]]` no longer bricks load, plus `gc config lint` (vc-quqf); Rig patch field sync (ga-mq52s); session command template validation (ga-iwz7u); `bd_compatibility` validation (vp-a45ji) | 1,633 | `internal/config/{patch,session_command_templates,rig_field_sync_test,unresolved_agent_patch_test}.go`, `cmd/gc/cmd_config.go` | 182e99313, b297d42d8, 9b835bfe6, 8a51a6908, 1bf5d890b | none (`TestRigFieldSync` exists only in the fork, although AGENTS.md describes it) | Upstream it. The rig-field-sync guard is cheap and clearly generic. |
| F31 | Port-file de-authority remainder: port-file-consistency doctor check, started_at preservation (vc-8mfq), "does this PID hold the port" (ga-yeyt3), lsof exit semantics, procstate | 1,232 | `cmd/gc/cmd_doctor_portfile.go`, `cmd/gc/dolt_process_inspection.go`, `internal/pidutil/procstate_*.go`, `cmd/gc/dolt_runtime_publication.go` | 4728b1781, b6bb73a89, f712d4913, 364f1d539, 5528ccc0a, d661d6076 | The live port resolver core is upstream (ca7b0e611 #3389); the doctor check and lsof fixes are not. Related upstream 1fd5c3dc5 (#7138) | Upstream it. `cmd_doctor_portfile.go` uses the F18 constant `proxiedServerClientInfoFile` (see Phase 2). |
| F32 | Session `no_history` storage policy carried through the create front door into the native store (vp-ia76) | 1,067 | `internal/session/create.go`, `internal/session/create_storage_test.go`, `cmd/gc/bead_policy_store.go`, `internal/beads/native_dolt_store_storage_class_test.go` | b1106f9fe, 9d9ad5f8e, 3f012b6e5, 2b6fa919b | Upstream has `CreateWithStorage` and its own emitter storage-class fixes (19e174b49 #7611, 56f41c496 #7623). Overlap is partial. | Re-check against #7611/#7623 during the resync. Upstream the remaining wiring. |
| F34 | Rig data-presence doctor check, archive-export row oracle, wisps-composite-index row-count guards (vp-pt73.3) | 774 | `internal/doctor/checks_rig_data_presence*.go`, `cmd/gc/doctor_archive_export_rows.go`, `schemas/wisps-composite-index/*.sh` | a2b56dabe, d6e4cb577, e616923cd | **Open #3732** | Drive #3732. |
| F35 | `gc beads state`: 16-state effective-state classifier, plus `scripts/bead-state.py` and taxonomy doc (vp-evof) | 1,514 | `cmd/gc/cmd_beads_state.go`, `internal/beads/state/classify.go`, `scripts/bead-state.py`, `engdocs/plans/vp-evof-gc-beads-state.md` | 4aeb4c103, 78f6c0526, aaf13cde6 | **Open #3304** | Drive #3304. Drop the Python duplicate and the plan doc. |
| F36 | Managed hooks and overlays call `${GC_BIN:-gc}` instead of a bare `gc` (shell, Gemini, pi/omp/kimi script hooks; bare-token upgrade) | 1,283 | `internal/hooks/hooks{,_test}.go`, `internal/overlay/merge{,_test}.go`, `internal/bootstrap/packs/core/overlay/per-provider/kimi/...` | 97154823c, 06ad05460, c85299b65, a3dcf18f9, 265057905; vp-7mjx vp-qgil vp-ythc | Upstream uses `GC_BIN:-gc` in some places (hooks.go has 6 occurrences upstream vs 24 in the fork) but not in managed hooks | Upstream it. Low risk, and it shrinks a frequent conflict file. |
| F37 | tmux reasserts PATH before a pane shell can clobber it (ga-tg8t5) | 813 | `internal/runtime/tmux/{tmux,adapter}.go`, `internal/runtime/tmux/pathreset*_test.go` | f21e6d292, f312db04d, 6d412f339 | none | Upstream it. |
| F38 | Dispatch repair of legacy self-closing edges (graphs compiled before #5202) | 894 | `internal/dispatch/control.go`, `internal/dispatch/control_legacy_cycle_test.go` | be0bed3e6, 3c28e31db, 79adc0aa5 (written during the #137 resync review) | none. It is a migration shim for upstream's #5202/#5199. | Retire once no pre-#5202 graph.v2 workflows are live; check voxist-city with a query. Otherwise offer it upstream as a migration repair. |
| F39 | Storeless fallback for `gc session nudge` (vl-3hb) | 690 | `cmd/gc/cmd_nudge_storeless{,_test}.go`, `cmd/gc/cmd_nudge.go` | 2102c3fd5, 173c745d7 (restore), 0951d52ab, 7f7aad130; ga-s2a21 | none. Upstream nudge has moved a lot (a62dabedc, fd99d7cd7, 945c067fe). | Re-validate against upstream's nudge queue, then upstream it or drop it. |
| F40 | Supervisor hardening: test supervisors kept out of the real launchd domain and port (ga-32bb2), managed FD limit / LimitNOFILE, PYTHONDONTWRITEBYTECODE, secrets-file parse (ga-ghb5q) | 693 | `cmd/gc/cmd_supervisor{,_lifecycle}.go`, `cmd/gc/cmd_supervisor_launchagents_isolation_test.go`, `test/integration/launchd_isolation_test.go` | bb68d0f20, 5d2ff2ec3, eaea8aa60, 20c495e99, 7f7ef71b7 | none (upstream d61b25100 #7048 and ae3993dbe #6371 touch launchd elsewhere) | Upstream it. The test-isolation part is especially valuable upstream. |
| F41 | pidutil sysctl migration, process-state probing, execgrace re-signal until dead (ga-4l4me) | 462 | `internal/pidutil/pidutil.go`, `internal/pidutil/*_portable_test.go`, `internal/runtime/proctable/kill_unix.go` | 8af6c7b7f, 18b8006e3 | Related upstream aa8afe87c (#7103), 968e8754b (#7018), 9d47dc65c (#7867) | Diff against those; upstream the remainder. |
| F42 | `gc rig list --json` skips the running-agent probe for suspended rigs (vp-zdp6e) | 371 | `cmd/gc/cmd_rig.go`, `cmd/gc/rig_list_json_cost_test.go` | 06e394c56 | none | Upstream it. |
| F43 | Order-dispatch residue: degrade-not-fatal unknown `[[orders.overrides]]` (vp-ia7c), dispatch-budget starvation (vp-cixi.6), on_boot stagger (vp-hsau), gate_timeout_fail_open event | 384 | `cmd/gc/order_dispatch.go`, `internal/orders/override.go`, `internal/events/payloads.go` | f8b3361b9, 37e66312b, 04e692d25, 0136547b8 | The flat gate (#3115) and per-order gate policy (#3102) are upstream | Upstream the small remainder. |
| F44 | `dolt-timeout-race` doctor check (vc-wz5.1) | 345 | `internal/doctor/checks_dolt_timeout_race{,_test}.go` | fff5f34ac | doltpool itself is upstream (#3313); upstream 28d16016b (#6774) bounds idle connections | Upstream it or drop it, depending on whether #6774 removed the race. |
| F45 | Hosted-bd runner selection tests, herdr XDG config dir on darwin | 331 | `cmd/gc/bd_env_hosted_selection_test.go`, `internal/runtime/herdr/client.go` | dbfba4365, 7430c10db (#137 review rounds) | Upstream herdr fixes 1cfa66847 (#6754) and b191701a7 | Upstream the herdr XDG fix. The tests ride along with F10. |
| F57 | Misc test hygiene: providerledger waiver expiries, productmetrics host-prefs isolation, git maintenance off in InitGitRepo (ga-gkcx0), workflow-report wait (ga-hwjxn), /simplify residue | 366 | `internal/testutil/providerledger/*`, `internal/productmetrics/main_test.go`, `internal/testutil/path.go` | fb0b2c888, 4ecbb0330, 0d03eb130, a059b6aed | none | Batch into one upstream test-hygiene PR. |

#### D. FORK-OWNED (isolate behind clear boundaries)

| id | feature | +lines | key files | commits / beads | evidence | action |
|---|---|---:|---|---|---|---|
| F02 | Deploy provenance: build manifest, machine-derived artifact names, beads/base provenance in `gc version`, `[beads] expected_build`, deploy-provenance doctor checks (vp-q1ho) | 2,124 | `internal/provenance/*`, `internal/doctor/checks_provenance*.go`, `cmd/artifactname/`, `cmd/gc/cmd_version.go` | a944e08e1, 7074e7901, ec00eaeee, b6d40324e, 9098c68f0 | none. Tied to the fork's `make install` and `deploy-fleet` pipeline. | Isolate: move it out of `internal/doctor` into a fork-owned package that registers its checks. `expected_build` alone could go upstream. |
| F06 | `run_on = fleet-host \| seat \| any` orders honored against `[city] role` / `VOXIST_FLEET_ROLE` (vp-sgxy5) | 2,535 | `internal/config/city_role.go`, `internal/orders/run_on.go`, `internal/doctor/checks_order_run_on.go`, `cmd/gc/order_dispatch.go` | e341af7bd (#176) | none. The Go code hardcodes the env name `VOXIST_FLEET_ROLE`, which is live on this host (`VOXIST_FLEET_ROLE=fleet-host`). | Rename to a neutral `GC_FLEET_ROLE`, keeping the old name as an alias. After that it is a credible upstream feature; until then it stays isolated in the fork. |
| F16 | Topology policy: reject upstream's proxied-by-default (plain `gc init` stays server mode; ungated provider-ownership writers closed) (ga-m07q9) | 1,041 | `cmd/gc/beads_provider_lifecycle.go`, `cmd/gc/beads_doltlite_binding_test.go`, `cmd/gc/init_provider_readiness_test.go`, `cmd/gc/init_hosted_dolt.go`, `test/acceptance/helpers/city.go` | 5edf79c49, 3979c2785, 0a5650b6e, 3a22c00c8, c1d8fb731, 131239e8c | Diverges on purpose from upstream af7ad8a0f (#6273) | Keep through Phase 2. Delete in Phase 3 when voxist-city migrates to bd-owned proxied (ga-s69mk.4). It is part of the proxied inventory. |
| F17 | Honor the fork bd MIGRATION-FREEZE marker on native, proxied, and factory opens (ga-vwupk) | 1,562 | `internal/beads/native_dolt_migration_freeze{,_test}.go`, `internal/beads/native_dolt_store.go`, `internal/beads/native_dolt_proxied_open.go` (+15), `cmd/gc/beads_proxied_native.go` (+15), `cmd/gc/native_reopen_wiring_test.go` | 942a0313d, bcafd1555, 59b82caea, eaed6de41, 25eceea30, e33b69aa2 | The marker is a Voxist/beads carry (F10 in the bd census). ga-s69mk.3 notes it STAYS. | Keep it, concentrated in `native_dolt_migration_freeze.go`. The +15/+15 hooks into upstream proxied-native files are the only spread. |
| F21 | Provider-health registry fed by an in-process model proxy, plus failover chain-walk (vp-ljfo, ADR-0013 M3b) | 1,375 | `cmd/gc/provider_health_registry.go`, `cmd/gc/model_proxy.go`, `cmd/gc/session_reconciler.go` (hunks), `plans/vp-ljfo-provider-health-registry.md` | c30b6a346, eb6f75a85, dfd6eca8e, f29dd6618 | Upstream keeps the file-based provider-health gate (from fork PR #2962). The registry replaces it in the fork. | Isolate behind a providergov-style boundary. Delete `plans/` from the tree. If upstream's v2 allocator absorbs provider health, re-evaluate as B. |
| F22 | Provider governor: quota poller, typed `provider.*` signals, SelectProvider, re-entry hysteresis, `gc provider quota` (+JSON schema), agent tier classification | 2,520 | `internal/providergov/*`, `cmd/gc/cmd_provider{,_test}.go`, `schemas/provider/quota/result.schema.json` | b4605e3e3, 992481fe3, 755c34d5c, b4d001106, da67176dc, 5413d32b3, b9ee68853; vp-v43d | none | Fork-owned, already package-isolated. Keep `cmd/gc` wiring to one file. |
| F33 | Rig identity triad doctor check, project_id from the `identity_map` in city.toml, degrade-not-fatal per-rig init, gastown maintenance-script tests (vp-cz7o.*) | 1,866 | `cmd/gc/dolt_project_id{,_test}.go`, `internal/doctor/checks_rig_identity*.go`, `cmd/gc/beads_provider_lifecycle_degrade_test.go`, `examples/gastown/maintenance_scripts_test.go` | 06e3d1fe0, 6477ec12e, 28c96fcbf, 7cd74d91c, 3c28e31db | Driven by voxist-city's June data-loss incident | Isolate. The degrade-not-fatal rig init (28c96fcbf) is generic and could go upstream on its own. |
| F46 | Doctor M7 pr-delivery check (delivery phase rollup) | 331 | `cmd/gc/doctor_delivery_view{,_test}.go`, `internal/beadmeta/keys.go` | 83243796d | Depends on the delivery phase machine, which upstream reverted (#3334) | Move to voxist-city or voxist-platform tooling, or delete if unused. |
| F47 | Resync-loss gate: `check-resync-loss.sh` Gates 1–3, extractor, scratch-repo suite, pre-push wiring, `.gitattributes` exemptions (ga-8gpw4) | 3,204 | `scripts/check-resync-loss.sh`, `scripts/check-resync-loss-extract.py`, `scripts/test-check-resync-loss.sh` (1,897), `AGENTS.md` | 228f6b9e4, e81cd0f21, 594caabc3, 00e490c3b, 5879161cc | Exists only because the fork exists | Keep, but move under a fork-owned path (for example `fork/scripts/`) so it never conflicts. |
| F48 | `go clean -cache` ban (shim, scanner, installer) and go build-cache trim script with size cap (ga-zqc34) | 3,627 | `scripts/{check-go-clean-cache,go-clean-cache-shim,install-go-clean-cache-shim,trim-go-build-cache}.sh` and their four test scripts, `engdocs/contributors/go-clean-cache-guard.md` | 9d25fc8c7, 824862154, cfcda1ac8, bbd90e21c | Upstream accepted the AGENTS.md convention (#3474) but not the tooling | Host-ops tooling. Move to voxist-platform or a fork-owned dir. It does not belong in the SDK tree. |
| F49 | Acceptance `-skip` guard (deny-by-default, mvdan.cc/sh parse) and acceptance row selection (ga-may9k, ga-1ebvc) | 2,322 | `scripts/acceptance_run_skip_allowlist_fixture_test.go` (1,155), `scripts/acceptance_run_selection_test.go` (908), `scripts/acceptance_shell_skips_test.go` | cbfc11f5f, 6249bfec5, 84c574897, e7fc9165c, 3818e8231 | Guards fork CI (`ci.yml`), which drifts from upstream | Keep with F47, under a fork-owned path. The `go.mod` mvdan dependency is a small upstream-visible footprint. |
| F50 | Push and commit hooks: pre-push `LOCAL_TESTS_ACK`, push-ownership guard, push-gate lock, errorlint precommit guard (vc-dq0b, ga-grenu, ga-w7nyj) | 1,259 | `.githooks/pre-{push,commit}`, `scripts/push-ownership-guard.sh`, `scripts/test-pre-push-local-tests-ack.sh`, `scripts/precommit_contract_test.go` | bca909670, a35437043, 39dc2729f, 490d01b50, 99aea182c | Fork process | Keep, isolated. |
| F51 | `make install` stops writing `~/.local/bin/gc`; `make deploy-fleet`; install ownership tests | 1,145 | `Makefile`, `scripts/makefile_install{,_ownership}_test.go`, `cmd/writebuildmanifest/`, `CONTRIBUTING.md` | aa0be6506, 546f04539, bd039e822, 7073781ff | Fork deploy pipeline | Move the deploy targets into a fork-owned `fork.mk` included from the Makefile, so the upstream Makefile stays near-identical. |
| F52 | Test-tier and CI sharding infrastructure (re-tier heavy packages, noncmdgc cover shard, scripts-package split, tier coverage) (ga-4h8bu, ga-gmhbf, ga-dejjh) | 1,245 | `scripts/test-tier-packages`, `scripts/test-tier-coverage.sh`, `scripts/noncmdgc-shard-packages`, `scripts/*_test.go`, `TESTING.md` | 9849d6feb, 43c19aa82, 26c258aa6, d8f911616, f4baf71a0 | Fork CI shape | Keep isolated. Parts (re-tier, noncmdgc shard) are upstreamable if upstream CI wants them. |
| F53 | bd pin and deps: `BD_VERSION` split into version + source ref, fork bd repins (v1.91–v1.94.1), bdflags manifest refreshes, autostart acceptance test | 1,428 | `deps.env`, `scripts/bd_version_pin_test.go`, `internal/bdflags/bdflags.go`, `test/acceptance/beads_dolt_autostart_config_test.go`, `.github/workflows/ci.yml` | 0cd576e0b, 43d1758c3, 441a8790d, af380f9b9, 4e11605e2; ga-kgluj ga-xwn1k ga-b2q0c | Fork pins Voxist/beads | Keep. Shrinks as Voxist/beads converges (bd census). bdflags manifest entries for upstream bd flags are upstreamable. |
| F54 | Security floors, CVE waivers, betterleaks Voxist secret scan, mail and agent image upgrades | 1,255 | `scripts/container_tool_security_test.go`, `.github/requirements/mcp-agent-mail.*`, `.voxist-betterleaks.toml`, `.github/workflows/voxist-secret-scan.yml`, `.trivyignore.yaml`, `contrib/k8s/Dockerfile.*` | 1254547f7, 006c37e4d, ce2d495fb, 622f442b5, db162ee24 | The secret scan is Voxist-only. CVE floors are generic. | Upstream the CVE floors (and drop them once upstream bumps). Keep `voxist-*` files. |
| F55 | macOS CI lane (icu4c discovery) and fork-verify workflow tweaks | 214 | `.github/actions/setup-gascity-macos/action.yml`, `.github/workflows/{fork-verify,mac-regression}.yml` | 6ae24600c, 88dd0d75c, 267001803, a96edcb3f | upstream has fork-verify (#3674) | Upstream the icu4c fix. Keep the rest. |
| F56 | City-scale architecture plan, load-test results doc, `test/loadharness` | 1,359 | `test/loadharness/*`, `engdocs/contributors/city-scale-architecture-plan.md`, `engdocs/contributors/load-test-2026-06-10.md` | df0dd7d0d, bc361c1e9, 2e1244a4f | Historical plan (June) | Move the docs to the fork wiki or voxist-platform. Delete the loadharness or upstream it as a benchmark. |
| F58 | AGENTS.md, CHANGELOG, .gitignore fork notes | 36 | `AGENTS.md`, `CHANGELOG.md`, `.gitignore` | 44c802509, dc9f7ce5b | — | Keep. CHANGELOG is union-merged. |

#### E. RESYNC-DEBT

| id | feature | +lines | key files | commits | action |
|---|---|---:|---|---|---|
| F59 | Ratchet, census, and policy bookkeeping: resource-census baselines, test-resources.toml, cipolicy CI/nightly hash re-derivations, go.sum, doctor-name and chargolden goldens, workquery goldens, testenv_import stubs, BUILD.bazel drift | 982 | `scripts/cipolicy/policy{,_test}.go`, `go.sum`, `test/test-resources.toml`, `scripts/ci_critical_path_slow_test.go`, `internal/beads/caching_store_reconcile_census_test.go` | 96 resync-repair commits (fix(resync), merge-fix, `#137 round N`, ratchet bumps) | Regenerate per AGENTS.md rule 1. It shrinks automatically as C and D features leave the fork. |
| F60 | Upstream divergence: the fork carries an older revision of upstream lines (for example `checks_order_firing.go` from 141573f96, `test-local-parallel` herdr exclusion, `init_provider_readiness_test.go`) | 204 | `internal/doctor/checks_order_firing.go`, `scripts/test-local-parallel`, `cmd/gc/init_provider_readiness_test.go` | merge resolutions | Take upstream on resync. The current worktree's uncommitted edits to `checks_order_firing.go` and `checks_order_outcome.go` touch this area. |

Resync churn, reported separately: 96 of the 575 fork commits exist only to repair merges. They authored 10,591
lines still present in the delta, mostly re-adding fork code that the 2026-08-31 (#137) and 2026-09-13 merges
dropped. Those lines are credited to their features above.

---

### Top 10 largest delta-reduction opportunities

Ranked by delta lines removed for the effort.

1. **Proxied opt-in, store-health patrol, and topology policy (Phase 2 + 3): about 4,134 lines** (F18 1,769 +
   F19 1,324 + F16 1,041), plus about 300 lines of docs/schema and generated output. F18 and F19 are inert in
   production (voxist-city: `proxied = false`), and upstream reverted F18's design (#3334). Phase 2 deletes
   F18 and F19 now; F16 goes in Phase 3.
2. **F01 binary-divergence doctor → upstream `supervisor-binary` (#6724): 3,728 lines.** Pure deletion after
   a coverage diff.
3. **Store-resilience PR stack already open upstream (#5945–#5966, #4839): about 8,036 lines** (F08 3,537 +
   F10 2,185 + F12 1,737 + F13 577). Once merged, these drop out. Re-scope #5945–#5947 (store-health) per item 1.
4. **F03 managed-Dolt recover gate: 5,771 lines.** The largest C feature, with no upstream equivalent. Upstream
   it as a series after the resync (it shares `gc-beads-bd.sh` with F14 and F15).
5. **Fork dev tooling moved out of the SDK tree: about 11,557 lines** (F48 3,627 + F47 3,204 + F49 2,322 +
   F50 1,259 + F51 1,145). This is not upstreamable, but moving it under a fork-owned path or into
   voxist-platform removes it from every resync conflict surface.
6. **Orders under store outage (F04 3,499 + F05 1,631 = 5,130 lines).** Self-contained, cmd/gc-only, and
   generic. Upstream it after F03.
7. **F28 native-store canary and custom-types preflight → retire: about 2,155 lines.** Default-off, unused in
   voxist-city, and superseded by native-store-on-everywhere plus upstream G4 (#7214).
8. **F14 + F09 already upstream (#7265, #7748/#7750): 1,578 lines.** Take upstream's version on conflict; this
   costs nothing beyond the resync.
9. **F07 delivery window and sync drain: 3,498 lines.** Generic managed-Dolt backup reliability. One
   dolt-pack PR.
10. **Provider stack (F20 1,800 + F21 1,375 + F22 2,520 = 5,695 lines).** Land credentials (#3525). Keep
    providergov and the provider-health registry fork-owned, but confine `cmd/gc` wiring to one or two files,
    which today are spread across `session_reconciler.go`, `city_runtime.go` and `cmd_supervisor_lifecycle.go`.

Honorable mentions:

- F06 run_on (2,535): rename `VOXIST_FLEET_ROLE` to make it upstreamable.
- F23 reconciler fixes (2,083): upstream's controller v2 rewrite will either absorb or break these. Port them as
  v2 regression tests before the legacy code is deleted upstream.
- F27 status: drop 437 lines of `engdocs/plans`.
- F35 beads-state: drop the Python duplicate.

---

### Phase 2 inventory: fork `[beads] proxied` / `proxy_pool_size` opt-in machinery

Live state: voxist-city `city.toml:1073-1074` has `proxied = false` and `proxy_pool_size = 2`. The opt-in is
**off in production**. `proxy_pool_size` is still **read**, by the S6 check (item 9).

Upstream equivalents, all present on `origin/main`:

- bd-owned proxied-server, the default for new cities since af7ad8a0f (#6273)
- `gc beads city migrate-proxied` (`cmd/gc/cmd_beads_city_migrate_proxied.go`)
- `[beads] proxied_idle_timeout` and the rig override `beads_proxied_idle_timeout` (`cmd/gc/beads_proxied_idle_timeout.go`; #7097/#7102/#7107; default 30m)
- the proxied-native lane (`cmd/gc/bd_env_proxied*.go`, `cmd/gc/beads_proxied_native.go`, `internal/beads/proxied_*`, `internal/beads/proxyendpoint/`)
- fast no-op stop of a proxied city (e39d98ccf, #7335)
- never start a stopped proxied store (084b3e7cc, #6817)

Upstream has **no** `proxy_pool_size` and no gc-side db-proxy reap; bd owns the proxy lifecycle.

| # | Piece (fork/main file:line) | What it does | Upstream equivalent | Verdict |
|---|---|---|---|---|
| 1 | `internal/config/config.go:1427-1452` (comments from 1427): `BeadsConfig.Proxied` (1435), `ProxyPoolSize` (1441), `ProxyIdleTimeout` (1452) | TOML `[beads] proxied`, `proxy_pool_size`, `proxy_idle_timeout` | `proxied_idle_timeout` (different key, bd-owned semantics). No pool size. No `proxied` bool; topology lives in bd metadata. | **Delete.** Register `beads.proxied`, `beads.proxy_pool_size` and `beads.proxy_idle_timeout` in `internal/config/undecoded.go` `retiredKeys`, or delete the two lines from voxist-city `city.toml` in the same change. Unknown keys are fatal at load, and voxist-city sets two of them. |
| 2 | `internal/config/config.go:1667-1706`: `ProxiedEnabled()`, `defaultBeadsProxyPoolSize`, `ProxyPoolSizeOrDefault()`, `defaultBeadsProxyIdleTimeout`, `ProxyIdleTimeoutOrDefault()` | Accessors | — | **Delete.** Callers are listed in rows 4–9. |
| 3 | `internal/config/config_test.go:8455` `TestBeadsProxiedGateAccessors`, `:8487` `TestBeadsProxyIdleTimeoutAccessor`, `:8521` `TestParseBeadsProxiedSection` | Accessor and parse tests | — | **Delete.** |
| 4 | `cmd/gc/beads_proxied_overlay.go` (176 lines): `proxiedServerClientInfo*` mirror types, `bdSupportsProxiedServer()` (`bd init --help` probe, :67), `proxiedServerScopeActive()` (:88), `applyProxiedPoolEnv()` (:97, sets `GC_BEADS_PROXIED`, `GC_BEADS_PROXY_POOL_SIZE`, `BEADS_PROXY_POOL_SIZE`, `GC_BEADS_PROXY_IDLE_TIMEOUT`, `BEADS_PROXY_IDLE_TIMEOUT`), `applyProxiedServerScopeOverlay()` (:126, flips `metadata.json` dolt_mode and writes `proxied_server_client_info.json`) | Opt-in overlay and env plumbing | Superseded by bd-owned proxied and `bd_env_proxied.go` | **Delete. Already dead code:** `applyProxiedPoolEnv` and `applyProxiedServerScopeOverlay` have no production callers (only `beads_proxied_overlay_test.go`), and `gc-beads-bd.sh` no longer reads `GC_BEADS_PROXIED*`. **Entangled:** the constant `proxiedServerClientInfoFile` (:30) is used by F31 `cmd/gc/cmd_doctor_portfile.go:127,155`. Move it there as a literal, as upstream does. `bdSupportsProxiedServer` is used only by row 6. |
| 5 | `cmd/gc/beads_proxied_overlay_test.go` (199 lines): `TestApplyProxiedServerScopeOverlay*` (:67, :89), `TestApplyProxiedPoolEnvInjectsOnlyWhenGateOnAndBDCapable` (:119), `TestBeadsProxiedCapabilityCheck` (:159) | Tests | — | **Delete.** |
| 6 | `cmd/gc/doctor_beads_proxied.go` (52 lines), registered at `cmd/gc/cmd_doctor.go:325` | Doctor check `beads-proxied-capability` (advisory: proxied=true but bd lacks `--proxied-server`) | Upstream doctor has proxied-specific checks (proxied idle timeout, preflight hints) | **Delete** the file and the registration line. Remove `beads-proxied-capability` from `cmd/gc/testdata/doctor_check_names.golden:46`. |
| 7 | `cmd/gc/beads_proxy_reap.go` (142 lines): `proxiedScopeBeadsDirs`, `proxyChildPIDsFromPS` (ps scan for `bd db-proxy-child --root`), `proxyChildRootArg`, `reapProxiedChildrenForCityPath`/`reapProxiedChildrenForCity` (:95, :107; gated on `ProxiedEnabled()`), `signalProxyChild` (SIGTERM, then SIGKILL) | gc owns the never-idle db-proxy lifecycle and reaps on `gc stop` | bd owns the proxy lifecycle with its idle timeout; upstream stop paths handle proxied cities | **Delete.** Callers: `cmd/gc/dolt_stop_managed.go:89-96` (`defer reapProxiedChildrenForCityPath` when `clearPublishedState`; remove those four lines and the comment) and row 9's `storehealth_patrol.go`. `cmd/gc/beads_proxy_reap_test.go` (85 lines) goes too. |
| 8 | `internal/events/proxy_payloads.go` (39 lines): `ProxyReaped = "proxy.reaped"`, `ProxyReapedPayload`, `RegisterPayload`; `internal/events/events.go:532-533` (`KnownEventTypes` entry with the "Fork-owned" comment) | Typed event for reaps | — | **Delete** both. `TestEveryKnownEventTypeHasRegisteredPayload` stays green because both sides go together. Regenerate `openapi.json`, `genclient` and the dashboard TS if the event is in the SSE schema. |
| 9 | `cmd/gc/storehealth_patrol.go` (402 lines; started from `cmd/gc/controller.go:1433` and `cmd/gc/cmd_supervisor.go:2535`). Proxied gate at `:45` (`if !cfg.Beads.ProxiedEnabled() { return }`); reap hooks at `:132-133`, `:154-160`; forensics on the proxy log `:293-338`; `reapProxiedChildForScope` `:386-394` (gated again at `:387`) | Store-health patrol, a no-op unless proxied | none (fork PRs #5945–#5947 open upstream) | **Delete the whole file** with the two call sites. Also delete `internal/storehealth/patrol.go` (362), `quarantine.go` (104), `patrol_test.go` (321) and `quarantine_test.go` (88). Keep upstream's `internal/storehealth/storehealth.go`, which is the unrelated size/row summary. Also delete `config.StoreHealthConfig` (`internal/config/config.go:298`, `:2748-2805`) and the `[storehealth]` entries in `validate_durations.go`, and register `storehealth` as a retired key. Delete the `store.degraded`, `store.recovered` and `store.probe_failed` events (`internal/events/storehealth_payloads.go:17-47`, `:66-94`, `:140-142`, and `events.go:529`). |
| 10 | `cmd/gc/supervisor_doctor.go:126-137`: S6 connection ceiling uses `cfg.Beads.ProxyPoolSizeOrDefault()` as `PoolSize` (`internal/supervisordoctor/doctor.go:119-160`); tests `cmd/gc/supervisor_doctor_test.go:207` and `:226` set `cfg.Beads.ProxyPoolSize` (`:213`, `:229`) | Connection-ceiling alert (scopes × (pool+1) ≤ 0.8 × max_connections) | — | **Entangled with code that stays (F12).** Re-key `PoolSize` to the bd admission cap (F13, `[beads.resilience]`), or drop S6. This is the **only live production reader** of `proxy_pool_size` (voxist-city sets it to 2). |
| 11 | `internal/beads/contract/preflight_checker.go:253-280`: the fork flips `dolt_mode_safe` for proxied-server from upstream's **Pass** to **Fail**, with a long rationale comment | Prevents the a74fefde8 zero-session outage (embedded-DB misroute) | Upstream: Pass, plus its own proxied-native lane | **Must stay through Phase 2** (ga-8r7c1, P0). Retire in Phase 3 only after the upstream proxied-native open path is verified on a voxist-city rehearsal (ga-s69mk.4). |
| 12 | F16 topology policy (ga-m07q9): `cmd/gc/beads_provider_lifecycle.go` (`defaultFreshScopeDoltMode="server"` / `explicitProxiedDoltMode` split, `preInitScopeDoltMode`/`postInitScopeDoltMode`), `cmd/gc/init_hosted_dolt.go`, `cmd/gc/beads_scope_ownership.go`, tests in `cmd/gc/beads_doltlite_binding_test.go:193-221+`, `init_provider_readiness_test.go`, `test/acceptance/helpers/city.go`, `test/acceptance/beads_proxied_default_test.go`, `test/integration/tutorial_path_test.go` (1,041 lines) | Rejects upstream's proxied-by-default so new cities stay server mode. Comments reference "config.Proxied stays defaulting false." | Upstream af7ad8a0f proxied by default | **Not part of Phase 2.** Keep it, but update the comments that reference `config.Proxied`. Delete in Phase 3 together with row 11. |
| 13 | Migration-freeze hooks inside upstream proxied files: `cmd/gc/beads_proxied_native.go` +15 (`open()` and `reopen()` call `CheckMigrationFreeze`) and `internal/beads/native_dolt_proxied_open.go` +15 | F17 (ga-vwupk) | — | **Stays** (ga-s69mk.3: the freeze stays). Not part of the opt-in machinery. |
| 14 | Docs and schema: `docs/reference/config.md:44` (`storehealth`), `:305-307` (`proxied`, `proxy_pool_size`, `proxy_idle_timeout`), `:942+` (`## StoreHealthConfig`); `docs/reference/schema/city-schema.{json,txt}:1103-1117` (the three keys), `:1352-1353`, `:3215-3245` (StoreHealthConfig) | Generated | — | **Regenerate** after rows 1 and 9; do not hand-edit. |
| 15 | `engdocs/contributors/proxy-patches/` (5 files: `0001-bd-phase0-portfile-clobber.patch` 183, `0002-gascity-phase1-proxy-lifecycle.patch` 402, `0004-…-native-.patch` 66, `PROXIED-FULL-SOLUTION-PLAN.md` 128, `README.md` 118), plus mentions in `engdocs/contributors/city-scale-architecture-plan.md:235,279` | Historical patch series and plans | — | **Delete** (keep in git history). Update the two plan mentions, or delete the plan with F56. |
| 16 | Env plumbing: `GC_BEADS_PROXIED`, `GC_BEADS_PROXY_POOL_SIZE`, `BEADS_PROXY_POOL_SIZE`, `GC_BEADS_PROXY_IDLE_TIMEOUT`, `BEADS_PROXY_IDLE_TIMEOUT` (only in `beads_proxied_overlay.go:110-115` and its test). bd's pool-size reader is in the Voxist/beads pooling carry (F1–F5 in the bd census). | Pool and idle-timeout env to bd | Upstream: `config.ProxiedIdleTimeoutEnv` via `bd init --proxied-server-idle-timeout` | **Delete** (dead on the gc side already). The bd-side reader goes in the separate Voxist/beads PR after #71 (ga-s69mk.3 note). |
| 17 | Not proxied, but the same name family, so do not delete: `GC_BEADS_PROXIED_NATIVE`, `GC_BEADS_PROXIED_GUARD_INTERVAL`, `GC_BEADS_PROXIED_READ_BUDGET` (`internal/beads/proxied_flag.go`), `BEADS_PROXIED_SERVER_ROOT_PATH` (`internal/beads/proxyendpoint/record.go:69`), `GC_DOLT_SCOPE_BD_PROXIED` (`examples/bd/dolt/assets/scripts/runtime.sh:231,241`), `cmd/gc/cmd_beads_city_migrate_proxied.go`, `cmd/gc/dolt_cleanup_{bd_proxy,proxied}.go` | Upstream's proxied-native lane and migration | — | **Upstream-owned. Leave alone.** |

**Deletion summary for Phase 2:** rows 1–9 and 14–16. That is about 2,400 delta lines plus about 400 lines of
generated docs and schema. Pure deletes: rows 3, 5, 6, 7 (after the stop hook), 8, 9, 15 and 16.

**Must-edit entanglements:**

- `cmd_doctor_portfile.go` (row 4): move the constant.
- `dolt_stop_managed.go:89-96` (row 7): remove the defer.
- `controller.go:1433` and `cmd_supervisor.go:2535` (row 9): remove the patrol start.
- `supervisor_doctor.go:134` and its tests (row 10): re-key or drop S6.
- `cmd_doctor.go:325` (row 6): remove the registration.
- `events.go:529,532-533` (rows 8 and 9).
- `undecoded.go` retired keys plus the voxist-city `city.toml` (row 1).

**Must stay in Phase 2:** row 11 (dolt_mode_safe Fail), row 12 (F16 topology), row 13 (freeze), row 17
(upstream's code).

Bazel note: `cmd/gc/BUILD.bazel` and `internal/storehealth/BUILD.bazel` do not list the fork files
(`patrol.go`, `quarantine.go`, `beads_proxy_reap.go`, and others). The Bazel lane has drifted from the fork
tree, so deleting them needs no BUILD edit.

#### Beads and issues referencing this machinery

- **ga-s69mk** (epic, P1, open): converge the fork onto upstream (2026-10-14).
  - ga-s69mk.1: Phase 0 census (this report).
  - ga-s69mk.2: Phase 1 resync, topology unchanged.
  - **ga-s69mk.3**: Phase 2, retire the fork `[beads] proxied` and `proxy_pool_size` opt-in in gc, plus the bd pooling patches. Note: the bd side is F1–F5 only, and the F10 MIGRATION-FREEZE stays.
  - **ga-s69mk.4**: Phase 3 prep, rehearsal and runbook for migrating voxist-city to bd-owned proxied-server.
- **ga-8r7c1** (P0, open): resync near-miss where upstream flips `checkDoltModeSafe(proxied-server)` from Fail to Pass, which would reproduce the a74fefde8 outage (row 11).
- **ga-m07q9** (P1, open): the fork diverges from upstream's proxied-by-default topology (row 12 / F16).
- ga-kcebr (P2, open): the `[beads] proxied` backend (dbproxy/server.DoltServer) spawns dolt sql-server with no auto-start policy check.
- ga-aytqx (P2, open): the proxied-server doctor hint is missed when another preflight fails first.
- ga-12emr (P3, open): proxied Ping/Recover rungs are gated only for MIGRATION-FREEZE, not for a stopped city.
- ga-2hgoz (P2, open; the F15 fix is merged on the fork): `TestBeadsProxiedDefault/legacy-managed-city-unchanged` flake.
- ga-le1af (P2, open): storehealth size/row threshold calibration. This is upstream's `storehealth.go` summary, not the patrol.
- ga-may9k (closed): proxied acceptance rows `-skip` guard (F49).
- Historical, in commit messages only: vp-3vx2 (M1 proxied-server pooling overlay, gascity #1978), ga-vwupk (freeze on proxied opens), ga-w9xm9 (proxied-native acceptance skips).
- Upstream PRs to close or re-scope with Phase 2: #5945 (storehealth quarantine), #5946 (storehealth patrol), #5947 (`[storehealth]` config). #5953 (events) contains the store.* payloads that would also go.

## beads (bd)

Summary of the Voxist/beads census (`origin/main` 9c4b3d8ba, `bfork/main`
cbb160158, merge-base b1bee123b). The fork's net delta is 136 files,
+10,632 / -417, with no patch-id equivalents upstream.

- **gc links no fork library code.** gascity's `BD_LIB_REF` is an upstream
  commit; the fork matters only as the `bd` binary gc and agents exec.
- **Proxied pooling (F1-F5) is fork-only code upstream declined**, not code
  upstream superseded: upstream's db-proxy dials one backend connection per
  client connection and has no pool. Dropping it is a pure deletion. It goes
  in a separate Voxist/beads PR after Voxist/beads#71, once gascity has
  dropped the `[beads] proxied` opt-in (ga-s69mk.3).
- **Must survive:** F16, the SHOW COLUMNS store-open probe (upstream #7297
  open). Without it every write-capable open on the shared 9-DB server pays
  a cross-database INFORMATION_SCHEMA scan (4.3 s observed).
- **Should survive:** F13, workspace `dolt.auto-start: false` honored without
  the env var. gc always injects `BEADS_DOLT_AUTO_START=0`, so this is
  defense in depth for bd run outside gc's environment.
- **Already upstream or net-zero:** F4, F7, F8, F11, F12 (one residue test),
  the grpc half of F17.
- **Version line:** the fork's 1.94.x numbering sits above upstream's 1.3.0,
  so an upstream binary registers as a downgrade against every live DB's
  `bd_version` high-water mark. Plan the version string before any cutover.
- **Recommended path:** rebuild the fork as `origin/main` plus a minimal patch
  stack (F16, F13, optional F14, x/crypto, fork ops, a version stamp) and
  merge it forward into `bfork/main` without rewriting history.

**Correction to the beads census, F10 (MIGRATION-FREEZE): the gate STAYS.**
The census recommended dropping the fork's store-open freeze gate in favour of
upstream's migration-consent gate (#5764). That reasoning covers only the
"a read under a freeze migrates" hazard. The fork gate also refuses CLI writes
during a maintenance window, which the consent gate does not, and
voxist-platform's safe-migrate flow depends on that refusal. Resolve the
freeze-against-consent conflict by keeping both gates, not by taking upstream.
gascity's own read-side freeze hooks on the proxied open paths
(`CheckMigrationFreeze` in `cmd/gc/beads_proxied_native.go` and
`internal/beads/native_dolt_proxied_open.go`) stay for the same reason.

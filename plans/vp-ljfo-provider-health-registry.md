# Plan: gc-core provider-health registry + auto-failover (vp-ljfo)

## Summary

Add a controller-resident provider-health registry fed by an in-process HTTP
reverse proxy that observes 429/401 on every model API call. Replace the current
file-polling gate (`loadProviderHealthSnapshot`) — which depends on a gc ORDER
and the beads store — with a live in-memory registry that survives store/dispatch
failure. Add chain-walk selection so the reconciler auto-selects the next healthy
provider instead of just blocking spawns.

### Key design decisions

| Decision | Choice | Rationale |
|---|---|---|
| Registry location | `cmd/gc/provider_health_registry.go` | Same package as the gate it replaces; avoids new internal/ package until this stabilises |
| Proxy placement | `cmd/gc/model_proxy.go` | Controller-owned; starts on `localhost:0`; wired into `CityRuntime` |
| Provider upstream lookup | `config.Providers[name].Env["ANTHROPIC_BASE_URL"]` (or Anthropic default) | Already in city.toml; no new config field needed for URLs |
| Credential transport | Session's own headers (from provider env) — proxy passes them through | Proxy never needs to manage secrets |
| Chain-walk entry | Per-tick, inside `reconcileSessionBeadsTracedWithNamedDemand` | Same site as current file-read; minimal diff |
| Config field | `[daemon] failover_chain = ["claude","claude2","zai","dashscope-anthropic","openrouter"]` | Daemon block already owns controller-level policy |
| Cooldown | 5-minute window; 3 consecutive 429s → red; 120s with no 429s → green | Matches pack-side watcher policy; reduces thrash |
| Backward compat | Keep `loadProviderHealthSnapshot` file path readable as a secondary signal | Smooth transition; file-based gate still fires if registry has no entry |

### Architecture diagram

```
[agent session] ──ANTHROPIC_BASE_URL──▶ [model proxy :0]
                                              │ /proxy/{provider}/v1/...
                                              ▼
                               [provider upstream API]
                                              │
                              RecordResponse(provider, status)
                                              ▼
                               [ProviderHealthRegistry (in-memory)]
                                              │
                           SelectHealthy(failoverChain) ─────▶ [reconciler spawn path]
```

## Micro-task table

| id | description | acceptance | est_minutes | slings |
|---|---|---|---|---|
| T-001 | Write failing test `TestProviderHealthRegistry_BasicRecord` in `cmd/gc/provider_health_registry_test.go` — verify `RecordResponse` and `Check` types exist | Compile error: `providerHealthRegistry` undefined | 2 | — |
| T-002 | Implement `providerHealthRegistry` struct with `RecordResponse(provider string, status int, now time.Time)` and `Check(provider string) (healthy, present bool)` | T-001 green | 4 | — |
| T-003 | Write failing test `TestProviderHealthRegistry_Cooldown` — after 3 × 429 in 5 min, `Check` returns `(false, true)`; after 120s without 429, returns `(true, true)` | Test fails (no cooldown logic) | 2 | — |
| T-004 | Implement cooldown threshold + failback window in `providerHealthRegistry` | T-003 green | 3 | — |
| T-005 | Write failing test `TestProviderHealthRegistry_SelectHealthy` — `SelectHealthy(["claude","zai"])` with claude red returns "zai"; all-red returns "" | Test fails (method missing) | 2 | — |
| T-006 | Implement `SelectHealthy(chain []string) string` — chain-walks to first non-red, non-absent entry | T-005 green | 3 | — |
| T-007 | Write failing test `TestModelProxy_ForwardsAndRecords` in `cmd/gc/model_proxy_test.go` — proxy routes `POST /proxy/claude/v1/messages` to a fake upstream; records 429 in registry | Test fails (handler missing) | 2 | — |
| T-008 | Implement `modelProxyHandler` — `http.Handler` that extracts `{provider}` from path, looks up upstream URL from config (or Anthropic default), reverse-proxies, calls `registry.RecordResponse` on response | T-007 green | 5 | — |
| T-009 | Write failing test `TestModelProxy_ProviderRouting` — requests for two providers route to their respective upstream test servers | Test fails (routing not scoped per-provider) | 2 | — |
| T-010 | Wire per-provider upstream URL resolution into `modelProxyHandler` using `config.City.Providers` | T-009 green | 3 | — |
| T-011 | Write failing test `TestReconcilerUsesLiveRegistry` in `cmd/gc/session_reconciler_test.go` — pass a pre-populated registry (claude=red); expect spawn skipped even when no `provider-health.json` file exists | Test fails (still reads file) | 2 | — |
| T-012 | Replace `phSnap := loadProviderHealthSnapshot(cityPath)` with `phSnap := registry.Snapshot()` in `reconcileSessionBeadsTracedWithNamedDemand`; update call sites and signatures to pass `*providerHealthRegistry` instead of `cityPath` for the health check | T-011 green | 4 | — |
| T-013 | Write failing test `TestReconcilerChainWalkSelectsAlternate` — configured provider "claude" red, registry has "zai" green, failover chain ["claude","zai"]; expect session spawned with provider "zai" resolved env | Test fails (no chain-walk in spawn path) | 2 | — |
| T-014 | Add chain-walk in spawn path: when `tp.ResolvedProvider` is red, call `registry.SelectHealthy(chain)` to get alternate; re-resolve provider via `config.ResolveProvider(agentWithAlt, ws, cfg.Providers, lookPath)` and override `tp`'s provider fields and env; skip spawn only when all-red | T-013 green | 5 | — |
| T-015 | Write failing test `TestBuildDesiredState_InjectsProxyBaseURL` — when `proxyAddr` is set on the controller context, session `tp.Env["ANTHROPIC_BASE_URL"]` equals `http://localhost:{port}/proxy/{provider}` | Test fails (no injection) | 2 | — |
| T-016 | Wire proxy URL injection in `buildDesiredState`: when `proxyAddr != ""`, set `tp.Env["ANTHROPIC_BASE_URL"] = proxyAddr + "/proxy/" + tp.ResolvedProvider.Name` (overrides provider-config value; provider still supplies credentials) | T-015 green | 3 | — |
| T-017 | Write failing test `TestFailoverChainConfig` — parse `[daemon] failover_chain = ["claude","zai"]` in city.toml; expect `cfg.Daemon.FailoverChain == ["claude","zai"]` | Test fails (field missing) | 2 | — |
| T-018 | Add `FailoverChain []string \`toml:"failover_chain,omitempty"\`` to `config.Daemon`; wire into `reconcileSessionBeadsTracedWithNamedDemand` via `CityRuntime` | T-017 green | 3 | — |
| T-019 | Write failing test `TestCityRuntime_StartsModelProxy` — `newCityRuntime` with proxy enabled starts the proxy and exposes a non-empty `ProxyAddr` | Test fails (no proxy startup) | 2 | — |
| T-020 | Wire model proxy startup into `CityRuntime.init`: start `modelProxyHandler` on `localhost:0`, store bound addr as `cr.modelProxyAddr`, pass to `buildDesiredState` and registry | T-019 green | 4 | — |
| T-021 | Run `make test-fast-parallel` and `go vet ./...`; fix all regressions | Zero failing tests, zero vet warnings | 4 | — |

## Open questions

- Should `SelectHealthy` fail-open (return chain[0] when all providers are unknown) or fail-closed (return "" and skip spawn)? Proposal: fail-open to chain[0] so an unconfigured registry has no effect.
- Cooldown window (5 min / 3 hits / 120s recovery): matches pack-side watcher today; confirm with Karel before finalising.
- Should `proxyAddr` injection be guarded by a config flag (`[daemon] model_proxy = true`)? Proposal: on by default when `failover_chain` is non-empty.

## GDPR data-flow impact

None. The model proxy observes HTTP status codes only (429/401) and the provider
name from the URL path. No message bodies, prompts, completions, or PII are
inspected or logged. The `ProviderHealthRegistry` stores only provider-name strings
and timestamps. No new data flows across process or network boundaries beyond the
existing agent→Anthropic API path.

## MDR Class I traceability

No-op. This change has no effect on the voxmemo clinical documentation pipeline
(microphone → voxist-api ASR → clinical note). Provider failover logic operates
at the gc session-orchestration layer, upstream of any clinical transcription flow.
No chain-of-evidence metadata is modified.

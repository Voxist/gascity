# Proxied db-proxy reliability + lifecycle — handoff for the pooling PRs

Root-caused and fixed 2026-06-06. Three phases. Phases 0 and 1 are
implemented, locally validated, and shipped as the patches in this directory.
Phase 2 is a design spec (optimization, not incident-critical).

Scrub note: these patches and this doc carry **no** Voxist identifiers; they are
ready to hand to the upstream pooling PRs as-is.

## The bug (single source of truth)

A proxied-server city stalled fleet-wide: agents active, 0 work claimed, every
`gc` command logging "bd context unreachable", `gc doctor` reporting dolt-drift
on every scope. `.beads/dolt-server.port` held an **ephemeral db-proxy listener
port** instead of the managed Dolt port, and that value went **stale** the
moment the proxy respawned on a new port.

### Confirmed mechanism (reproduced locally + stack-traced)

`newProxiedServerRoutedStore` (`cmd/bd/uow_factory.go`) makes the legacy
store-based commands (`list`, `ready`, `update`, `--claim`, `close`) work in
proxied mode by opening a **`ServerMode: true`** `dolt.Config` pointed at the
db-proxy's ephemeral listener:

```
cfg := &dolt.Config{ ServerMode: true, ServerHost: "127.0.0.1", ServerPort: pf.Port }
dolt.New(ctx, cfg)  ->  newServerMode  ->  store.go: EnsurePortFile(beadsDir, cfg.ServerPort)
```

Because the connection looks like a localhost server, `newServerMode` persisted
`pf.Port` (the **proxy** port) into `.beads/dolt-server.port`. The call-site
comment already guarded against auto-*spawning* a server for that port but
missed the port-file *persist*.

Reproduction (external dolt on a fixed port + `bd init --proxied-server` + one
`bd` op): before the fix `dolt-server.port` = proxy port; after, it is absent
(never written by the proxied path), and store commands still work through the
proxy. A `runtime/debug.PrintStack()` in `writePortFile` pinned the exact
caller chain above.

### Why it stalls the fleet

`dolt-server.port` is a multi-writer compatibility mirror. gascity writes the
managed port (48770); the bd proxied routed store overwrote it with the proxy's
ephemeral port. On the next proxy respawn (idle-death → new port), the file is
stale, and any resolver that trusts it targets a dead listener → connection
fails → claim writes silently no-op → nothing transitions to in_progress.

Note: the earlier "proxy-pool leak (8 vs configured 4)" framing was a category
error — 8 = one db-proxy-child **per scope** (7 rigs + city), 4 = backend
**connections within** one proxy. Not a leak; it is the fragmentation Phase 2
consolidates.

## Phase 0 — bd: proxied routed store must not clobber dolt-server.port

Patch: `0001-bd-phase0-portfile-clobber.patch` (apply onto the beads
connection-pooling branch).

- New `dolt.Config.RoutedThroughProxy`; set it in `newProxiedServerRoutedStore`.
- Gate the port-file persist behind `shouldPersistPortFileForConfig(cfg)` so only
  the canonical local dolt sql-server records its port; a proxied routed store
  never does.
- Pure-Go unit test covers the persist matrix
  (`internal/storage/dolt/port_file_persist_test.go`).

Result: with Phase 0, `dolt-server.port` becomes a **single-owner** artifact —
gascity writes the managed port, bd never clobbers it. This alone removes the
stall mechanism. Validated by re-running the reproduction (port file no longer
written with the proxy port) + `go test` + `go build ./...` + `go vet`.

## Phase 1 — gascity: gc owns the db-proxy lifecycle (never-idle + reap)

Patch: `0002-gascity-phase1-proxy-lifecycle.patch` (apply onto gascity #3082).

Any finite idle timeout is starved by sparse controller probes → the proxy
spawns, serves one op, idle-dies, respawns — churn that never reaches the
warm-pool steady state. So:

- Default `proxy_idle_timeout` to **"0"** (never idle). gc owns the lifecycle:
  the proxy stays warm for the city's lifetime; a crash at worst leaves a
  still-usable warm proxy.
- Reap the proxies on genuine `gc stop` (`reapProxiedChildrenForCity`, wired
  into `stopManagedDoltProcessWithOptions` on the `clearPublishedState` path).
  Discovery is by **live process table** matched on the `db-proxy-child --root`
  under each scope's `.beads` — never by pidfile (a stale pidfile must not make
  us signal an unrelated PID; the process table is the source of truth).

bd needs **no change** for Phase 1 — `--idle-timeout 0` is already supported and
the spawn-or-reuse path keeps a single warm proxy. Validated locally: with
idle-timeout 0 one proxy (pid stable) is reused across calls; with a 1s timeout
it dies between sparse calls (the churn). Unit tests cover root enumeration, ps
parsing, and the proxied-off no-op. `go build ./cmd/gc` + `go vet` + schema
regen all clean; pushed green-or-pending on fork-verify CI.

Deploy note: voxist `city.toml` still pins `proxy_idle_timeout = "10m"`
explicitly; drop that line (or set "0") so the new default takes effect, then
`gc stop && gc start`.

## Phase 2 — bd: consolidate per-scope proxies into one shared pool (DESIGN)

Not incident-critical — Phases 0+1 resolve the stall and the churn. Phase 2 is
the 8→1 process consolidation (`BackendLocalSharedServer`, already stubbed
"not yet implemented" in `internal/storage/dbproxy/proxy/db_proxy_child.go`),
which also bounds total Dolt connections (today: up to 8×pool_size fragmented).

The deciding technical question for cstar: **is the warm backend pool
database-pinned?** The proxy is a transparent MySQL wire proxy
(`runPooledSession` borrows a warm backend and forwards the client handshake);
`ExternalDoltServer.DSN(database, …)` takes a database. Two implementation paths
follow:

1. **If a pooled backend can serve any database** (the proxy forwards the
   client's DB selection / issues `USE` on borrow): Phase 2 is mostly a
   **config change** — point every scope at one shared proxy rootDir so bd's
   existing spawn-or-reuse yields a single shared proxy fronting 48770. Validate
   by initializing two proxied scopes against one shared rootDir and confirming
   one db-proxy-child serves both databases.
2. **If backends are DB-pinned**: implement `BackendLocalSharedServer` as
   per-database sub-pools behind one listener (one process, N warm sub-pools),
   modeled on the external backend.

Recommendation: settle (1) vs (2) empirically with the two-scope shared-root
test before writing code; either way it composes with Phases 0+1 unchanged.

## Additional fixes found 2026-06-07 (same proxied-pooling incident)

Two more gates were making `gc hook` return `[]` fleet-wide for a city placed
under a shared directory (`/Users/Shared`) in proxied mode:

1. **SEC-003 `/Users/Shared` boundary — fixed in beads PR `gc/vp-mwm7`, NOT a
   patch here.** bd's `isPathInSafeBoundary` rejected `/Users/Shared` as "another
   user's home", so `bd context` hard-failed → gc's `bd_context_agreement` gate
   failed → native store unavailable. PR `gc/vp-mwm7` is the canonical fix: it
   trusts a non-system path **only when `BEADS_DIR` is set explicitly** (operator
   intent), preserving SEC-003 for accidental paths. gc always sets `BEADS_DIR`
   for its `bd context` call (`cmd/gc/bd_env.go`), so this fully covers gascity.
   (An earlier always-allow patch `0003` was **superseded by `gc/vp-mwm7`** and
   removed — do not re-introduce it; it weakened SEC-003 for auto-discovered paths.)

2. **proxied-server dolt mode — `0004-...proxied-server...patch` (gascity).**
   `checkDoltModeSafe` only passed `dolt_mode=server`; `proxied-server` fell
   through to Fail → native store disabled fleet-wide → slow BdStore fallback.
   The patch adds a `proxied-server` pass case + regression test.

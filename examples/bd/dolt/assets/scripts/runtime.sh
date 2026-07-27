#!/bin/sh

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LEGACY_GC_DIR="$GC_CITY_PATH/.gc"

if [ -d "$PACK_STATE_DIR" ] || [ ! -d "$LEGACY_GC_DIR/dolt-data" ]; then
  DOLT_STATE_DIR="$PACK_STATE_DIR"
else
  DOLT_STATE_DIR="$LEGACY_GC_DIR"
fi

# Data lives under .beads/dolt (gc-beads-bd canonical path). Honor
# GC_DOLT_DATA_DIR first so shell pack commands target the same managed data
# directory as the Go lifecycle and doctor code.
DOLT_BEADS_DATA_DIR="${GC_DOLT_DATA_DIR:-$GC_CITY_PATH/.beads/dolt}"
if [ -n "${GC_DOLT_DATA_DIR:-}" ]; then
  DOLT_DATA_DIR="$GC_DOLT_DATA_DIR"
elif [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="${GC_DOLT_LOG_FILE:-$DOLT_STATE_DIR/dolt.log}"
DOLT_PID_FILE="${GC_DOLT_PID_FILE:-$DOLT_STATE_DIR/dolt.pid}"
if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
  DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi
DOLT_PROVIDER_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh"

# is_local_dolt_host returns 0 (true) when the argument names the local managed
# Dolt server — loopback, the unspecified address, or an unset/empty host — and
# 1 (false) for a configured external endpoint. The health, status, and logs
# commands share it so they agree on whether GC owns a local managed process or
# is merely pointed at a remote server it cannot inspect on-disk. Mirrors the
# gc-beads-bd `is_remote` classification (gastownhall/gascity su-deol8).
is_local_dolt_host() {
  case "$1" in
    ""|127.0.0.1|0.0.0.0|localhost|::1|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
  case "$value" in
    true|false)
      printf '%s\n' "$value"
      ;;
  esac
)

read_runtime_state_number() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

canonical_path() (
  path="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

print(os.path.realpath(sys.argv[1]))
PY
    return $?
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$path" 2>/dev/null && return 0
  fi
  printf '%s\n' "$path"
)

same_path() (
  left="$1"
  right="$2"
  [ "$left" = "$right" ] && return 0
  [ "$(canonical_path "$left")" = "$(canonical_path "$right")" ]
)

pid_is_running() (
  pid="$1"

  case "$pid" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi

  if command -v ps >/dev/null 2>&1; then
    ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
    [ "$ps_pid" = "$pid" ] && return 0
  fi

  return 1
)

managed_runtime_listener_pid() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 0
      ;;
  esac

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
    | while IFS= read -r holder_pid; do
        case "$holder_pid" in
          ''|*[!0-9]*)
            continue
            ;;
        esac
        if pid_is_running "$holder_pid"; then
          printf '%s\n' "$holder_pid"
          break
        fi
      done
)

managed_runtime_tcp_reachable() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
    return $?
  fi

  return 1
)

managed_runtime_port() (
  state_file="$1"
  expected_data_dir="$2"

  [ -f "$state_file" ] || return 0

  running=$(read_runtime_state_flag "$state_file" running)
  pid=$(read_runtime_state_number "$state_file" pid)
  port=$(read_runtime_state_number "$state_file" port)
  data_dir=$(read_runtime_state_string "$state_file" data_dir)

  [ "$running" = "true" ] || return 0
  [ -n "$pid" ] || return 0
  [ -n "$port" ] || return 0
  if ! same_path "$data_dir" "$expected_data_dir"; then
    printf 'dolt runtime: managed state data_dir=%s does not match expected data_dir=%s\n' \
      "$data_dir" "$expected_data_dir" >&2
    return 0
  fi
  pid_is_running "$pid" || return 0

  holder_pid=$(managed_runtime_listener_pid "$port" || true)
  if [ -n "$holder_pid" ]; then
    [ "$holder_pid" = "$pid" ] || return 0
    printf '%s\n' "$port"
    return 0
  fi

  if ! managed_runtime_tcp_reachable "$port"; then
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
# runtime state over stale inherited env, then falls back to GC_DOLT_PORT as an
# operator seed, and exits 78 if neither yields a port.
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?

# Resolve a bounded-execution helper. Prefer gtimeout (coreutils on
# macOS), fall back to timeout (coreutils on Linux), then to running
# the command directly if neither is installed. Running unbounded is
# still better than letting a wedged dolt client hang the caller, but
# patrol callers need a hard upper bound wherever possible.
if command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
else
  TIMEOUT_BIN=""
fi

_run_bounded_warned_no_timeout=""

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. If no bounded execution mechanism is available, fail closed rather
# than running a potentially wedged Dolt client unbounded.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$_t" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}

# --- gc-managed Dolt push orphan-reaping (vc-ewyro) -------------------------
# A client-side push bound (run_bounded) or an order-cancel SIGKILLs the dolt
# CLIENT, but the dolt sql-server keeps executing the orphaned CALL DOLT_PUSH:
# it does not cancel a running query when the client connection drops. The
# orphan holds a server worker and contends on the shared multi-database server
# for as long as the push would have taken, starving every other order that
# touches Dolt (vc-ewyro: an order-cancel mid-push left a va backup push running
# ~3600s server-side, wedging gc-CLI and the whole order patrol on the shared
# 9-DB server). The client bound alone cannot stop it — the server side must be
# killed explicitly.
#
# Each managed push is tagged /* gc-dolt-sync:<pid>:<db> */; Dolt preserves the
# comment verbatim in information_schema.processlist.info and reports the
# executing sub-statement of a `USE db; CALL ...` batch (i.e. `CALL ...`), so
# the exact orphan is reaped by matching the tag. GC_DOLT_PUSH_TAG_PREFIX is the
# single source of truth for the marker, shared by the tagger and the reaper so
# the two cannot silently drift.
GC_DOLT_PUSH_TAG_PREFIX="gc-dolt-sync"

# dolt_push_tag DB — the reap marker for a push of DB from THIS run.
dolt_push_tag() {
  printf '%s:%s:%s' "$GC_DOLT_PUSH_TAG_PREFIX" "$$" "$1"
}

# reap_dolt_push_by_tag EXACT_TAG [BOUND_SECS] — KILL the server-side push
# carrying EXACT_TAG ("gc-dolt-sync:<pid>:<db>"). Used the instant a push's own
# client bound fires (exit 124): the owning script is still alive but its dolt
# CLIENT was killed, so the orphan is reaped directly by its unique tag — no
# liveness check needed. The `info LIKE 'CALL %'` guard scopes to a running push
# statement, which also excludes this helper's own SELECT. Every dolt call is
# short-bounded (default 15s): a metadata SELECT + a KILL are cheap and are NOT
# queued behind the push's data transfer, so the reaper never blocks on the
# saturated server it is relieving. Best-effort — a failure never propagates.
reap_dolt_push_by_tag() {
  _rdp_tag="$1"
  _rdp_bound="${2:-15}"
  [ -n "$_rdp_tag" ] && [ -n "${GC_DOLT_PORT:-}" ] || return 0
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  _rdp_host="${GC_DOLT_HOST:-127.0.0.1}"
  _rdp_ids=$(run_bounded "$_rdp_bound" dolt --host "$_rdp_host" --port "$GC_DOLT_PORT" \
    --user "${GC_DOLT_USER:-root}" --no-tls sql --result-format csv \
    -q "SELECT id FROM information_schema.processlist WHERE info LIKE '%$_rdp_tag%' AND info LIKE 'CALL %'" 2>/dev/null \
    | awk -F, 'NR > 1 { gsub(/^"|"$/, "", $1); if ($1 ~ /^[0-9]+$/) print $1 }') || return 0
  for _rdp_id in $_rdp_ids; do
    if run_bounded "$_rdp_bound" dolt --host "$_rdp_host" --port "$GC_DOLT_PORT" \
        --user "${GC_DOLT_USER:-root}" --no-tls sql -q "KILL $_rdp_id" >/dev/null 2>&1; then
      printf '  gc dolt: reaped orphaned server-side push (connection %s)\n' "$_rdp_id" >&2
    fi
  done
}

# reap_dead_owner_pushes [BOUND_SECS] — start-of-run SWEEP: KILL every gc-managed
# push whose OWNING process (the <pid> in its /* gc-dolt-sync:<pid>:<db> */ tag)
# is no longer alive — a push stranded on the server by a previous run that
# crashed or was cancelled mid-push. This is the dominant orphan path: an order
# context-deadline SIGTERMs the whole script (its client bound never fires) while
# the sql-server keeps running the push. Doing it here, outside any signal
# handler, avoids the process-group / blocking-before-exit / exit-code
# fragilities of a TERM trap.
#
# Liveness (kill -0), not age, is the discriminator, so a concurrently-running
# sync OR compact push with a LIVE owner is never touched — no cross-command
# false-reap and no threshold tuning (both commands share GC_DOLT_PUSH_TAG_PREFIX
# so the sweep cleans up either). The owning pid is extracted server-side with
# SUBSTRING_INDEX so the id,pid rows stay clean numeric CSV even though `info`
# itself contains commas. Bounded + best-effort.
reap_dead_owner_pushes() {
  _rdo_bound="${1:-15}"
  [ -n "${GC_DOLT_PORT:-}" ] || return 0
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  _rdo_host="${GC_DOLT_HOST:-127.0.0.1}"
  run_bounded "$_rdo_bound" dolt --host "$_rdo_host" --port "$GC_DOLT_PORT" \
    --user "${GC_DOLT_USER:-root}" --no-tls sql --result-format csv \
    -q "SELECT id, SUBSTRING_INDEX(SUBSTRING_INDEX(info, '$GC_DOLT_PUSH_TAG_PREFIX:', -1), ':', 1) AS pid FROM information_schema.processlist WHERE info LIKE '%$GC_DOLT_PUSH_TAG_PREFIX:%' AND info LIKE 'CALL %'" 2>/dev/null \
    | awk -F, 'NR > 1 { gsub(/^"|"$/, "", $1); gsub(/^"|"$/, "", $2); if ($1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/) print $1, $2 }' \
    | while read -r _rdo_id _rdo_pid; do
        kill -0 "$_rdo_pid" 2>/dev/null && continue
        if run_bounded "$_rdo_bound" dolt --host "$_rdo_host" --port "$GC_DOLT_PORT" \
            --user "${GC_DOLT_USER:-root}" --no-tls sql -q "KILL $_rdo_id" >/dev/null 2>&1; then
          printf '  gc dolt: reaped stranded push from dead run (connection %s, owner pid %s)\n' "$_rdo_id" "$_rdo_pid" >&2
        fi
      done
  return 0
}

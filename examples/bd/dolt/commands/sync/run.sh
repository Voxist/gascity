#!/bin/sh
# gc dolt sync — Push Dolt databases to their configured remotes.
#
# Uses the live Dolt SQL server when reachable so sync does not restart
# active databases. Falls back to CLI mode only when no server is running.
# Pushes committed branch state only; it does not auto-commit working
# changes before pushing.
# Use --gc to purge closed ephemeral beads before syncing.
# Use --dry-run to preview without pushing.
#
# Refspec resolution (per database):
#   1. GC_DOLT_REFSPEC_<DB_UPPER> env var override, in <local>:<remote> form
#      (e.g. GC_DOLT_REFSPEC_GA=main:gascity-3). DB name is uppercased with
#      '-' replaced by '_' to derive the env var key; database names that
#      differ only by '-' vs '_' intentionally share the same env var key.
#   2. Default: the database's active branch is pushed to a same-named branch
#      on the remote (i.e. <active>:<active>). This works transparently for the
#      common case where local and remote branch names match, including 'main'
#      on legacy setups.
#   3. Fallback when active_branch() cannot be resolved (or in CLI mode): 'main'.
#
# Environment:
#   GC_CITY_PATH                          (required) — city root
#   GC_DOLT_PORT                          (required) — managed dolt port
#   GC_DOLT_USER                          (default: root)
#   GC_DOLT_PASSWORD                      (optional)
#   GC_DOLT_SYNC_PUSH_TIMEOUT_SECS
#     (default: 1800) — wall-clock bound for SQL-mode remote push. Increase for
#                     slow links or large first pushes (a multi-GB first push to
#                     a fresh remote can exceed the prior fixed 120s ceiling).
#                     Metadata queries (remote lookup, active branch) keep their
#                     own 120s bound.
#   GC_DOLT_SYNC_DRAIN_ATTEMPTS
#     (default: 3)    — --drain only. How many push+verify rounds a single store
#                     gets before it is declared undrainable and reported under
#                     'BACKLOG NOT DRAINED:'. See ADR-0064 D1.
set -e

dry_run=false
force=false
do_gc=false
db_filter=""
# ADR-0064 D1/D3. --drain is the delivery-window mode: every store must be
# driven to a VERIFIED zero backlog, and a store that cannot get there is a
# terminal, named, non-zero failure. Off by default so the routine sync patrol
# keeps its current semantics — on a live (non-quiesced) city new commits can
# legitimately land between push and verification, and failing the patrol for
# that would be a false alarm. The residual is still measured and reported in
# both modes; only the enforcement is gated.
drain=false

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=true; shift ;;
    --force)   force=true; shift ;;
    --gc)      do_gc=true; shift ;;
    --drain)   drain=true; shift ;;
    --db)      db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt sync [--dry-run] [--force] [--gc] [--drain] [--db NAME]"
      echo ""
      echo "Fast-forward-push Dolt databases to their configured remotes."
      echo "Each database is fetched and classified against its remote; only"
      echo "fast-forward (ahead-only or first) pushes proceed. A behind or"
      echo "diverged database is refused with an actionable status and is never"
      echo "force-pushed. This keeps shared multi-writer databases safe."
      echo ""
      echo "Flags:"
      echo "  --dry-run   Show the per-database classification without pushing"
      echo "              (fetches read-only to classify; makes no other change)"
      echo "  --force     Force-push to remotes (bypasses the fast-forward check)"
      echo "  --gc        Purge closed ephemeral beads before sync"
      echo "  --drain     Delivery-window mode (ADR-0064): re-verify each store's"
      echo "              backlog after pushing and re-push until it reaches zero."
      echo "              A store still carrying undelivered commits is reported"
      echo "              terminally under 'BACKLOG NOT DRAINED:' and the run exits"
      echo "              non-zero. Intended for a quiesced city; on a live city"
      echo "              new commits can arrive mid-run and read as a residual."
      echo "  --db NAME   Sync only the named database"
      echo ""
      echo "Policy:"
      echo "  Create .no-sync in a database's .beads/dolt/<db>/ directory to"
      echo "  exclude it from sync (reported as 'skipped (.no-sync)')."
      echo ""
      echo "Environment:"
      echo "  GC_DOLT_SYNC_FETCH_TIMEOUT_SECS  pre-push fetch bound (default 60)"
      echo "  GC_DOLT_SYNC_PUSH_TIMEOUT_SECS   push bound (default 1800)"
      echo "  GC_DOLT_SYNC_DRAIN_ATTEMPTS      --drain push+verify rounds (default 3)"
      exit 0
      ;;
    *) echo "gc dolt sync: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt sync: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

beads_bd="$GC_BEADS_BD_SCRIPT"
data_dir="$DOLT_DATA_DIR"

# Wall-clock bound for SQL-mode remote push (seconds). Defaults to 1800s; the
# prior fixed 120s ceiling SIGKILLed large first pushes that succeed when issued
# directly to the running sql-server. An explicitly-empty / non-numeric / any
# numeric-zero value is rejected (not silently defaulted) so a misconfigured
# bound fails loud instead of producing a misleading "TIMEOUT after 0s".
# Validated before any per-database logic so an invalid value aborts before any
# db is touched.
#
# A valid value is non-empty, all-digit, and has at least one non-zero digit.
# Matching only the literal "0" would let leading-zero forms ("00", "000")
# through; GNU `timeout` treats a 0 duration as "disable the timeout", which
# would run the push UNBOUNDED — the exact anti-hang outcome this bound exists
# to prevent. The first arm rejects empty/non-digit input; the second accepts
# any all-digit string containing a non-zero digit; the default arm rejects the
# remaining all-digit-but-all-zero forms.
push_timeout="${GC_DOLT_SYNC_PUSH_TIMEOUT_SECS-1800}"
case "$push_timeout" in
  ''|*[!0-9]*) push_timeout_valid=false ;;
  *[1-9]*)     push_timeout_valid=true ;;
  *)           push_timeout_valid=false ;;
esac
if [ "$push_timeout_valid" != true ]; then
  printf 'gc dolt sync: invalid GC_DOLT_SYNC_PUSH_TIMEOUT_SECS=%s (must be a positive integer)\n' \
    "$push_timeout" >&2
  exit 2
fi

# Wall-clock bound for the SQL-mode pre-push fetch (seconds). Defaults to 60s.
# A hung fetch against a sick remote must not stall the whole patrol, so the
# fetch is bounded and a timeout skips that database without pushing. Validated
# with the same rules as the push timeout (reject empty / non-numeric /
# all-zero — GNU `timeout 0` disables the timeout, i.e. unbounded).
fetch_timeout="${GC_DOLT_SYNC_FETCH_TIMEOUT_SECS-60}"
case "$fetch_timeout" in
  ''|*[!0-9]*) fetch_timeout_valid=false ;;
  *[1-9]*)     fetch_timeout_valid=true ;;
  *)           fetch_timeout_valid=false ;;
esac
if [ "$fetch_timeout_valid" != true ]; then
  printf 'gc dolt sync: invalid GC_DOLT_SYNC_FETCH_TIMEOUT_SECS=%s (must be a positive integer)\n' \
    "$fetch_timeout" >&2
  exit 2
fi

# ADR-0064 D1 step 2: how many push+verify rounds a single store gets before it
# is declared undrainable. The 2026-08-04 window pushed ONCE per store and hq
# came out at ~2 unpushed, with vp/va/vr falling out again within hours on the
# same server — push cost is driven by backlog as well as by cold/warm, so a
# missed window makes the next push bigger and the two compound into a ratchet.
# Bounded rather than unbounded: on a quiesced city the residual converges in a
# round or two, and a store that will not converge must be *reported*, not
# retried forever while the city waits to be admitted. Same validation rules as
# the timeouts above (reject empty / non-numeric / all-zero).
drain_attempts="${GC_DOLT_SYNC_DRAIN_ATTEMPTS-3}"
case "$drain_attempts" in
  ''|*[!0-9]*) drain_attempts_valid=false ;;
  *[1-9]*)     drain_attempts_valid=true ;;
  *)           drain_attempts_valid=false ;;
esac
if [ "$drain_attempts_valid" != true ]; then
  printf 'gc dolt sync: invalid GC_DOLT_SYNC_DRAIN_ATTEMPTS=%s (must be a positive integer)\n' \
    "$drain_attempts" >&2
  exit 2
fi

# Check if server is running.
is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

# routes_files — emit one routes.jsonl path per line.
# Uses gc rig list --json when available so external rigs are included.
# Falls back to a filesystem glob when gc is absent.
routes_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/routes.jsonl"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/routes.jsonl"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/routes.jsonl' 2>/dev/null || true
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_branch_name() {
  case "$1" in
    -*|.*|*..*|*@{*) return 1 ;;
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_./-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

# refspec_env_value <db> — emit the GC_DOLT_REFSPEC_<DB_UPPER> override, if any.
# DB name is uppercased and '-' is replaced with '_' to form a valid env key.
refspec_env_value() {
  db="$1"
  valid_database_name "$db" || return 1
  key=$(printf '%s' "$db" | tr 'a-z-' 'A-Z_')
  case "$key" in
    *[!A-Z0-9_]*) return 0 ;;
  esac
  eval "printf '%s' \"\${GC_DOLT_REFSPEC_$key:-}\""
}

warn_refspec_fallback() {
  printf '  %s: WARN: active branch unresolved; falling back to main\n' "$1" >&2
}

# refspec_parts <refspec> — split <local>:<remote> into two lines.
# A bare <branch> expands to <branch>:<branch>. Returns 1 if either side is
# empty or invalid.
refspec_parts() {
  rs="$1"
  case "$rs" in
    *:*)
      l=${rs%%:*}
      r=${rs#*:}
      ;;
    *)
      l="$rs"
      r="$rs"
      ;;
  esac
  [ -z "$l" ] && return 1
  [ -z "$r" ] && return 1
  valid_branch_name "$l" || return 1
  valid_branch_name "$r" || return 1
  printf '%s\n%s\n' "$l" "$r"
}

# dolt_sql QUERY [TIMEOUT_SECS] — run a SQL query against the live server under a
# wall-clock bound. The optional second arg overrides the bound; it defaults to
# 120s, which is sized for SHORT METADATA QUERIES ONLY (remote lookup,
# active_branch). This is a load-bearing contract: any data-transfer operation
# (e.g. DOLT_PUSH) MUST pass its own larger bound, or it will silently re-hit
# this 120s ceiling and be SIGKILLed mid-transfer.
dolt_sql() {
  query="$1"
  tmo="${2:-120}"
  host="${GC_DOLT_HOST:-127.0.0.1}"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded "$tmo" dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql --result-format csv -q "$query"
}

# classify_count <db> <revrange> — emit the dolt_log commit count for a revision
# range (e.g. "remotes/origin/main..main" = commits on the remote-tracking ref
# not on the local branch). Returns non-zero when the range cannot be resolved —
# notably when the remote-tracking ref does not exist yet (Dolt errors
# "branch not found: remotes/..."), which the caller treats as a first push.
# Read-only; bounded by the metadata ceiling. Verified against Dolt 2.1.0:
#   dolt_log('A..B') counts commits reachable from B but not A.
classify_count() {
  cc_db="$1"
  cc_range="$2"
  cc_out=$(dolt_sql "USE \`$cc_db\`; SELECT COUNT(*) AS n FROM dolt_log('$cc_range')") || return 1
  printf '%s\n' "$cc_out" | awk -F, 'NR == 2 { gsub(/^"|"$/, "", $1); print $1; exit }'
}

# ADR-0064 AC2. residual_backlog <db> <remote> <local-branch> <remote-branch> —
# re-fetch the remote tracking ref and emit how many local commits are STILL not
# on it. Prints the count (possibly 0) on stdout; returns 2 when the residual
# cannot be measured at all.
#
# Why this exists: `CALL DOLT_PUSH` returning 0 reports that *this push*
# succeeded, not that the store has nothing left to deliver. The 2026-08-04
# window trusted the push result, pushed once per store, and hq came out at ~2
# unpushed — so the window read as complete while the store still had no
# complete off-box copy. Measuring the post-push `ahead` count is the only
# statement of delivery that survives that. This is the same classify_count
# range the pre-push fast-forward check uses, re-run after the fact.
#
# An indeterminate result (rc 2) is deliberately NOT collapsed into "zero": a
# store whose residual cannot be read is exactly a store whose off-box copy
# cannot be asserted, and D3 requires that to be loud rather than absent.
# The re-fetch is cheap here by construction — the pre-push fetch already
# succeeded this run, so the remote blobset is spooled and the store is warm.
residual_backlog() {
  rb_db="$1"
  rb_remote="$2"
  rb_local="$3"
  rb_remote_branch="$4"
  dolt_sql "USE \`$rb_db\`; CALL DOLT_FETCH('$rb_remote', '$rb_remote_branch')" "$fetch_timeout" \
    >/dev/null 2>&1 || return 2
  rb_ahead=$(classify_count "$rb_db" "remotes/$rb_remote/$rb_remote_branch..$rb_local") || return 2
  case "$rb_ahead" in
    ''|*[!0-9]*) return 2 ;;
  esac
  printf '%s\n' "$rb_ahead"
}

find_remote_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes LIMIT 1") || return 1
  printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2; exit}'
}

# resolve_refspec_sql <db> — emit two lines: local-branch and remote-branch.
# Honors GC_DOLT_REFSPEC_<DB> first, then falls back to active_branch() over SQL,
# then to 'main' if both fail.
resolve_refspec_sql() {
  db="$1"
  if ! valid_database_name "$db"; then
    echo "  $db: ERROR: invalid database name" >&2
    return 1
  fi
  override=$(refspec_env_value "$db") || return 1
  if [ -n "$override" ]; then
    parts=$(refspec_parts "$override") || {
      echo "  $db: ERROR: invalid refspec override: $override" >&2
      return 1
    }
    printf '%s\n' "$parts"
    return 0
  fi
  if active_csv=$(dolt_sql "USE \`$db\`; SELECT active_branch()" 2>/dev/null); then
    active=$(printf '%s\n' "$active_csv" | awk 'NR > 1 && $0 != "" {gsub(/^"|"$/, ""); print; exit}')
    if [ -n "$active" ] && valid_branch_name "$active"; then
      printf '%s\n%s\n' "$active" "$active"
      return 0
    fi
  fi
  warn_refspec_fallback "$db"
  printf 'main\nmain\n'
}

# resolve_refspec_cli <db-dir> <db-name> — same as resolve_refspec_sql, but
# resolves the active branch from repo_state.json when the SQL server is down.
repo_state_active_branch() {
  awk '
    function emit(line) {
      sub(/.*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//, "", line)
      sub(/".*/, "", line)
      print line
      exit
    }
    {
      line = $0
      if (depth == 1 && line ~ /^[[:space:]]*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//) {
        emit(line)
      }
      if (depth == 0 && line ~ /^[[:space:]]*\{[[:space:]]*"head"[[:space:]]*:[[:space:]]*"refs\/heads\//) {
        emit(line)
      }
      opens = gsub(/\{/, "{", line)
      closes = gsub(/\}/, "}", line)
      depth += opens - closes
      if (depth < 0) {
        depth = 0
      }
    }
  ' "$1"
}

resolve_refspec_cli() {
  d="$1"
  db="$2"
  if ! valid_database_name "$db"; then
    echo "  $db: ERROR: invalid database name" >&2
    return 1
  fi
  override=$(refspec_env_value "$db") || return 1
  if [ -n "$override" ]; then
    parts=$(refspec_parts "$override") || {
      echo "  $db: ERROR: invalid refspec override: $override" >&2
      return 1
    }
    printf '%s\n' "$parts"
    return 0
  fi
  state="$d/.dolt/repo_state.json"
  if [ -f "$state" ]; then
    head=$(repo_state_active_branch "$state" | head -1)
    if [ -n "$head" ] && valid_branch_name "$head"; then
      printf '%s\n%s\n' "$head" "$head"
      return 0
    fi
  fi
  warn_refspec_fallback "$db"
  printf 'main\nmain\n'
}

# listener_read_timeout_secs — the live managed Dolt listener's
# read_timeout_millis (the hard per-query kill deadline), in whole seconds.
# Read from the generated dolt-config.yaml (GC_DOLT_CONFIG_FILE, else the
# packs/dolt default under GC_CITY_PATH) rather than city.toml: the two
# diverge during a warming window, and cold-open classification (vp-9v6f9)
# must describe the server that is actually running, not operator intent.
# Falls back to the documented managed default (15) when the file is absent
# or unparseable — the generated config is optional in this script's
# environment.
listener_read_timeout_secs() {
  cfg="${GC_DOLT_CONFIG_FILE:-$GC_CITY_PATH/.gc/runtime/packs/dolt/dolt-config.yaml}"
  millis=$(sed -n 's/^[[:space:]]*read_timeout_millis:[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
    "$cfg" 2>/dev/null | head -1)
  case "$millis" in
    ''|*[!0-9]*) echo 15 ;;
    *) echo $(( millis / 1000 )) ;;
  esac
}

sync_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    last_fail_reason="invalid database name"
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    echo "  $name: ERROR: failed to query remotes" >&2
    last_fail_reason="failed to query remotes"
    return 1
  }
  if [ -z "$remote_pair" ]; then
    # ADR-0064 D3/AC4. Outside the delivery window this is a benign "nothing to
    # do". Inside it, a store with no remote configured is the P0 condition
    # itself — it has no off-box copy and never will — so the window must not
    # exit 0 having quietly stepped over it. `.no-sync` remains the sanctioned
    # opt-out for genuinely local-only stores (scratch/test databases); this
    # branch is for a store that simply has no remote, which is indistinguishable
    # from a misconfigured real store.
    if [ "$drain" = true ]; then
      echo "  $name: NO REMOTE — no off-box copy is possible for this store; configure a remote or mark it .no-sync" >&2
      undrained_stores="$undrained_stores $name"
      return 1
    fi
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    last_fail_reason="invalid remote name: $remote_name"
    return 1
  fi

  refspec_pair=$(resolve_refspec_sql "$name") || { last_fail_reason="refspec resolution failed"; return 1; }
  local_branch=$(printf '%s\n' "$refspec_pair" | sed -n '1p')
  remote_branch=$(printf '%s\n' "$refspec_pair" | sed -n '2p')

  # gc-6ommo: fast-forward-only-or-refuse. Unless --force, fetch the remote and
  # classify local vs remotes/<remote>/<remote_branch>. Push only on a
  # fast-forward (ahead-only, or a first push where the remote branch does not
  # exist yet). behind / diverged refuse with an actionable status; a fetch
  # timeout or error skips WITHOUT pushing. A patrol never auto-merges (ZFC):
  # it surfaces state + the owning command and lets a human/agent reconcile.
  ff_decision="push"   # push | skip
  ff_status="force"    # human-readable classification (for dry-run / output)
  ff_rc=0              # return code when skipping
  if [ "$force" != true ]; then
    remote_tracking="remotes/$remote_name/$remote_branch"
    fetch_err_tmp=$(mktemp) || {
      echo "  $name: ERROR: cannot create temp file for fetch diagnostics" >&2
      last_fail_reason="cannot create temp file for fetch diagnostics"
      return 1
    }
    fetch_rc=0
    fetch_start=$(date +%s)
    dolt_sql "USE \`$name\`; CALL DOLT_FETCH('$remote_name', '$remote_branch')" "$fetch_timeout" \
      >/dev/null 2>"$fetch_err_tmp" || fetch_rc=$?
    fetch_elapsed=$(( $(date +%s) - fetch_start ))
    if [ "$fetch_rc" -ne 0 ] && { grep -q "no branches found in remote" "$fetch_err_tmp" 2>/dev/null || grep -q "invalid ref spec" "$fetch_err_tmp" 2>/dev/null; }; then
      # The remote has no such branch: an empty remote ("no branches found in
      # remote") or a brand-new branch on a populated remote ("invalid ref
      # spec" — both verified on Dolt 2.1.0). The first push creates the branch
      # and is necessarily a fast-forward.
      ff_status="first-push"
      rm -f "$fetch_err_tmp"
    elif [ "$fetch_rc" -eq 124 ]; then
      rm -f "$fetch_err_tmp"
      echo "  $name: fetch timed out after ${fetch_timeout}s — skipped (NOT pushed)" >&2
      last_fail_reason="fetch timed out after ${fetch_timeout}s"
      return 1
    elif [ "$fetch_rc" -ne 0 ]; then
      # vp-9v6f9: classify by ELAPSED TIME against the live listener deadline,
      # not by matching an error string — the exact text at the wall is not
      # recorded in the evidence this classification is based on, and the
      # elapsed-time signature is the measured physics regardless of which
      # layer reports the kill. A store whose first-in-lifetime fetch must
      # spool its whole remote blobset (no server-side range read) is killed
      # by the listener's hard per-query read_timeout_millis before the spool
      # can persist — not a transient blip, and no retry budget converges it
      # (every attempt dies at the same wall). Threshold is 90% of the live
      # deadline: comfortably above fast-failure noise (e.g. a corrupt remote,
      # vp-catlj) and below the deadline itself.
      deadline=$(listener_read_timeout_secs)
      threshold=$(( deadline * 9 / 10 ))
      if [ "$fetch_elapsed" -ge "$threshold" ]; then
        rm -f "$fetch_err_tmp"
        echo "  $name: COLD-OPEN WALL — fetch killed at ${fetch_elapsed}s (listener deadline ${deadline}s); NO off-box copy this server lifetime — skipped (NOT pushed)" >&2
        cold_wall_stores="$cold_wall_stores $name"
        return 1
      fi
      echo "  $name: fetch failed (exit $fetch_rc) after ${fetch_elapsed}s (listener deadline ${deadline}s) — skipped (NOT pushed)" >&2
      if [ -s "$fetch_err_tmp" ]; then
        while IFS= read -r line || [ -n "$line" ]; do
          printf '  %s: %s\n' "$name" "$line" >&2
        done < "$fetch_err_tmp"
      fi
      rm -f "$fetch_err_tmp"
      last_fail_reason="fetch failed (exit $fetch_rc)"
      return 1
    else
      rm -f "$fetch_err_tmp"
      # Remote reachable and the branch exists (fetch succeeded) -> classify by
      # ancestry. BOTH range queries must succeed; if either fails, fail closed
      # (skip without pushing) rather than guessing a count and risking a push.
      if ahead=$(classify_count "$name" "$remote_tracking..$local_branch") &&
        behind=$(classify_count "$name" "$local_branch..$remote_tracking"); then
        [ -n "$ahead" ] || ahead=0
        [ -n "$behind" ] || behind=0
        # diverged returns non-zero (needs human action); behind alone is a
        # benign "nothing to push, pull needed" state and returns success.
        if [ "$ahead" = 0 ] && [ "$behind" = 0 ]; then
          ff_decision="skip"; ff_status="up-to-date"; ff_rc=0
        elif [ "$behind" = 0 ]; then
          ff_status="ahead $ahead"
        elif [ "$ahead" = 0 ]; then
          ff_decision="skip"; ff_status="behind $behind"; ff_rc=0
        else
          ff_decision="skip"; ff_status="diverged ($ahead ahead / $behind behind)"; ff_rc=1
        fi
      else
        ff_decision="skip"; ff_status="classify failed"; ff_rc=1
      fi
    fi
  fi

  if [ "$dry_run" = true ]; then
    if [ "$ff_decision" = "skip" ]; then
      echo "  $name: would skip $local_branch -> $remote_name:$remote_branch ($remote_url) [$ff_status]"
    elif [ "$force" = true ]; then
      echo "  $name: would force-push $local_branch -> $remote_name:$remote_branch ($remote_url)"
    else
      echo "  $name: would push $local_branch -> $remote_name:$remote_branch ($remote_url) [$ff_status]"
    fi
    return 0
  fi

  if [ "$ff_decision" = "skip" ]; then
    case "$ff_status" in
      up-to-date) echo "  $name: up-to-date with $remote_name:$remote_branch" ;;
      behind*)    echo "  $name: $ff_status — pull needed (gc dolt pull)" ;;
      diverged*)  echo "  $name: $ff_status — manual reconcile" >&2 ;;
      *)          echo "  $name: skipped [$ff_status]" ;;
    esac
    last_fail_reason="$ff_status"
    return "$ff_rc"
  fi

  if [ "$local_branch" = "$remote_branch" ]; then
    refspec_arg="$local_branch"
  else
    refspec_arg="$local_branch:$remote_branch"
  fi

  # Tag the push so an orphaned server-side CALL DOLT_PUSH (client bound fired,
  # or the order was cancelled mid-push) can be reaped precisely — Dolt
  # preserves the comment in processlist.info. See reap_dolt_push_by_tag.
  push_tag=$(dolt_push_tag "$name")
  if [ "$force" = true ]; then
    push_query="USE \`$name\`; CALL DOLT_PUSH('--force', '--set-upstream', '$remote_name', '$refspec_arg') /* $push_tag */"
  else
    push_query="USE \`$name\`; CALL DOLT_PUSH('$remote_name', '$refspec_arg') /* $push_tag */"
  fi
  # ADR-0064 D1 step 2. Push, then in --drain mode VERIFY the store actually
  # reached a zero backlog and re-push while it has not. Bounded by
  # drain_attempts: on a quiesced city the residual converges in a round or two,
  # and a store that will not converge must be reported rather than retried
  # forever while the city waits to be admitted.
  #
  # Only the SUCCESSFUL-push branch loops. A hard push failure keeps its
  # existing single-shot semantics and returns immediately: retrying a push that
  # errored is a different decision with its own failure modes (it is what
  # PR #515's retry loop covers), and the cold-open wall in particular converges
  # for no retry budget — every attempt dies at the same listener deadline.
  push_round=0
  while : ; do
    push_round=$((push_round + 1))
    push_rc=0
    # Guard mktemp: under `set -e` a bare `$(mktemp)` failure (unwritable or
    # exhausted TMPDIR) would abort the whole multi-db sync run with an opaque
    # error — itself the swallowed/opaque-failure class this command set out to
    # eliminate. Degrade to a per-db error so the loop reports this db and moves
    # on rather than killing the run.
    push_err_tmp=$(mktemp) || {
      echo "  $name: ERROR: cannot create temp file for push diagnostics" >&2
      return 1
    }
    # Route push under push_timeout (not dolt_sql's 120s metadata ceiling) and
    # capture stderr so the underlying dolt diagnostic survives, preserving the
    # real exit code via `|| push_rc=$?`.
    dolt_sql "$push_query" "$push_timeout" >/dev/null 2>"$push_err_tmp" || push_rc=$?

    if [ "$push_rc" -eq 0 ]; then
      rm -f "$push_err_tmp"
      # Outside the delivery window keep the pre-existing semantics exactly: no
      # extra fetch, no extra output. The verification costs a round trip per
      # store and, on a store that is cold for this server lifetime, that fetch
      # is itself liable to hit the listener wall — so making the routine patrol
      # pay for it would add both latency and false "unverified" noise to a path
      # that is not trying to make a durability claim.
      if [ "$drain" != true ]; then
        echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote_url)"
        return 0
      fi

      residual_rc=0
      residual=$(residual_backlog "$name" "$remote_name" "$local_branch" "$remote_branch") || residual_rc=$?
      if [ "$residual_rc" -ne 0 ]; then
        echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote_url)"
        echo "  $name: DELIVERY UNVERIFIED — push reported success but the residual backlog could not be measured; this store's off-box copy cannot be asserted" >&2
        undrained_stores="$undrained_stores $name"
        return 1
      fi
      if [ "$residual" -eq 0 ]; then
        echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote_url) [backlog 0, verified]"
        return 0
      fi
      if [ "$push_round" -lt "$drain_attempts" ]; then
        echo "  $name: pushed, $residual commit(s) still undelivered — re-pushing (round $push_round/$drain_attempts)"
        continue
      fi
      echo "  $name: BACKLOG NOT DRAINED — $residual commit(s) still undelivered after $drain_attempts push round(s); this store has NO complete off-box copy" >&2
      undrained_stores="$undrained_stores $name"
      return 1
    fi

    if [ "$push_rc" -eq 124 ]; then
      # Exit 124 is overloaded: a real wall-clock timeout (run_bounded via
      # timeout/gtimeout, runtime.sh) AND the no-mechanism fall-through where
      # neither timeout/gtimeout nor python3 exists and dolt never ran. A
      # SIGKILLed client leaves no stderr; the no-mechanism path leaves the
      # "cannot run bounded command" marker, so the stderr replay below
      # disambiguates the two at zero extra mechanism.
      echo "  $name: TIMEOUT after ${push_timeout}s — push manually or increase timeout (GC_DOLT_SYNC_PUSH_TIMEOUT_SECS)" >&2
      last_fail_reason="TIMEOUT after ${push_timeout}s"
      # The client bound killed our dolt client, but the server-side push keeps
      # running orphaned — reap this run's tagged push so it stops contending on
      # the shared server (vc-ewyro). Short-bounded; never blocks the patrol.
      reap_dolt_push_by_tag "$push_tag"
    else
      echo "  $name: ERROR: push failed (exit $push_rc)" >&2
      # Feed the end-of-run summary (failed_summary) the same way the CLI push
      # path does. Without this the drain loop's failures reach the summary as
      # "unknown error", which is the whole point of the ga-g514cl gate.
      last_fail_reason="push failed (exit $push_rc)"
    fi

    # Replay the captured dolt stderr, prefixed with the db name for scannable
    # multi-db output. Safe to emit unfiltered (RB6): the password reaches dolt via
    # the DOLT_CLI_PASSWORD env var (see dolt_sql), never as an argv flag, so
    # dolt's own stderr cannot echo it back. The -s guard skips an empty capture so
    # no spurious blank line is emitted.
    if [ -s "$push_err_tmp" ]; then
      # `|| [ -n "$line" ]` flushes a final line that lacks a trailing newline:
      # POSIX `read` returns non-zero at an unterminated EOF, so a terse
      # newline-less dolt diagnostic (e.g. a SIGKILL-truncated `fatal: ...`) would
      # otherwise be captured but never replayed — re-introducing the swallowed
      # failure this command set out to surface.
      while IFS= read -r line || [ -n "$line" ]; do
        printf '  %s: %s\n' "$name" "$line" >&2
      done < "$push_err_tmp"
    fi
    rm -f "$push_err_tmp"
    # A push that failed outright is also a store with no complete off-box copy
    # for this window, so name it in the drain summary rather than letting the
    # per-store line scroll past. The generic exit_code=1 already fails the run;
    # this makes WHICH store failed survive into the end-of-run block (D3).
    if [ "$drain" = true ]; then
      undrained_stores="$undrained_stores $name"
    fi
    return 1
  done
}

sync_database_cli() {
  d="$1"
  name="$2"

  # Check for remote.
  remote_name=""
  remote=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remote_name=$(grep -o '"name":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"name":"//;s/"//' || true)
    remote=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi
  [ -z "$remote_name" ] && remote_name="origin"

  if [ -z "$remote" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    last_fail_reason="invalid remote name: $remote_name"
    return 1
  fi

  refspec_pair=$(resolve_refspec_cli "$d" "$name") || { last_fail_reason="refspec resolution failed"; return 1; }
  local_branch=$(printf '%s\n' "$refspec_pair" | sed -n '1p')
  remote_branch=$(printf '%s\n' "$refspec_pair" | sed -n '2p')

  if [ "$dry_run" = true ]; then
    echo "  $name: would push $local_branch -> $remote_name:$remote_branch ($remote)"
    return 0
  fi

  if [ "$local_branch" = "$remote_branch" ]; then
    refspec_arg="$local_branch"
  else
    refspec_arg="$local_branch:$remote_branch"
  fi

  # Capture the real exit code via `|| cli_rc=$?` on each branch BEFORE the
  # success test — a post-`if` `$?` would read the compound's 0 and silently lose
  # the failure code. `2>&1` is preserved so dolt's stderr still reaches the
  # terminal (CLI mode has no wall-clock ceiling; exit 124 cannot occur here).
  cli_rc=0
  if [ "$force" = true ]; then
    (cd "$d" && dolt push --force --set-upstream "$remote_name" "$refspec_arg" 2>&1) || cli_rc=$?
  else
    (cd "$d" && dolt push "$remote_name" "$refspec_arg" 2>&1) || cli_rc=$?
  fi

  if [ "$cli_rc" -eq 0 ]; then
    echo "  $name: pushed $local_branch -> $remote_name:$remote_branch ($remote)"
    return 0
  fi

  echo "  $name: ERROR: push failed (exit $cli_rc)" >&2
  last_fail_reason="push failed (exit $cli_rc)"
  return 1
}

# Optional GC phase: purge closed ephemerals while server is still up.
if [ "$do_gc" = true ] && [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    beads_dir=""
    # Find the .beads directory for this database.
    while IFS= read -r route_file; do
      [ -f "$route_file" ] || continue
      if grep -q "\"$name\"" "$route_file" 2>/dev/null; then
        beads_dir="$(dirname "$route_file")"
        break
      fi
    done <<ROUTES_LIST
$(routes_files)
ROUTES_LIST
    if [ -n "$beads_dir" ]; then
      purge_args=""
      [ "$dry_run" = true ] && purge_args="--dry-run"
      purged=$(BEADS_DIR="$beads_dir" bd purge $purge_args 2>/dev/null | grep -c "purged" || true)
      [ "$purged" -gt 0 ] && echo "Purged $purged ephemeral bead(s) from $name"
    fi
  done
fi

# Sync each database.
exit_code=0
fail_count=0
total_count=0
failed_summary=""
server_running=false
is_running && server_running=true

# ADR-0064 D3. --drain asserts a durability property, and only the SQL path can
# verify it: the residual re-classification runs through the live server. In CLI
# mode (server down) each store would be pushed unverified and the run would
# still exit 0 — a window that claims delivery it never checked, which is worse
# than no window at all because order.completed would then read as evidence of
# freshness. Refuse instead of degrading silently.
if [ "$drain" = true ] && [ "$server_running" != true ]; then
  echo "gc dolt sync --drain: no managed Dolt server reachable on port ${GC_DOLT_PORT:-?}; the delivery window cannot verify a zero backlog without it" >&2
  exit 2
fi
# vp-9v6f9: every store name that hit a COLD-OPEN WALL this run, for the
# end-of-run backup-coverage summary below. Appended to directly by
# sync_database_sql (same shell, not a subshell — plain assignment persists).
cold_wall_stores=""
# ADR-0064 D3: every store this --drain run could not prove it drove to a zero
# backlog — undrainable residual, unmeasurable residual, outright push failure,
# or no remote at all. Same same-shell append discipline as cold_wall_stores.
undrained_stores=""

# Does this run have at least one database it will actually sync? Mirrors the
# selection filters of the sync loop below (.dolt present, not a system schema,
# matches --db, not .no-sync). A run whose databases are all excluded must not
# contact the server at all — "excluded from sync" means no server traffic on
# its behalf, not merely no push.
has_syncable_database() {
  [ -d "$data_dir" ] || return 1
  for _d in "$data_dir"/*/; do
    [ ! -d "$_d/.dolt" ] && continue
    _name="$(basename "$_d")"
    case "$(printf '%s' "$_name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$_name" != "$db_filter" ] && continue
    [ -f "$_d/.no-sync" ] && continue
    return 0
  done
  return 1
}

# Start-of-run sweep (vc-ewyro): reap any gc-managed push stranded on the shared
# server by a previous run that crashed or was cancelled mid-push — the dominant
# orphan path, where an order context-deadline SIGTERMs the script (its client
# bound never fires) while the server keeps running the push. Reaps by
# owner-pid liveness, so a concurrently-running sync/compact push is spared.
# Only meaningful in SQL/server mode; short-bounded and best-effort.
# Gated on there being real work: sweeping is maintenance for a run that is
# about to push, so a pure-skip run stays silent (ga-zf03v).
if [ "$server_running" = true ] && has_syncable_database; then
  reap_dead_owner_pushes
fi

if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    total_count=$((total_count + 1))
    last_fail_reason=""
    call_rc=0
    if [ "$server_running" = true ]; then
      sync_database_sql "$name" || call_rc=$?
    else
      sync_database_cli "$d" "$name" || call_rc=$?
    fi
    if [ "$call_rc" -ne 0 ]; then
      exit_code=1
      fail_count=$((fail_count + 1))
      failed_summary="$failed_summary$name (${last_fail_reason:-unknown error}); "
    fi
  done
fi

# vp-9v6f9: an operator scanning per-database lines can miss a P0 fleet-backup
# gap buried among routine "up-to-date" output. Silent when the list is empty
# so a clean run stays quiet.
if [ -n "$cold_wall_stores" ]; then
  echo ""
  echo "NO OFF-BOX COPY:"
  for cw_store in $cold_wall_stores; do
    echo "  $cw_store"
  done
fi

# ADR-0064 D3 / AC4. The delivery window's whole claim is "every store reached a
# verified zero backlog". Anything short of that must be terminal, named, and
# non-zero — never a silent exit 0 that an order summary then records as a fresh
# backup. This is the failure shape vp-cblo produced for darc-backup ("skipping
# sweep exits 0 -> order.completed counts as fresh") and it must not recur here.
if [ "$drain" = true ] && [ -n "$undrained_stores" ]; then
  echo ""
  echo "BACKLOG NOT DRAINED:"
  for ud_store in $undrained_stores; do
    echo "  $ud_store"
  done
  echo "These stores have NO complete off-box copy for this server lifetime."
  exit_code=1
fi

# Positioned as the last line of output: an OrderFailed event built from this
# script's output (tailForOrderFailureEvent, cmd/gc/order_dispatch.go) keeps
# only a bounded tail, so this summary must survive that truncation window.
# Emitted AFTER the drain block above so a drain-only failure is counted too.
if [ "$exit_code" -ne 0 ]; then
  echo "sync: $fail_count/$total_count database(s) failed: $failed_summary" >&2
fi

exit $exit_code

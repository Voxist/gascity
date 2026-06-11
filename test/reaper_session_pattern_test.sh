#!/usr/bin/env bash
# Test: reaper Step 6 — configurable session-bead pattern (vp-nyboh / T-001)
#
# Acceptance criteria:
#   1. GC_REAPER_SESSION_BEAD_PATTERN=""    → SQL path: dolt called, bd NOT called
#   2. GC_REAPER_SESSION_BEAD_PATTERN="gm-*" → bd prune path: bd called with --pattern gm-*

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REAPER="$SCRIPT_DIR/../internal/bootstrap/packs/core/assets/scripts/reaper.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

if [ ! -f "$REAPER" ]; then
    printf 'ERROR: reaper.sh not found at %s\n' "$REAPER" >&2
    exit 1
fi

# Extract Step 6 block from reaper.sh.
# Uses depth-counting on column-0 if/fi to capture the complete outer block;
# blank lines inside the block are preserved correctly.
STEP6=$(awk '
  /^# Step 6:/{found=1; depth=0}
  found && /^if[[:space:]]/{depth++}
  found{
    print
    if(/^fi$/) {
      depth--
      if(depth<=0) {found=0; exit}
    }
  }
' "$REAPER")

run_step6() {
    local pattern="$1"
    local tmpdir bd_flag dolt_flag bd_args_file step6_file run_script
    tmpdir=$(mktemp -d)
    bd_flag="$tmpdir/bd_called"
    dolt_flag="$tmpdir/dolt_called"
    bd_args_file="$tmpdir/bd_args"
    step6_file="$tmpdir/step6.sh"
    run_script="$tmpdir/run.sh"

    mkdir -p "$tmpdir/.beads"
    printf '{"dolt_database":"test_db"}' > "$tmpdir/.beads/metadata.json"

    printf '%s\n' "$STEP6" > "$step6_file"

    # Write the harness script that sets up stubs, variables, then sources Step 6.
    # NB: heredoc terminator must be at column 0; variables below are expanded by
    # the outer shell when writing the script (intentional), except \$* which we
    # want runtime-expanded inside the generated bd() stub.
    cat > "$run_script" << RUNEOF
#!/usr/bin/env bash
set -euo pipefail
bd()            { printf '%s\n' "\$*" > '$bd_args_file'; touch '$bd_flag'; printf '{"pruned_count":3}'; }
dolt()          { touch '$dolt_flag'; }
record_anomaly(){ :; }
export -f bd dolt record_anomaly
CITY_ABS='$tmpdir'
CITY_BEADS_DIR='$tmpdir/.beads'
SESSION_BEAD_PATTERN='$pattern'
SESSION_PURGE_AGE='720h'
DRY_RUN=''
TOTAL_SESSIONS_PRUNED=0
SESSION_PRUNE_ATTEMPTED=0
GC_DOLT_HOST='127.0.0.1'
GC_DOLT_PORT='48770'
GC_DOLT_USER='root'
GC_DOLT_PASSWORD=''
. '$step6_file'
RUNEOF

    bash "$run_script" 2>/dev/null || true

    local bd_result dolt_result bd_args_val
    bd_result=$([ -f "$bd_flag" ] && echo yes || echo no)
    dolt_result=$([ -f "$dolt_flag" ] && echo yes || echo no)
    bd_args_val=$(cat "$bd_args_file" 2>/dev/null || echo "")
    rm -rf "$tmpdir"
    printf '%s|%s|%s\n' "$bd_result" "$dolt_result" "$bd_args_val"
}

# ── T1: empty pattern → SQL path ───────────────────────────────────────────────
result=$(run_step6 "")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
dolt_called=$(printf '%s' "$result" | cut -d'|' -f2)
if [ "$dolt_called" = "yes" ] && [ "$bd_called" = "no" ]; then
    pass "T1: SESSION_BEAD_PATTERN='' → SQL path (dolt=yes bd=no)"
else
    fail "T1: SESSION_BEAD_PATTERN='' → expected SQL path; bd=$bd_called dolt=$dolt_called"
fi

# ── T2: gm-* pattern → bd prune path ──────────────────────────────────────────
result=$(run_step6 "gm-*")
bd_called=$(printf '%s' "$result" | cut -d'|' -f1)
dolt_called=$(printf '%s' "$result" | cut -d'|' -f2)
bd_args=$(printf '%s' "$result" | cut -d'|' -f3-)
if [ "$bd_called" = "yes" ] && [ "$dolt_called" = "no" ] \
        && printf '%s' "$bd_args" | grep -qF -- '--pattern gm-*'; then
    pass "T2: SESSION_BEAD_PATTERN=gm-* → bd prune with --pattern gm-* (dolt=no)"
else
    fail "T2: SESSION_BEAD_PATTERN=gm-* → expected bd path with --pattern gm-*; bd=$bd_called dolt=$dolt_called args=$bd_args"
fi

[ "$FAILED" -eq 0 ] && exit 0 || exit 1

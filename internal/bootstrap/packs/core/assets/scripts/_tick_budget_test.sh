#!/usr/bin/env bash
# _tick_budget_test.sh — self-test for _tick_budget.sh (vp-psukt / vc-q7hn).
#
# The Go packlint suite cannot carry this file's assertions while the
# pre-commit hook blocks .go commits on the build host (vp-od9jv), so this
# bash test stands in; a Go port in internal/bootstrap/packs/core is the
# follow-up once that unblocks. Run directly:
#
#   ./_tick_budget_test.sh
#
# Exits 0 on pass; on failure prints "FAIL: <what>" and exits 1.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Caller-facing helper variables assigned for the helper to read — SC2034 expected.
# shellcheck disable=SC2034
# shellcheck disable=SC1091
. "$HERE/_tick_budget.sh"

FAILURES=0
check() { # check <what> <want> <got>
    if [ "$2" = "$3" ]; then
        printf 'ok   %s\n' "$1"
    else
        printf 'FAIL %-30s want=[%s] got=[%s]\n' "$1" "$2" "$3"
        FAILURES=$((FAILURES + 1))
    fi
}
check_rc() { # check_rc <what> <want_rc(0|1)> <cmd...>
    local what="$1" want="$2"
    shift 2
    if "$@"; then got=0; else got=1; fi
    check "$what" "$want" "$got"
}

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
export VOXIST_ORDER_CURSOR_DIR="$SANDBOX/cursors"

# ── env sizing: per-order > global > default; junk and <=0 fall back ──
tb_init orphan-sweep 120
check "default sizing" "120" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S_ORPHAN_SWEEP=30 tb_init orphan-sweep 120
check "per-order override wins" "30" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S=45 tb_init other-order 120
check "global override" "45" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S=abc tb_init other-order 120
check "junk falls back" "120" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S=0 tb_init other-order 120
check "zero falls back" "120" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S=-5 tb_init other-order 120
check "negative falls back" "120" "$TB_SECONDS"
VOXIST_TICK_BUDGET_S=1.2.3 tb_init other-order 120
check "malformed decimal falls back" "120" "$TB_SECONDS"

# ── budget semantics ──
VOXIST_TICK_BUDGET_S=100 tb_init t 10
# shellcheck disable=SC2034
TB_RESERVE_S=3 TB_MIN_CALL_S=5
check_rc "fresh budget can start" 0 tb_can_start
check_rc "fresh budget not expired" 1 tb_expired
check "call_timeout clamps to cap" "60" "$(tb_call_timeout 60)"
check "call_timeout floors at min(cap, min_call)" "2" "$(tb_call_timeout 2)"
VOXIST_TICK_BUDGET_S=7 tb_init t2 10
# shellcheck disable=SC2034
TB_RESERVE_S=3 TB_MIN_CALL_S=5
check_rc "spendable below min_call is expired" 0 tb_expired
check "expired call_timeout still >= 1" "5" "$(tb_call_timeout 60)"

# ── cursor round-trip ──
tb_init x 10
tb_write_cursor x "bead-123"
check "cursor round-trip" "bead-123" "$(tb_read_cursor x)"
tb_write_cursor x -
check "cleared cursor reads empty" "" "$(tb_read_cursor x)"
check "missing cursor reads empty" "" "$(tb_read_cursor never-written)"

# ── batch_after: the gclib contract table ──
ITEMS="a
b
c
d"
TB_CURSOR=""; tb_batch_after 2 <<< "$ITEMS"
check "batch from top" "a
b" "$(printf '%s' "$TB_BATCH_ITEMS")"
check "batch from top next=b" "b" "$TB_NEXT_CURSOR"
check "batch from top deferred=2" "2" "$TB_DEFERRED"
TB_CURSOR="b"; tb_batch_after 2 <<< "$ITEMS"
check "resume after b" "c
d" "$(printf '%s' "$TB_BATCH_ITEMS")"
check "resume after b at end clears cursor" "" "$TB_NEXT_CURSOR"
TB_CURSOR="d"; tb_batch_after 2 <<< "$ITEMS"
check "cursor at last item wraps to top" "a
b" "$(printf '%s' "$TB_BATCH_ITEMS")"
TB_CURSOR="vanished"; tb_batch_after 2 <<< "$ITEMS"
check "unknown cursor restarts at top" "a
b" "$(printf '%s' "$TB_BATCH_ITEMS")"
TB_CURSOR=""; tb_batch_after 0 <<< "$ITEMS"
check "zero size defers everything" "4" "$TB_DEFERRED"
TB_CURSOR=""; tb_batch_after 9 <<< "$ITEMS"
check "oversize batch takes all" "a
b
c
d" "$(printf '%s' "$TB_BATCH_ITEMS")"
check "oversize batch at end clears cursor" "" "$TB_NEXT_CURSOR"

# ── tick_log shape: one greppable line ──
VOXIST_TICK_BUDGET_S=10 tb_init logorder 10
OUT="$(tb_log 4 6 "note" 2>&1)"
case "$OUT" in
    logorder:\ tick\ *s\ —\ 4\ processed,\ 6\ deferred,\ note) printf 'ok   %s\n' "tick_log shape" ;;
    *) printf 'FAIL %-30s got=[%s]\n' "tick_log shape" "$OUT"; FAILURES=$((FAILURES + 1)) ;;
esac

# ── two-tick cursor resume (plan AC6): deferred>0, cursor advances ──
tb_init resume-demo 60
tb_write_cursor resume-demo ""
# shellcheck disable=SC2034
TB_CURSOR="$(tb_read_cursor resume-demo)"
tb_batch_after 2 <<< "$ITEMS" >/dev/null
check "tick1 processed 2 of 4" "2" "$TB_DEFERRED"
tb_write_cursor resume-demo "$TB_NEXT_CURSOR"   # "b"
# shellcheck disable=SC2034
TB_CURSOR="$(tb_read_cursor resume-demo)"
tb_batch_after 2 <<< "$ITEMS" >/dev/null
check "tick2 resumes after cursor" "c
d" "$(printf '%s' "$TB_BATCH_ITEMS")"
check "tick2 completes the rotation" "" "$TB_NEXT_CURSOR"

# ── tb_timeout degrades without timeout(1) ──
check "tb_timeout runs the command" "hello" "$(tb_timeout 5 echo hello)"

# ── AC1: every sweep script sources the shared helper; no private copy ──
for script in cross-rig-deps.sh renudge-stale-human-gates.sh \
              cascade-nudge-on-blocker-close.sh orphan-sweep.sh reaper.sh; do
    if grep -q '_tick_budget.sh' "$HERE/$script"; then
        printf 'ok   %s sources _tick_budget.sh\n' "$script"
    else
        printf 'FAIL %-30s does not source the shared helper\n' "$script"
        FAILURES=$((FAILURES + 1))
    fi
    # A private sizing would drift from the shared constants (AC1: no
    # private copy of the budget logic).
    if grep -qE 'TICK_BUDGET_RESERVE_S=[0-9]|TICK_MIN_CALL_S=[0-9]' "$HERE/$script"; then
        printf 'FAIL %-30s carries a private copy of the budget constants\n' "$script"
        FAILURES=$((FAILURES + 1))
    else
        printf 'ok   %s has no private budget constants\n' "$script"
    fi
done

if [ "$FAILURES" -gt 0 ]; then
    printf '%d check(s) FAILED\n' "$FAILURES"
    exit 1
fi
printf 'all checks passed\n'

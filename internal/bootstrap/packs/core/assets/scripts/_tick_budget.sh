#!/bin/bash
# _tick_budget.sh — bash port of the gclib.py TickBudget contract (vc-djwg /
# vc-q7hn / vp-psukt). Sourced by the core-pack sweep scripts so a tick
# bounds ITSELF instead of being killed by the order deadline mid-write:
# a tick that runs out of budget is a SHORT tick, not a failed one — the
# next tick resumes after the cursor and nothing is skipped, only deferred.
#
# Source with the order name:
#
#   . "$(dirname "$0")/_tick_budget.sh" && tb_init "<order>" <default_s>
#
# Reference (do not reimplement here, keep the two reviewable against each
# other): voxist-platform packs/voxist-city/bin/gclib.py — class TickBudget,
# order_cursor_path, batch_after, tick_log. Constants carried VERBATIM.
#
# THE CONTRACT. Size the budget BELOW the order's `timeout` (default: 0.5x —
# see the order TOMLs). Guard the START of every work item with tb_expired /
# tb_can_start — never abandon an item mid-write — and wrap every in-loop
# subprocess in `timeout "$(tb_call_timeout N)"`. Emit exactly one tb_log
# line per tick.
#
# WHY A RESERVE. The budget is spent before the order deadline so the tick
# still has time to write its cursor and print its timing line. A tick killed
# by the supervisor prints nothing and advances nothing.
#
# MONOTONICITY GAP (deliberate weakening, do not "fix" back to wall-clock
# optimism): bash has no time.monotonic(). tb_now prefers the boot-relative
# clock (/proc/uptime on Linux) and falls back to EPOCHSECONDS/date +%s wall
# time elsewhere (macOS). A backward NTP step would hand a wall-clock sweep
# an infinite or negative budget, so tb_elapsed CLAMPS negative elapsed to 0
# — degrading to "budget exhausted" (safe), never "infinite budget".

# Wall-clock kept back from the budget for cursor write + tick line + exit.
TICK_BUDGET_RESERVE_S=3.0

# Smallest slice of budget worth STARTING a subprocess call with. See
# gclib.py TICK_MIN_CALL_S: below this floor a started call is an rc=124 the
# caller reads as a failed store; deferring is strictly better.
TICK_MIN_CALL_S=5.0

TICK_BUDGET_ENV="VOXIST_TICK_BUDGET_S"
TICK_ORDER_CURSOR_DIR_ENV="VOXIST_ORDER_CURSOR_DIR"

# Boot-relative seconds when available, else wall clock (see header note).
tb_now() {
    if [ -r /proc/uptime ]; then
        awk '{print $1}' /proc/uptime 2>/dev/null && return 0
    fi
    if [ -n "${EPOCHSECONDS:-}" ]; then
        printf '%s\n' "$EPOCHSECONDS"
    else
        date +%s
    fi
}

# `phase-feeder` -> `PHASE_FEEDER`, for per-order env overrides.
_tb_env_order_suffix() {
    printf '%s' "$1" | sed 's/[^A-Za-z0-9]/_/g' | tr '[:lower:]' '[:upper:]' | sed 's/[^A-Z0-9_]//g'
}

# tb_init <order> <default_s> — TickBudget.from_env.
# VOXIST_TICK_BUDGET_S_<ORDER> wins over VOXIST_TICK_BUDGET_S; an unset,
# unparseable, or non-positive value falls back to default_s so a typo
# degrades to the shipped sizing, not to a zero budget.
tb_init() {
    TB_ORDER="${1:?tb_init: order name required}"
    TB_DEFAULT_S="${2:?tb_init: default seconds required}"
    TB_RESERVE_S="${TB_RESERVE_S:-$TICK_BUDGET_RESERVE_S}"
    TB_MIN_CALL_S="${TB_MIN_CALL_S:-$TICK_MIN_CALL_S}"
    local __suffix __raw __secs
    __suffix="$(_tb_env_order_suffix "$TB_ORDER")"
    __varname="${TICK_BUDGET_ENV}_${__suffix}"
    __raw="${!__varname:-}"
    [ -z "$__raw" ] && __raw="${!TICK_BUDGET_ENV:-}"
    __raw="$(printf '%s' "$__raw" | tr -d '[:space:]')"
    __secs=""
    case "$__raw" in
        ''|*[!0-9.]*) __secs="" ;;
        *) __secs="$__raw" ;;
    esac
    # Decimal sanity: reject junk like "1.2.3" that the case above let through.
    case "$__secs" in
        *.*.*) __secs="" ;;
    esac
    # Fall back to the shipped sizing when the env value is absent or <= 0.
    if [ -z "$__secs" ] || awk -v s="$__secs" 'BEGIN{exit (s+0>0)?1:0}'; then
        __secs="$TB_DEFAULT_S"
    fi
    TB_SECONDS="$__secs"
    TB_START="$(tb_now)"
}

tb_elapsed() {
    local __e
    __e="$(awk -v now="$(tb_now)" -v start="${TB_START:-0}" 'BEGIN{d=now-start; printf "%.3f", (d<0?0:d)}')"
    printf '%s\n' "$__e"
}

tb_remaining() {
    awk -v s="${TB_SECONDS:-0}" -v e="$(tb_elapsed)" 'BEGIN{printf "%.3f", s-e}'
}

# Remaining time minus the reserve — what work may still be STARTED.
tb_spendable() {
    awk -v r="$(tb_remaining)" -v res="${TB_RESERVE_S:-$TICK_BUDGET_RESERVE_S}" \
        'BEGIN{printf "%.3f", r-res}'
}

# True (rc 0) when there is room to START a call needing at least $1 seconds
# (default TICK_MIN_CALL_S).
tb_can_start() {
    local __floor="${1:-}"
    [ -z "$__floor" ] && __floor="$TICK_MIN_CALL_S"
    awk -v s="$(tb_spendable)" -v f="$__floor" 'BEGIN{exit !(s+0 >= f+0)}'
}

# True (rc 0) once no further work item may be STARTED this tick. Use as
# `tb_expired && break` at the top of every loop body. This is "not
# tb_can_start", NOT "the budget hit zero" — a tick with 0.1s of room is out
# of budget for any real purpose. See TICK_MIN_CALL_S above.
tb_expired() {
    ! tb_can_start
}

# Subprocess bound for the next call: cap clamped to what is left. Never
# below min(cap, min_call) — a bound under the floor is an rc=124 waiting to
# happen. Only call this after tb_can_start cleared the call.
tb_call_timeout() {
    local __cap="${1:?tb_call_timeout: cap required}"
    awk -v cap="$__cap" -v s="$(tb_spendable)" -v m="${TB_MIN_CALL_S:-$TICK_MIN_CALL_S}" \
        'BEGIN{room=(s>m?s:m); t=int(cap<room?cap:room); printf "%d", (t<1?1:t)}'
}

# tb_timeout <seconds> <cmd...> — run cmd under timeout(1) where it exists.
# Deliberate weakening (same class as the monotonic-clock note above): stock
# macOS ships no timeout(1), so there the command runs UNBOUNDED and the
# budget bounds only when calls may START (tb_can_start / tb_expired still
# gate every loop). One backward-compatible seam for every wrapped call.
tb_timeout() {
    local __t="${1:?tb_timeout: seconds required}"
    shift
    if command -v timeout >/dev/null 2>&1; then
        timeout "$__t" "$@"
    else
        "$@"
    fi
}

# tb_cursor_path <order> — `<city>/.gc/cache/<order>.cursor` (gclib
# order_cursor_path). VOXIST_ORDER_CURSOR_DIR overrides the base outright so
# a test never touches the live city's cursors.
tb_cursor_path() {
    local __order="$1" __base __safe
    __base="${!TICK_ORDER_CURSOR_DIR_ENV:-}"
    if [ -z "$__base" ]; then
        __base="${GC_CITY_PATH:-${GC_CITY:-.}}/.gc/cache"
    fi
    __safe="$(printf '%s' "$__order" | sed 's/[^A-Za-z0-9._-]/_/g')"
    printf '%s/%s.cursor\n' "$__base" "$__safe"
}

# tb_read_cursor <order> — the item this order stopped after last tick, or
# empty to start at the top. Missing/corrupt cursor is empty, never an error
# (losing a cursor costs one repeated batch; refusing to run converts a cache
# blip into an outage).
tb_read_cursor() {
    local __path __after
    __path="$(tb_cursor_path "$1")"
    [ -r "$__path" ] || return 0
    __after="$(jq -r '.after // empty' "$__path" 2>/dev/null || true)"
    printf '%s\n' "$__after"
}

# tb_write_cursor <order> <after|-> — persist the carry-over point; `-`
# clears it (the sweep wrapped). Cursor loss repeats a batch; it must never
# fail a tick. Atomic via tmp+mv.
tb_write_cursor() {
    local __order="$1" __after="$2" __path __tmp __json
    __path="$(tb_cursor_path "$__order")"
    __tmp="${__path}.tmp.$$"
    if [ "$__after" = "-" ]; then
        __json='{"after":null}'
    else
        __json="$(jq -cn --arg a "$__after" --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
            '{after:$a, at:$t}' 2>/dev/null)" || __json="{\"after\":\"$__after\"}"
    fi
    mkdir -p "$(dirname "$__path")" 2>/dev/null || return 0
    printf '%s\n' "$__json" > "$__tmp" 2>/dev/null && mv -f "$__tmp" "$__path" 2>/dev/null || true
}

# tb_batch_after <size> — gclib batch_after over newline-separated items on
# stdin, resuming after $TB_CURSOR (set it from tb_read_cursor first).
# Invoke with a REDIRECT (`tb_batch_after 2 <<< "$items"`), never a pipeline
# or command substitution: the function must run in the calling shell to set
# TB_BATCH_ITEMS (the taken batch, newline-separated), TB_NEXT_CURSOR (empty
# = batch reached the end, next tick restarts at the top) and TB_DEFERRED.
# No wrap within a tick: re-taking handled items every rotation is worse
# than one short tick per rotation. An unknown cursor restarts at the top
# instead of erroring.
# Caller-facing output variables (TB_NEXT_CURSOR / TB_DEFERRED) are assigned
# here and read by the caller, so shellcheck's SC2034 is expected.
# shellcheck disable=SC2034
tb_batch_after() {
    local __size="${1:-0}" __items __count=0 __start=0 __i __key __found=0
    __items="$(cat)"
    TB_BATCH_ITEMS=""
    TB_NEXT_CURSOR=""
    TB_DEFERRED=0
    case "$__size" in ''|*[!0-9]*) __size=0 ;; esac
    [ "$__size" -gt 0 ] || {
        TB_DEFERRED="$(printf '%s\n' "$__items" | grep -c . 2>/dev/null || true)"
        return 0
    }
    __count="$(printf '%s\n' "$__items" | grep -c . 2>/dev/null || true)"
    if [ "$__count" -eq 0 ]; then
        return 0
    fi
    if [ -n "${TB_CURSOR:-}" ]; then
        __i=0
        while IFS= read -r __key; do
            [ -z "$__key" ] && continue
            if [ "$__key" = "$TB_CURSOR" ]; then
                __start=$((__i + 1))
                __found=1
                break
            fi
            __i=$((__i + 1))
        done <<EOF
$__items
EOF
    fi
    if [ "$__found" -eq 1 ] && [ "$__start" -ge "$__count" ]; then
        __start=0
    fi
    # Collect items[start : start+size] (skipping blanks) and remember the
    # last taken. Items accumulate in TB_BATCH_ITEMS, never stdout: the
    # function must run in the caller's shell for its output variables to
    # survive (a command substitution would run it in a subshell).
    __i=0
    while IFS= read -r __key; do
        [ -z "$__key" ] && continue
        if [ "$__i" -ge "$__start" ] && [ "$__i" -lt "$(( __start + __size ))" ]; then
            TB_BATCH_ITEMS="${TB_BATCH_ITEMS}${__key}"$'\n'
            TB_NEXT_CURSOR="$__key"
        fi
        __i=$((__i + 1))
    done <<EOF
$__items
EOF
    if [ "$(( __start + __size ))" -ge "$__count" ]; then
        TB_NEXT_CURSOR=""
    fi
    TB_DEFERRED=$(( __count > __size ? __count - __size : 0 ))
}

# tb_log <processed> <deferred> [note] — ONE machine-greppable timing line
# per tick (the measurement that was missing when these deadlines were first
# guessed). Printed unconditionally, including on an empty tick.
tb_log() {
    local __processed="${1:-0}" __deferred="${2:-0}" __note="${3:-}" __tail=""
    [ -n "$__note" ] && __tail=", $__note"
    printf '%s: tick %ss — %s processed, %s deferred%s\n' \
        "${TB_ORDER:-unknown}" "$(tb_elapsed)" "$__processed" "$__deferred" "$__tail" >&2
}

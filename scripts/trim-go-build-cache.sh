#!/usr/bin/env bash
#
# Age-based trim of the shared Go build cache.
#
# Every agent on this host builds against ONE shared Go build cache (each
# [providers.*].env block in voxist-city/city.toml sets
# GOCACHE=${HOME}/Library/Caches/go-build, which is also Go's macOS default).
# Go trims that cache itself, but only at a fixed 5-day horizon
# (cmd/go/internal/cache: trimLimit = 5 * 24 * time.Hour) and with NO size cap
# of any kind -- there is no GOCACHE size limit in Go 1.27. At this fleet's
# build volume the 5-day working set reached 463 GiB / 843k entries and filled
# the volume. This job applies the same operation Go applies, at a shorter
# horizon, so the cache stays bounded.
#
# CONCURRENCY: WHAT WAS MEASURED
#
# An earlier revision of this script carried a section arguing, from a reading
# of cmd/go/internal/cache, that "every Go read path degrades a missing or
# short entry to a cache miss and a rebuild, never to a bad build". That claim
# was FALSE. On 2026-09-05 a snapshot-then-delete run of this script broke
# builds for three agents plus a developer push, repeatedly:
#
#     could not import slices (open .../bc/bc66f4...-d: no such file or directory)
#     could not import log    (open .../6d/6d4b98...-d: no such file or directory)
#     link: cannot open file  .../74/74a81b...-d: no such file or directory
#
# Hard failures, not cache misses. `slices` and `log` are stdlib and GOROOT was
# intact. Retrying did not converge: each attempt died on a different, freshly
# deleted entry. Do not restore a claim of the old shape without a test that
# demonstrates it; see "what is demonstrated" below.
#
# The mechanism, which the original reasoning got wrong by covering the wrong
# window: cmd/go's miss handling protects the LOOKUP, not the later open.
# GetFile stats the "-d" and rejects it if absent or the wrong size; GetBytes
# verifies SHA-256. But OutputFile() returns a PATH, and cmd/go hands that path
# to the compiler or linker, which opens it later. An unlink landing in between
# is ENOENT at the tool, and the build fails outright. Go tolerates an entry it
# never found; it does not tolerate one that disappears after it was found.
#
# The original reasoning about os.Remove and open descriptors is correct but
# irrelevant to this: the failing process had not opened the file yet, it held
# only the path.
#
# WHY THE CURRENT DESIGN DOES NOT DO THAT
#
# Go's trimSubdir stats and removes adjacently --
#
#     info, err := os.Stat(entry)
#     if err == nil && info.ModTime().Before(cutoff) { os.Remove(entry) }
#
# -- so its exposure is microseconds. The broken revision selected the whole
# tree with find and deleted from that list minutes later; on this cache the
# scan alone takes 10-20 minutes. Because markUsed() bumps an entry's mtime the
# moment a build looks it up, a build could resolve an entry INSIDE that window
# and the stale list would delete it anyway. The threshold was never the
# problem. The window was.
#
# The delete phase below therefore re-stats every entry immediately before
# removing it and skips anything whose mtime moved. Since markUsed() leaves an
# mtime at most one hour stale after any lookup, an entry still older than
# TRIM_DAYS at that instant cannot be held by a live build.
#
# WHAT IS DEMONSTRATED, AND BY WHICH TEST (scripts/test-trim-go-build-cache.sh)
#
#   CASE 11  an entry refreshed between selection and deletion survives. This
#            is the regression for the failure above; reverting the re-stat
#            guard fails it.
#   CASE 10  real `go build`s racing a from-zero trim still exit 0 with correct
#            output. Also fails if the re-stat guard is reverted.
#
# What is NOT demonstrated and is stated as reasoning only: that a removal can
# never yield a WRONG build (as opposed to a failed one). That rests on unlink
# not being truncation, and on Go committing a "-d" by writing its final byte
# last so a short file is never read as complete. Treat it accordingly.
#
# NEVER use "go clean -cache" for this. It RemoveAll's all 256 shard
# directories, hot entries included, so every concurrent build misses on
# everything at once -- the cascading-rebuild incident vp-g96b. That is a
# different operation from an age-based trim and is banned repo-wide
# (AGENTS.md, "Build Cache Conventions"). "go clean -testcache" is allowed but
# is not needed here and is not used.
#
# SIZE CAP (TRIM_MAX_GIB)
#
# Age alone does not bound size: at this fleet's build volume, even a 1-day
# cutoff can leave ~70 GiB resident, and the host has filled before the next
# scheduled run (ga-zqc34, 2026-10-01: 108 GiB free, cache at 114 GiB, 98%
# disk). When TRIM_MAX_GIB is set, a second pass runs after the age-based
# pass: if the cache is still above the cap, it removes candidate entries
# oldest-first, by mtime, until at or under the cap. It reuses the identical
# candidate filter as the age-based pass (<cache>/<xx>/<hash>-{a,d} only, via
# the same -mindepth/-maxdepth confinement) and the identical re-stat-before-
# unlink guard against the 2026-09-05 race described above -- an entry a build
# resolves between the sizing pass and the delete pass is skipped, not
# removed, and the next-oldest entry is evicted in its place so the cap is
# still met.
#
# Ordering uses mtime, not atime, and this is a deliberate choice, not an
# oversight: a touch+read probe on this volume (APFS) showed atime does NOT
# update on a plain read, so it cannot distinguish hot from cold entries here.
# mtime does not have that problem for this purpose -- but only coarsely. Go's
# markUsed() touches an entry's mtime on lookup ONLY when the entry is already
# >= 1h old (mtimeInterval = 1h in cmd/go/internal/cache). So an entry written
# or used within the last hour keeps its old mtime, and "oldest mtime" means
# "least recently used" only to within about 1h.
#
# That 1h blind spot is why the re-stat guard alone is not enough for the size
# cap, and why the cap pass has a hard AGE FLOOR (TRIM_CAP_MIN_AGE_HOURS,
# default 3, minimum 2): an entry whose mtime is newer than now-floor is never
# evicted, because cmd/go may have resolved it (OutputFile returns a PATH the
# compiler opens later) without any mtime change a re-stat could see. If the
# cap cannot be met without crossing the floor, the pass evicts what it may,
# logs "sizecap UNMET remaining_gib=... floor=...", and exits non-zero, so the
# shortfall is loud instead of silent. Unlinking an entry a writer still holds
# open is safe (Go writes entries in place and the writer keeps its inode); the
# floor protects the resolved-but-not-yet-opened window, not writers.
#
# Usage:
#   trim-go-build-cache.sh [--dry-run]
#
# Environment:
#   TRIM_DAYS           entries unused for longer than this are removed (default 3)
#   TRIM_HOURS          same cutoff as TRIM_DAYS but in hours (integer >= 3);
#                       overrides TRIM_DAYS for the age-based pass when set
#                       (no default)
#   TRIM_CAP_MIN_AGE_HOURS  hard age floor for the size-cap pass: an entry
#                       newer than this is NEVER evicted, even if the cap is
#                       then not met (integer >= 2, default 3)
#   TRIM_MAX_GIB        after the age-based pass, if the cache is still above
#                       this many GiB, remove candidates oldest-first (by
#                       mtime) until at or under it (no default: unset means
#                       no size cap, age-only, matching prior behavior)
#   GO_BUILD_CACHE_DIR  cache to trim (default $HOME/Library/Caches/go-build)
#   TRIM_LOG            log file (default $HOME/Library/Logs/gocache-trim.log)
#   TRIM_LOG_MAX_LINES  log is truncated to this many lines each run (default 365)
#   TRIM_DELAY_BEFORE_DELETE  TEST ONLY. Seconds to widen the age-based pass's
#                       snapshot-to-delete window so the regression test can
#                       refresh an entry inside it. Always 0 in production.
#   TRIM_SIZE_DELAY_BEFORE_DELETE  TEST ONLY. Same, for the size-cap pass's
#                       window. A separate knob because the two passes run
#                       sequentially in one invocation. Always 0 in production.
#
# This script deliberately sets neither GOCACHE nor TMPDIR.

set -euo pipefail
export LC_ALL=C

# /usr/bin/find, never bare "find". On this host "find" resolves to a shell
# function that routes to bfs, which does not accept -newermt: it fails the
# expression, exits 0, and the trim silently does nothing while reporting
# success. Pinning the real BSD find is load-bearing, not stylistic.
# TRIM_FIND exists so the self-test can exercise the preflight below; the
# preflight, not the path, is what makes an unsuitable find safe.
FIND="${TRIM_FIND:-/usr/bin/find}"

TRIM_DAYS="${TRIM_DAYS:-3}"
TRIM_HOURS="${TRIM_HOURS:-}"
TRIM_MAX_GIB="${TRIM_MAX_GIB:-}"
TRIM_CAP_MIN_AGE_HOURS="${TRIM_CAP_MIN_AGE_HOURS:-3}"
CACHE_DIR="${GO_BUILD_CACHE_DIR:-$HOME/Library/Caches/go-build}"
LOG="${TRIM_LOG:-$HOME/Library/Logs/gocache-trim.log}"
LOG_MAX_LINES="${TRIM_LOG_MAX_LINES:-365}"

DRY_RUN=0
case "${1:-}" in
	--dry-run) DRY_RUN=1 ;;
	"") ;;
	*) echo "usage: $(basename "$0") [--dry-run]" >&2; exit 2 ;;
esac

# log_line appends one line and re-truncates, so the log can never grow without
# bound. An unrotated append-only log is how otel-events.jsonl reached 123 MB
# and starved order dispatch on this fleet.
log_line() {
	[ "${DRY_RUN:-0}" -eq 0 ] || return 0
	mkdir -p "$(dirname "$LOG")" 2>/dev/null || return 0
	printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >> "$LOG"
	if [ "$(wc -l < "$LOG")" -gt "$LOG_MAX_LINES" ]; then
		tail -n "$LOG_MAX_LINES" "$LOG" > "$LOG.tmp" && mv -f "$LOG.tmp" "$LOG"
	fi
}

# awk does the formatting so printf only ever sees a plain string: awk emits
# scientific notation for small byte counts, which printf %f rejects.
gib() { echo "$1" | awk '{printf "%.1f", $1/1073741824}'; }

# Failures land in the same bounded log, so the launchd job needs no stdout or
# stderr redirect file of its own -- those grow unbounded and are exactly what
# this script is trying not to do.
die() {
	echo "trim-go-build-cache: $*" >&2
	log_line "FAILED: $*"
	exit 1
}

# Preflight: the find binary must exist and must actually honour -newermt.
# Probing it here converts trap-1 (a find that silently ignores the predicate)
# from a silent no-op into a loud failure.
[ -x "$FIND" ] || die "$FIND is missing or not executable"
probe_dir="$(mktemp -d)"
trap 'rm -rf "$probe_dir"' EXIT
# TWO polarities, deliberately. Selection below uses the NEGATED predicate
# (`! -newermt`), so probing only that a fresh file MATCHES would pass a find
# that evaluates -newermt as a constant true -- and `! true` then selects
# nothing, so the job deletes nothing and exits 0. That is the same observable
# outcome as the bfs trap this preflight exists to close: a silent no-op
# reported as success. Asserting both directions catches a find that ignores
# the predicate whichever way it fails.
: > "$probe_dir/probe_new"
: > "$probe_dir/probe_old"
touch -t "$(date -v-10d '+%Y%m%d%H%M' 2>/dev/null || date -d '10 days ago' '+%Y%m%d%H%M')" \
	"$probe_dir/probe_old" 2>/dev/null \
	|| die "neither BSD nor GNU date accepted a relative offset; cannot probe $FIND"
probe_out="$("$FIND" "$probe_dir" -maxdepth 1 -name 'probe_*' -newermt '1 day ago' -print 2>/dev/null)"
if ! printf '%s\n' "$probe_out" | grep -q probe_new; then
	die "$FIND does not support -newermt; refusing to run (a trim that cannot
evaluate the age predicate would report success while deleting nothing)"
fi
if printf '%s\n' "$probe_out" | grep -q probe_old; then
	die "$FIND evaluates -newermt as always-true; refusing to run (the negated
predicate used for selection would then match nothing and the trim would
report success while deleting nothing)"
fi

[ -n "$TRIM_DAYS" ] && [ "$TRIM_DAYS" -ge 1 ] 2>/dev/null \
	|| die "TRIM_DAYS must be an integer >= 1 (got: ${TRIM_DAYS})"

# TRIM_HOURS, when set, is a finer-grained override of the same cutoff that
# TRIM_DAYS otherwise provides -- AGE_SPEC/AGE_DESC below are the single
# place that choice is resolved, so the find predicate and every log/report
# line downstream agree with each other.
if [ -n "$TRIM_HOURS" ]; then
	# >= 3: Go refreshes an entry mtime only once it is >= 1h old, so an age
	# cutoff of 1h has zero margin over that interval.
	[[ "$TRIM_HOURS" =~ ^[0-9]+$ ]] && [ "$TRIM_HOURS" -ge 3 ] \
		|| die "TRIM_HOURS must be an integer >= 3 (got: ${TRIM_HOURS})"
	AGE_SPEC="${TRIM_HOURS} hours ago"
	AGE_DESC="${TRIM_HOURS}h"
else
	AGE_SPEC="${TRIM_DAYS} days ago"
	AGE_DESC="${TRIM_DAYS}d"
fi

# TRIM_MAX_GIB is optional (unset = no size cap, age-only, matching prior
# behavior). When set it must be a positive number; awk does the numeric
# comparison so this also accepts decimals (e.g. "40.5"), which plain [ -ge ]
# cannot.
if [ -n "$TRIM_MAX_GIB" ]; then
	echo "$TRIM_MAX_GIB" | grep -qE '^[0-9]+(\.[0-9]+)?$' \
		|| die "TRIM_MAX_GIB must be a positive number of GiB (got: ${TRIM_MAX_GIB})"
	awk -v v="$TRIM_MAX_GIB" 'BEGIN { exit !(v + 0 > 0) }' \
		|| die "TRIM_MAX_GIB must be a positive number of GiB (got: ${TRIM_MAX_GIB})"
fi

[[ "$TRIM_CAP_MIN_AGE_HOURS" =~ ^[0-9]+$ ]] && [ "$TRIM_CAP_MIN_AGE_HOURS" -ge 2 ] \
	|| die "TRIM_CAP_MIN_AGE_HOURS must be an integer >= 2 (got: ${TRIM_CAP_MIN_AGE_HOURS})"
CAP_FLOOR_EPOCH="$(date -v-"${TRIM_CAP_MIN_AGE_HOURS}"H '+%s' 2>/dev/null \
	|| date -d "${TRIM_CAP_MIN_AGE_HOURS} hours ago" '+%s')" \
	|| die "cannot compute the size-cap age floor for TRIM_CAP_MIN_AGE_HOURS=${TRIM_CAP_MIN_AGE_HOURS}"

[ -d "$CACHE_DIR" ] || die "cache dir does not exist: $CACHE_DIR"

# Refuse to run against anything that is not shaped like a Go build cache, so a
# mistyped GO_BUILD_CACHE_DIR cannot aim this at an unrelated tree.
if ! "$FIND" "$CACHE_DIR" -mindepth 1 -maxdepth 1 -type d -name '[0-9a-f][0-9a-f]' \
	-print -quit | grep -q .; then
	die "$CACHE_DIR does not look like a Go build cache (no NN shard dirs)"
fi

# Resolve to a physical path so the validator below compares like with like.
CACHE_DIR="$(cd "$CACHE_DIR" && pwd -P)"

# Candidate selection.
#
#   -mindepth 2 -maxdepth 2   confines this to <cache>/<xx>/<hash>-{a,d}. The
#                             cache root itself, the 256 shard dirs, and the
#                             depth-1 metadata files (trim.txt, testexpire.txt,
#                             README, log.txt) are all out of range. trim.txt in
#                             particular is Go's own trim bookkeeping: deleting
#                             it would make Go re-trim the whole tree.
#   -name '*-a' -o '*-d'      exactly Go's filter ("Remove only cache entries
#                             (xxxx-a and xxxx-d)").
#   ! -newermt "N days ago"   not used in N days. Preferred over -mtime +N,
#                             which truncates to whole 24h periods.
#
list="$probe_dir/candidates"
"$FIND" "$CACHE_DIR" -mindepth 2 -maxdepth 2 \
	\( -name '*-a' -o -name '*-d' \) \
	! -newermt "$AGE_SPEC" \
	-print0 > "$list" 2>/dev/null || true

# Selection above produced a SNAPSHOT. Between that snapshot and the unlink
# below, a build can look the entry up -- and Go's markUsed() then bumps its
# mtime to now, making it hot. Deleting it anyway is how this job broke a
# concurrent `make test-fast-parallel` on 2026-09-05: cmd/go resolved a cached
# "-d" through OutputFile(), handed the PATH to the linker, and the linker got
# ENOENT when it opened it. cmd/go's miss handling (GetFile's size check,
# GetBytes' SHA-256) protects the lookup, NOT the later open by the tool, so a
# vanished-after-lookup entry is a hard build failure, not a rebuild.
#
# Go does not have this problem at any meaningful width because trimSubdir
# stats and removes ADJACENTLY:
#
#     info, err := os.Stat(entry)
#     if err == nil && info.ModTime().Before(cutoff) { os.Remove(entry) }
#
# so its window is microseconds. A snapshot-then-delete pass leaves a window as
# wide as the scan -- 10-20 minutes on this cache. The threshold was never the
# problem; the window was.
#
# So the delete phase below re-stats every entry immediately before removing it
# and skips anything whose mtime moved, which is exactly Go's check. Because
# markUsed() leaves an mtime at most one hour stale after any lookup, an entry
# still older than TRIM_DAYS at that instant cannot be held by a live build.
# AGE_CUTOFF_EPOCH is the single place TRIM_DAYS/TRIM_HOURS become a concrete
# instant, computed once in bash (via the already-validated AGE_SPEC) and
# handed to the python phases below as a number, so the selection predicate
# above and the re-stat check in delete_phase can never disagree with each
# other about what "stale" means.
if [ -n "$TRIM_HOURS" ]; then
	AGE_CUTOFF_EPOCH="$(date -v-"${TRIM_HOURS}"H '+%s' 2>/dev/null \
		|| date -d "${TRIM_HOURS} hours ago" '+%s')" \
		|| die "cannot compute the age cutoff for TRIM_HOURS=${TRIM_HOURS}"
else
	AGE_CUTOFF_EPOCH="$(date -v-"${TRIM_DAYS}"d '+%s' 2>/dev/null \
		|| date -d "${TRIM_DAYS} days ago" '+%s')" \
		|| die "cannot compute the age cutoff for TRIM_DAYS=${TRIM_DAYS}"
fi

# PY_COMMON is the Python prelude shared by both phases, so the path validator,
# the entry regex and the size computation exist exactly once. It must contain
# no single quotes: it is concatenated into python3 -c '...' programs below.
PY_COMMON='
import os, re, shutil, sys, time

cache = os.environ["CACHE_DIR"]
dry   = os.environ["DRY_RUN"] == "1"

# fullmatch, not match with a trailing dollar: a dollar also accepts a
# trailing newline, which would let a path with a newline through.
entry_re = re.compile(re.escape(cache) + r"/[0-9a-f]{2}/[0-9a-f]{64}-[ad]")

def load_candidates():
    # Fail closed: validate the whole snapshot before touching anything. One
    # unexpected path aborts the run rather than being skipped. This is the
    # guard against the bug that retired the previous prune script, whose
    # "go-build-*" glob also matched the empty suffix -- the live cache root --
    # and would have deleted the entire shared cache.
    decoded = []
    for raw in sys.stdin.buffer.read().split(b"\0"):
        if not raw:
            continue
        p = raw.decode("utf-8", "surrogateescape")
        if not entry_re.fullmatch(p):
            sys.stderr.write("refusing to delete unexpected path: %s\n" % p)
            sys.exit(3)
        decoded.append(p)
    return decoded

def is_dir(p):
    return os.path.isdir(p) and not os.path.islink(p)

def entry_size(p, st, isdir):
    if not isdir:
        return st.st_size
    size = 0
    for root, _, files in os.walk(p):
        for f in files:
            try: size += os.lstat(os.path.join(root, f)).st_size
            except OSError: pass
    return size
'

delete_phase() {
	CACHE_DIR="$CACHE_DIR" AGE_CUTOFF_EPOCH="$AGE_CUTOFF_EPOCH" DRY_RUN="$DRY_RUN" \
	TRIM_DELAY_BEFORE_DELETE="${TRIM_DELAY_BEFORE_DELETE:-0}" \
	python3 -c "$PY_COMMON"'
cutoff = float(os.environ["AGE_CUTOFF_EPOCH"])
decoded = load_candidates()

# Test-only seam: widen the snapshot-to-delete window on purpose so the
# regression test can refresh an entry inside it. Always 0 in production.
delay = float(os.environ.get("TRIM_DELAY_BEFORE_DELETE", "0") or 0)
if delay:
    # Announce the open window so the test can refresh an entry inside it
    # deterministically instead of guessing with a sleep (host load makes
    # any fixed sleep flaky).
    sys.stderr.write("trim-test-seam: window open\n"); sys.stderr.flush()
    time.sleep(delay)

removed = skipped = gone = 0
freed = 0
for p in decoded:
    try:
        st = os.lstat(p)            # fresh stat, immediately before removal
    except FileNotFoundError:
        gone += 1
        continue
    if st.st_mtime >= cutoff:       # refreshed since selection: now hot, leave it
        skipped += 1
        continue
    isdir = is_dir(p)
    size = entry_size(p, st, isdir)
    if dry:
        removed += 1; freed += size
        continue
    try:
        shutil.rmtree(p) if isdir else os.remove(p)
    except FileNotFoundError:
        gone += 1
        continue
    except OSError as e:
        sys.stderr.write("remove %s: %s\n" % (p, e))
        continue
    removed += 1; freed += size

print("%d %d %d %d" % (removed, freed, skipped, gone))
'
}

# size_cap_phase implements the TRIM_MAX_GIB pass described at the top of this
# file. It takes the SAME null-delimited candidate list shape as delete_phase
# (and applies the identical fail-closed path validation), but unlike
# delete_phase it is handed every surviving candidate, not just the
# age-stale ones -- it has to know the cache's current total size to decide
# whether to do anything at all.
#
# Ordering is oldest mtime first (see the top-of-file rationale). The
# re-stat-immediately-before-unlink guard is the same one delete_phase uses.
# BUT that guard is not sufficient on its own: markUsed() only touches an entry
# that is already >= 1h old, so an entry written or used in the last hour keeps
# its old mtime and the re-stat cannot tell it is in use. The AGE FLOOR
# (CAP_FLOOR_EPOCH) is therefore a hard rule: nothing newer than the floor is
# ever evicted, however far over the cap the cache is. If the cap cannot be met
# without crossing it, the phase stops and reports the shortfall.
#
# Prints: removed freed skipped total remaining errors
size_cap_phase() {
	# Deliberately its OWN delay knob (TRIM_SIZE_DELAY_BEFORE_DELETE), not
	# TRIM_DELAY_BEFORE_DELETE: the two phases run sequentially in the same
	# invocation, so sharing one knob would make a test of this phase also pay
	# delete_phase's delay. Always 0 in production either way.
	CACHE_DIR="$CACHE_DIR" CAP_GIB="$TRIM_MAX_GIB" DRY_RUN="$DRY_RUN" \
	CAP_FLOOR_EPOCH="$CAP_FLOOR_EPOCH" \
	TRIM_DELAY_BEFORE_DELETE="${TRIM_SIZE_DELAY_BEFORE_DELETE:-0}" \
	python3 -c "$PY_COMMON"'
cap_bytes = float(os.environ["CAP_GIB"]) * 1073741824
floor = float(os.environ["CAP_FLOOR_EPOCH"])
decoded = load_candidates()

# Single stat pass over every surviving candidate: (mtime, path, isdir, size).
# The current total comes from here rather than a separate du, since every
# entry is visited anyway.
entries = []
total = 0
for p in decoded:
    try:
        st = os.lstat(p)
    except FileNotFoundError:
        continue
    isdir = is_dir(p)
    size = entry_size(p, st, isdir)
    total += size
    entries.append((st.st_mtime, p, isdir, size))

if total <= cap_bytes:
    print("0 0 0 %d %d 0" % (total, total))
    sys.exit(0)

# Oldest mtime first: within about 1h this is least-recently-used, because
# markUsed() refreshes the mtime of an entry on lookup only once it is >= 1h old.
entries.sort(key=lambda e: e[0])

delay = float(os.environ.get("TRIM_DELAY_BEFORE_DELETE", "0") or 0)
if delay:
    # Announce the open window so the test can refresh an entry inside it
    # deterministically instead of guessing with a sleep (host load makes
    # any fixed sleep flaky).
    sys.stderr.write("trim-test-seam: window open\n"); sys.stderr.flush()
    time.sleep(delay)

removed = skipped = errors = 0
freed = 0
remaining = total
for mtime, p, isdir, size in entries:
    if remaining <= cap_bytes:
        break
    if mtime >= floor:
        break                       # sorted: everything after is newer still
    try:
        st = os.lstat(p)            # fresh stat, immediately before removal
    except FileNotFoundError:
        remaining -= size           # someone else removed it: it no longer counts
        continue
    if st.st_mtime > mtime or st.st_mtime >= floor:
        skipped += 1                # refreshed since the sizing pass: now hot
        continue                    # cap not yet met: evict the next-oldest instead
    if dry:
        removed += 1; freed += size; remaining -= size
        continue
    try:
        shutil.rmtree(p) if isdir else os.remove(p)
    except FileNotFoundError:
        remaining -= size
        continue
    except OSError as e:
        sys.stderr.write("remove %s: %s\n" % (p, e))
        errors += 1
        continue
    removed += 1; freed += size; remaining -= size

print("%d %d %d %d %d %d" % (removed, freed, skipped, total, remaining, errors))
'
}

candidates="$(tr -cd '\0' < "$list" | wc -c | tr -d ' ')"
log_line "start candidates=$candidates age=$AGE_DESC cache=$CACHE_DIR"

if ! result="$(delete_phase < "$list")"; then
	die "delete phase refused to run (see message above); nothing was removed"
fi
count="${result%% *}"
rest="${result#* }"
bytes="${rest%% *}"
rest="${rest#* }"
skipped="${rest%% *}"
gone="${rest#* }"

if [ "$DRY_RUN" -eq 1 ]; then
	printf 'dry-run: would remove %d entries (%d bytes, %s GiB) older than %s from %s\n' \
		"$count" "$bytes" "$(gib "$bytes")" "$AGE_DESC" "$CACHE_DIR"
else
	log_line "$(printf 'trimmed=%d bytes=%d gib=%s skipped_hot=%d already_gone=%d age=%s cache=%s' \
		"$count" "$bytes" "$(gib "$bytes")" "$skipped" "$gone" "$AGE_DESC" "$CACHE_DIR")"

	printf 'trimmed %d entries (%d bytes), skipped %d refreshed, %d already gone, older than %s from %s\n' \
		"$count" "$bytes" "$skipped" "$gone" "$AGE_DESC" "$CACHE_DIR"
fi

# ---------------------------------------------------------------------------
# SIZE CAP (TRIM_MAX_GIB). Runs after the age-based pass above, whether or not
# that pass found anything to do: age-only trimming does not bound size (see
# the top-of-file rationale), so this pass is what actually keeps the cache
# under TRIM_MAX_GIB when the age cutoff alone does not get there.
#
# Skipped entirely when TRIM_MAX_GIB is unset, which is the default and
# preserves the prior (age-only) behavior exactly.
if [ -n "$TRIM_MAX_GIB" ]; then
	# A fresh listing: the age-based pass above may have just removed some of
	# these candidates, and new entries may have appeared since the first
	# find ran. Same selector, same depth confinement, same name filter --
	# this is not a new candidate definition, just a re-scan of it.
	size_list="$probe_dir/size_candidates"
	"$FIND" "$CACHE_DIR" -mindepth 2 -maxdepth 2 \
		\( -name '*-a' -o -name '*-d' \) \
		-print0 > "$size_list" 2>/dev/null || true

	if ! size_result="$(size_cap_phase < "$size_list")"; then
		die "size-cap phase refused to run (see message above); nothing further was removed"
	fi
	read -r sc_removed sc_freed sc_skipped sc_total sc_remaining sc_errors <<< "$size_result"

	total_gib="$(gib "$sc_total")"
	remaining_gib="$(gib "$sc_remaining")"
	cap_bytes="$(awk -v g="$TRIM_MAX_GIB" 'BEGIN { printf "%.0f", g * 1073741824 }')"
	cap_met=1
	[ "$sc_remaining" -le "$cap_bytes" ] || cap_met=0

	if [ "$DRY_RUN" -eq 1 ]; then
		printf 'dry-run: cache is %s GiB (cap %s GiB); would remove %d more entries (%d bytes, %s GiB) oldest-first to reach the cap (overlaps the age-pass figure above: that pass is not applied first in a dry run); cap_met=%d remaining=%s GiB\n' \
			"$total_gib" "$TRIM_MAX_GIB" "$sc_removed" "$sc_freed" "$(gib "$sc_freed")" "$cap_met" "$remaining_gib"
	else
		log_line "$(printf 'sizecap total_before_gib=%s cap_gib=%s removed=%d bytes=%d gib=%s skipped_hot=%d errors=%d remaining_gib=%s cap_met=%d cache=%s' \
			"$total_gib" "$TRIM_MAX_GIB" "$sc_removed" "$sc_freed" "$(gib "$sc_freed")" "$sc_skipped" "$sc_errors" "$remaining_gib" "$cap_met" "$CACHE_DIR")"

		printf 'size cap: cache was %s GiB (cap %s GiB), removed %d more entries (%d bytes, %s GiB) oldest-first, skipped %d refreshed, %d errors; remaining %s GiB cap_met=%d\n' \
			"$total_gib" "$TRIM_MAX_GIB" "$sc_removed" "$sc_freed" "$(gib "$sc_freed")" "$sc_skipped" "$sc_errors" "$remaining_gib" "$cap_met"
	fi

	if [ "$cap_met" -eq 0 ]; then
		unmet="sizecap UNMET remaining_gib=${remaining_gib} cap_gib=${TRIM_MAX_GIB} floor=${TRIM_CAP_MIN_AGE_HOURS}h errors=${sc_errors}"
		echo "trim-go-build-cache: $unmet" >&2
		log_line "$unmet"
		# A dry run only plans; the real run is what must fail loudly.
		[ "$DRY_RUN" -eq 1 ] || exit 1
	fi
fi

exit 0

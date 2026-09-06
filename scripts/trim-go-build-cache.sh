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
# Usage:
#   trim-go-build-cache.sh [--dry-run]
#
# Environment:
#   TRIM_DAYS           entries unused for longer than this are removed (default 3)
#   GO_BUILD_CACHE_DIR  cache to trim (default $HOME/Library/Caches/go-build)
#   TRIM_LOG            log file (default $HOME/Library/Logs/gocache-trim.log)
#   TRIM_LOG_MAX_LINES  log is truncated to this many lines each run (default 365)
#   TRIM_DELAY_BEFORE_DELETE  TEST ONLY. Seconds to widen the snapshot-to-delete
#                       window so the regression test can refresh an entry
#                       inside it. Always 0 in production.
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
	! -newermt "${TRIM_DAYS} days ago" \
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
delete_phase() {
	CACHE_DIR="$CACHE_DIR" TRIM_DAYS="$TRIM_DAYS" DRY_RUN="$DRY_RUN" \
	TRIM_DELAY_BEFORE_DELETE="${TRIM_DELAY_BEFORE_DELETE:-0}" \
	python3 -c '
import os, re, shutil, sys, time

cache = os.environ["CACHE_DIR"]
days  = int(os.environ["TRIM_DAYS"])
dry   = os.environ["DRY_RUN"] == "1"
cutoff = time.time() - days * 86400

entry_re = re.compile(r"^" + re.escape(cache) + r"/[0-9a-f]{2}/[0-9a-f]{64}-[ad]$")

paths = [p for p in sys.stdin.buffer.read().split(b"\0") if p]

# Fail closed: validate the whole snapshot before touching anything. One
# unexpected path aborts the run rather than being skipped. This is the guard
# against the bug that retired the previous prune script, whose "go-build-*"
# glob also matched the empty suffix -- the live cache root -- and would have
# deleted the entire shared cache.
decoded = []
for raw in paths:
    p = raw.decode("utf-8", "surrogateescape")
    if not entry_re.match(p):
        sys.stderr.write("refusing to delete unexpected path: %s\n" % p)
        sys.exit(3)
    decoded.append(p)

# Test-only seam: widen the snapshot-to-delete window on purpose so the
# regression test can refresh an entry inside it. Always 0 in production.
delay = float(os.environ.get("TRIM_DELAY_BEFORE_DELETE", "0") or 0)
if delay:
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
    isdir = os.path.isdir(p) and not os.path.islink(p)
    if isdir:
        size = 0
        for root, _, files in os.walk(p):
            for f in files:
                try: size += os.lstat(os.path.join(root, f)).st_size
                except OSError: pass
    else:
        size = st.st_size
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

candidates="$(tr -cd '\0' < "$list" | wc -c | tr -d ' ')"
log_line "start candidates=$candidates days=$TRIM_DAYS cache=$CACHE_DIR"

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
	printf 'dry-run: would remove %d entries (%d bytes, %s GiB) older than %s days from %s\n' \
		"$count" "$bytes" "$(gib "$bytes")" "$TRIM_DAYS" "$CACHE_DIR"
	exit 0
fi

log_line "$(printf 'trimmed=%d bytes=%d gib=%s skipped_hot=%d already_gone=%d days=%s cache=%s' \
	"$count" "$bytes" "$(gib "$bytes")" "$skipped" "$gone" "$TRIM_DAYS" "$CACHE_DIR")"

printf 'trimmed %d entries (%d bytes), skipped %d refreshed, %d already gone, older than %s days from %s\n' \
	"$count" "$bytes" "$skipped" "$gone" "$TRIM_DAYS" "$CACHE_DIR"

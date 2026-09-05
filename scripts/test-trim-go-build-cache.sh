#!/usr/bin/env bash
#
# Self-test for scripts/trim-go-build-cache.sh.
#
# Hermetic: every case builds a synthetic Go build cache under a temp dir. The
# real cache at $HOME/Library/Caches/go-build is never read or written.
#
# The load-bearing case is CASE 1. The prune script this replaces
# (prune-tmp-gocache.sh) selected caches with the glob 'go-build-*', which also
# matches 'go-build-' -- the empty suffix -- i.e. the live shared cache root
# itself, and would have rm -rf'd the entire fleet cache. CASE 1 pins that the
# replacement can never select a cache root, and demonstrates the old glob
# still matching so the regression stays visible.

set -euo pipefail

# BSD `date -v-10d` is macOS-only; GNU date wants `-d '10 days ago'`. The trim
# script targets the macOS host, but these assertions -- glob safety, depth
# confinement, re-stat before unlink -- are worth running on the Linux CI
# runner too, so the harness resolves the spelling once instead of the suite
# being skipped there.
if date -v-1d '+%Y' >/dev/null 2>&1; then
	old_stamp() { date -v-"$1"d '+%Y%m%d%H%M'; }
elif date -d '1 day ago' '+%Y' >/dev/null 2>&1; then
	old_stamp() { date -d "$1 days ago" '+%Y%m%d%H%M'; }
else
	echo "FATAL: neither BSD nor GNU date accepted a relative offset" >&2
	exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
TRIM="$SCRIPT_DIR/trim-go-build-cache.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok - $*"; }

hex64() { printf '%s' "$1" | shasum -a 256 | cut -c1-64; }

# make_cache <dir> -- a cache with depth-1 metadata, two shards, and entries at
# several ages. Fresh entries are 0 days old; stale entries are 10 days old.
make_cache() {
	local root="$1" shard hash
	mkdir -p "$root"
	: > "$root/trim.txt"
	: > "$root/testexpire.txt"
	: > "$root/README"
	for shard in 00 ff; do
		mkdir -p "$root/$shard"
		for n in 1 2; do
			hash="$(hex64 "$n$shard")"
			printf 'fresh' > "$root/$shard/${hash}-a"
			printf 'fresh-output' > "$root/$shard/${hash}-d"
		done
		for n in 3 4; do
			hash="$(hex64 "$n$shard")"
			printf 'stale' > "$root/$shard/${hash}-a"
			printf 'stale-output' > "$root/$shard/${hash}-d"
			touch -t "$(old_stamp 10)" \
				"$root/$shard/${hash}-a" "$root/$shard/${hash}-d"
		done
	done
	# A stale executable-cache entry: "<hash>-d" is a DIRECTORY holding the
	# linked binary. Go removes these with os.RemoveAll; a trim using plain
	# "rm -f" would silently skip them.
	hash="$(hex64 9)"
	mkdir -p "$root/00/${hash}-d"
	printf 'ELF' > "$root/00/${hash}-d/gc"
	touch -t "$(old_stamp 10)" "$root/00/${hash}-d"
}

# ---------------------------------------------------------------- CASE 1
# The selector can never match a cache root, and the retired glob still can.
root="$WORK/case1/go-build"
make_cache "$root"

# The replacement selector, run against the PARENT of the cache, must not
# return the cache root no matter how the root is named.
hits="$(/usr/bin/find "$WORK/case1" -mindepth 2 -maxdepth 2 \
	\( -name '*-a' -o -name '*-d' \) ! -newermt '3 days ago' -print 2>/dev/null \
	| grep -c "^${root}\$" || true)"
[ "$hits" -eq 0 ] || fail "selector matched the cache root itself"

# The retired glob DOES match a cache root -- this is the regression being
# pinned. If this assertion ever stops holding, the comparison above has
# stopped being meaningful and this test needs rewriting, not deleting.
old_hits="$(/usr/bin/find "$WORK/case1" -maxdepth 1 -name 'go-build-*' -type d -print 2>/dev/null | wc -l)"
mkdir -p "$WORK/case1/go-build-"
old_hits="$(/usr/bin/find "$WORK/case1" -maxdepth 1 -name 'go-build-*' -type d -print 2>/dev/null | wc -l)"
[ "$old_hits" -ge 1 ] || fail "retired glob no longer matches an empty suffix; regression pin is stale"
rmdir "$WORK/case1/go-build-"
pass "CASE 1: selector never matches a cache root (retired glob still does)"

# ---------------------------------------------------------------- CASE 2
# Depth-1 metadata is never selected. trim.txt is Go's own trim bookkeeping.
meta="$(/usr/bin/find "$root" -mindepth 1 -maxdepth 1 \
	\( -name '*-a' -o -name '*-d' \) -print 2>/dev/null | wc -l)"
[ "$meta" -eq 0 ] || fail "selector matched depth-1 entries"

# A stale, entry-SHAPED file sitting at depth 1 rather than inside a shard.
# Go never creates one, but it pins the depth confinement independently of the
# name filter: only -mindepth 2 keeps this out of the candidate list, and if it
# ever reached the list the validator would abort the run.
depth1="$root/$(hex64 depth1)-d"
printf 'x' > "$depth1"
touch -t "$(old_stamp 10)" "$depth1"

TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root" TRIM_LOG="$WORK/log1" "$TRIM" >/dev/null
for f in trim.txt testexpire.txt README; do
	[ -f "$root/$f" ] || fail "$f was deleted"
done
[ -f "$depth1" ] || fail "depth-1 entry-shaped file was deleted"
rm -f "$depth1"
pass "CASE 2: depth-1 metadata and entry-shaped files survive a real run"

# ---------------------------------------------------------------- CASE 3
# Stale entries go, fresh entries stay, executable-cache dirs go.
for shard in 00 ff; do
	for n in 1 2; do
		h="$(hex64 "$n$shard")"
		[ -f "$root/$shard/${h}-a" ] || fail "fresh ${h}-a was deleted"
		[ -f "$root/$shard/${h}-d" ] || fail "fresh ${h}-d was deleted"
	done
	for n in 3 4; do
		h="$(hex64 "$n$shard")"
		[ ! -e "$root/$shard/${h}-a" ] || fail "stale ${h}-a survived"
		[ ! -e "$root/$shard/${h}-d" ] || fail "stale ${h}-d survived"
	done
done
[ ! -e "$root/00/$(hex64 9)-d" ] || fail "stale executable-cache directory survived"
pass "CASE 3: stale entries and executable-cache dirs removed, fresh entries kept"

# ---------------------------------------------------------------- CASE 4
# A find that ignores -newermt must fail loudly, not report a successful no-op.
# This is the bfs trap: bare 'find' on this host routes to bfs, which rejects
# -newermt, exits 0, and deletes nothing.
root4="$WORK/case4/go-build"
make_cache "$root4"
fake="$WORK/fakefind"
cat > "$fake" <<'EOF'
#!/usr/bin/env bash
# Mimics bfs: every other predicate works, but -newermt is rejected by exiting
# 0 with no output -- the exact silent no-op this job must refuse to run under.
for a in "$@"; do [ "$a" = "-newermt" ] && exit 0; done
exec /usr/bin/find "$@"
EOF
chmod +x "$fake"
set +e
out4="$(TRIM_FIND="$fake" TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root4" TRIM_LOG="$WORK/log4" \
	"$TRIM" 2>&1)"
rc4=$?
set -e
[ "$rc4" -ne 0 ] || fail "script accepted a find that does not honour -newermt"
# Assert it failed for THIS reason. Without this the shard-shape check would
# also exit non-zero and the case would pass while the preflight was gone.
echo "$out4" | grep -q 'newermt' \
	|| fail "script failed, but not on the -newermt preflight: $out4"
# And the stale entries must still be there: it refused rather than half-ran.
[ -f "$root4/00/$(hex64 300)-a" ] || fail "rejected run still deleted entries"
pass "CASE 4: a find lacking -newermt is rejected loudly, before any deletion"

# ---------------------------------------------------------------- CASE 5
# Fail closed: an entry-shaped name that is not a real cache entry aborts the
# whole run rather than being silently skipped or blindly deleted.
root5="$WORK/case5/go-build"
make_cache "$root5"
printf 'x' > "$root5/00/not-a-real-hash-a"
touch -t "$(old_stamp 10)" "$root5/00/not-a-real-hash-a"
if TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root5" TRIM_LOG="$WORK/log5" "$TRIM" >/dev/null 2>&1; then
	fail "script did not abort on an unexpected path"
fi
[ -f "$root5/00/not-a-real-hash-a" ] || fail "aborting run still deleted the odd path"
h3="$(hex64 300)"
[ -f "$root5/00/${h3}-a" ] || fail "aborting run deleted entries before validating"
pass "CASE 5: unexpected path aborts the run before any deletion"

# ---------------------------------------------------------------- CASE 6
# Refuse a directory that is not shaped like a Go build cache.
mkdir -p "$WORK/notacache/docs"
if TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$WORK/notacache" TRIM_LOG="$WORK/log6" \
	"$TRIM" >/dev/null 2>&1; then
	fail "script ran against a directory with no shard dirs"
fi
pass "CASE 6: non-cache directory is refused"

# ---------------------------------------------------------------- CASE 7
# The log stays bounded.
root7="$WORK/case7/go-build"
make_cache "$root7"
for _ in $(seq 1 12); do
	TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root7" TRIM_LOG="$WORK/log7" \
		TRIM_LOG_MAX_LINES=5 "$TRIM" >/dev/null
done
lines="$(wc -l < "$WORK/log7")"
[ "$lines" -le 5 ] || fail "log grew to $lines lines with TRIM_LOG_MAX_LINES=5"
pass "CASE 7: log is truncated to TRIM_LOG_MAX_LINES"

# ---------------------------------------------------------------- CASE 8
# --dry-run reports and deletes nothing.
root8="$WORK/case8/go-build"
make_cache "$root8"
before="$(/usr/bin/find "$root8" -mindepth 2 | wc -l)"
out="$(TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root8" TRIM_LOG="$WORK/log8" "$TRIM" --dry-run)"
after="$(/usr/bin/find "$root8" -mindepth 2 | wc -l)"
[ "$before" -eq "$after" ] || fail "--dry-run deleted entries"
echo "$out" | grep -q 'dry-run: would remove' || fail "--dry-run did not report a count"
[ ! -f "$WORK/log8" ] || fail "--dry-run wrote to the log"
pass "CASE 8: --dry-run reports without deleting"

# ---------------------------------------------------------------- CASE 9
# Static guarantees about the script itself.
grep -q '/usr/bin/find' "$TRIM" || fail "script does not pin /usr/bin/find"
if grep -nE '(^|[^-/[:alnum:]_])find[[:space:]]+["$/]' "$TRIM" \
	| grep -v 'FIND=' | grep -v '^[[:space:]]*#' | grep -q .; then
	fail "script invokes bare 'find' somewhere"
fi
# The banned string is assembled rather than written, so this file does not
# itself trip scripts/check-go-clean-cache.sh. An exemption marker would work
# too, but building the pattern keeps this file off the exemption list
# entirely -- the guard has no blind spot to maintain here.
ban="go clean"
ban="$ban -cache"
# Assert the ban appears only on COMMENT lines. The previous shape -- "fail if
# the ban appears and the warning does not" -- could not fail once the warning
# was present, which it always is: the script explains the ban. A check that
# cannot fail in the way its name claims is worth less than no check.
if grep -n "$ban" "$TRIM" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -q .; then
	fail "script references the banned command on a non-comment line"
fi
if grep -nE '^[[:space:]]*(export[[:space:]]+)?(GOCACHE|TMPDIR)=' "$TRIM" | grep -q .; then
	fail "script sets GOCACHE or TMPDIR"
fi
pass "CASE 9: pins /usr/bin/find, no bare find, no banned wipe, sets no GOCACHE/TMPDIR"


# --------------------------------------------------------------- CASE 10
# End-to-end: a real `go build` resolving cache entries that THIS SCRIPT has
# already selected as stale must still succeed.
#
# This models the 2026-09-05 incident directly. Every entry is backdated so the
# whole cache is a candidate; the trim is started with its snapshot-to-delete
# window forced open; a real build runs inside that window, resolving entries
# and bumping their mtimes via Go's markUsed(); the delete phase must then skip
# them. With the re-stat guard removed this fails with the incident's own
# signature ("could not import ...: no such file or directory").
#
# Note this drives the trim SCRIPT, not a hand-rolled rm -- an earlier version
# of this case piped find straight into `rm -rf`, which tested wholesale
# deletion (genuinely unsafe) rather than the script, and passed or failed on
# timing luck.
#
# Skipped when no go toolchain is on PATH.
if ! command -v go >/dev/null 2>&1; then
	echo "ok - CASE 10 skipped (no go toolchain on PATH)"
else
	cc_src="$WORK/case10/src"
	cc_cache="$WORK/case10/cache"
	mkdir -p "$cc_src" "$cc_cache"
	cat > "$cc_src/go.mod" <<'EOF'
module trimprobe

go 1.21
EOF
	cat > "$cc_src/main.go" <<'EOF'
package main

import (
	"fmt"
	"log"
	"slices"
)

func main() {
	s := []int{3, 1, 2}
	slices.Sort(s)
	if len(s) != 3 {
		log.Fatal("bad")
	}
	fmt.Println("ok", s)
}
EOF

	# Populate the cache, then make every entry look stale.
	( cd "$cc_src" && GOCACHE="$cc_cache" GOFLAGS= go build -o "$WORK/case10/warm" ./... ) \
		|| fail "CASE 10 warm-up build failed"
	/usr/bin/find "$cc_cache" -mindepth 2 -maxdepth 2 \
		\( -name '*-a' -o -name '*-d' \) \
		-exec touch -t "$(old_stamp 10)" {} + 2>/dev/null || true

	before10="$(/usr/bin/find "$cc_cache" -mindepth 2 -maxdepth 2 \
		\( -name '*-a' -o -name '*-d' \) | wc -l | tr -d ' ')"
	[ "$before10" -gt 20 ] || fail "CASE 10 fixture too small ($before10 entries)"

	# Trim takes its snapshot, then holds it open. The build runs inside that
	# window and resolves entries, which makes Go's markUsed() bump their
	# mtimes. The delete phase then fires against the now-stale snapshot.
	#
	# The assertion is the INVARIANT that makes the incident impossible, not
	# the race itself: every entry the build touched must survive. Asserting on
	# the build's exit code cannot work here -- a short build finishes before
	# the delete phase fires, so it would pass either way, which is exactly how
	# the previous version of this case passed while testing nothing. Without
	# the re-stat guard the snapshot is applied blindly and these entries are
	# deleted out from under any build still holding them.
	TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$cc_cache" TRIM_LOG="$WORK/log10" \
		TRIM_DELAY_BEFORE_DELETE=8 "$TRIM" >"$WORK/out10" 2>&1 &
	trimmer=$!
	sleep 1

	set +e
	build_out="$( cd "$cc_src" && GOCACHE="$cc_cache" GOFLAGS= \
		go build -o "$WORK/case10/bin" ./... 2>&1 )"
	build_rc=$?
	set -e
	[ "$build_rc" -eq 0 ] \
		|| fail "build inside the trim window FAILED (rc=$build_rc): $build_out"

	# Entries the build just resolved: mtime bumped to now, so newer than the
	# 10-day backdate. Snapshot this list BEFORE the delete phase runs.
	touched="$WORK/case10/touched"
	/usr/bin/find "$cc_cache" -mindepth 2 -maxdepth 2 \
		\( -name '*-a' -o -name '*-d' \) -newermt '1 hour ago' -print > "$touched"
	ntouched="$(wc -l < "$touched" | tr -d ' ')"
	[ "$ntouched" -gt 0 ] \
		|| fail "CASE 10: build resolved no entries; fixture is not exercising the window"

	wait "$trimmer" || fail "trim itself failed: $(cat "$WORK/out10")"

	missing=0
	while IFS= read -r e; do [ -e "$e" ] || missing=$((missing + 1)); done < "$touched"
	[ "$missing" -eq 0 ] \
		|| fail "trim deleted $missing of $ntouched entries a live build had just resolved -- this is the 2026-09-05 build breakage"

	got10="$("$WORK/case10/bin" 2>&1 || true)"
	[ "$got10" = "ok [1 2 3]" ] || fail "binary produced wrong output: $got10"

	# And the cache must still be usable afterwards.
	set +e
	( cd "$cc_src" && GOCACHE="$cc_cache" GOFLAGS= \
		go build -o "$WORK/case10/bin2" ./... ) >"$WORK/out10b" 2>&1
	rc10b=$?
	set -e
	[ "$rc10b" -eq 0 ] || fail "build after the trim failed: $(cat "$WORK/out10b")"
	pass "CASE 10: entries a live build resolved are not deleted by an in-flight trim"
fi

# --------------------------------------------------------------- CASE 11
# THE REGRESSION FOR THE 2026-09-05 BUILD BREAKAGE.
#
# An entry selected as a stale candidate, whose mtime is then refreshed before
# the delete phase runs, must SURVIVE. That refresh is what Go's markUsed()
# does the moment a build looks the entry up, and deleting it anyway is what
# made a concurrent linker fail with:
#
#     link: cannot open file .../02/02b1f0c8...-d: no such file or directory
#
# The window is forced open with TRIM_DELAY_BEFORE_DELETE so this is
# deterministic rather than a race the test might lose.
root11="$WORK/case11/go-build"
make_cache "$root11"

hot="$root11/00/$(hex64 300)-a"        # stale at selection time
cold="$root11/ff/$(hex64 3ff)-a"       # stale, never touched: control
[ -f "$hot" ] && [ -f "$cold" ] || fail "CASE 11 fixture missing"

TRIM_DAYS=3 GO_BUILD_CACHE_DIR="$root11" TRIM_LOG="$WORK/log11" \
	TRIM_DELAY_BEFORE_DELETE=3 "$TRIM" >"$WORK/out11" 2>&1 &
trim_pid=$!
sleep 1
touch "$hot"                           # a build resolves it: markUsed() bumps mtime
wait "$trim_pid" || fail "trim failed: $(cat "$WORK/out11")"

[ -f "$hot" ] \
	|| fail "entry refreshed between selection and delete was REMOVED -- this is the bug that broke a concurrent build"
[ ! -e "$cold" ] \
	|| fail "control entry that stayed stale was not removed"
grep -q 'skipped 1 refreshed' "$WORK/out11" \
	|| fail "trim did not report the skipped entry: $(cat "$WORK/out11")"
pass "CASE 11: an entry refreshed after selection survives (build-breakage regression)"

echo
echo "all trim-go-build-cache tests passed"

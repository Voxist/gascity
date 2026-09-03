#!/usr/bin/env bash
# check-resync-loss.sh — detect Category-A resync merge loss on a merge
# commit (bead ga-d32bn; mechanism recorded in the bd memory
# resync-loss-mechanism-took-theirs-on-shared-test-files).
#
# THE PROBLEM: the 2026-08-31 resync (merge 15913af6a) silently dropped 234
# fork-added declarations. Git did not lose them — `git merge-file` retains
# every one of them (see Gate 2 below). They were lost because shared
# `_test.go` files were resolved by taking upstream's blob wholesale: a rule
# AGENTS.md's "Resync conventions" rule 1 states for GENERATED artifacts
# only, mis-applied here to hand-authored test files. Nothing in
# build/vet/CI catches this class of loss: deleting a test never fails a
# build, so the suite passes because the tests that would have caught the
# regression are the ones that got deleted. This script is the gate that
# catches it, meant to run on the merge commit before it is pushed.
#
# USAGE:
#   check-resync-loss.sh                 # MERGE=HEAD, must be a merge commit
#   check-resync-loss.sh <merge-commit>   # MERGE=<merge-commit>
#   check-resync-loss.sh <base> <ours> <theirs> <merge>   # fully explicit
#
# Run from the repo root (or set --repo via the extractor; this script
# itself assumes cwd is a work tree of the target repo).
#
# GATE 1 — file-level: for every file the fork touched (ours != base),
# classify what the merge kept:
#   DROPPED-FILE  merge is missing the file entirely (fork added/kept it).
#   PURE-LOSS     merge == theirs AND theirs == base: upstream never had an
#                 opinion on this file, so the fork's changes were purely
#                 discarded.
#   TOOK-THEIRS   merge == theirs AND theirs != base: upstream changed the
#                 file too, and the merge is upstream's version verbatim (or
#                 a trivial, near-identical patch of it — see
#                 GATE1_NEAR_THEIRS_MAX_DELTA below), discarding the fork's
#                 side entirely.
# Every hit is reported and fails the gate UNLESS the path matches the
# EXEMPT_GLOBS list below, which is — and must stay — an exact mirror of
# the generated-artifact list in AGENTS.md's "Resync conventions" rule 1
# ("Generated artifacts are regenerated, never merged"). That rule licenses
# taking upstream wholesale ONLY for that list. A shared `_test.go` file (or
# any other hand-authored file) is never exempt: expanding this list without
# also updating AGENTS.md rule 1 defeats the point of the gate.
#
# GATE 2 — declaration-level: fork-added top-level Go declarations (present
# in OURS, absent from both BASE and THEIRS — upstream never had an opinion
# on them) that are missing from MERGE. Delegates the decl extraction and
# the `git merge-file -p` oracle to check-resync-loss-extract.py (kept in
# Python because grouping const/var blocks and caching per-file 3-way merges
# is unpleasant in POSIX shell). For each missing declaration:
#   RESOLUTION-BUG  a plain 3-way merge of the file would have kept it — the
#                   loss came from how the conflict was hand-resolved, not
#                   from an unavoidable conflict. Always fails the gate.
#   MERGE-OUTCOME   even a plain 3-way merge would not have kept it (real
#                   rewrite on both sides). Reported for human triage but
#                   does not fail the gate on its own.
#
# ADD-ONS (informational only, never fail the gate):
#   - dangling references: `git grep -w` each Gate-2-missing symbol against
#     MERGE. Since the tree presumably builds, any hit is a comment or
#     string naming a symbol that no longer exists — a reliable loss marker
#     even when Gate 2 called the removal a MERGE-OUTCOME.
#   - dead config knobs: exported accessor methods defined in
#     internal/config/*.go with zero call sites anywhere else in MERGE. This
#     is exactly the class of defect the 2026-08-31 loss produced
#     ([daemon].on_boot_stagger survived as a published, documented,
#     completely unread config knob after its readers were dropped).
#
# EXIT STATUS: non-zero if any unexempted Gate-1 hit or any Gate-2
# RESOLUTION-BUG is found. Zero otherwise (MERGE-OUTCOME and the add-ons do
# not affect exit status — they need a human, not a mechanical fix).
set -uo pipefail # intentionally NOT -e: run every check and aggregate.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXTRACTOR="$SCRIPT_DIR/check-resync-loss-extract.py"

note() { echo "check-resync-loss: $*" >&2; }

usage() {
	cat >&2 <<'EOF'
usage: check-resync-loss.sh [MERGE]
       check-resync-loss.sh BASE OURS THEIRS MERGE
EOF
}

# ---------------------------------------------------------------------------
# Resolve BASE / OURS / THEIRS / MERGE.
# ---------------------------------------------------------------------------
BASE=""
OURS=""
THEIRS=""
MERGE=""
case "$#" in
0) MERGE="HEAD" ;;
1) MERGE="$1" ;;
4)
	BASE="$1"
	OURS="$2"
	THEIRS="$3"
	MERGE="$4"
	;;
*)
	usage
	exit 2
	;;
esac

if ! MERGE=$(git rev-parse --verify "${MERGE}^{commit}" 2>/dev/null); then
	note "BLOCKED — '$MERGE' does not resolve to a commit"
	exit 2
fi

if [ -z "$OURS" ]; then
	OURS=$(git rev-parse --verify "${MERGE}^1" 2>/dev/null) || {
		note "BLOCKED — $MERGE has no first parent (not a merge commit?)"
		exit 2
	}
fi
if [ -z "$THEIRS" ]; then
	THEIRS=$(git rev-parse --verify "${MERGE}^2" 2>/dev/null) || {
		note "BLOCKED — $MERGE has no second parent; pass BASE OURS THEIRS MERGE explicitly for a non-merge target"
		exit 2
	}
fi
OURS=$(git rev-parse --verify "${OURS}^{commit}") || {
	note "BLOCKED — OURS does not resolve to a commit"
	exit 2
}
THEIRS=$(git rev-parse --verify "${THEIRS}^{commit}") || {
	note "BLOCKED — THEIRS does not resolve to a commit"
	exit 2
}
if [ -z "$BASE" ]; then
	BASE=$(git merge-base "$OURS" "$THEIRS") || {
		note "BLOCKED — no merge base between OURS and THEIRS"
		exit 2
	}
fi
BASE=$(git rev-parse --verify "${BASE}^{commit}") || {
	note "BLOCKED — BASE does not resolve to a commit"
	exit 2
}

note "BASE=$BASE OURS=$OURS THEIRS=$THEIRS MERGE=$MERGE"

failed=0

# ---------------------------------------------------------------------------
# GATE 1 exemptions — EXACT mirror of AGENTS.md "Resync conventions" rule 1's
# generated-artifact list. Do not add a path here without adding it there.
# ---------------------------------------------------------------------------
EXEMPT_GLOBS=(
	'internal/api/openapi.json'
	'docs/reference/schema/openapi.*'
	'docs/reference/schema/city-schema.*'
	'docs/reference/cli.md'
	'docs/reference/config.md'
	'cmd/gc/productmetrics_command_census.json'
	'cmd/gc/metrics_census_gen.go'
	'internal/api/dashboardspa/*'
	'internal/testpolicy/resourcecensus/census.go'
	'internal/testenv/testdata/*.golden'
	'scripts/*baseline*'
	'scripts/*manifest*'
)

is_exempt() {
	local f="$1" pat
	for pat in "${EXEMPT_GLOBS[@]}"; do
		# shellcheck disable=SC2254 # deliberate glob match, not literal
		case "$f" in
		$pat) return 0 ;;
		esac
	done
	return 1
}

# A merge-vs-theirs diff at or under this many changed lines (insertions +
# deletions from `git diff --numstat`) is treated as "took theirs, then
# reapplied an isolated unrelated fix" rather than a genuine hand-merge —
# the 2026-08-31 merge's internal/config/config_test.go is exactly this
# shape (9 lines of a --sort flag rename touched out of 8392, everywhere
# else byte-identical to upstream). This heuristic is gated on Gate 2
# corroboration below (a real missing fork-added declaration attributed to
# the same file) so a file that legitimately ends up close to upstream's
# text — because upstream's own rewrite subsumed most of the fork's intent
# — is not misclassified just for being textually close to theirs.
GATE1_NEAR_THEIRS_MAX_DELTA=20

# ---------------------------------------------------------------------------
# GATE 2 — fork-added declarations missing from the merge. Computed BEFORE
# Gate 1's report is printed: Gate 1's near-theirs heuristic (see above)
# consults gate2_bug_files, the set of files Gate 2 proved actually lost a
# declaration a plain 3-way merge would have kept.
# ---------------------------------------------------------------------------
gate2_summary=$(mktemp "${TMPDIR:-/tmp}/crl-gate2-summary.XXXXXX") || exit 1
gate2_report=$(mktemp "${TMPDIR:-/tmp}/crl-gate2-report.XXXXXX") || exit 1
gate2_bug_files=$(mktemp "${TMPDIR:-/tmp}/crl-gate2-bugfiles.XXXXXX") || exit 1
trap 'rm -f "$gate2_summary" "$gate2_report" "$gate2_bug_files"' EXIT

if ! python3 "$EXTRACTOR" "$BASE" "$OURS" "$THEIRS" "$MERGE" --summary-out "$gate2_summary" >"$gate2_report" 2>&2; then
	note "BLOCKED — check-resync-loss-extract.py failed (fail-closed)"
	failed=1
	resolution_bugs=1
	merge_outcomes=0
	missing=0
else
	resolution_bugs=0
	merge_outcomes=0
	missing=0
	while IFS='=' read -r key val; do
		case "$key" in
		RESOLUTION_BUGS) resolution_bugs="$val" ;;
		MERGE_OUTCOMES) merge_outcomes="$val" ;;
		MISSING) missing="$val" ;;
		esac
	done <"$gate2_summary"
fi
awk '$1 == "RESOLUTION-BUG" { print $NF }' "$gate2_report" | tr -d '()' | sort -u >"$gate2_bug_files"

near_theirs_corroborated() {
	# True iff Gate 2 already proved a lost declaration in $1.
	grep -qxF "$1" "$gate2_bug_files"
}

echo
echo "=== GATE 1: fork-changed files whose merge result matches upstream's blob ==="
gate1_hits=0
gate1_exempt=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	oh=$(git rev-parse "${OURS}:${f}" 2>/dev/null || true)
	[ -n "$oh" ] || continue # fork deleted the file; not this gate's concern

	mh=$(git rev-parse "${MERGE}:${f}" 2>/dev/null || true)
	th=$(git rev-parse "${THEIRS}:${f}" 2>/dev/null || true)
	bh=$(git rev-parse "${BASE}:${f}" 2>/dev/null || true)

	verdict=""
	detail=""
	if [ -z "$mh" ]; then
		verdict="DROPPED-FILE"
	elif [ -n "$th" ] && [ "$mh" = "$th" ]; then
		if [ "$th" = "$bh" ]; then
			verdict="PURE-LOSS"
			detail="upstream never touched it"
		else
			verdict="TOOK-THEIRS"
		fi
	elif [ -n "$th" ] && [ "$mh" != "$th" ] && near_theirs_corroborated "$f"; then
		dt=$(git diff --numstat "$THEIRS" "$MERGE" -- "$f" 2>/dev/null | awk '{s+=$1+$2} END{print s+0}')
		do_=$(git diff --numstat "$OURS" "$MERGE" -- "$f" 2>/dev/null | awk '{s+=$1+$2} END{print s+0}')
		if [ "$dt" -gt 0 ] && [ "$dt" -le "$GATE1_NEAR_THEIRS_MAX_DELTA" ] && [ "$dt" -lt "$do_" ]; then
			verdict="TOOK-THEIRS"
			detail="near-identical to theirs (Δ${dt} lines, vs Δ${do_} from ours; corroborated by Gate 2)"
		fi
	fi

	[ -n "$verdict" ] || continue

	if is_exempt "$f"; then
		gate1_exempt=$((gate1_exempt + 1))
		note "  EXEMPT (generated, AGENTS.md rule 1) $verdict $f"
		continue
	fi

	gate1_hits=$((gate1_hits + 1))
	failed=1
	if [ -n "$detail" ]; then
		echo "  $verdict	$f	($detail)"
	else
		echo "  $verdict	$f"
	fi
done < <(git diff --name-only "$BASE" "$OURS")
echo "Gate 1: $gate1_hits unexempted hit(s), $gate1_exempt exempted (generated-artifact) hit(s)."

echo
echo "=== GATE 2: fork-added top-level decls absent from the merge result ==="
cat "$gate2_report"
echo "Gate 2: $missing missing decl(s) — $resolution_bugs RESOLUTION-BUG, $merge_outcomes MERGE-OUTCOME."
if [ "$resolution_bugs" -gt 0 ]; then
	failed=1
fi

# ---------------------------------------------------------------------------
# ADD-ON: dangling references to a Gate-2-missing symbol.
# ---------------------------------------------------------------------------
echo
echo "=== ADD-ON: dangling references to a missing symbol (informational) ==="
dangling=0
if [ -s "$gate2_report" ]; then
	while read -r _verdict name _rest; do
		[ -n "$name" ] || continue
		hits=$(git grep -wI -n "$name" "$MERGE" -- '*.go' 2>/dev/null | grep -v "^${MERGE}:.*:.*\b${name}\b(" || true)
		if [ -n "$hits" ]; then
			dangling=$((dangling + 1))
			echo "  DANGLING-REF	$name"
			while IFS= read -r hit; do
				echo "    $hit"
			done <<<"$hits"
		fi
	done < <(awk '{print $1, $2}' "$gate2_report" | sort -u)
fi
echo "Add-on: $dangling dangling reference(s) found (does not affect exit status)."

# ---------------------------------------------------------------------------
# ADD-ON: dead config knobs — exported internal/config accessor with zero
# non-definition readers in MERGE.
# ---------------------------------------------------------------------------
echo
echo "=== ADD-ON: dead config knobs (informational) ==="
dead_knobs=0
config_files=$(git ls-tree -r --name-only "$MERGE" -- internal/config | grep -v '_test\.go$' || true)
if [ -n "$config_files" ]; then
	accessors=$(git show "$MERGE:$(echo "$config_files" | head -1)" >/dev/null 2>&1 && \
		for f in $config_files; do git show "$MERGE:$f" 2>/dev/null; done | \
		grep -oE '^func \([^)]*\) [A-Z][A-Za-z0-9_]*\(' | \
		sed -E 's/^func \([^)]*\) ([A-Za-z0-9_]+)\(.*/\1/' | sort -u)
	for acc in $accessors; do
		# git grep -c on an explicit rev prints "rev:path:count" — the count is
		# always the LAST field ($NF), never $2 (a path can itself contain a
		# colon-free component but the rev prefix always adds one extra field).
		# No -w here: the pattern is already anchored by a leading literal "."
		# and a trailing "(", and -w additionally requires the match's own
		# first/last characters to be word constituents, which a leading "."
		# can never satisfy — it silently zeroes every match.
		#
		# Call-site count only — no subtraction of the declaration count. The
		# call-site pattern ("\.<name>(") and the declaration pattern
		# ("func (recv) <name>(") match disjoint line shapes (a method
		# declaration line is never preceded by a literal dot), so the
		# call-site count already excludes the definition by construction;
		# subtracting the declaration count double-counted definitions living
		# in a DIFFERENT file than their first caller and zeroed out real
		# external readers (e.g. Agent.AttachEnabled(), defined in config.go,
		# called from internal/config/session_sleep.go).
		readers=$(git grep -c "\.${acc}(" "$MERGE" -- '*.go' 2>/dev/null | awk -F: '{s+=$NF} END{print s+0}')
		if [ "$readers" -le 0 ]; then
			dead_knobs=$((dead_knobs + 1))
			echo "  DEAD-KNOB	$acc	(0 non-definition readers)"
		fi
	done
fi
echo "Add-on: $dead_knobs dead config knob(s) found (does not affect exit status)."

# ---------------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------------
echo
echo "=== SUMMARY ==="
printf '%-14s %s\n' "Gate 1:" "$gate1_hits unexempted hit(s) ($gate1_exempt exempted)"
printf '%-14s %s\n' "Gate 2:" "$resolution_bugs RESOLUTION-BUG, $merge_outcomes MERGE-OUTCOME (of $missing missing)"
printf '%-14s %s\n' "Dangling refs:" "$dangling"
printf '%-14s %s\n' "Dead knobs:" "$dead_knobs"

if [ "$failed" -ne 0 ]; then
	note "RESYNC LOSS DETECTED — see Gate 1 / Gate 2 hits above. Do not push until triaged (bead ga-d32bn)."
	exit 1
fi
note "OK — no unexempted Gate 1 hit and no Gate 2 RESOLUTION-BUG."
exit 0

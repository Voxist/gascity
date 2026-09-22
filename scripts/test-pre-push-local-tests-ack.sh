#!/usr/bin/env bash
#
# test-pre-push-local-tests-ack.sh — self-test for the LOCAL_TESTS_ACK escape
# on gate 3 of .githooks/pre-push (bead vc-dq0b).
#
# The pre-push hook runs three gates in order:
#   1. check-resync-loss  (ga-d32bn)     — ack: RESYNC_LOSS_ACK=1
#   2. bead-ownership     (ga-fip9ps.1)  — fail-closed, no scoped ack
#   3. make test-fast-parallel           — ack: LOCAL_TESTS_ACK=<pushed sha>  <- under test
#
# Before vc-dq0b, gate 3's only bypass was `git push --no-verify`, which
# disarms all three. The property these cases pin is therefore NOT "the ack
# works" (that is cheap and would be green even if the ack disarmed
# everything) but "the ack is SCOPED": it skips gate 3 and leaves gates 1
# and 2 armed. Cases C and D are the load-bearing ones — they must fail if
# the ack is ever moved earlier in the hook or widened into a --no-verify
# equivalent. Cases E-H pin that the ack is BOUND to the pushed commit: any
# other value (0, 1, a stale SHA, a too-short prefix) must still run gate 3,
# and so must an ack for one of several different commits in one push (I).
#
# Hermetic: temp git repos, a stub bd, and a Makefile whose tier target is a
# simulated failure. Never runs the real Go suite (that is the tier this bead
# exists because nobody can run on a loaded host) and asserts no wall-clock
# budget of any kind.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

pass=0
fail=0
record_pass() { echo "  PASS: $1"; pass=$((pass + 1)); }
record_fail() {
	echo "  FAIL: $1${2:+ — $2}"
	fail=$((fail + 1))
}

# Gate 2 reads this session's identity and bd state. Inheriting the ambient
# agent session's values would make these results depend on the machine's
# live bead store — the exact host-coupling this bead is about.
unset GC_AGENT GC_SESSION_NAME GC_SESSION_ID GC_ALIAS GC_TEMPLATE
export POG_READ_ATTEMPTS=1

tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/pplta.XXXXXX")"
trap 'rm -rf "$tmp_root"' EXIT

# setup_repo: a bare remote plus a work clone carrying the real pre-push hook,
# the real ownership guard, a passing gate-1 stub, and a Makefile whose
# test-fast-parallel target FAILS — the state this bead is about. core.hooksPath
# is enabled only AFTER the base push, so the fixture's own setup is not gated.
# Echoes "<remote-dir> <work-dir>".
setup_repo() {
	local remote work
	remote="$(mktemp -d "$tmp_root/remote.XXXXXX")"
	git init -q --bare "$remote"
	git -C "$remote" symbolic-ref HEAD refs/heads/main
	work="$(mktemp -d "$tmp_root/work.XXXXXX")"
	git clone -q "$remote" "$work" 2>/dev/null
	(
		set -e
		cd "$work"
		git config user.email test@example.invalid
		git config user.name "pre-push ack test"
		mkdir -p scripts .githooks/lib
		cp "$REPO_ROOT/.githooks/pre-push" .githooks/pre-push
		chmod +x .githooks/pre-push
		# The hook's first action is chaining to beads through this script.
		# Without it every push dies on "No such file or directory" before any
		# gate runs, which case A would misread as gate 3 blocking. The real
		# script exits 0 without bd and swallows `bd hooks run`'s exit 3 for a
		# repo with no beads database, so it is safe to install as-is.
		cp "$REPO_ROOT/.githooks/lib/beads-chain.sh" .githooks/lib/beads-chain.sh
		chmod +x .githooks/lib/beads-chain.sh
		cp "$REPO_ROOT/scripts/push-ownership-guard.sh" scripts/push-ownership-guard.sh
		printf '#!/usr/bin/env bash\nexit 0\n' >scripts/check-resync-loss.sh
		chmod +x scripts/check-resync-loss.sh
		# Tab-indented recipes: this is a real Makefile.
		# $(MERGE) is a Makefile variable, not a shell expansion — single quotes
		# are deliberate here.
		# shellcheck disable=SC2016
		printf 'check-resync-loss:\n\t./scripts/check-resync-loss.sh $(MERGE)\n\ntest-fast-parallel:\n\t@echo "SIMULATED gate-3 tier failure (test fixture)" >&2; exit 1\n' >Makefile
		echo base >f.txt
		git add -A
		git commit -qm base
		git branch -M main
		git push -q origin main
		git config core.hooksPath .githooks
	) >/dev/null 2>&1
	printf '%s %s' "$remote" "$work"
}

remote_sha() { git -C "$1" rev-parse -q --verify "$2" 2>/dev/null || true; }

echo "== pre-push gate 3 / LOCAL_TESTS_ACK (vc-dq0b) =="

# ---------------------------------------------------------------------------
# A — BASELINE / FALSIFIABILITY. Tier fails, no ack: the push must be blocked.
# Without this case the whole file could pass while gate 3 never ran at all.
# ---------------------------------------------------------------------------
echo "-- gate 3 failure blocks the push when the ack is absent --"
read -r remA workA <<<"$(setup_repo)"
(
	set -e
	cd "$workA"
	git checkout -q -b ours
	echo "package main" >a.go
	git add -A
	git commit -qm "go change"
) >/dev/null 2>&1
outA="$(mktemp "$tmp_root/outA.XXXXXX")"
(cd "$workA" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$outA" 2>&1
rcA=$?
# The simulated-tier line must be present: a push rejected for any other
# reason (a hook dying before gate 3) proves nothing about gate 3.
if [ "$rcA" -ne 0 ] && [ -z "$(remote_sha "$remA" refs/heads/ours)" ] &&
	grep -q 'SIMULATED gate-3 tier failure' "$outA"; then
	record_pass "no-ack/tier-failure-blocks-push (rejected by gate 3, remote untouched)"
else
	record_fail "no-ack/tier-failure-blocks-push" "rc=$rcA remote_sha=$(remote_sha "$remA" refs/heads/ours)"
	sed 's/^/    /' "$outA"
fi

# ---------------------------------------------------------------------------
# B — THE ACK WORKS, AND SAYS SO. AC3 (push proceeds) + AC4 (named, greppable
# line on stderr). The audit line is asserted by content, not merely by rc:
# a silent skip is the failure mode today's --no-verify bypass already has.
# ---------------------------------------------------------------------------
echo "-- LOCAL_TESTS_ACK=<pushed sha> skips gate 3 and emits a named line --"
read -r remB workB <<<"$(setup_repo)"
(
	set -e
	cd "$workB"
	git checkout -q -b ours
	echo "package main" >a.go
	git add -A
	git commit -qm "go change"
) >/dev/null 2>&1
outB="$(mktemp "$tmp_root/outB.XXXXXX")"
shaB="$(git -C "$workB" rev-parse HEAD)"
(cd "$workB" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$shaB" git push origin ours) >"$outB" 2>&1
rcB=$?
if [ "$rcB" -eq 0 ] && [ -n "$(remote_sha "$remB" refs/heads/ours)" ]; then
	# The notice must name the acked commit and the ref it covers.
	if grep -q "LOCAL_TESTS_ACK=$shaB — gate 3 .* SKIPPED .*commit $shaB (refs: refs/heads/ours)" "$outB"; then
		record_pass "ack/skips-gate-3-and-emits-named-line"
	else
		record_fail "ack/skips-gate-3-and-emits-named-line" "push succeeded but no greppable notice"
		sed 's/^/    /' "$outB"
	fi
else
	record_fail "ack/skips-gate-3-and-emits-named-line" "rc=$rcB"
	sed 's/^/    /' "$outB"
fi

# B2 — a 7-character prefix of the pushed commit is accepted too.
echo "-- a 7-character prefix of the pushed commit is accepted --"
read -r remB2 workB2 <<<"$(setup_repo)"
(
	set -e
	cd "$workB2"
	git checkout -q -b ours
	echo "package main" >a.go
	git add -A
	git commit -qm "go change"
) >/dev/null 2>&1
outB2="$(mktemp "$tmp_root/outB2.XXXXXX")"
(cd "$workB2" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$(git rev-parse --short=7 HEAD)" git push origin ours) >"$outB2" 2>&1
rcB2=$?
if [ "$rcB2" -eq 0 ] && [ -n "$(remote_sha "$remB2" refs/heads/ours)" ] && grep -q 'SKIPPED' "$outB2"; then
	record_pass "ack/seven-char-prefix-skips-gate-3"
else
	record_fail "ack/seven-char-prefix-skips-gate-3" "rc=$rcB2"
	sed 's/^/    /' "$outB2"
fi

# ---------------------------------------------------------------------------
# C — LOAD-BEARING. The ack must NOT disarm gate 2. A bead that reads back as
# claimed by a different session is exactly the ownership violation gate 2
# exists to catch (the PR #4243 shape). With LOCAL_TESTS_ACK=1 set and the
# tier still failing, the push must STILL be blocked — by gate 2, not gate 3.
# This is the case that fails if the ack is ever hoisted above gate 2.
# ---------------------------------------------------------------------------
echo "-- a valid LOCAL_TESTS_ACK leaves the bead-ownership guard armed --"
read -r remC workC <<<"$(setup_repo)"
binC="$(mktemp -d "$tmp_root/binC.XXXXXX")"
cat >"$binC/bd" <<'BDSTUB'
#!/usr/bin/env bash
# Stub bd: the branch-derived bead reads back in_progress but assigned to a
# DIFFERENT session than the one pushing.
case "${1:-}" in
show) printf '[{"id":"ga-zzzzzz","status":"in_progress","assignee":"another-session","labels":[],"metadata":{}}]\n' ;;
*) printf '[]\n' ;;
esac
BDSTUB
chmod +x "$binC/bd"
(
	set -e
	cd "$workC"
	git checkout -q -b builder/ga-zzzzzz-ack-test
	echo "package main" >a.go
	git add -A
	git commit -qm "go change"
) >/dev/null 2>&1
outC="$(mktemp "$tmp_root/outC.XXXXXX")"
(cd "$workC" && GIT_TERMINAL_PROMPT=0 PATH="$binC:$PATH" GC_SESSION_NAME="pushing-session" \
	LOCAL_TESTS_ACK="$(git rev-parse HEAD)" git push origin builder/ga-zzzzzz-ack-test) >"$outC" 2>&1
rcC=$?
if [ "$rcC" -ne 0 ] && [ -z "$(remote_sha "$remC" refs/heads/builder/ga-zzzzzz-ack-test)" ] &&
	grep -q 'push-ownership-guard: BLOCKED' "$outC"; then
	record_pass "ack/does-not-disarm-ownership-guard (blocked by gate 2)"
else
	record_fail "ack/does-not-disarm-ownership-guard" "rc=$rcC"
	sed 's/^/    /' "$outC"
fi

# ---------------------------------------------------------------------------
# D — LOAD-BEARING. The ack must NOT disarm gate 1 either. A merge commit as
# the pushed tip with a failing check-resync-loss must still block under the
# ack. Fails if the ack is hoisted above the resync-loss gate.
# ---------------------------------------------------------------------------
echo "-- a valid LOCAL_TESTS_ACK leaves the resync-loss gate armed --"
read -r remD workD <<<"$(setup_repo)"
(
	set -e
	cd "$workD"
	printf '#!/usr/bin/env bash\necho "SIMULATED Category-A loss (test fixture)" >&2\nexit 1\n' \
		>scripts/check-resync-loss.sh
	chmod +x scripts/check-resync-loss.sh
	git add -A
	git commit -qm "poison the resync-loss gate"
	git checkout -q -b theirs main
	echo theirs >>f.txt
	git add -A
	git commit -qm "upstream: touch f.txt"
	git checkout -q -b ours main
	echo ours >>g.txt
	git add -A
	git commit -qm "fork: add g.txt"
	git merge -q --no-edit theirs >/dev/null
) >/dev/null 2>&1
outD="$(mktemp "$tmp_root/outD.XXXXXX")"
(cd "$workD" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$(git rev-parse HEAD)" git push origin ours) >"$outD" 2>&1
rcD=$?
if [ "$rcD" -ne 0 ] && [ -z "$(remote_sha "$remD" refs/heads/ours)" ] &&
	grep -q 'check-resync-loss failed' "$outD"; then
	record_pass "ack/does-not-disarm-resync-loss-gate (blocked by gate 1)"
else
	record_fail "ack/does-not-disarm-resync-loss-gate" "rc=$rcD"
	sed 's/^/    /' "$outD"
fi

# ---------------------------------------------------------------------------
# E-H — THE ACK IS BOUND TO THE PUSH. Any value that is not the pushed commit
# must leave gate 3 armed: the push is still blocked by the failing tier, and
# the hook says why it ignored the ack. These fail if the check is widened
# back to "any non-empty value" or loosened to accept a non-matching SHA.
# ---------------------------------------------------------------------------
# assert_ack_ignored <case-name> <ack-value|STALE|SHORT>: a Go change pushed
# with that ack must be rejected by gate 3 with an IGNORED notice.
assert_ack_ignored() {
	local name="$1" value="$2" rem work out rc
	read -r rem work <<<"$(setup_repo)"
	(
		set -e
		cd "$work"
		git checkout -q -b ours
		echo "package main" >a.go
		git add -A
		git commit -qm "go change"
	) >/dev/null 2>&1
	case "$value" in
	STALE) value="$(git -C "$work" rev-parse HEAD~1)" ;;
	SHORT) value="$(git -C "$work" rev-parse --short=6 HEAD)" ;;
	esac
	out="$(mktemp "$tmp_root/out.XXXXXX")"
	(cd "$work" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$value" git push origin ours) >"$out" 2>&1
	rc=$?
	if [ "$rc" -ne 0 ] && [ -z "$(remote_sha "$rem" refs/heads/ours)" ] &&
		grep -q "LOCAL_TESTS_ACK=$value IGNORED" "$out" &&
		grep -q 'SIMULATED gate-3 tier failure' "$out"; then
		record_pass "$name (ignored, gate 3 ran and blocked)"
	else
		record_fail "$name" "rc=$rc"
		sed 's/^/    /' "$out"
	fi
}

echo "-- LOCAL_TESTS_ACK values other than the pushed commit still run gate 3 --"
assert_ack_ignored "ack/value-1-runs-gate-3" 1
assert_ack_ignored "ack/value-0-runs-gate-3" 0
assert_ack_ignored "ack/stale-sha-runs-gate-3" STALE
assert_ack_ignored "ack/six-char-prefix-runs-gate-3" SHORT

# ---------------------------------------------------------------------------
# I-K — MULTI-REF PUSHES AND CASE. The SHA match is a prefix strip over the
# pushed commits, so without the one-commit rule an ack for one commit would
# also skip gate 3 for every other commit in the same push.
# ---------------------------------------------------------------------------
# setup_two_branches <same|different>: work clone with branches ours and other
# each carrying a Go change — on the same commit, or on two different commits.
# Echoes "<remote> <work> <sha-of-ours> <sha-of-other>".
setup_two_branches() {
	local mode="$1" rem work
	read -r rem work <<<"$(setup_repo)"
	(
		set -e
		cd "$work"
		git checkout -q -b ours
		echo "package main" >a.go
		git add -A
		git commit -qm "go change"
		if [ "$mode" = same ]; then
			git branch other
		else
			git checkout -q -b other main
			echo "package other" >b.go
			git add -A
			git commit -qm "other go change"
		fi
	) >/dev/null 2>&1
	printf '%s %s %s %s' "$rem" "$work" "$(git -C "$work" rev-parse ours)" "$(git -C "$work" rev-parse other)"
}

echo "-- an ack for one of two different pushed commits is ignored --"
read -r remI workI shaI1 shaI2 <<<"$(setup_two_branches different)"
# Ack the commit that sorts FIRST: the prefix strip over the sorted list would
# match it, so only the one-commit rule can refuse this ack.
ackI="$(printf '%s\n' "$shaI1" "$shaI2" | sort | head -n 1)"
outI="$(mktemp "$tmp_root/outI.XXXXXX")"
(cd "$workI" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$ackI" git push origin ours other) >"$outI" 2>&1
rcI=$?
if [ "$rcI" -ne 0 ] && [ -z "$(remote_sha "$remI" refs/heads/ours)" ] &&
	[ -z "$(remote_sha "$remI" refs/heads/other)" ] &&
	grep -q "LOCAL_TESTS_ACK=$ackI IGNORED — this push carries more than one commit" "$outI" &&
	grep -q 'SIMULATED gate-3 tier failure' "$outI"; then
	record_pass "ack/two-commits-one-acked-runs-gate-3 (ignored, gate 3 ran and blocked)"
else
	record_fail "ack/two-commits-one-acked-runs-gate-3" "rc=$rcI"
	sed 's/^/    /' "$outI"
fi

echo "-- an ack covers two refs carrying the same commit --"
read -r remJ workJ shaJ _ <<<"$(setup_two_branches same)"
outJ="$(mktemp "$tmp_root/outJ.XXXXXX")"
(cd "$workJ" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$shaJ" git push origin ours other) >"$outJ" 2>&1
rcJ=$?
if [ "$rcJ" -eq 0 ] && [ "$(remote_sha "$remJ" refs/heads/ours)" = "$shaJ" ] &&
	[ "$(remote_sha "$remJ" refs/heads/other)" = "$shaJ" ] &&
	grep -q "SKIPPED .*commit $shaJ (refs: refs/heads/ours refs/heads/other)" "$outJ"; then
	record_pass "ack/two-refs-same-commit-skips-gate-3"
else
	record_fail "ack/two-refs-same-commit-skips-gate-3" "rc=$rcJ"
	sed 's/^/    /' "$outJ"
fi

echo "-- an uppercase SHA ack is accepted --"
read -r remK workK shaK _ <<<"$(setup_two_branches same)"
ackK="$(printf '%s' "$shaK" | tr 'a-f' 'A-F')"
outK="$(mktemp "$tmp_root/outK.XXXXXX")"
(cd "$workK" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK="$ackK" git push origin ours) >"$outK" 2>&1
rcK=$?
if [ "$rcK" -eq 0 ] && [ "$(remote_sha "$remK" refs/heads/ours)" = "$shaK" ] &&
	grep -q "SKIPPED .*commit $shaK" "$outK"; then
	record_pass "ack/uppercase-sha-skips-gate-3"
else
	record_fail "ack/uppercase-sha-skips-gate-3" "rc=$rcK"
	sed 's/^/    /' "$outK"
fi

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]

#!/usr/bin/env bash
#
# test-pre-push-local-tests-ack.sh — self-test for gate 3 of .githooks/pre-push
# (bead vc-dq0b introduced the LOCAL_TESTS_ACK; bead vp-iauu made the gate
# ADVISORY).
#
# The pre-push hook runs three gates in order:
#   1. check-resync-loss  (ga-d32bn)     — ack: RESYNC_LOSS_ACK=1   — fail-closed
#   2. bead-ownership     (ga-fip9ps.1)  — fail-closed, no scoped ack
#   3. make test-fast-parallel           — ADVISORY (vp-iauu); LOCAL_TESTS_ACK=<sha>
#                                          remains the deliberate skip
#
# vp-iauu: a fail-closed local tier converts every host-dependent failure —
# the darwin PATH_MAX/NOFILE red-at-base is the proven case — into permanent
# work loss, because the seat that hits the rejection is ephemeral, cannot
# attribute a failure in code it never touched, and dies with its worktree
# unpushed. CI is the authoritative gate, so gate 3 now REPORTS (named,
# greppable line on stderr, red or green) and the push always proceeds.
# The property this file pins is therefore threefold: (a) a red tier is
# AUDITABLE — the push proceeds but the output names the failure, so a red
# tier can never pass silently (that is what today's --no-verify bypass
# already allowed); (b) LOCAL_TESTS_ACK remains SCOPED — it skips gate 3 and
# leaves gates 1 and 2 armed, and cases C and D are the load-bearing ones:
# they must fail if the ack is ever moved earlier in the hook or widened into
# a --no-verify equivalent; (c) ack values that do not bind to the pushed
# commit are IGNORED — the tier still runs, with the red visible in the
# output, and the push still proceeds (there is nothing left for a forged
# ack to unlock).
#
# Hermetic: temp git repos, a stub bd, and a Makefile whose tier target is a
# simulated failure (or a simulated pass, case L). Never runs the real Go
# suite (that is the tier this bead exists because nobody can run on a loaded
# host) and asserts no wall-clock budget of any kind.
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

# setup_repo [passing]: a bare remote plus a work clone carrying the real
# pre-push hook, the real ownership guard, a passing gate-1 stub, and a
# Makefile whose test-fast-parallel target FAILS (default — the state
# vp-iauu is about) or PASSES (arg "passing" — the green advisory path).
# core.hooksPath is enabled only AFTER the base push, so the fixture's own
# setup is not gated. Echoes "<remote-dir> <work-dir>".
setup_repo() {
	local tier_rc=1
	[ "${1:-}" = passing ] && tier_rc=0
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
		if [ "$tier_rc" -eq 0 ]; then
			printf 'check-resync-loss:\n\t./scripts/check-resync-loss.sh $(MERGE)\n\ntest-fast-parallel:\n\t@echo "SIMULATED gate-3 tier pass (test fixture)"\n' >Makefile
		else
			printf 'check-resync-loss:\n\t./scripts/check-resync-loss.sh $(MERGE)\n\ntest-fast-parallel:\n\t@echo "SIMULATED gate-3 tier failure (test fixture)" >&2; exit 1\n' >Makefile
		fi
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

echo "== pre-push gate 3 / LOCAL_TESTS_ACK (vc-dq0b, advisory since vp-iauu) =="

# ---------------------------------------------------------------------------
# A — THE ADVISORY CONTRACT. Tier fails, no ack: the push PROCEEDS, but the
# output must carry BOTH the tier's own failure and the hook's named RED
# advisory line. Without this case the whole file could pass while gate 3
# never ran at all; without the greppable line a red tier could pass
# silently — the audit hole --no-verify already had.
# ---------------------------------------------------------------------------
echo "-- tier failure with no ack: push proceeds, red reported by name --"
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
if [ "$rcA" -eq 0 ] && [ -n "$(remote_sha "$remA" refs/heads/ours)" ] &&
	grep -q 'SIMULATED gate-3 tier failure' "$outA" &&
	grep -q 'gate 3 (make test-fast-parallel) RED' "$outA" &&
	grep -q 'push CONTINUING' "$outA"; then
	record_pass "no-ack/tier-failure-reported-push-proceeds (advisory red, remote updated)"
else
	record_fail "no-ack/tier-failure-reported-push-proceeds" "rc=$rcA remote_sha=$(remote_sha "$remA" refs/heads/ours)"
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
# tier still failing, the push must STILL be blocked — by gate 2, not gate 3
# (which is advisory and could not block anything anyway). This is the case
# that fails if the ack is ever hoisted above gate 2.
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
# must NOT skip gate 3: the tier runs, the hook says why it ignored the ack,
# and the tier's red (the fixture always fails without "passing") is visible
# in the output. Gate 3 cannot block anymore, so the assertion is audit, not
# rejection: the IGNORED notice and the RED advisory line must both be
# present. These fail if the check is widened back to "any non-empty value"
# or loosened to accept a non-matching SHA.
# ---------------------------------------------------------------------------
# assert_ack_ignored <case-name> <ack-value|STALE|SHORT>: a Go change pushed
# with that ack must run gate 3 (tier failure + IGNORED notice + RED advisory
# in the output) and still land on the remote.
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
	if [ "$rc" -eq 0 ] && [ -n "$(remote_sha "$rem" refs/heads/ours)" ] &&
		grep -q "LOCAL_TESTS_ACK=$value IGNORED" "$out" &&
		grep -q 'SIMULATED gate-3 tier failure' "$out" &&
		grep -q 'push CONTINUING' "$out"; then
		record_pass "$name (ignored, tier ran and reported red, push proceeded)"
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
if [ "$rcI" -eq 0 ] && [ -n "$(remote_sha "$remI" refs/heads/ours)" ] &&
	[ -n "$(remote_sha "$remI" refs/heads/other)" ] &&
	grep -q "LOCAL_TESTS_ACK=$ackI IGNORED — this push carries more than one commit" "$outI" &&
	grep -q 'SIMULATED gate-3 tier failure' "$outI" &&
	grep -q 'push CONTINUING' "$outI"; then
	record_pass "ack/two-commits-one-acked-runs-gate-3 (ignored, tier ran, push proceeded)"
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

# ---------------------------------------------------------------------------
# L — THE GREEN PATH. Tier passes, no ack: the push proceeds and the hook
# emits the PASSED advisory line, so an audit can distinguish a green tier
# from a skipped one (the SKIPPED ack line) and from a red one (the RED
# line). Without this case, a hook that skipped the tier unconditionally
# would satisfy every other case in this file.
# ---------------------------------------------------------------------------
echo "-- tier passes with no ack: push proceeds, green reported by name --"
read -r remL workL <<<"$(setup_repo passing)"
(
	set -e
	cd "$workL"
	git checkout -q -b ours
	echo "package main" >a.go
	git add -A
	git commit -qm "go change"
) >/dev/null 2>&1
outL="$(mktemp "$tmp_root/outL.XXXXXX")"
(cd "$workL" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$outL" 2>&1
rcL=$?
if [ "$rcL" -eq 0 ] && [ -n "$(remote_sha "$remL" refs/heads/ours)" ] &&
	grep -q 'SIMULATED gate-3 tier pass' "$outL" &&
	grep -q 'gate 3 (make test-fast-parallel) PASSED' "$outL"; then
	record_pass "no-ack/tier-pass-reported-push-proceeds (advisory green)"
else
	record_fail "no-ack/tier-pass-reported-push-proceeds" "rc=$rcL"
	sed 's/^/    /' "$outL"
fi

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]

#!/usr/bin/env bash
#
# test-pre-push-local-tests-ack.sh — self-test for the LOCAL_TESTS_ACK escape
# on gate 3 of .githooks/pre-push (bead vc-dq0b).
#
# The pre-push hook runs three gates in order:
#   1. check-resync-loss  (ga-d32bn)     — ack: RESYNC_LOSS_ACK=1
#   2. bead-ownership     (ga-fip9ps.1)  — fail-closed, no scoped ack
#   3. make test-fast-parallel           — ack: LOCAL_TESTS_ACK=1  <- under test
#
# Before vc-dq0b, gate 3's only bypass was `git push --no-verify`, which
# disarms all three. The property these cases pin is therefore NOT "the ack
# works" (that is cheap and would be green even if the ack disarmed
# everything) but "the ack is SCOPED": it skips gate 3 and leaves gates 1
# and 2 armed. Cases C and D are the load-bearing ones — they must fail if
# the ack is ever moved earlier in the hook or widened into a --no-verify
# equivalent.
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
		mkdir -p scripts .githooks
		cp "$REPO_ROOT/.githooks/pre-push" .githooks/pre-push
		chmod +x .githooks/pre-push
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
if [ "$rcA" -ne 0 ] && [ -z "$(remote_sha "$remA" refs/heads/ours)" ]; then
	record_pass "no-ack/tier-failure-blocks-push (rejected, remote untouched)"
else
	record_fail "no-ack/tier-failure-blocks-push" "rc=$rcA remote_sha=$(remote_sha "$remA" refs/heads/ours)"
	sed 's/^/    /' "$outA"
fi

# ---------------------------------------------------------------------------
# B — THE ACK WORKS, AND SAYS SO. AC3 (push proceeds) + AC4 (named, greppable
# line on stderr). The audit line is asserted by content, not merely by rc:
# a silent skip is the failure mode today's --no-verify bypass already has.
# ---------------------------------------------------------------------------
echo "-- LOCAL_TESTS_ACK=1 skips gate 3 and emits a named line --"
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
(cd "$workB" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK=1 git push origin ours) >"$outB" 2>&1
rcB=$?
if [ "$rcB" -eq 0 ] && [ -n "$(remote_sha "$remB" refs/heads/ours)" ]; then
	if grep -q 'LOCAL_TESTS_ACK=1' "$outB" && grep -q 'gate 3' "$outB"; then
		record_pass "ack/skips-gate-3-and-emits-named-line"
	else
		record_fail "ack/skips-gate-3-and-emits-named-line" "push succeeded but no greppable notice"
		sed 's/^/    /' "$outB"
	fi
else
	record_fail "ack/skips-gate-3-and-emits-named-line" "rc=$rcB"
	sed 's/^/    /' "$outB"
fi

# ---------------------------------------------------------------------------
# C — LOAD-BEARING. The ack must NOT disarm gate 2. A bead that reads back as
# claimed by a different session is exactly the ownership violation gate 2
# exists to catch (the PR #4243 shape). With LOCAL_TESTS_ACK=1 set and the
# tier still failing, the push must STILL be blocked — by gate 2, not gate 3.
# This is the case that fails if the ack is ever hoisted above gate 2.
# ---------------------------------------------------------------------------
echo "-- LOCAL_TESTS_ACK=1 leaves the bead-ownership guard armed --"
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
	LOCAL_TESTS_ACK=1 git push origin builder/ga-zzzzzz-ack-test) >"$outC" 2>&1
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
echo "-- LOCAL_TESTS_ACK=1 leaves the resync-loss gate armed --"
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
(cd "$workD" && GIT_TERMINAL_PROMPT=0 LOCAL_TESTS_ACK=1 git push origin ours) >"$outD" 2>&1
rcD=$?
if [ "$rcD" -ne 0 ] && [ -z "$(remote_sha "$remD" refs/heads/ours)" ] &&
	grep -q 'check-resync-loss failed' "$outD"; then
	record_pass "ack/does-not-disarm-resync-loss-gate (blocked by gate 1)"
else
	record_fail "ack/does-not-disarm-resync-loss-gate" "rc=$rcD"
	sed 's/^/    /' "$outD"
fi

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]

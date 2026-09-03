#!/usr/bin/env bash
#
# test-check-resync-loss.sh — tests for scripts/check-resync-loss.sh (bead
# ga-d32bn). Two layers:
#
#   1. Historical regression: run the script against the REAL 2026-08-31
#      resync merge (15913af6aed2dae965d0fd6706bbf3e66458afca, in this
#      repo's own history) and assert it flags the known losses — the
#      on_boot_stagger tests and TestPreflightProxiedServerFallsBackToBdStore
#      as Gate 2 RESOLUTION-BUG, and internal/config/config_test.go as a
#      Gate 1 TOOK-THEIRS — and exits non-zero. This is the strongest test
#      available: it is the exact incident the script exists to catch.
#   2. Negative control: a tiny synthetic repo with a real two-sided merge
#      that loses nothing (both sides touch disjoint regions of a shared
#      file, plus a fork-added file that neither conflicts with nor is
#      touched by upstream) must exit 0.
#
# No network — the historical case reads this repo's own already-fetched
# history; the negative control builds a hermetic temp repo.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$TEST_DIR/check-resync-loss.sh"
REPO_ROOT="$(cd "$TEST_DIR/.." && pwd)"

pass=0
fail=0
record_pass() {
	echo "  ok   $1"
	pass=$((pass + 1))
}
record_fail() {
	echo "  FAIL $1 — $2"
	fail=$((fail + 1))
}

export GIT_AUTHOR_NAME="Test Author" GIT_AUTHOR_EMAIL="author@example.com"
export GIT_COMMITTER_NAME="Test Committer" GIT_COMMITTER_EMAIL="committer@example.com"
export GIT_CONFIG_NOSYSTEM=1
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

# ---------------------------------------------------------------------------
# 1. Historical regression against the real 2026-08-31 resync merge.
# ---------------------------------------------------------------------------
echo "== historical: 2026-08-31 resync merge (15913af6a) =="

HIST_MERGE="15913af6aed2dae965d0fd6706bbf3e66458afca"
if ! git -C "$REPO_ROOT" cat-file -e "${HIST_MERGE}^{commit}" 2>/dev/null; then
	record_fail "historical fixture reachable" "commit $HIST_MERGE not found in this checkout; fetch fork/main history first"
else
	hist_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-hist.XXXXXX")
	(cd "$REPO_ROOT" && bash "$SCRIPT" "$HIST_MERGE") >"$hist_out" 2>&1
	hist_rc=$?

	if [ "$hist_rc" -ne 0 ]; then
		record_pass "exits non-zero on a merge with real loss"
	else
		record_fail "exits non-zero on a merge with real loss" "exit code was 0"
	fi

	if grep -q "RESOLUTION-BUG   TestPreflightProxiedServerFallsBackToBdStore" "$hist_out"; then
		record_pass "flags TestPreflightProxiedServerFallsBackToBdStore as RESOLUTION-BUG"
	else
		record_fail "flags TestPreflightProxiedServerFallsBackToBdStore as RESOLUTION-BUG" "not found in output"
	fi

	if grep -qE "RESOLUTION-BUG   Test(DaemonOnBootStagger|RunPoolOnBootStagger)" "$hist_out"; then
		record_pass "flags an on_boot_stagger test as RESOLUTION-BUG"
	else
		record_fail "flags an on_boot_stagger test as RESOLUTION-BUG" "not found in output"
	fi

	if grep -q "TOOK-THEIRS	internal/config/config_test.go" "$hist_out"; then
		record_pass "flags internal/config/config_test.go as Gate 1 TOOK-THEIRS"
	else
		record_fail "flags internal/config/config_test.go as Gate 1 TOOK-THEIRS" "not found in output"
	fi

	rm -f "$hist_out"
fi

# ---------------------------------------------------------------------------
# 2. Negative control: a clean merge that loses nothing must exit 0.
#
# Shape: BASE has a.go with func A. OURS edits a.go (adds a trailing
# function, Ours) and adds a wholly new file b.go (func Bravo) that neither
# side else touches. THEIRS edits a.go in a disjoint region (adds a leading
# function, Theirs). A real `git merge` auto-resolves this with no conflict,
# keeping every fork-added declaration and the fork-added file.
# ---------------------------------------------------------------------------
echo "== negative control: clean two-sided merge =="

neg_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-neg.XXXXXX")"
(
	set -e
	cd "$neg_repo"
	git init -q -b main
	git config commit.gpgsign false

	cat >a.go <<'EOF'
package fixture

func A() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >a.go <<'EOF'
package fixture

func A() {}

func Ours() {}
EOF
	cat >b.go <<'EOF'
package fixture

func Bravo() {}
EOF
	git add -A
	git commit -qm "fork: add Ours() and b.go (Bravo)"

	git checkout -q -b theirs main
	cat >a.go <<'EOF'
package fixture

func Theirs() {}

func A() {}
EOF
	git add -A
	git commit -qm "upstream: add Theirs()"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null
)
neg_rc_setup=$?

if [ "$neg_rc_setup" -ne 0 ]; then
	record_fail "negative-control fixture builds and merges cleanly" "setup/merge failed, rc=$neg_rc_setup"
else
	neg_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-neg-out.XXXXXX")
	(cd "$neg_repo" && bash "$SCRIPT") >"$neg_out" 2>&1
	neg_rc=$?
	if [ "$neg_rc" -eq 0 ]; then
		record_pass "exits zero on a clean merge that loses nothing"
	else
		record_fail "exits zero on a clean merge that loses nothing" "exit code was $neg_rc:"
		sed 's/^/    /' "$neg_out"
	fi
	rm -f "$neg_out"
fi
rm -rf "$neg_repo"

# ---------------------------------------------------------------------------
echo
echo "== summary: $pass passed, $fail failed =="
[ "$fail" -eq 0 ]

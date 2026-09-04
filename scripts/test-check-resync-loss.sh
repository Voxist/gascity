#!/usr/bin/env bash
#
# test-check-resync-loss.sh — tests for scripts/check-resync-loss.sh (bead
# ga-d32bn). Three layers:
#
#   1. Historical regression: run the script against the REAL 2026-08-31
#      resync merge (15913af6aed2dae965d0fd6706bbf3e66458afca, in this
#      repo's own history) and assert it flags the known losses — the
#      on_boot_stagger tests and TestPreflightProxiedServerFallsBackToBdStore
#      as Gate 2 RESOLUTION-BUG, and internal/config/config_test.go as a
#      Gate 1 TOOK-THEIRS — and exits non-zero. This is the strongest test
#      available: it is the exact incident the script exists to catch.
#   2. Synthetic-repo cases: a negative control (a clean two-sided merge
#      that loses nothing) plus targeted fixtures for DROPPED-FILE, the
#      identical-change skip, conflict-hunk stripping, and (name, file)
#      keying — each isolates one specific fix so a regression in that one
#      fix fails only its own case.
#   3. Hook wiring: a real .githooks/pre-push, installed into a real temp
#      repo pushing to a real bare remote, actually refuses a push whose
#      tip is a merge commit with real Category-A loss, allows one whose
#      tip is a clean merge, never invokes check-resync-loss.sh at all for
#      an ordinary non-merge push, and lets --no-verify bypass it.
#
# No network — the historical case reads this repo's own already-fetched
# history; every other case builds a hermetic temp repo.
set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$TEST_DIR/check-resync-loss.sh"
REPO_ROOT="$(cd "$TEST_DIR/.." && pwd)"

pass=0
fail=0
skip=0
record_pass() {
	echo "  ok   $1"
	pass=$((pass + 1))
}
record_fail() {
	echo "  FAIL $1 — $2"
	fail=$((fail + 1))
}
record_skip() {
	echo "  skip $1 — $2"
	skip=$((skip + 1))
}

export GIT_AUTHOR_NAME="Test Author" GIT_AUTHOR_EMAIL="author@example.com"
export GIT_COMMITTER_NAME="Test Committer" GIT_COMMITTER_EMAIL="committer@example.com"
export GIT_CONFIG_NOSYSTEM=1
# Neutralize the operator's global git config too (merge.ff=only, a merge
# driver, an alias) — NOSYSTEM alone leaves ~/.gitconfig in play, and a
# real `git merge` in the negative-control fixture below is not hermetic
# against it.
export GIT_CONFIG_GLOBAL=/dev/null
unset GIT_DIR GIT_WORK_TREE 2>/dev/null || true

# ---------------------------------------------------------------------------
# 1. Historical regression against the real 2026-08-31 resync merge.
# ---------------------------------------------------------------------------
echo "== historical: 2026-08-31 resync merge (15913af6a) =="

HIST_MERGE="15913af6aed2dae965d0fd6706bbf3e66458afca"
if ! git -C "$REPO_ROOT" cat-file -e "${HIST_MERGE}^{commit}" 2>/dev/null; then
	# A shallow clone (e.g. CI's fetch-depth: 2 checkout of a PR branch) will
	# not have this commit's history at all. That is an environment limit,
	# not a regression in the script under test — skip, don't fail, so this
	# suite stays runnable (and meaningful for the other cases) on shallow
	# checkouts. A full local clone with fork/main history always has it.
	record_skip "historical fixture reachable" "commit $HIST_MERGE not found in this checkout (shallow clone?); fetch fork/main history to run this case"
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
# 3. Gate 1 DROPPED-FILE: a hand-resolved merge that drops fork-added files
#    entirely must be caught. Without `-q --verify` on the per-file
#    `git rev-parse REV:path` lookups, a missing path made rev-parse ECHO the
#    argument to stdout on a nonzero exit instead of resolving to empty — the
#    `[ -z "$mh" ]` emptiness check that detects DROPPED-FILE could then
#    never fire.
# ---------------------------------------------------------------------------
echo "== gate 1: DROPPED-FILE (fork-added files missing from merge) =="

drop_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-drop.XXXXXX")"
(
	set -e
	cd "$drop_repo"
	git init -q -b main
	git config commit.gpgsign false

	cat >a.go <<'EOF'
package fixture

func A() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	mkdir -p .github/workflows
	echo "# fork notes" >docs-notes.md
	echo "name: fork-ci" >.github/workflows/fork-ci.yml
	git add -A
	git commit -qm "fork: add docs-notes.md and fork-ci.yml"

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

	# Simulate a hand-resolved merge that dropped both fork-added files
	# entirely (the real ga-d32bn incident's shape) by rebuilding the merge
	# tree without them and committing it with the same two parents.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git rm -q docs-notes.md .github/workflows/fork-ci.yml
	bad_tree=$(git write-tree)
	git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop fork-added files" >BAD_MERGE_SHA
)
drop_rc_setup=$?

if [ "$drop_rc_setup" -ne 0 ]; then
	record_fail "DROPPED-FILE fixture builds" "setup failed, rc=$drop_rc_setup"
else
	bad_merge=$(cat "$drop_repo/BAD_MERGE_SHA")
	drop_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-drop-out.XXXXXX")
	(cd "$drop_repo" && bash "$SCRIPT" "$bad_merge") >"$drop_out" 2>&1
	drop_rc=$?

	drop_hits=$(grep -c '^  DROPPED-FILE' "$drop_out" || true)
	if [ "$drop_rc" -ne 0 ] && [ "$drop_hits" -eq 2 ]; then
		record_pass "flags both dropped fork-added files as DROPPED-FILE"
	else
		record_fail "flags both dropped fork-added files as DROPPED-FILE" "exit=$drop_rc DROPPED-FILE count=$drop_hits:"
		sed 's/^/    /' "$drop_out"
	fi
	rm -f "$drop_out"
fi
rm -rf "$drop_repo"

# ---------------------------------------------------------------------------
# 4. Gate 1 identical-change skip: OURS and THEIRS independently add the
#    exact same declaration. merge == theirs is then the ONLY correct 3-way
#    result (theirs' content equals ours'), not a loss — must not be
#    reported TOOK-THEIRS.
# ---------------------------------------------------------------------------
echo "== gate 1: identical fork/upstream change is not a false TOOK-THEIRS =="

ident_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-ident.XXXXXX")"
(
	set -e
	cd "$ident_repo"
	git init -q -b main
	git config commit.gpgsign false

	cat >a.go <<'EOF'
package fixture

func A() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >>a.go <<'EOF'

func Fix() {}
EOF
	git add -A
	git commit -qm "fork: add Fix()"

	git checkout -q -b theirs main
	cat >>a.go <<'EOF'

func Fix() {}
EOF
	git add -A
	git commit -qm "upstream: independently add identical Fix()"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null
)
ident_rc_setup=$?

if [ "$ident_rc_setup" -ne 0 ]; then
	record_fail "identical-change fixture builds and merges cleanly" "setup/merge failed, rc=$ident_rc_setup"
else
	ident_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-ident-out.XXXXXX")
	(cd "$ident_repo" && bash "$SCRIPT") >"$ident_out" 2>&1
	ident_rc=$?
	if [ "$ident_rc" -eq 0 ] && ! grep -q "TOOK-THEIRS.*a.go" "$ident_out"; then
		record_pass "exits zero, does not misreport an identical add as TOOK-THEIRS"
	else
		record_fail "exits zero, does not misreport an identical add as TOOK-THEIRS" "exit code was $ident_rc:"
		sed 's/^/    /' "$ident_out"
	fi
	rm -f "$ident_out"
fi
rm -rf "$ident_repo"

# ---------------------------------------------------------------------------
# 5. Gate 2 conflict-hunk stripping: a real both-sides conflict resolved
#    toward upstream drops a fork-added declaration that survives ONLY
#    inside the `git merge-file` oracle's conflict markers. That is a
#    judgment call the human resolver made, not proof a plain 3-way merge
#    would have kept it — must be MERGE-OUTCOME, not RESOLUTION-BUG, and
#    must not fail the gate.
# ---------------------------------------------------------------------------
echo "== gate 2: conflict resolved toward upstream is MERGE-OUTCOME, not RESOLUTION-BUG =="

conflict_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-conflict.XXXXXX")"
(
	set -e
	cd "$conflict_repo"
	git init -q -b main
	git config commit.gpgsign false

	# base's Common() body is left unclosed (no trailing "}") on purpose: it
	# leaves no shared trailing context after the point of divergence, so
	# the 3-way merge's conflict block runs to EOF and swallows ForkOnly()
	# along with the differing Common() body — the shape needed to prove
	# ForkOnly only ever exists inside a conflict marker, never as free
	# text a plain merge would deliver on its own.
	cat >a.go <<'EOF'
package fixture

func Common() {
	// base
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >a.go <<'EOF'
package fixture

func Common() {
	// fork tweak
}

func ForkOnly() {}
EOF
	git add -A
	git commit -qm "fork: tweak Common() and add ForkOnly()"

	git checkout -q -b theirs main
	cat >a.go <<'EOF'
package fixture

func Common() {
	// upstream tweak
}
EOF
	git add -A
	git commit -qm "upstream: tweak Common() differently"

	git checkout -q ours
	# Expect a real conflict; resolve it by hand toward upstream, discarding
	# ForkOnly() along with the fork's tweak — the shape of the real
	# ga-d32bn incident.
	git merge --no-edit theirs >/dev/null 2>&1 || true
	git show theirs:a.go >a.go
	git add -A
	git commit -qm "resync: resolve toward upstream (drops ForkOnly)"
)
conflict_rc_setup=$?

if [ "$conflict_rc_setup" -ne 0 ]; then
	record_fail "conflict fixture builds" "setup failed, rc=$conflict_rc_setup"
else
	conflict_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-conflict-out.XXXXXX")
	(cd "$conflict_repo" && bash "$SCRIPT") >"$conflict_out" 2>&1
	conflict_rc=$?
	if grep -q "MERGE-OUTCOME.*ForkOnly" "$conflict_out" && ! grep -q "RESOLUTION-BUG.*ForkOnly" "$conflict_out"; then
		record_pass "classifies ForkOnly (lost only inside a conflict marker) as MERGE-OUTCOME"
	else
		record_fail "classifies ForkOnly (lost only inside a conflict marker) as MERGE-OUTCOME" "exit=$conflict_rc:"
		sed 's/^/    /' "$conflict_out"
	fi
	rm -f "$conflict_out"
fi
rm -rf "$conflict_repo"

# ---------------------------------------------------------------------------
# 6. Gate 2 keyed by (name, file): a fork-added declaration duplicated across
#    two files that is dropped from only one of them must still be reported
#    — checking "is this name present anywhere in the merge" hides the loss
#    because the surviving copy in the other file satisfies a bare-name
#    check.
# ---------------------------------------------------------------------------
echo "== gate 2: (name, file)-keyed declarations catch a partial drop =="

dup_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-dup.XXXXXX")"
(
	set -e
	cd "$dup_repo"
	git init -q -b main
	git config commit.gpgsign false

	mkdir -p a b
	cat >a/a.go <<'EOF'
package a

func A() {}
EOF
	cat >b/b.go <<'EOF'
package b

func B() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >>a/a.go <<'EOF'

func newHelper() {}
EOF
	cat >>b/b.go <<'EOF'

func newHelper() {}
EOF
	git add -A
	git commit -qm "fork: add newHelper() to both a/a.go and b/b.go"

	git checkout -q -b theirs main
	cat >c.go <<'EOF'
package fixture

func C() {}
EOF
	git add -A
	git commit -qm "upstream: unrelated addition"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	# Simulate a hand-resolved merge that dropped newHelper() from a/a.go
	# ONLY (b/b.go keeps its copy) — the exact shape a bare-name Gate 2 check
	# cannot see.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git show main:a/a.go >a/a.go
	git add -A
	bad_tree=$(git write-tree)
	git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop newHelper from a/a.go only" >BAD_MERGE_SHA
)
dup_rc_setup=$?

if [ "$dup_rc_setup" -ne 0 ]; then
	record_fail "(name, file)-keyed fixture builds" "setup failed, rc=$dup_rc_setup"
else
	bad_merge=$(cat "$dup_repo/BAD_MERGE_SHA")
	dup_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-dup-out.XXXXXX")
	(cd "$dup_repo" && bash "$SCRIPT" "$bad_merge") >"$dup_out" 2>&1
	dup_rc=$?
	if grep -q "newHelper.*(a/a.go)" "$dup_out"; then
		record_pass "reports newHelper missing from a/a.go even though it survives in b/b.go"
	else
		record_fail "reports newHelper missing from a/a.go even though it survives in b/b.go" "exit=$dup_rc:"
		sed 's/^/    /' "$dup_out"
	fi
	rm -f "$dup_out"
fi
rm -rf "$dup_repo"

# ---------------------------------------------------------------------------
# 7. Hook wiring: a real .githooks/pre-push, installed into a real temp repo
#    with a real bare remote. Before this, `make check-resync-loss` existed
#    but nothing called it — the gate only ran when an operator remembered
#    the AGENTS.md sentence. These prove the hook actually enforces it.
# ---------------------------------------------------------------------------
echo "== hook wiring: .githooks/pre-push runs check-resync-loss on a merge-commit tip =="

hook_new_bare_remote() {
	local d
	d="$(mktemp -d "${TMPDIR:-/tmp}/crl-hook-remote.XXXXXX")"
	git init -q --bare -b main "$d"
	printf '%s' "$d"
}

hook_remote_sha() {
	git -C "$1" rev-parse --verify -q "$2" 2>/dev/null || true
}

# install_resync_hook <repo> <resync_script>: wires the REAL
# .githooks/pre-push and REAL scripts/push-ownership-guard.sh into <repo>,
# using <resync_script> as its scripts/check-resync-loss.sh — either the
# real script (tests that need it to actually run and actually catch loss)
# or a poison stub (the "never invoked on a non-merge push" test). Every
# branch used against this hook is deliberately not ga-XXXXXX-shaped and
# GC_AGENT is left unset, so push-ownership-guard.sh's bead-id resolution
# finds nothing to check and allows unconditionally — no fake `bd` needed.
# A trivial Makefile stands in for the real one (mirrors
# test-push-ownership-guard.sh's install_guard_hook).
install_resync_hook() {
	local repo="$1" resync_script="$2"
	mkdir -p "$repo/scripts" "$repo/.githooks"
	cp "$REPO_ROOT/.githooks/pre-push" "$repo/.githooks/pre-push"
	chmod +x "$repo/.githooks/pre-push"
	cp "$REPO_ROOT/scripts/push-ownership-guard.sh" "$repo/scripts/push-ownership-guard.sh"
	cp "$resync_script" "$repo/scripts/check-resync-loss.sh"
	chmod +x "$repo/scripts/check-resync-loss.sh"
	if [ "$resync_script" = "$REPO_ROOT/scripts/check-resync-loss.sh" ]; then
		cp "$REPO_ROOT/scripts/check-resync-loss-extract.py" "$repo/scripts/check-resync-loss-extract.py"
	fi
	cat >"$repo/Makefile" <<'EOF'
check-resync-loss:
	./scripts/check-resync-loss.sh $(MERGE)
test-fast-parallel:
	@true
EOF
	git -C "$repo" config core.hooksPath .githooks
}

# poison_resync_script: fails loudly if ever invoked — proves the hook does
# NOT call check-resync-loss.sh for an ordinary non-merge push.
poison_resync_script="$(mktemp "${TMPDIR:-/tmp}/crl-test-poison.XXXXXX")"
cat >"$poison_resync_script" <<'EOF'
#!/usr/bin/env bash
echo "POISON: check-resync-loss.sh was invoked (MERGE=$1) but this push has no merge commit" >&2
exit 1
EOF
chmod +x "$poison_resync_script"

# hook_setup_repo <real|poison>: a bare remote plus a work clone with the
# resync-loss hook installed and one commit already pushed to origin/main.
# Echoes "<remote-dir> <work-dir>".
hook_setup_repo() {
	local which="$1" remote work
	remote="$(hook_new_bare_remote)"
	work="$(mktemp -d "${TMPDIR:-/tmp}/crl-hook-work.XXXXXX")"
	git clone -q "$remote" "$work"
	(
		set -e
		cd "$work"
		git config commit.gpgsign false
		if [ "$which" = "real" ]; then
			install_resync_hook "$work" "$REPO_ROOT/scripts/check-resync-loss.sh"
		else
			install_resync_hook "$work" "$poison_resync_script"
		fi
		echo base >f.txt
		git add -A
		git commit -qm base
		git push -q origin main
	)
	printf '%s %s' "$remote" "$work"
}

echo "-- refuses a push whose tip is a merge commit with real Category-A loss --"
read -r hookA_remote hookA_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookA_work"
	git checkout -q -b ours
	echo "# fork notes" >docs-notes.md
	git add -A
	git commit -qm "fork: add docs-notes.md"

	git checkout -q -b theirs main
	echo theirs >>f.txt
	git add -A
	git commit -qm "upstream: touch f.txt"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	# Simulate a hand-resolved merge that drops the fork-added file — same
	# shape as the DROPPED-FILE fixture above (test 3), now as the actual
	# tip of a branch about to be pushed.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git rm -q docs-notes.md
	bad_tree=$(git write-tree)
	bad_merge=$(git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop docs-notes.md")
	git reset -q --hard "$bad_merge"
)
hookA_setup_rc=$?
if [ "$hookA_setup_rc" -ne 0 ]; then
	record_fail "hook/blocks-push-with-real-resync-loss" "fixture setup failed, rc=$hookA_setup_rc"
else
	hookA_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outA.XXXXXX")
	(cd "$hookA_work" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$hookA_out" 2>&1
	hookA_rc=$?
	if [ "$hookA_rc" -ne 0 ] && [ -z "$(hook_remote_sha "$hookA_remote" "refs/heads/ours")" ]; then
		record_pass "hook/blocks-push-with-real-resync-loss (rejected, remote untouched)"
	else
		record_fail "hook/blocks-push-with-real-resync-loss" "rc=$hookA_rc remote_sha=$(hook_remote_sha "$hookA_remote" "refs/heads/ours"):"
		sed 's/^/    /' "$hookA_out"
	fi
	rm -f "$hookA_out"
fi
rm -rf "$hookA_remote" "$hookA_work"

echo "-- allows a push whose tip is a clean merge --"
read -r hookB_remote hookB_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookB_work"
	git checkout -q -b ours
	echo "# fork notes" >docs-notes.md
	git add -A
	git commit -qm "fork: add docs-notes.md"

	git checkout -q -b theirs main
	echo theirs >>f.txt
	git add -A
	git commit -qm "upstream: touch f.txt"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null
)
hookB_setup_rc=$?
if [ "$hookB_setup_rc" -ne 0 ]; then
	record_fail "hook/allows-push-with-clean-merge" "fixture setup failed, rc=$hookB_setup_rc"
else
	hookB_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outB.XXXXXX")
	(cd "$hookB_work" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$hookB_out" 2>&1
	hookB_rc=$?
	if [ "$hookB_rc" -eq 0 ] && [ -n "$(hook_remote_sha "$hookB_remote" "refs/heads/ours")" ]; then
		record_pass "hook/allows-push-with-clean-merge"
	else
		record_fail "hook/allows-push-with-clean-merge" "rc=$hookB_rc:"
		sed 's/^/    /' "$hookB_out"
	fi
	rm -f "$hookB_out"
fi
rm -rf "$hookB_remote" "$hookB_work"

echo "-- never invokes check-resync-loss.sh for an ordinary non-merge push --"
read -r hookC_remote hookC_work <<<"$(hook_setup_repo poison)"
(
	set -e
	cd "$hookC_work"
	git checkout -q -b solo
	echo "solo change" >>f.txt
	git add -A
	git commit -qm "solo: an ordinary single-parent commit"
)
hookC_setup_rc=$?
if [ "$hookC_setup_rc" -ne 0 ]; then
	record_fail "hook/non-merge-push-never-invokes-check-resync-loss" "fixture setup failed, rc=$hookC_setup_rc"
else
	hookC_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outC.XXXXXX")
	(cd "$hookC_work" && GIT_TERMINAL_PROMPT=0 git push origin solo) >"$hookC_out" 2>&1
	hookC_rc=$?
	if [ "$hookC_rc" -eq 0 ] && [ -n "$(hook_remote_sha "$hookC_remote" "refs/heads/solo")" ] && ! grep -q "POISON:" "$hookC_out"; then
		record_pass "hook/non-merge-push-never-invokes-check-resync-loss"
	else
		record_fail "hook/non-merge-push-never-invokes-check-resync-loss" "rc=$hookC_rc:"
		sed 's/^/    /' "$hookC_out"
	fi
	rm -f "$hookC_out"
fi
rm -rf "$hookC_remote" "$hookC_work"

echo "-- --no-verify bypasses the resync-loss gate --"
read -r hookD_remote hookD_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookD_work"
	git checkout -q -b ours
	echo "# fork notes" >docs-notes.md
	git add -A
	git commit -qm "fork: add docs-notes.md"

	git checkout -q -b theirs main
	echo theirs >>f.txt
	git add -A
	git commit -qm "upstream: touch f.txt"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git rm -q docs-notes.md
	bad_tree=$(git write-tree)
	bad_merge=$(git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop docs-notes.md")
	git reset -q --hard "$bad_merge"
)
hookD_setup_rc=$?
if [ "$hookD_setup_rc" -ne 0 ]; then
	record_fail "hook/no-verify-bypasses-resync-loss-gate" "fixture setup failed, rc=$hookD_setup_rc"
else
	hookD_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outD.XXXXXX")
	(cd "$hookD_work" && GIT_TERMINAL_PROMPT=0 git push --no-verify origin ours) >"$hookD_out" 2>&1
	hookD_rc=$?
	if [ "$hookD_rc" -eq 0 ] && [ -n "$(hook_remote_sha "$hookD_remote" "refs/heads/ours")" ]; then
		record_pass "hook/no-verify-bypasses-resync-loss-gate (push succeeded despite real loss)"
	else
		record_fail "hook/no-verify-bypasses-resync-loss-gate" "rc=$hookD_rc:"
		sed 's/^/    /' "$hookD_out"
	fi
	rm -f "$hookD_out"
fi
rm -rf "$hookD_remote" "$hookD_work"
rm -f "$poison_resync_script"

# ---------------------------------------------------------------------------
echo
echo "== summary: $pass passed, $fail failed, $skip skipped =="
[ "$fail" -eq 0 ]

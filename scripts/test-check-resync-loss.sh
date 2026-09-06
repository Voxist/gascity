#!/usr/bin/env bash
#
# test-check-resync-loss.sh — tests for scripts/check-resync-loss.sh (beads
# ga-d32bn, ga-8gpw4, ga-qq43h). Four layers:
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
#   3. Matched-pair fixtures (ga-qq43h): ONE fixture, three merge commits
#      over identical parents differing by exactly one declaration — the
#      correct merge (must exit 0), the same tree minus the fork's
#      declaration (must fail Gate 2), and the same tree minus upstream's
#      (must fail Gate 3). Every other case proves the gate reacts to
#      something; these prove it reacts to the MINIMAL realistic regression,
#      and that it goes green again the moment the declaration is restored.
#   4. Hook wiring: a real .githooks/pre-push, installed into a real temp
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
# 7. Gate 2 recognizes grouped `type ( ... )` blocks: a fork-added type
#    declared inside one is a declaration like any other and must not be
#    invisible to the extractor just because it sits in a `type (` group
#    instead of a bare top-level `type Foo ...` line.
# ---------------------------------------------------------------------------
echo "== gate 2: fork-added declaration inside a grouped type ( ... ) block =="

typegrp_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-typegrp.XXXXXX")"
(
	set -e
	cd "$typegrp_repo"
	git init -q -b main

	cat >a.go <<'EOF'
package fixture

type (
	Base int
)
EOF
	echo base >other.go
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >a.go <<'EOF'
package fixture

type (
	Base int
	ForkType struct {
		X int
	}
)
EOF
	git add -A
	git commit -qm "fork: add ForkType inside the grouped type ( ... ) block"

	git checkout -q -b theirs main
	echo "upstream touch" >>other.go
	git add -A
	git commit -qm "upstream: unrelated change to other.go"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	# Simulate a hand-resolved merge that drops the fork's addition to the
	# type group entirely (reverts a.go to base's content) — same shape as
	# the (name, file) fixture above, now for a grouped type member instead
	# of a func.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git show main:a.go >a.go
	git add -A
	bad_tree=$(git write-tree)
	git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop ForkType from the type group" >BAD_MERGE_SHA
)
typegrp_rc_setup=$?

if [ "$typegrp_rc_setup" -ne 0 ]; then
	record_fail "grouped-type fixture builds" "setup failed, rc=$typegrp_rc_setup"
else
	bad_merge=$(cat "$typegrp_repo/BAD_MERGE_SHA")
	typegrp_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-typegrp-out.XXXXXX")
	(cd "$typegrp_repo" && bash "$SCRIPT" "$bad_merge") >"$typegrp_out" 2>&1
	typegrp_rc=$?
	if [ "$typegrp_rc" -ne 0 ] && grep -q "RESOLUTION-BUG.*ForkType" "$typegrp_out"; then
		record_pass "reports ForkType dropped from a grouped type ( ... ) block"
	else
		record_fail "reports ForkType dropped from a grouped type ( ... ) block" "exit=$typegrp_rc:"
		sed 's/^/    /' "$typegrp_out"
	fi
	rm -f "$typegrp_out"
fi
rm -rf "$typegrp_repo"

# ---------------------------------------------------------------------------
# 8. Gate 1 exemption is driven by the `linguist-generated` .gitattributes
#    attribute (ga-d32bn review): a file so marked and taken wholesale from
#    upstream in a real conflict must be exempt, not reported TOOK-THEIRS —
#    this is what licenses AGENTS.md "Resync conventions" rule 1's "take
#    upstream wholesale" instruction for the paths in .gitattributes.
# ---------------------------------------------------------------------------
echo "== gate 1: a linguist-generated file taken wholesale is exempt =="

genattr_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-genattr.XXXXXX")"
(
	set -e
	cd "$genattr_repo"
	git init -q -b main
	echo "gen.txt -diff linguist-generated" >.gitattributes
	echo base >gen.txt
	git add -A
	git commit -qm base

	git checkout -q -b ours
	echo "fork version" >gen.txt
	git add -A
	git commit -qm "fork: regenerate gen.txt (fork side)"

	git checkout -q -b theirs main
	echo "upstream version" >gen.txt
	git add -A
	git commit -qm "upstream: regenerate gen.txt (upstream side)"

	git checkout -q ours
	# Real conflict (both sides changed the same line); resolve by taking
	# upstream's regenerated content wholesale, per AGENTS.md rule 1.
	git merge --no-edit theirs >/dev/null 2>&1 || true
	git checkout --theirs -- gen.txt
	git add -A
	git commit -qm "resync: take upstream's regenerated gen.txt wholesale"
)
genattr_rc_setup=$?

if [ "$genattr_rc_setup" -ne 0 ]; then
	record_fail "linguist-generated fixture builds" "setup failed, rc=$genattr_rc_setup"
else
	genattr_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-genattr-out.XXXXXX")
	(cd "$genattr_repo" && bash "$SCRIPT") >"$genattr_out" 2>&1
	genattr_rc=$?
	if [ "$genattr_rc" -eq 0 ] && grep -q "EXEMPT (generated, AGENTS.md rule 1) TOOK-THEIRS gen.txt" "$genattr_out"; then
		record_pass "exempts a linguist-generated file taken wholesale"
	else
		record_fail "exempts a linguist-generated file taken wholesale" "exit=$genattr_rc:"
		sed 's/^/    /' "$genattr_out"
	fi
	rm -f "$genattr_out"
fi
rm -rf "$genattr_repo"

# ---------------------------------------------------------------------------
# 9. Gate 2 must be keyed by (name, file) pairs from the start, not by bare
#    name filtered down to pairs afterward: computing forkadded as
#    `set(d_ours) - set(d_base) - set(d_theirs)` (bare names) hides a real
#    fork-added declaration whenever its NAME collides with an unrelated
#    declaration of the same name anywhere else in BASE or THEIRS — exactly
#    the shared-`_test.go`-helper shape ga-d32bn exists to catch. Fork adds
#    writeFile() to cfg/a_test.go; upstream independently adds an unrelated
#    writeFile() to other/o_test.go; a hand-resolved merge drops the fork's
#    copy. The bare-name computation would exclude "writeFile" entirely
#    (it exists in d_theirs) and the loss would be invisible.
# ---------------------------------------------------------------------------
echo "== gate 2: fork-added decl name collides with an unrelated upstream decl =="

collide_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-collide.XXXXXX")"
(
	set -e
	cd "$collide_repo"
	git init -q -b main
	mkdir -p cfg other
	cat >cfg/a_test.go <<'EOF'
package cfg

func TestA(t *testing.T) {}
EOF
	cat >other/o_test.go <<'EOF'
package other

func TestO(t *testing.T) {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >>cfg/a_test.go <<'EOF'

func writeFile() {}
EOF
	git add -A
	git commit -qm "fork: add writeFile() helper to cfg/a_test.go"

	git checkout -q -b theirs main
	cat >>other/o_test.go <<'EOF'

func writeFile() {}
EOF
	git add -A
	git commit -qm "upstream: independently add an unrelated writeFile() to other/o_test.go"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	# Simulate a hand-resolved merge that drops the fork's writeFile() from
	# cfg/a_test.go only — other/o_test.go's unrelated writeFile() (upstream's)
	# survives untouched.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git show main:cfg/a_test.go >cfg/a_test.go
	git add -A
	bad_tree=$(git write-tree)
	git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop writeFile from cfg/a_test.go only" >BAD_MERGE_SHA
)
collide_rc_setup=$?

if [ "$collide_rc_setup" -ne 0 ]; then
	record_fail "name-collision fixture builds" "setup failed, rc=$collide_rc_setup"
else
	bad_merge=$(cat "$collide_repo/BAD_MERGE_SHA")
	collide_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-collide-out.XXXXXX")
	(cd "$collide_repo" && bash "$SCRIPT" "$bad_merge") >"$collide_out" 2>&1
	collide_rc=$?
	if [ "$collide_rc" -ne 0 ] && grep -q "RESOLUTION-BUG.*writeFile.*cfg/a_test.go" "$collide_out"; then
		record_pass "reports writeFile dropped from cfg/a_test.go despite an unrelated writeFile surviving in other/o_test.go"
	else
		record_fail "reports writeFile dropped from cfg/a_test.go despite an unrelated writeFile surviving in other/o_test.go" "exit=$collide_rc:"
		sed 's/^/    /' "$collide_out"
	fi
	rm -f "$collide_out"
fi
rm -rf "$collide_repo"

# ---------------------------------------------------------------------------
# 10. Gate 2 exemption: a fork-added declaration "lost" from a
#     linguist-generated file is the licensed AGENTS.md rule 1 outcome
#     (take upstream wholesale), not a resolution defect — must report
#     EXEMPT and never fail the gate, the same as Gate 1's file-level
#     exemption for the same path.
# ---------------------------------------------------------------------------
echo "== gate 2: fork-added decl dropped from a linguist-generated file is EXEMPT =="

gen2_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-gen2.XXXXXX")"
(
	set -e
	cd "$gen2_repo"
	git init -q -b main
	echo "gen.go -diff linguist-generated" >.gitattributes
	cat >gen.go <<'EOF'
package fixture

func Base() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >gen.go <<'EOF'
package fixture

func Base() {}

func ForkFunc() {}
EOF
	git add -A
	git commit -qm "fork: regenerate gen.go with ForkFunc (fork side)"

	git checkout -q -b theirs main
	cat >gen.go <<'EOF'
package fixture

func Base() {}

func UpstreamFunc() {}
EOF
	git add -A
	git commit -qm "upstream: regenerate gen.go with UpstreamFunc (upstream side)"

	git checkout -q ours
	# Real conflict; resolve by taking upstream's regenerated content
	# wholesale, per AGENTS.md rule 1 — this drops ForkFunc.
	git merge --no-edit theirs >/dev/null 2>&1 || true
	git checkout --theirs -- gen.go
	git add -A
	git commit -qm "resync: take upstream's regenerated gen.go wholesale"
)
gen2_rc_setup=$?

if [ "$gen2_rc_setup" -ne 0 ]; then
	record_fail "generated-file Gate 2 fixture builds" "setup failed, rc=$gen2_rc_setup"
else
	gen2_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-gen2-out.XXXXXX")
	(cd "$gen2_repo" && bash "$SCRIPT") >"$gen2_out" 2>&1
	gen2_rc=$?
	if [ "$gen2_rc" -eq 0 ] && grep -q "EXEMPT.*ForkFunc.*gen.go" "$gen2_out" && ! grep -qE "(RESOLUTION-BUG|MERGE-OUTCOME).*ForkFunc" "$gen2_out"; then
		record_pass "reports ForkFunc dropped from a linguist-generated file as EXEMPT, not a failure"
	else
		record_fail "reports ForkFunc dropped from a linguist-generated file as EXEMPT, not a failure" "exit=$gen2_rc:"
		sed 's/^/    /' "$gen2_out"
	fi
	rm -f "$gen2_out"
fi
rm -rf "$gen2_repo"

# ---------------------------------------------------------------------------
# 11. Gate 1 DROPPED-FILE must not fire when UPSTREAM deleted a file the
#     fork had merely modified — a correct, routine merge outcome, not a
#     resync mishandling. Base has old.go; fork tweaks it; upstream deletes
#     it; the merge accepts the deletion. Must exit 0, not report
#     DROPPED-FILE.
# ---------------------------------------------------------------------------
echo "== gate 1: DROPPED-FILE does not fire on a legitimate upstream deletion =="

updel_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-updel.XXXXXX")"
(
	set -e
	cd "$updel_repo"
	git init -q -b main
	cat >old.go <<'EOF'
package fixture

func Old() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >>old.go <<'EOF'

func ForkTweak() {}
EOF
	git add -A
	git commit -qm "fork: tweak old.go"

	git checkout -q -b theirs main
	git rm -q old.go
	git commit -qm "upstream: delete old.go"

	git checkout -q ours
	# Real conflict (modify/delete); resolve by accepting the deletion, the
	# only sane outcome once upstream has removed the file.
	git merge --no-edit theirs >/dev/null 2>&1 || true
	git rm -qf old.go 2>/dev/null || rm -f old.go
	git add -A
	git commit -qm "resync: accept upstream's deletion of old.go"
)
updel_rc_setup=$?

if [ "$updel_rc_setup" -ne 0 ]; then
	record_fail "upstream-deletion fixture builds" "setup failed, rc=$updel_rc_setup"
else
	updel_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-updel-out.XXXXXX")
	(cd "$updel_repo" && bash "$SCRIPT") >"$updel_out" 2>&1
	updel_rc=$?
	if [ "$updel_rc" -eq 0 ] && ! grep -q "DROPPED-FILE" "$updel_out"; then
		record_pass "does not report DROPPED-FILE for a legitimate upstream deletion"
	else
		record_fail "does not report DROPPED-FILE for a legitimate upstream deletion" "exit=$updel_rc:"
		sed 's/^/    /' "$updel_out"
	fi
	rm -f "$updel_out"
fi
rm -rf "$updel_repo"

# ---------------------------------------------------------------------------
# 12. MATCHED-PAIR fixtures (bead ga-qq43h: "every guard ships a fixture
#     proving it fails without the fix"). Every case above proves the gate
#     reacts to SOMETHING. These prove it reacts to THIS — the minimal
#     realistic regression — by holding one fixture fixed and perturbing
#     exactly one declaration.
#
#     One BASE/OURS/THEIRS, three merge commits over the identical parent
#     pair, differing from each other by exactly one declaration:
#
#       kept          the correct 3-way result (both sides' additions) — the
#                     control. Must exit 0 with no hit in either direction.
#       drop-fork     `kept` minus the fork's TestForkOnly, nothing else.
#                     Must fail with a Gate 2 RESOLUTION-BUG.
#       drop-upstream `kept` minus upstream's TestUpstreamOnly, nothing else.
#                     Must fail with a Gate 3 UPSTREAM-RESOLUTION-BUG.
#
#     The two additions sit in disjoint regions of one shared _test.go with
#     filler between them, so a plain `git merge-file` keeps both with no
#     conflict: that is what makes each loss a RESOLUTION-BUG (a defect in
#     how the merge was resolved) rather than a MERGE-OUTCOME, and it is the
#     precise shape of the ga-d32bn / ga-8gpw4 incidents.
#
#     The drop-upstream case is the one the shipped gate could not see at
#     all before ga-8gpw4: run it against a Gate-1+2-only check-resync-loss.sh
#     and it exits 0.
# ---------------------------------------------------------------------------
echo "== matched pair: one fixture, one declaration dropped in each direction =="

pair_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-pair.XXXXXX")"
(
	set -e
	cd "$pair_repo"
	git init -q -b main

	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}
EOF
	git add -A
	git commit -qm base

	# Fork appends TestForkOnly at the END and annotates helperTen. The
	# annotation's length and position are both load-bearing; the block
	# itself explains why.
	git checkout -q -b ours
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
// fork: boot-path notes for helperTen.
//
// This block is deliberately long. Gate 1's near-theirs heuristic treats a
// merge result within GATE1_NEAR_THEIRS_MAX_DELTA (20) lines of THEIRS as a
// took-theirs once Gate 2 corroborates it, so a one-line fork-side edit here
// would leave the drop-fork merge inside that window and fire a Gate 1
// verdict on top of the Gate 2 one. The case would still go red, but it would
// no longer isolate Gate 2, which is the entire point of a matched pair
// (ga-qq43h: the perturbation must be minimal AND the guard's scope must
// equal the invariant's scope). Twenty-plus lines of fork-side content that
// the merge KEEPS puts the delta outside the window, so the only verdict on
// the drop-fork merge is the declaration one.
//
// It also has to sit far away from every upstream-side edit. `git merge-file`
// emits a conflict hunk when the two sides touch overlapping regions, and the
// extractor strips conflict hunks before asking whether a declaration
// survived — content that reached the merge only inside a conflict marker is
// content a human had to adjudicate, which is a MERGE-OUTCOME, not a
// RESOLUTION-BUG. Upstream edits the top of the file and helperThree; this
// block is anchored at helperTen, six declarations below, well outside the
// three lines of context diff3 uses.
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}

func TestForkOnly(t *testing.T) {}
EOF
	git add -A
	git commit -qm "fork: add TestForkOnly, annotate helperTen"

	# Upstream inserts TestUpstreamOnly near the TOP and annotates
	# helperThree — both far from every fork-side edit.
	git checkout -q -b theirs main
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func TestUpstreamOnly(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
// upstream: empty-pool notes for helperThree.
//
// The mirror of the fork's block below, and long for the mirror reason: it
// keeps the drop-upstream merge from being byte-identical to the
// fork's blob, which would fire Gate 3's file-level UPSTREAM-TOOK-OURS on
// top of the declaration verdict and stop the case from isolating Gate 3's
// declaration-level half.
//
// Gate 3 has no near-ours counterpart to GATE1_NEAR_THEIRS_MAX_DELTA, so
// length is not strictly load-bearing on this side — but keeping the two
// sides the same shape means a future change to either heuristic breaks
// both cases together instead of leaving one silently vacuous.
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}
EOF
	git add -A
	git commit -qm "upstream: add TestUpstreamOnly, annotate helperThree"

	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git checkout -q ours

	# --- control: the correct 3-way result, every edit from both sides ---
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func TestUpstreamOnly(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
// upstream: empty-pool notes for helperThree.
//
// The mirror of the fork's block below, and long for the mirror reason: it
// keeps the drop-upstream merge from being byte-identical to the
// fork's blob, which would fire Gate 3's file-level UPSTREAM-TOOK-OURS on
// top of the declaration verdict and stop the case from isolating Gate 3's
// declaration-level half.
//
// Gate 3 has no near-ours counterpart to GATE1_NEAR_THEIRS_MAX_DELTA, so
// length is not strictly load-bearing on this side — but keeping the two
// sides the same shape means a future change to either heuristic breaks
// both cases together instead of leaving one silently vacuous.
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
// fork: boot-path notes for helperTen.
//
// This block is deliberately long. Gate 1's near-theirs heuristic treats a
// merge result within GATE1_NEAR_THEIRS_MAX_DELTA (20) lines of THEIRS as a
// took-theirs once Gate 2 corroborates it, so a one-line fork-side edit here
// would leave the drop-fork merge inside that window and fire a Gate 1
// verdict on top of the Gate 2 one. The case would still go red, but it would
// no longer isolate Gate 2, which is the entire point of a matched pair
// (ga-qq43h: the perturbation must be minimal AND the guard's scope must
// equal the invariant's scope). Twenty-plus lines of fork-side content that
// the merge KEEPS puts the delta outside the window, so the only verdict on
// the drop-fork merge is the declaration one.
//
// It also has to sit far away from every upstream-side edit. `git merge-file`
// emits a conflict hunk when the two sides touch overlapping regions, and the
// extractor strips conflict hunks before asking whether a declaration
// survived — content that reached the merge only inside a conflict marker is
// content a human had to adjudicate, which is a MERGE-OUTCOME, not a
// RESOLUTION-BUG. Upstream edits the top of the file and helperThree; this
// block is anchored at helperTen, six declarations below, well outside the
// three lines of context diff3 uses.
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}

func TestForkOnly(t *testing.T) {}
EOF
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: correct merge, both sides kept" >KEPT_SHA

	# --- perturbation 1: the SAME tree minus the fork's one declaration ---
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func TestUpstreamOnly(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
// upstream: empty-pool notes for helperThree.
//
// The mirror of the fork's block below, and long for the mirror reason: it
// keeps the drop-upstream merge from being byte-identical to the
// fork's blob, which would fire Gate 3's file-level UPSTREAM-TOOK-OURS on
// top of the declaration verdict and stop the case from isolating Gate 3's
// declaration-level half.
//
// Gate 3 has no near-ours counterpart to GATE1_NEAR_THEIRS_MAX_DELTA, so
// length is not strictly load-bearing on this side — but keeping the two
// sides the same shape means a future change to either heuristic breaks
// both cases together instead of leaving one silently vacuous.
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
// fork: boot-path notes for helperTen.
//
// This block is deliberately long. Gate 1's near-theirs heuristic treats a
// merge result within GATE1_NEAR_THEIRS_MAX_DELTA (20) lines of THEIRS as a
// took-theirs once Gate 2 corroborates it, so a one-line fork-side edit here
// would leave the drop-fork merge inside that window and fire a Gate 1
// verdict on top of the Gate 2 one. The case would still go red, but it would
// no longer isolate Gate 2, which is the entire point of a matched pair
// (ga-qq43h: the perturbation must be minimal AND the guard's scope must
// equal the invariant's scope). Twenty-plus lines of fork-side content that
// the merge KEEPS puts the delta outside the window, so the only verdict on
// the drop-fork merge is the declaration one.
//
// It also has to sit far away from every upstream-side edit. `git merge-file`
// emits a conflict hunk when the two sides touch overlapping regions, and the
// extractor strips conflict hunks before asking whether a declaration
// survived — content that reached the merge only inside a conflict marker is
// content a human had to adjudicate, which is a MERGE-OUTCOME, not a
// RESOLUTION-BUG. Upstream edits the top of the file and helperThree; this
// block is anchored at helperTen, six declarations below, well outside the
// three lines of context diff3 uses.
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}
EOF
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: (simulated --theirs-shaped) drop TestForkOnly" >DROP_FORK_SHA

	# --- perturbation 2: the SAME tree minus upstream's one declaration ---
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func helperOne() {}
func helperTwo() {}
// upstream: empty-pool notes for helperThree.
//
// The mirror of the fork's block below, and long for the mirror reason: it
// keeps the drop-upstream merge from being byte-identical to the
// fork's blob, which would fire Gate 3's file-level UPSTREAM-TOOK-OURS on
// top of the declaration verdict and stop the case from isolating Gate 3's
// declaration-level half.
//
// Gate 3 has no near-ours counterpart to GATE1_NEAR_THEIRS_MAX_DELTA, so
// length is not strictly load-bearing on this side — but keeping the two
// sides the same shape means a future change to either heuristic breaks
// both cases together instead of leaving one silently vacuous.
func helperThree() {}
func helperFour() {}
func helperFive() {}
func helperSix() {}
func helperSeven() {}
func helperEight() {}
func helperNine() {}
// fork: boot-path notes for helperTen.
//
// This block is deliberately long. Gate 1's near-theirs heuristic treats a
// merge result within GATE1_NEAR_THEIRS_MAX_DELTA (20) lines of THEIRS as a
// took-theirs once Gate 2 corroborates it, so a one-line fork-side edit here
// would leave the drop-fork merge inside that window and fire a Gate 1
// verdict on top of the Gate 2 one. The case would still go red, but it would
// no longer isolate Gate 2, which is the entire point of a matched pair
// (ga-qq43h: the perturbation must be minimal AND the guard's scope must
// equal the invariant's scope). Twenty-plus lines of fork-side content that
// the merge KEEPS puts the delta outside the window, so the only verdict on
// the drop-fork merge is the declaration one.
//
// It also has to sit far away from every upstream-side edit. `git merge-file`
// emits a conflict hunk when the two sides touch overlapping regions, and the
// extractor strips conflict hunks before asking whether a declaration
// survived — content that reached the merge only inside a conflict marker is
// content a human had to adjudicate, which is a MERGE-OUTCOME, not a
// RESOLUTION-BUG. Upstream edits the top of the file and helperThree; this
// block is anchored at helperTen, six declarations below, well outside the
// three lines of context diff3 uses.
func helperTen() {}
func helperEleven() {}
func helperTwelve() {}

func TestForkOnly(t *testing.T) {}
EOF
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: (simulated --ours-shaped) drop TestUpstreamOnly" >DROP_UPSTREAM_SHA
)
pair_rc_setup=$?

if [ "$pair_rc_setup" -ne 0 ]; then
	record_fail "matched-pair fixture builds" "setup failed, rc=$pair_rc_setup"
else
	pair_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-pair-out.XXXXXX")

	# Control: the restored/correct merge must be clean in BOTH directions.
	# Anchored to the two-space verdict-line prefix both gates print, NOT a
	# bare substring: the per-gate summary lines spell every verdict name out
	# with a count in front of it ("0 RESOLUTION-BUG, 0 MERGE-OUTCOME"), so an
	# unanchored negative grep can never pass and the case would fail on a
	# perfectly clean merge.
	(cd "$pair_repo" && bash "$SCRIPT" "$(cat "$pair_repo/KEPT_SHA")") >"$pair_out" 2>&1
	pair_kept_rc=$?
	if [ "$pair_kept_rc" -eq 0 ] &&
		! grep -qE "^  (UPSTREAM-)?(RESOLUTION-BUG|MERGE-OUTCOME|DROPPED-FILE|PURE-LOSS|TOOK-THEIRS|TOOK-OURS)" "$pair_out"; then
		record_pass "control: correct merge keeping both declarations exits 0"
	else
		record_fail "control: correct merge keeping both declarations exits 0" "exit=$pair_kept_rc:"
		sed 's/^/    /' "$pair_out"
	fi

	# Perturbation 1 — fork direction (Gate 2). EXACTLY one verdict: the
	# summary lines assert both other gates stayed at zero. Without that, a
	# fixture whose perturbation also trips Gate 1 or Gate 3 would still
	# "pass" and prove nothing about Gate 2 specifically.
	(cd "$pair_repo" && bash "$SCRIPT" "$(cat "$pair_repo/DROP_FORK_SHA")") >"$pair_out" 2>&1
	pair_fork_rc=$?
	if [ "$pair_fork_rc" -ne 0 ] &&
		grep -q "^  RESOLUTION-BUG   TestForkOnly  (shared_test.go)" "$pair_out" &&
		grep -q "^Gate 1: 0 unexempted hit(s)" "$pair_out" &&
		grep -q "^Gate 2: 1 missing (decl, file) pair(s) — 1 RESOLUTION-BUG" "$pair_out" &&
		grep -q "^Gate 3: 0 unexempted file-level hit(s) (0 exempted); 0 missing" "$pair_out"; then
		record_pass "gate 2 fails on exactly one dropped fork-added declaration"
	else
		record_fail "gate 2 fails on exactly one dropped fork-added declaration" "exit=$pair_fork_rc:"
		sed 's/^/    /' "$pair_out"
	fi

	# Perturbation 2 — upstream direction (Gate 3, ga-8gpw4). This is the
	# case that exits 0 on a Gate-1+2-only script.
	(cd "$pair_repo" && bash "$SCRIPT" "$(cat "$pair_repo/DROP_UPSTREAM_SHA")") >"$pair_out" 2>&1
	pair_up_rc=$?
	if [ "$pair_up_rc" -ne 0 ] &&
		grep -q "^  UPSTREAM-RESOLUTION-BUG TestUpstreamOnly  (shared_test.go)" "$pair_out" &&
		grep -q "^Gate 1: 0 unexempted hit(s)" "$pair_out" &&
		grep -q "^Gate 2: 0 missing (decl, file) pair(s)" "$pair_out" &&
		grep -q "^Gate 3: 0 unexempted file-level hit(s) (0 exempted); 1 missing (decl, file) pair(s) — 1 UPSTREAM-RESOLUTION-BUG" "$pair_out"; then
		record_pass "gate 3 fails on exactly one dropped upstream-added declaration"
	else
		record_fail "gate 3 fails on exactly one dropped upstream-added declaration" "exit=$pair_up_rc:"
		sed 's/^/    /' "$pair_out"
	fi

	rm -f "$pair_out"
fi
rm -rf "$pair_repo"

# ---------------------------------------------------------------------------
# 13. Gate 3 file-level, both polarities of the one condition that separates
#     them. "OURS does not have this file" has two causes, and treating them
#     alike is a live bug in either direction:
#
#       BASE lacks it too  -> UPSTREAM ADDED IT. The merge dropping an
#                             upstream-added file wholesale is the most
#                             clear-cut upstream loss there is; the
#                             2026-08-31 merge did exactly this to
#                             cmd/gc/dolt_cleanup_discovery{,_test}.go.
#                             MUST fail.
#       BASE has it        -> the FORK deleted it before the merge. Accepting
#                             that standing fork decision is correct, and
#                             re-raising it on every resync forever would
#                             train operators to ignore the gate. MUST NOT
#                             fail, at the file level OR the declaration
#                             level (the two must agree).
#
#     Testing only the first would leave the skip untested; testing only the
#     second would pass on a gate that never fires at all.
# ---------------------------------------------------------------------------
echo "== gate 3: upstream-added file dropped by the merge, vs a fork-deleted file =="

upfile_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-upfile.XXXXXX")"
(
	set -e
	cd "$upfile_repo"
	git init -q -b main
	cat >keep.go <<'EOF'
package fixture

func Keep() {}
EOF
	cat >gone.go <<'EOF'
package fixture

func Gone() {}
EOF
	git add -A
	git commit -qm base

	# Fork deletes gone.go and leaves keep.go alone.
	git checkout -q -b ours
	git rm -q gone.go
	git commit -qm "fork: delete gone.go"

	# Upstream adds a wholly new file AND keeps extending the file the fork
	# deleted.
	git checkout -q -b theirs main
	cat >added.go <<'EOF'
package fixture

func UpstreamAdded() {}
EOF
	cat >>gone.go <<'EOF'

func UpstreamTouchedGone() {}
EOF
	git add -A
	git commit -qm "upstream: add added.go, extend gone.go"

	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git checkout -q ours

	# Merge that correctly takes upstream's new file and correctly honors the
	# fork's deletion of gone.go. Must be clean.
	git checkout -q theirs -- added.go
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: take upstream's added.go, honor the fork's deletion of gone.go" >GOOD_SHA

	# Same merge, minus upstream's added.go. One file, nothing else changed.
	git rm -q --cached added.go
	rm -f added.go
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: (simulated) drop upstream's added.go" >BAD_SHA
)
upfile_rc_setup=$?

if [ "$upfile_rc_setup" -ne 0 ]; then
	record_fail "gate 3 file-level fixture builds" "setup failed, rc=$upfile_rc_setup"
else
	upfile_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-upfile-out.XXXXXX")

	(cd "$upfile_repo" && bash "$SCRIPT" "$(cat "$upfile_repo/GOOD_SHA")") >"$upfile_out" 2>&1
	upfile_good_rc=$?
	if [ "$upfile_good_rc" -eq 0 ] &&
		! grep -q "^  UPSTREAM-DROPPED-FILE" "$upfile_out" &&
		! grep -q "^  UPSTREAM-RESOLUTION-BUG" "$upfile_out" &&
		grep -q "^  UPSTREAM-FORK-DELETED UpstreamTouchedGone" "$upfile_out"; then
		record_pass "gate 3 honors a fork deletion (informational only) and exits 0"
	else
		record_fail "gate 3 honors a fork deletion (informational only) and exits 0" "exit=$upfile_good_rc:"
		sed 's/^/    /' "$upfile_out"
	fi

	(cd "$upfile_repo" && bash "$SCRIPT" "$(cat "$upfile_repo/BAD_SHA")") >"$upfile_out" 2>&1
	upfile_bad_rc=$?
	if [ "$upfile_bad_rc" -ne 0 ] &&
		grep -q "^  UPSTREAM-DROPPED-FILE	added.go" "$upfile_out"; then
		record_pass "gate 3 fails on an upstream-added file the merge dropped"
	else
		record_fail "gate 3 fails on an upstream-added file the merge dropped" "exit=$upfile_bad_rc:"
		sed 's/^/    /' "$upfile_out"
	fi

	rm -f "$upfile_out"
fi
rm -rf "$upfile_repo"

# ---------------------------------------------------------------------------
# 14. Gate 3 file-level UPSTREAM-TOOK-OURS — the shape NO declaration-level
#     check can see. Upstream MODIFIES an existing declaration rather than
#     adding one, so the declaration set does not change and Gate 3's
#     decl-level half reports a clean "0 missing" on a merge that took the
#     fork's blob verbatim and discarded upstream's edit outright. On the
#     real 2026-08-31 merge this is cmd/gc/dolt_cleanup_drop.go and
#     internal/doctor/checks_order_firing_bounded_test.go — invisible to
#     every other check in the script.
#
#     The same fixture carries the both-sides-identical negative control in
#     same.go: OURS and THEIRS converged on byte-identical content, so
#     merge == ours is the ONLY correct 3-way result and nothing was
#     discarded. It must produce no verdict at all, in either merge. A gate
#     that called that a took-ours would fire on every upstreamed fork patch
#     forever.
# ---------------------------------------------------------------------------
echo "== gate 3: merge takes the fork's blob verbatim (no decl-set change) =="

tookours_repo="$(mktemp -d "${TMPDIR:-/tmp}/crl-test-tookours.XXXXXX")"
(
	set -e
	cd "$tookours_repo"
	git init -q -b main
	cat >mod.go <<'EOF'
package fixture

func Existing() {}
EOF
	cat >same.go <<'EOF'
package fixture

func Same() {}
EOF
	git add -A
	git commit -qm base

	git checkout -q -b ours
	cat >mod.go <<'EOF'
package fixture

// fork: Existing is called from the fork's boot path.
func Existing() {}
EOF
	cat >same.go <<'EOF'
package fixture

// both sides converged on this exact line.
func Same() {}
EOF
	git add -A
	git commit -qm "fork: annotate Existing; converge same.go"

	git checkout -q -b theirs main
	cat >mod.go <<'EOF'
package fixture

// upstream: Existing now returns early when the pool is empty.
func Existing() {}
EOF
	cat >same.go <<'EOF'
package fixture

// both sides converged on this exact line.
func Same() {}
EOF
	git add -A
	git commit -qm "upstream: annotate Existing; converge same.go identically"

	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git checkout -q ours

	# Correct resolution: a real merge of the two annotations. Neither
	# parent's blob verbatim, so no file-level verdict in either direction.
	cat >mod.go <<'EOF'
package fixture

// fork: Existing is called from the fork's boot path.
// upstream: Existing now returns early when the pool is empty.
func Existing() {}
EOF
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: keep both annotations on Existing" >GOOD_SHA

	# The minimal regression: mod.go resolved --ours. Nothing else differs,
	# and no declaration name changed anywhere in the tree.
	git checkout -q ours -- mod.go
	git add -A
	git commit-tree "$(git write-tree)" -p "$ours_sha" -p "$theirs_sha" \
		-m "resync: (simulated --ours) take the fork's mod.go verbatim" >BAD_SHA
)
tookours_rc_setup=$?

if [ "$tookours_rc_setup" -ne 0 ]; then
	record_fail "gate 3 took-ours fixture builds" "setup failed, rc=$tookours_rc_setup"
else
	tookours_out=$(mktemp "${TMPDIR:-/tmp}/crl-test-tookours-out.XXXXXX")

	(cd "$tookours_repo" && bash "$SCRIPT" "$(cat "$tookours_repo/GOOD_SHA")") >"$tookours_out" 2>&1
	tookours_good_rc=$?
	if [ "$tookours_good_rc" -eq 0 ] &&
		! grep -qE "^  (UPSTREAM-)?(TOOK-OURS|TOOK-THEIRS|PURE-LOSS|UPSTREAM-PURE-LOSS)" "$tookours_out" &&
		! grep -q "same.go" "$tookours_out"; then
		record_pass "control: a real merge of both annotations exits 0, and an identical both-sides change is never a took-ours"
	else
		record_fail "control: a real merge of both annotations exits 0, and an identical both-sides change is never a took-ours" "exit=$tookours_good_rc:"
		sed 's/^/    /' "$tookours_out"
	fi

	(cd "$tookours_repo" && bash "$SCRIPT" "$(cat "$tookours_repo/BAD_SHA")") >"$tookours_out" 2>&1
	tookours_bad_rc=$?
	# The decl-level assertion is the point: 0 missing pairs in BOTH
	# directions, and the file-level check is the only thing that fires.
	if [ "$tookours_bad_rc" -ne 0 ] &&
		grep -q "^  UPSTREAM-TOOK-OURS	mod.go" "$tookours_out" &&
		grep -q "^Gate 2: 0 missing (decl, file) pair(s)" "$tookours_out" &&
		grep -q "0 UPSTREAM-RESOLUTION-BUG" "$tookours_out" &&
		! grep -q "same.go" "$tookours_out"; then
		record_pass "gate 3 fails on a took-ours file whose declaration set never changed"
	else
		record_fail "gate 3 fails on a took-ours file whose declaration set never changed" "exit=$tookours_bad_rc:"
		sed 's/^/    /' "$tookours_out"
	fi

	rm -f "$tookours_out"
fi
rm -rf "$tookours_repo"

# ---------------------------------------------------------------------------
# 15. Hook wiring: a real .githooks/pre-push, installed into a real temp repo
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

echo "-- refuses a push whose tip is a merge commit with UPSTREAM-side loss only --"
# The end-to-end proof for ga-8gpw4: a merge whose ONLY defect is an
# --ours-shaped resolution dropping an upstream-added declaration. Gates 1
# and 2 are clean on it by construction (the fork's own additions all
# survive, and no file matches upstream's blob), so before Gate 3 this push
# sailed through the hook. The declaration-level loss is invisible to
# `go build` and `go vet` for the reason recorded in check-resync-loss.sh's
# header: deleting a test never fails a build.
read -r hookG_remote hookG_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookG_work"
	git checkout -q -b ours
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func helperOne()   {}
func helperTwo()   {}
func helperThree() {}
func helperFour()  {}
func helperFive()  {}
func helperSix()   {}
EOF
	git add -A
	git commit -qm "fork: add shared_test.go"
	git checkout -q main
	git merge -q --ff-only ours
	git push -q origin main
	git checkout -q ours

	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func helperOne()   {}
func helperTwo()   {}
func helperThree() {}
func helperFour()  {}
func helperFive()  {}
func helperSix()   {}

func TestForkOnly(t *testing.T) {}
EOF
	git add -A
	git commit -qm "fork: add TestForkOnly at the end"

	git checkout -q -b theirs main
	cat >shared_test.go <<'EOF'
package fixture

func TestBase(t *testing.T) {}

func TestUpstreamOnly(t *testing.T) {}

func helperOne()   {}
func helperTwo()   {}
func helperThree() {}
func helperFour()  {}
func helperFive()  {}
func helperSix()   {}
EOF
	git add -A
	git commit -qm "upstream: add TestUpstreamOnly near the top"

	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git checkout -q ours
	# --ours-shaped resolution: the fork's blob verbatim. Every fork-added
	# declaration survives; upstream's TestUpstreamOnly does not.
	git checkout -q ours -- shared_test.go
	git add -A
	bad_tree=$(git write-tree)
	bad_merge=$(git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated --ours) drop TestUpstreamOnly")
	git reset -q --hard "$bad_merge"
)
hookG_setup_rc=$?
if [ "$hookG_setup_rc" -ne 0 ]; then
	record_fail "hook/blocks-push-with-upstream-side-loss" "fixture setup failed, rc=$hookG_setup_rc"
else
	hookG_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outG.XXXXXX")
	(cd "$hookG_work" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$hookG_out" 2>&1
	hookG_rc=$?
	if [ "$hookG_rc" -ne 0 ] &&
		[ -z "$(hook_remote_sha "$hookG_remote" "refs/heads/ours")" ] &&
		grep -q "^  UPSTREAM-RESOLUTION-BUG TestUpstreamOnly" "$hookG_out"; then
		record_pass "hook/blocks-push-with-upstream-side-loss (rejected, remote untouched)"
	else
		record_fail "hook/blocks-push-with-upstream-side-loss" "rc=$hookG_rc remote_sha=$(hook_remote_sha "$hookG_remote" "refs/heads/ours"):"
		sed 's/^/    /' "$hookG_out"
	fi
	rm -f "$hookG_out"
fi
rm -rf "$hookG_remote" "$hookG_work"

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

echo "-- RESYNC_LOSS_ACK=1 bypasses the resync-loss gate without --no-verify --"
read -r hookE_remote hookE_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookE_work"
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
hookE_setup_rc=$?
if [ "$hookE_setup_rc" -ne 0 ]; then
	record_fail "hook/resync-loss-ack-bypasses-gate" "fixture setup failed, rc=$hookE_setup_rc"
else
	hookE_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outE.XXXXXX")
	(cd "$hookE_work" && GIT_TERMINAL_PROMPT=0 RESYNC_LOSS_ACK=1 git push origin ours) >"$hookE_out" 2>&1
	hookE_rc=$?
	if [ "$hookE_rc" -eq 0 ] && [ -n "$(hook_remote_sha "$hookE_remote" "refs/heads/ours")" ]; then
		record_pass "hook/resync-loss-ack-bypasses-gate (push succeeded despite real loss)"
	else
		record_fail "hook/resync-loss-ack-bypasses-gate" "rc=$hookE_rc:"
		sed 's/^/    /' "$hookE_out"
	fi
	rm -f "$hookE_out"
fi
rm -rf "$hookE_remote" "$hookE_work"

echo "-- blocks a push whose tip is a normal commit stacked on top of a lossy merge --"
read -r hookF_remote hookF_work <<<"$(hook_setup_repo real)"
(
	set -e
	cd "$hookF_work"
	git checkout -q -b ours
	# A baseline commit, pushed FIRST, so the merge below lands via a
	# fast-forward update with a non-zero remote_sha — a brand-new branch's
	# first-ever push only checks whether its tip is a merge (see the "new
	# remote branch" comment in .githooks/pre-push); this test targets the
	# bounded `git rev-list "$remote_sha..$local_sha" --merges` scan an
	# UPDATE to an already-pushed branch goes through instead.
	echo "baseline" >baseline.txt
	git add -A
	git commit -qm "ours: baseline commit (no merge yet)"
	git push -q origin ours

	echo "# fork notes" >docs-notes.md
	git add -A
	git commit -qm "fork: add docs-notes.md"

	git checkout -q -b theirs main
	echo theirs >>f.txt
	git add -A
	git commit -qm "upstream: touch f.txt"

	git checkout -q ours
	git merge -q --no-edit theirs >/dev/null

	# Simulate a hand-resolved merge that dropped the fork-added file, THEN
	# a routine follow-up commit on top — the AGENTS.md-documented
	# `fix(resync)`/`chore(regen)` shape. The pushed TIP is not a merge;
	# only a range scan (git rev-list --merges) finds the loss underneath.
	ours_sha=$(git rev-parse ours)
	theirs_sha=$(git rev-parse theirs)
	git rm -q docs-notes.md
	bad_tree=$(git write-tree)
	bad_merge=$(git commit-tree "$bad_tree" -p "$ours_sha" -p "$theirs_sha" -m "resync: (simulated) drop docs-notes.md")
	git reset -q --hard "$bad_merge"
	echo "cleanup" >>f.txt
	git add -A
	git commit -qm "fix(resync): unrelated follow-up commit on top of the merge"
)
hookF_setup_rc=$?
hookF_baseline_sha=$(hook_remote_sha "$hookF_remote" "refs/heads/ours")
if [ "$hookF_setup_rc" -ne 0 ]; then
	record_fail "hook/blocks-follow-up-commit-on-lossy-merge" "fixture setup failed, rc=$hookF_setup_rc"
else
	hookF_out=$(mktemp "${TMPDIR:-/tmp}/crl-hook-outF.XXXXXX")
	(cd "$hookF_work" && GIT_TERMINAL_PROMPT=0 git push origin ours) >"$hookF_out" 2>&1
	hookF_rc=$?
	# Remote must stay at the pre-existing baseline commit pushed during
	# fixture setup, above — not empty (this branch already had one
	# legitimate push before the lossy update was attempted).
	if [ "$hookF_rc" -ne 0 ] && [ "$(hook_remote_sha "$hookF_remote" "refs/heads/ours")" = "$hookF_baseline_sha" ]; then
		record_pass "hook/blocks-follow-up-commit-on-lossy-merge (rejected, remote stayed at baseline)"
	else
		record_fail "hook/blocks-follow-up-commit-on-lossy-merge" "rc=$hookF_rc remote_sha=$(hook_remote_sha "$hookF_remote" "refs/heads/ours") baseline=$hookF_baseline_sha:"
		sed 's/^/    /' "$hookF_out"
	fi
	rm -f "$hookF_out"
fi
rm -rf "$hookF_remote" "$hookF_work"
rm -f "$poison_resync_script"

# ---------------------------------------------------------------------------
echo
echo "== summary: $pass passed, $fail failed, $skip skipped =="
[ "$fail" -eq 0 ]

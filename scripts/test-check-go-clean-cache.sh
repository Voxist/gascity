#!/usr/bin/env bash
#
# Self-test for scripts/check-go-clean-cache.sh.
#
# gocacheguard:allow-file  a test for a guard against the command is made of
#                          fixtures containing the command.
#
# Hermetic: every case builds a throwaway git repo under a temp dir. The real
# repository is scanned exactly once, at the end, to assert the guard's
# baseline is clean on this tree.
#
# What this guard is for, and what it is NOT for, is worth restating here
# because the distinction is easy to lose: it stops `go clean -cache` from
# entering the CODEBASE (a script, a Makefile recipe, a CI step, an order).
# It does NOT stop anyone from RUNNING it -- nobody committed the command in
# either the 2026-06-13 (vp-g96b) or 2026-09-05 incident; a process executed it.
# The runtime half of the enforcement is scripts/go-clean-cache-shim.sh.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
CHECK="$SCRIPT_DIR/check-go-clean-cache.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"

WORK="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok - $*"; }

OUT="$WORK/out"

# new_repo <name> -- a temp git repo with the guard runnable inside it.
new_repo() {
	local d="$WORK/$1"
	mkdir -p "$d"
	git -C "$d" init -q
	git -C "$d" config user.email t@example.com
	git -C "$d" config user.name t
	printf '%s' "$d"
}

# commit_file <repo> <path> <content>
commit_file() {
	local repo="$1" path="$2" content="$3"
	mkdir -p "$repo/$(dirname "$path")"
	printf '%s\n' "$content" >"$repo/$path"
	git -C "$repo" add -- "$path"
	git -C "$repo" -c commit.gpgsign=false commit -qm "add $path"
}

# run_check <repo> [args...] -- echoes the exit status; output lands in $OUT.
run_check() {
	local repo="$1"
	shift
	(cd "$repo" && bash "$CHECK" "$@") >"$OUT" 2>&1
	echo $?
}

# assert_blocks <label> <path> <content>
assert_blocks() {
	local label="$1" path="$2" content="$3" repo rc
	repo="$(new_repo "$(echo "$label" | tr -c 'a-zA-Z0-9' '_')")"
	commit_file "$repo" "$path" "$content"
	rc="$(run_check "$repo")"
	if [ "$rc" -eq 0 ]; then
		fail "$label: guard passed a violation in $path"
		return
	fi
	if ! grep -q -- "$path" "$OUT"; then
		fail "$label: guard blocked but did not name $path; output: $(cat "$OUT")"
		return
	fi
	pass "$label"
}

# assert_allows <label> <path> <content>
assert_allows() {
	local label="$1" path="$2" content="$3" repo rc
	repo="$(new_repo "$(echo "$label" | tr -c 'a-zA-Z0-9' '_')")"
	commit_file "$repo" "$path" "$content"
	rc="$(run_check "$repo")"
	if [ "$rc" -ne 0 ]; then
		fail "$label: guard blocked legitimate content in $path; output: $(cat "$OUT")"
		return
	fi
	pass "$label"
}

# ---------------------------------------------------------------- CASE 1
# The thing being guarded: an executable surface acquiring the banned command.
assert_blocks "CASE 1a: shell script" "scripts/wipe.sh" 'go clean -cache'
assert_blocks "CASE 1b: --cache spelling" "scripts/wipe.sh" 'go clean --cache'
assert_blocks "CASE 1c: combined with other flags" "scripts/wipe.sh" 'go clean -r -cache'
assert_blocks "CASE 1d: Makefile recipe" "Makefile" '	go clean -cache'
assert_blocks "CASE 1e: Makefile via $(GO)" "Makefile" '	$(GO) clean -cache'
assert_blocks "CASE 1f: CI workflow step" ".github/workflows/x.yml" '        run: go clean -cache'
assert_blocks "CASE 1g: git hook" ".githooks/pre-commit" 'go clean -cache'
assert_blocks "CASE 1h: order toml" "orders/x.toml" 'command = "go clean -cache"'
assert_blocks "CASE 1i: after a shell operator" "scripts/wipe.sh" 'true && go clean -cache'
assert_blocks "CASE 1j: relative invocation" "scripts/wipe.sh" './go clean -cache'
assert_blocks "CASE 1k: Go source exec" "internal/x/x.go" '	exec.Command("go", "clean", "-cache")'

# ---------------------------------------------------------------- CASE 2
# Sanctioned neighbours. Blocking any of these makes the guard noise, and a
# noisy guard gets bypassed.
assert_allows "CASE 2a: go clean -testcache is allowed by AGENTS.md" "scripts/x.sh" 'go clean -testcache'
assert_allows "CASE 2b: go clean -modcache" "scripts/x.sh" 'go clean -modcache'
assert_allows "CASE 2c: go clean -fuzzcache" "scripts/x.sh" 'go clean -fuzzcache'
assert_allows "CASE 2d: bare go clean" "scripts/x.sh" 'go clean ./...'
assert_allows "CASE 2e: cargo clean --cache is not go" "scripts/x.sh" 'cargo clean --cache'
assert_allows "CASE 2f: package path containing the text" "scripts/x.sh" 'go build ./cmd/go-clean-cache'
assert_allows "CASE 2g: -cache belonging to another tool" "scripts/x.sh" 'bazel clean -cache'

# ---------------------------------------------------------------- CASE 3
# Prose is out of scope BY CONSTRUCTION, not by exemption list. AGENTS.md, the
# engdocs handoffs and the release gates all state the ban by quoting it; a
# guard that fired on the rule text would be unusable. Markdown is not executed,
# so it is not part of the surface.
assert_allows "CASE 3a: markdown prose stating the ban" "docs/x.md" 'Never run `go clean -cache`.'
assert_allows "CASE 3b: AGENTS.md itself" "AGENTS.md" '**Hard ban: never run `go clean -cache`**'
assert_allows "CASE 3c: a .txt runbook" "notes/x.txt" 'do not run go clean -cache'
assert_allows "CASE 3d: testdata fixture" "internal/x/testdata/f.sh" 'go clean -cache'

# ---------------------------------------------------------------- CASE 4
# Comment lines on an executable surface. scripts/trim-go-build-cache.sh warns
# against the command in a header comment; that comment is the point of the
# file, and a comment cannot execute.
assert_allows "CASE 4a: shell comment" "scripts/x.sh" '# NEVER use "go clean -cache" for this.'
assert_allows "CASE 4b: indented shell comment" "scripts/x.sh" '	# go clean -cache is banned'
assert_allows "CASE 4c: Go line comment" "internal/x/x.go" '// go clean -cache corrupts the shared cache'
assert_allows "CASE 4d: Makefile comment" "Makefile" '# go clean -cache is banned'

# ---------------------------------------------------------------- CASE 5
# The explicit escape hatch, for a code line that must contain the string --
# a static guard searching for it, for instance. Deliberate and greppable.
assert_allows "CASE 5a: annotated code line" "scripts/x.sh" \
	"grep -q 'go clean -cache' \"\$f\" # gocacheguard:allow"
assert_blocks "CASE 5b: annotation on a different line does not carry over" \
	"scripts/x.sh" "$(printf 'true # gocacheguard:allow\ngo clean -cache')"

# ---------------------------------------------------------------- CASE 6
# Reporting: a violation must be locatable without re-grepping by hand, and
# must say how to resolve it.
repo="$(new_repo case6)"
commit_file "$repo" "scripts/a.sh" "$(printf '#!/bin/sh\ntrue\ngo clean -cache\n')"
rc="$(run_check "$repo")"
[ "$rc" -ne 0 ] || fail "CASE 6: violation not blocked"
grep -q 'scripts/a.sh:3' "$OUT" || fail "CASE 6: no file:line in the report; got: $(cat "$OUT")"
grep -q 'AGENTS.md' "$OUT" || fail "CASE 6: report does not cite AGENTS.md"
grep -q 'gocacheguard:allow' "$OUT" || fail "CASE 6: report does not name the escape hatch"
grep -q 'go clean -testcache' "$OUT" || fail "CASE 6: report does not name the allowed alternative"
pass "CASE 6: report gives file:line, the rule, the alternative and the escape hatch"

# ---------------------------------------------------------------- CASE 7
# Multiple violations are all reported, not just the first -- otherwise fixing
# them is a serial game of whack-a-mole across pre-commit runs.
repo="$(new_repo case7)"
commit_file "$repo" "scripts/a.sh" 'go clean -cache'
commit_file "$repo" "scripts/b.sh" 'go clean --cache'
rc="$(run_check "$repo")"
[ "$rc" -ne 0 ] || fail "CASE 7: violations not blocked"
grep -q 'scripts/a.sh' "$OUT" && grep -q 'scripts/b.sh' "$OUT" \
	|| fail "CASE 7: not every violation was reported; got: $(cat "$OUT")"
pass "CASE 7: every violation is reported in one pass"

# ---------------------------------------------------------------- CASE 8
# --staged scans the index, which is what pre-commit needs: a dirty working
# tree elsewhere in the repo must not block an unrelated commit, and a staged
# violation must.
repo="$(new_repo case8)"
commit_file "$repo" "scripts/ok.sh" 'true'
printf 'go clean -cache\n' >"$repo/scripts/dirty.sh"
rc="$(run_check "$repo" --staged)"
[ "$rc" -eq 0 ] || fail "CASE 8: --staged blocked on an unstaged/untracked file; got: $(cat "$OUT")"
pass "CASE 8: --staged ignores unstaged work"

git -C "$repo" add -- scripts/dirty.sh
rc="$(run_check "$repo" --staged)"
[ "$rc" -ne 0 ] || fail "CASE 8: --staged passed a staged violation"
grep -q 'scripts/dirty.sh' "$OUT" || fail "CASE 8: --staged did not name the file"
pass "CASE 8: --staged blocks a staged violation"

# --staged scans the STAGED CONTENT, not the worktree copy. Staging a clean
# version and then re-dirtying the file must not block the commit that is
# actually being made.
repo="$(new_repo case8b)"
commit_file "$repo" "scripts/a.sh" 'true'
printf 'true\n' >"$repo/scripts/a.sh"
git -C "$repo" add -- scripts/a.sh
printf 'go clean -cache\n' >"$repo/scripts/a.sh"
rc="$(run_check "$repo" --staged)"
[ "$rc" -eq 0 ] || fail "CASE 8: --staged read the worktree instead of the index; got: $(cat "$OUT")"
pass "CASE 8: --staged reads the index, not the worktree"

# A staged DELETION must not be scanned as content.
repo="$(new_repo case8c)"
commit_file "$repo" "scripts/a.sh" 'true'
git -C "$repo" rm -q -- scripts/a.sh
rc="$(run_check "$repo" --staged)"
[ "$rc" -eq 0 ] || fail "CASE 8: --staged choked on a staged deletion; got: $(cat "$OUT")"
pass "CASE 8: --staged tolerates a staged deletion"

# ---------------------------------------------------------------- CASE 9
# Fail closed. A guard that reports success when it could not evaluate
# manufactures exactly the false confidence it exists to remove.
notrepo="$WORK/notrepo"
mkdir -p "$notrepo"
rc="$(run_check "$notrepo")"
[ "$rc" -ne 0 ] || fail "CASE 9: guard passed outside a git work tree"
pass "CASE 9: outside a git work tree the guard fails closed"

# ---------------------------------------------------------------- CASE 10
# A tracked path containing whitespace survives the scan rather than being
# split into two nonexistent paths (which would silently skip it).
repo="$(new_repo case10)"
commit_file "$repo" "scripts/a b.sh" 'go clean -cache'
rc="$(run_check "$repo")"
[ "$rc" -ne 0 ] || fail "CASE 10: a path with whitespace was skipped"
pass "CASE 10: a tracked path containing whitespace is still scanned"

# ---------------------------------------------------------------- CASE 11
# Static guarantees about the guard itself.
if grep -nE '^[[:space:]]*(export[[:space:]]+)?(GOCACHE|TMPDIR|GOTMPDIR)=' "$CHECK" | grep -q .; then
	fail "CASE 11: guard sets GOCACHE/TMPDIR/GOTMPDIR"
fi
if grep -nE '^[[:space:]]*exec[[:space:]]' "$CHECK" | grep -q .; then
	fail "CASE 11: guard exec's something"
fi
pass "CASE 11: guard sets no cache env and exec's nothing"

# ---------------------------------------------------------------- CASE 12
# The baseline: this repository must pass its own guard.
rc="$(run_check "$REPO_ROOT")"
[ "$rc" -eq 0 ] || fail "CASE 12: this repository does not pass the guard: $(cat "$OUT")"
pass "CASE 12: the real repository passes the guard"

# ---------------------------------------------------------------- CASE 13
# Wiring. A guard nothing invokes is documentation with a shebang.
HOOK="$REPO_ROOT/.githooks/pre-commit"
grep -q 'check-go-clean-cache.sh' "$HOOK" || fail "CASE 13: pre-commit does not invoke the guard"
grep -q -- '--staged' "$HOOK" || fail "CASE 13: pre-commit does not use --staged"
grep -q 'check-go-clean-cache' "$REPO_ROOT/Makefile" || fail "CASE 13: no Makefile target"
grep -q 'check-go-clean-cache' "$REPO_ROOT/.github/workflows/ci.yml" || fail "CASE 13: not wired into CI"

# It must run BEFORE pre-commit's four-category early exit. A shell script, a
# Makefile recipe or a CI step is in none of those categories, so a guard
# placed after the exit would never see the files most at risk.
guard_line="$(grep -n 'check-go-clean-cache.sh' "$HOOK" | head -1 | cut -d: -f1)"
exit_line="$(grep -n 'exit 0' "$HOOK" | head -1 | cut -d: -f1)"
if [ -z "$guard_line" ] || [ -z "$exit_line" ] || [ "$guard_line" -ge "$exit_line" ]; then
	fail "CASE 13: guard (line ${guard_line:-?}) does not precede pre-commit's early exit (line ${exit_line:-?})"
else
	pass "CASE 13: guard runs before pre-commit's four-category early exit"
fi
pass "CASE 13: wired into pre-commit, the Makefile and CI"

# ---------------------------------------------------------------- CASE 14
# End to end through the real hook: staging a violation in a file type that
# pre-commit's four categories do not cover must still block the commit. This
# is the case the wiring exists for, and the one a guard placed after the early
# exit would silently pass.
repo="$(new_repo case14)"
commit_file "$repo" "README.dummy" 'seed'
printf 'go clean -cache
' >"$repo/wipe.sh"
git -C "$repo" add -- wipe.sh
if (cd "$repo" && bash "$REPO_ROOT/.githooks/pre-commit") >"$OUT" 2>&1; then
	fail "CASE 14: pre-commit passed a staged shell script containing the ban"
else
	grep -q 'wipe.sh' "$OUT" || fail "CASE 14: hook blocked but did not name the file; got: $(cat "$OUT")"
	pass "CASE 14: pre-commit blocks a staged .sh violation end to end"
fi

# ...and a commit that stages nothing relevant still passes the guard, so the
# hook is not a blanket tax on every commit.
repo="$(new_repo case14b)"
commit_file "$repo" "README.dummy" 'seed'
printf 'echo hi
' >"$repo/ok.sh"
git -C "$repo" add -- ok.sh
rc="$( (cd "$repo" && bash "$REPO_ROOT/.githooks/pre-commit") >"$OUT" 2>&1; echo $?)"
[ "$rc" -eq 0 ] || fail "CASE 14: pre-commit blocked a clean commit; got: $(cat "$OUT")"
pass "CASE 14: a clean commit passes the hook"

# ---------------------------------------------------------------- CASE 15
# MULTI-LINE FORMS. The scanner originally matched within a single physical
# line, so a logical command split across lines was invisible -- including the
# form `gofmt` PRODUCES the moment an exec.Command call exceeds the line limit.
# A guard defeated by ordinary formatting is worse than one defeated by
# evasion: the miss correlates with routine maintenance rather than with
# somebody trying, so it manufactures false confidence exactly when the
# codebase is being looked after.
assert_blocks "CASE 15a: gofmt-wrapped exec.Command" "internal/x/x.go" \
	"$(printf 'exec.Command("go",\n\t"clean",\n\t"-cache")')"
assert_blocks "CASE 15b: backslash continuation in a shell script" "scripts/x.sh" \
	"$(printf 'go clean \\\n  -cache')"
assert_blocks "CASE 15c: backslash continuation in a Makefile recipe" "Makefile" \
	"$(printf '\tgo clean \\\n\t  -cache')"
assert_blocks "CASE 15d: double-quoted flag" "scripts/x.sh" 'go clean "-cache"'
assert_blocks "CASE 15e: single-quoted flag" "scripts/x.sh" "go clean '-cache'"
assert_blocks "CASE 15f: continuation split before the flag" "scripts/x.sh" \
	"$(printf 'go clean -r \\\n  -x \\\n  -cache')"

# The same joining must not start blocking legitimate multi-line commands.
assert_allows "CASE 15g: continuation ending in -testcache" "scripts/x.sh" \
	"$(printf 'go clean \\\n  -testcache')"
assert_allows "CASE 15h: gofmt-wrapped exec.Command for -testcache" "internal/x/x.go" \
	"$(printf 'exec.Command("go",\n\t"clean",\n\t"-testcache")')"
assert_allows "CASE 15i: a continuation that merely mentions a package path" "scripts/x.sh" \
	"$(printf 'go build \\\n  ./cmd/go-clean-cache')"

# A comment leading the logical line still exempts the whole of it, and the
# marker may be placed on any physical line of it.
assert_allows "CASE 15j: comment leading a continuation" "scripts/x.sh" \
	"$(printf '# go clean \\\n#   -cache')"
assert_allows "CASE 15k: marker on the last physical line" "scripts/x.sh" \
	"$(printf 'go clean \\\n  -cache # gocacheguard:allow')"

# Reporting must point at the FIRST physical line of the logical command, so
# the file:line is where a reader actually finds it.
repo="$(new_repo case15report)"
commit_file "$repo" "scripts/a.sh" "$(printf '#!/bin/sh\ntrue\ngo clean \\\n  -cache\n')"
rc="$(run_check "$repo")"
[ "$rc" -ne 0 ] || fail "CASE 15: continuation not blocked in the reporting probe"
grep -q 'scripts/a.sh:3' "$OUT" \
	|| fail "CASE 15: continuation reported at the wrong line; got: $(cat "$OUT")"
pass "CASE 15: a continuation is reported at its first physical line"

repo="$(new_repo case15goreport)"
commit_file "$repo" "internal/x/x.go" "$(printf 'package x\n\nvar _ = 1\nvar c = exec.Command("go",\n\t"clean",\n\t"-cache")\n')"
rc="$(run_check "$repo")"
[ "$rc" -ne 0 ] || fail "CASE 15: wrapped exec.Command not blocked in the reporting probe"
grep -q 'internal/x/x.go:4' "$OUT" \
	|| fail "CASE 15: wrapped exec.Command reported at the wrong line; got: $(cat "$OUT")"
pass "CASE 15: a wrapped exec.Command is reported at its first physical line"

# ------------------------------------------------------------------ verdict
if [ "$failures" -ne 0 ]; then
	echo "FAILED: $failures case(s)" >&2
	exit 1
fi
echo "all check-go-clean-cache cases passed"

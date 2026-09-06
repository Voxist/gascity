#!/usr/bin/env bash
#
# Self-test for scripts/go-clean-cache-shim.sh and
# scripts/install-go-clean-cache-shim.sh.
#
# gocacheguard:allow-file  a test for a guard against the command is made of
#                          fixtures containing the command.
#
# Hermetic and, above all, SAFE: every case drives the shim against a FAKE go
# on a temp PATH. The real Go toolchain is never invoked, and `go clean -cache`
# is never actually executed -- which is the entire point of the thing under
# test. The fake go records its argv, its stdin, its own pid and its exit
# status so the passthrough can be asserted precisely.
#
# The passthrough cases matter more than the refusal cases. A shim installed
# ahead of the real go on PATH intercepts EVERY Go build on the host; a bug in
# the 99.99% of invocations it is supposed to wave through is far worse than
# the wipe it prevents. So: argv fidelity, exit-status fidelity, stdin/stdout/
# stderr fidelity, and that the passthrough really is an exec (no wrapper
# process left in the middle to eat a signal) are all pinned here.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SHIM_SRC="$SCRIPT_DIR/go-clean-cache-shim.sh"
INSTALLER="$SCRIPT_DIR/install-go-clean-cache-shim.sh"

# Canonicalised: on macOS mktemp -d hands back a /var/... symlink into
# /private/var, and the installer bakes the resolved path, so the expected and
# actual real-go paths would otherwise differ by that symlink alone.
WORK="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok - $*"; }

# ---------------------------------------------------------------- fixtures

FAKE_GO="$WORK/fakego/go"
mkdir -p "$WORK/fakego"
cat >"$FAKE_GO" <<'FAKE'
#!/usr/bin/env bash
# Fake go. Records how it was called so the shim's passthrough can be asserted.
# It never touches a build cache.
: >"$FAKE_GO_ARGV"
for a in "$@"; do printf '%s\n' "$a" >>"$FAKE_GO_ARGV"; done
printf '%s' "$$" >"$FAKE_GO_PID"
if [ -n "${FAKE_GO_READ_STDIN:-}" ]; then cat >"$FAKE_GO_STDIN"; fi
printf 'fake-go-stdout\n'
printf 'fake-go-stderr\n' >&2
exit "${FAKE_GO_EXIT:-0}"
FAKE
chmod 755 "$FAKE_GO"

ARGV="$WORK/argv"
PIDFILE="$WORK/pid"
STDINFILE="$WORK/stdin"

# run_shim <expect-exit> -- <args...>
# Invokes the shim with the fake go pinned, capturing stdout/stderr apart.
OUT="$WORK/out"
ERR="$WORK/err"
run_shim() {
	rm -f "$ARGV" "$PIDFILE" "$STDINFILE"
	GC_GO_SHIM_REAL_GO="$FAKE_GO" \
		FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" FAKE_GO_STDIN="$STDINFILE" \
		FAKE_GO_EXIT="${FAKE_GO_EXIT:-0}" FAKE_GO_READ_STDIN="${FAKE_GO_READ_STDIN:-}" \
		GC_ALLOW_GO_CLEAN_CACHE="${GC_ALLOW_GO_CLEAN_CACHE:-}" \
		bash "$SHIM_SRC" "$@" >"$OUT" 2>"$ERR"
	echo $?
}

argv_joined() { [ -f "$ARGV" ] && tr '\n' '|' <"$ARGV" || printf '<not-invoked>'; }

# assert_allowed <label> <args...> -- the shim must exec the fake go with argv
# passed through byte for byte.
assert_allowed() {
	local label="$1"
	shift
	local rc
	rc="$(run_shim "$@")"
	if [ "$rc" -ne 0 ]; then
		fail "$label: shim exited $rc (expected 0); stderr: $(cat "$ERR")"
		return
	fi
	if [ ! -f "$ARGV" ]; then
		fail "$label: real go was never invoked"
		return
	fi
	local want got a
	want=""
	for a in "$@"; do want="$want$a|"; done
	got="$(argv_joined)"
	if [ "$want" != "$got" ]; then
		fail "$label: argv not passed through verbatim; want [$want] got [$got]"
		return
	fi
	pass "$label: passed through verbatim"
}

# assert_refused <label> <args...> -- the shim must refuse without invoking go.
assert_refused() {
	local label="$1"
	shift
	local rc
	rc="$(run_shim "$@")"
	if [ "$rc" -eq 0 ]; then
		fail "$label: shim exited 0 (expected non-zero refusal)"
		return
	fi
	if [ -f "$ARGV" ]; then
		fail "$label: refusal still invoked the real go with [$(argv_joined)]"
		return
	fi
	if ! grep -q 'REFUSED' "$ERR"; then
		fail "$label: refusal did not print a REFUSED message; stderr: $(cat "$ERR")"
		return
	fi
	pass "$label: refused, real go never invoked"
}

# ---------------------------------------------------------------- CASE 1
# The banned operation, in every spelling cmd/go accepts.
assert_refused "CASE 1a: go clean -cache" clean -cache
assert_refused "CASE 1b: go clean --cache" clean --cache
assert_refused "CASE 1c: -cache combined with an allowed flag" clean -cache -testcache
assert_refused "CASE 1d: -cache after another flag" clean -r -cache
assert_refused "CASE 1e: -cache before another flag" clean -cache -r
assert_refused "CASE 1f: -cache=true" clean -cache=true
assert_refused "CASE 1g: -cache=1" clean -cache=1
assert_refused "CASE 1h: go -C dir clean -cache" -C "$WORK" clean -cache
assert_refused "CASE 1i: -x -cache with packages" clean -x -cache ./...

# ---------------------------------------------------------------- CASE 2
# Everything else about `go clean` is allowed and must pass through untouched.
# `-testcache` is explicitly sanctioned by AGENTS.md; blocking it would be a
# false positive that gets this shim deleted.
assert_allowed "CASE 2a: go clean -testcache" clean -testcache
assert_allowed "CASE 2b: go clean -modcache" clean -modcache
assert_allowed "CASE 2c: go clean -fuzzcache" clean -fuzzcache
assert_allowed "CASE 2d: bare go clean" clean
assert_allowed "CASE 2e: go clean -i -r ./..." clean -i -r ./...
assert_allowed "CASE 2f: go clean -cache=false" clean -cache=false
assert_allowed "CASE 2g: go -C dir clean -testcache" -C "$WORK" clean -testcache

# ---------------------------------------------------------------- CASE 3
# Substring false positives. The refusal is a parse of the argument list, not a
# grep of the command line, so a package path or a test filter that happens to
# contain the text must sail through.
assert_allowed "CASE 3a: package path containing the text" build ./cmd/go-clean-cache
assert_allowed "CASE 3b: -run filter containing the text" test -run 'go clean -cache' ./...
assert_allowed "CASE 3c: -ldflags containing the text" build -ldflags=-X=main.note=go_clean_-cache ./...
assert_allowed "CASE 3d: a non-clean subcommand named later" list -m all
assert_allowed "CASE 3e: -cache after the package list ends" clean ./... -cache

# ---------------------------------------------------------------- CASE 4
# Ordinary builds -- the overwhelming majority of what a PATH shim intercepts.
assert_allowed "CASE 4a: go build ./..." build ./...
assert_allowed "CASE 4b: go test with flags" test -count=1 -race ./internal/...
assert_allowed "CASE 4c: go version" version
assert_allowed "CASE 4d: no arguments at all" 
assert_allowed "CASE 4e: argument containing spaces" test "-run=Foo Bar" ./...
assert_allowed "CASE 4f: empty-string argument" build "" ./...

# ---------------------------------------------------------------- CASE 5
# Exit-status fidelity. A shim that normalises a failing build to 0 (or to 1)
# silently breaks every CI gate downstream of it.
for code in 0 1 2 7 42 125; do
	rc="$(FAKE_GO_EXIT="$code" run_shim build ./...)"
	[ "$rc" -eq "$code" ] || fail "CASE 5: exit status $code became $rc"
done
pass "CASE 5: exit status is passed through unchanged (0 1 2 7 42 125)"

# ---------------------------------------------------------------- CASE 6
# stdout and stderr stay separate and unbuffered-through.
rc="$(run_shim build ./...)"
[ "$rc" -eq 0 ] || fail "CASE 6: unexpected exit $rc"
grep -qx 'fake-go-stdout' "$OUT" || fail "CASE 6: stdout not passed through (got: $(cat "$OUT"))"
grep -qx 'fake-go-stderr' "$ERR" || fail "CASE 6: stderr not passed through (got: $(cat "$ERR"))"
grep -q 'fake-go-stderr' "$OUT" && fail "CASE 6: stderr leaked into stdout"
pass "CASE 6: stdout and stderr pass through on their own streams"

# ---------------------------------------------------------------- CASE 7
# stdin reaches the real go. `go run -` and `gofmt`-style pipes depend on it.
rm -f "$ARGV" "$PIDFILE" "$STDINFILE"
printf 'package main\n' | GC_GO_SHIM_REAL_GO="$FAKE_GO" \
	FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" FAKE_GO_STDIN="$STDINFILE" \
	FAKE_GO_READ_STDIN=1 bash "$SHIM_SRC" run - >/dev/null 2>&1
if [ ! -f "$STDINFILE" ] || ! grep -qx 'package main' "$STDINFILE"; then
	fail "CASE 7: stdin was not passed through to the real go"
else
	pass "CASE 7: stdin is passed through to the real go"
fi

# ---------------------------------------------------------------- CASE 8
# The passthrough is a real exec, not a fork-and-wait. An intermediate process
# would still be sitting between the caller and the compiler, where it can eat
# a signal or add a process to every build on the host.
#
# Demonstrated rather than asserted from the source: the shim is started in the
# background, its pid recorded by the shell, and the fake go writes its OWN pid.
# They are equal only if the shim's process image was replaced.
rm -f "$ARGV" "$PIDFILE"
GC_GO_SHIM_REAL_GO="$FAKE_GO" FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" \
	bash "$SHIM_SRC" build ./... >/dev/null 2>&1 &
shim_pid=$!
wait "$shim_pid"
if [ ! -f "$PIDFILE" ]; then
	fail "CASE 8: fake go never ran"
elif [ "$(cat "$PIDFILE")" != "$shim_pid" ]; then
	fail "CASE 8: passthrough forked instead of exec'ing (shim pid $shim_pid, go pid $(cat "$PIDFILE"))"
else
	pass "CASE 8: passthrough replaces the process image (exec, not fork)"
fi

# ---------------------------------------------------------------- CASE 9
# The documented escape hatch. A guard with no override gets deleted the first
# time somebody genuinely needs the operation; it must be explicit, and it must
# still say out loud what it just let through.
rc="$(GC_ALLOW_GO_CLEAN_CACHE=1 run_shim clean -cache)"
if [ "$rc" -ne 0 ]; then
	fail "CASE 9: override did not allow the operation (exit $rc)"
elif [ "$(argv_joined)" != "clean|-cache|" ]; then
	fail "CASE 9: override did not pass the original argv through: [$(argv_joined)]"
elif ! grep -q 'GC_ALLOW_GO_CLEAN_CACHE' "$ERR"; then
	fail "CASE 9: override was silent; it must announce itself on stderr"
else
	pass "CASE 9: GC_ALLOW_GO_CLEAN_CACHE=1 allows the operation, loudly"
fi
rc="$(GC_ALLOW_GO_CLEAN_CACHE=0 run_shim clean -cache)"
[ "$rc" -ne 0 ] || fail "CASE 9: GC_ALLOW_GO_CLEAN_CACHE=0 must NOT be an override"
pass "CASE 9: only a truthy override counts"

# ---------------------------------------------------------------- CASE 10
# The refusal must be actionable, not just a wall. It names the rule, the
# incident, the sanctioned alternative, and the override.
run_shim clean -cache >/dev/null
for want in 'AGENTS.md' 'vp-g96b' 'go clean -testcache' 'GC_ALLOW_GO_CLEAN_CACHE'; do
	grep -q -- "$want" "$ERR" || fail "CASE 10: refusal message does not mention '$want'"
done
pass "CASE 10: refusal cites the rule, the incident, the alternative and the override"

# ---------------------------------------------------------------- CASE 11
# Misconfiguration fails LOUD, never open. A shim that cannot find the real go
# and quietly exits 0 turns every build on the host into a silent no-op, which
# is a worse outage than the one it prevents.
rc=$(GC_GO_SHIM_REAL_GO="$WORK/definitely-not-here" bash "$SHIM_SRC" build ./... >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -ne 0 ] || fail "CASE 11: unresolvable real go exited 0"
grep -q 'go-clean-cache-shim' "$ERR" || fail "CASE 11: unresolvable real go did not identify the shim"
pass "CASE 11: an unresolvable real go fails loudly"

# The unconfigured shim (still carrying its install-time placeholder) must also
# fail rather than guess.
rc=$(env -u GC_GO_SHIM_REAL_GO PATH="$WORK/fakego:$PATH" bash "$SHIM_SRC" build ./... >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -ne 0 ] || fail "CASE 11: uninstalled shim (placeholder REAL_GO) exited 0"
pass "CASE 11: an uninstalled shim does not fall back to a PATH search"

# ---------------------------------------------------------------- CASE 12
# A shim pointed at itself must die, not recurse until the process table gives
# out.
rc=$(GC_GO_SHIM_REAL_GO="$SHIM_SRC" bash "$SHIM_SRC" build ./... >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -ne 0 ] || fail "CASE 12: self-referential shim exited 0"
grep -qi 'itself' "$ERR" || fail "CASE 12: self-reference was not diagnosed; stderr: $(cat "$ERR")"
pass "CASE 12: a shim pointed at itself refuses instead of recursing"

# ---------------------------------------------------------------- CASE 13
# Static guarantees about the shim source itself.
grep -q 'exec "\$REAL_GO"' "$SHIM_SRC" || fail "CASE 13: shim does not exec the real go"
if grep -nE '^[[:space:]]*(export[[:space:]]+)?(GOCACHE|TMPDIR|GOTMPDIR)=' "$SHIM_SRC" | grep -q .; then
	fail "CASE 13: shim sets GOCACHE/TMPDIR/GOTMPDIR"
fi
# Exactly one command is ever executed, and it is the passthrough. Anything
# else executing from inside a PATH shim is a surprise on every build.
execs="$(grep -cE '^[[:space:]]*exec[[:space:]]' "$SHIM_SRC")"
[ "$execs" -eq 1 ] || fail "CASE 13: shim has $execs exec lines, expected exactly 1"
grep -qE '^exec "\$REAL_GO" "\$@"$' "$SHIM_SRC" \
	|| fail "CASE 13: the single exec is not the verbatim passthrough"
pass "CASE 13: shim exec's exactly once, sets no cache env, and that exec is the passthrough"

# ---------------------------------------------------------------- CASE 14
# Installer: refuses a directory that does not precede the real go on PATH.
# This is the trap on this host -- ~/.gc/bin is LAST in the operator's PATH,
# behind /opt/homebrew/bin, so a shim installed there is shadowed by the real
# go and silently guards nothing.
shadowed="$WORK/shadowed"
mkdir -p "$shadowed"
rc=$(PATH="$WORK/fakego:$shadowed:$PATH" GC_GO_SHIM_REAL_GO="$FAKE_GO" \
	bash "$INSTALLER" --dir "$shadowed" >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -ne 0 ] || fail "CASE 14: installer accepted a PATH-shadowed directory"
[ ! -e "$shadowed/go" ] || fail "CASE 14: installer wrote a shim into a shadowed directory"
grep -qi 'shadow' "$ERR" || fail "CASE 14: installer did not explain the shadowing; stderr: $(cat "$ERR")"
pass "CASE 14: installer refuses a directory shadowed by the real go"

# ---------------------------------------------------------------- CASE 15
# Installer: happy path. Installs ahead of the real go, bakes the resolved
# absolute path in, and the installed shim then refuses the ban and passes
# everything else through.
bindir="$WORK/bin"
mkdir -p "$bindir"
rc=$(PATH="$bindir:$WORK/fakego:$PATH" bash "$INSTALLER" --dir "$bindir" >"$OUT" 2>"$ERR"; echo $?)
if [ "$rc" -ne 0 ]; then
	fail "CASE 15: installer failed: $(cat "$ERR")"
elif [ ! -x "$bindir/go" ]; then
	fail "CASE 15: installer did not produce an executable shim"
else
	grep -q "REAL_GO_PINNED='$FAKE_GO'" "$bindir/go" \
		|| fail "CASE 15: installer did not bake the resolved real go path in"
	rm -f "$ARGV"
	FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" "$bindir/go" build ./... >/dev/null 2>&1
	[ -f "$ARGV" ] || fail "CASE 15: installed shim did not reach the real go"
	rm -f "$ARGV"
	if FAKE_GO_ARGV="$ARGV" "$bindir/go" clean -cache >/dev/null 2>&1; then
		fail "CASE 15: installed shim allowed go clean -cache"
	fi
	[ ! -f "$ARGV" ] || fail "CASE 15: installed shim ran the real go for a refused command"
	pass "CASE 15: installed shim guards the ban and passes builds through"
fi

# ---------------------------------------------------------------- CASE 16
# Installer: idempotent, and --uninstall removes only a shim.
rc=$(PATH="$bindir:$WORK/fakego:$PATH" bash "$INSTALLER" --dir "$bindir" >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -eq 0 ] || fail "CASE 16: re-install over an existing shim failed: $(cat "$ERR")"
[ -x "$bindir/go" ] || fail "CASE 16: re-install removed the shim"
pass "CASE 16: install is idempotent"

rc=$(bash "$INSTALLER" --dir "$bindir" --uninstall >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -eq 0 ] || fail "CASE 16: uninstall failed: $(cat "$ERR")"
[ ! -e "$bindir/go" ] || fail "CASE 16: uninstall left the shim in place"
pass "CASE 16: uninstall removes the shim"

# The load-bearing one: uninstall must never delete a REAL go. If the operator
# points it at the wrong directory, the worst case has to be a no-op.
notashim="$WORK/notashim"
mkdir -p "$notashim"
cp -f "$FAKE_GO" "$notashim/go"
rc=$(bash "$INSTALLER" --dir "$notashim" --uninstall >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -ne 0 ] || fail "CASE 16: uninstall deleted a non-shim go and reported success"
[ -x "$notashim/go" ] || fail "CASE 16: uninstall DELETED A REAL go binary"
pass "CASE 16: uninstall refuses to delete anything that is not this shim"

# Uninstalling when nothing is installed is a clean no-op, not an error.
empty="$WORK/emptybin"
mkdir -p "$empty"
rc=$(bash "$INSTALLER" --dir "$empty" --uninstall >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -eq 0 ] || fail "CASE 16: uninstall with nothing installed returned $rc"
pass "CASE 16: uninstall with nothing installed is a no-op"

# ---------------------------------------------------------------- CASE 17
# Installer: --dry-run writes nothing.
dry="$WORK/drybin"
mkdir -p "$dry"
rc=$(PATH="$dry:$WORK/fakego:$PATH" bash "$INSTALLER" --dir "$dry" --dry-run >"$OUT" 2>"$ERR"; echo $?)
[ "$rc" -eq 0 ] || fail "CASE 17: --dry-run failed: $(cat "$ERR")"
[ ! -e "$dry/go" ] || fail "CASE 17: --dry-run installed a shim"
pass "CASE 17: --dry-run reports without installing"

# ---------------------------------------------------------------- CASE 18
# A second shim, installed into a directory ahead of an existing one, must be
# pinned at the REAL go -- never at the other shim. Chaining shims would make
# the refusal depend on which one PATH happens to reach first, and would leave
# a wrapper process in front of every build for each link in the chain.
chain="$WORK/chainbin"
mkdir -p "$chain"
PATH="$bindir:$WORK/fakego:$PATH" bash "$INSTALLER" --dir "$bindir" >/dev/null 2>&1
rc=$(PATH="$chain:$bindir:$WORK/fakego:$PATH" bash "$INSTALLER" --dir "$chain" >"$OUT" 2>"$ERR"; echo $?)
if [ "$rc" -ne 0 ]; then
	fail "CASE 18: installer failed with another shim on PATH: $(cat "$ERR")"
elif grep -q "REAL_GO_PINNED='$bindir/go'" "$chain/go"; then
	fail "CASE 18: installer pinned one shim at another"
elif ! grep -q "REAL_GO_PINNED='$FAKE_GO'" "$chain/go"; then
	fail "CASE 18: installer did not resolve past the existing shim to the real go"
else
	pass "CASE 18: installer resolves past an existing shim to the real go"
fi

# ---------------------------------------------------------------- CASE 19
# The installer's own post-install refusal probe must not be able to reach the
# real toolchain. It asks a just-installed, not-yet-trusted shim to decide
# whether to wipe the shared cache; if the refusal logic were broken in exactly
# the way the probe exists to detect, a probe wired to the real go would perform
# the wipe ITSELF and cause the incident the shim exists to prevent.
# Matched on the joined text so the assertion survives the line wrap, and stated
# as "the probe carries the no-op pin" rather than as one exact spelling.
tr '\n' ' ' <"$INSTALLER" \
	| grep -qE 'GC_GO_SHIM_REAL_GO=/usr/bin/true[^"]{0,12}"\$TARGET" clean -cache' \
	|| fail "CASE 19: installer's refusal probe is wired to the real toolchain"

# The probe must also not inherit the documented escape hatch: exported (which
# it may well be, in the shell someone used to wipe the cache before installing)
# it makes a WORKING shim pass the ban through and the probe then condemns a
# good install.
tr '\n' ' ' <"$INSTALLER" \
	| grep -qE 'env -u GC_ALLOW_GO_CLEAN_CACHE[^"]{0,60}"\$TARGET" clean -cache' \
	|| fail "CASE 19b: installer's refusal probe inherits GC_ALLOW_GO_CLEAN_CACHE"

# Demonstrated, not merely read. A sabotaged shim SOURCE is rendered by a copy
# of the real installer; the installer must reject the result, and the fake go
# must show that the last thing it saw was the harmless `version` probe -- never
# `clean -cache`.
src="$WORK/sabotage-src"
mkdir -p "$src"
cp -f "$INSTALLER" "$src/"
sed 's/^if refuses_clean_cache "\$@"; then$/if false; then/' "$SHIM_SRC" >"$src/go-clean-cache-shim.sh"
grep -q '^if false; then$' "$src/go-clean-cache-shim.sh" \
	|| fail "CASE 19: could not sabotage the shim source for the test"

sabbin="$WORK/sabotagebin"
mkdir -p "$sabbin"
rm -f "$ARGV"
if FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" PATH="$sabbin:$WORK/fakego:$PATH" \
	bash "$src/install-go-clean-cache-shim.sh" --dir "$sabbin" >"$OUT" 2>"$ERR"; then
	fail "CASE 19: installer reported success over a shim that does not refuse"
elif [ "$(argv_joined)" = "clean|-cache|" ]; then
	fail "CASE 19: the refusal probe REACHED the real toolchain as 'clean -cache'"
elif [ "$(argv_joined)" != "version|" ]; then
	fail "CASE 19: unexpected final real-go invocation [$(argv_joined)] (wanted the version probe)"
else
	pass "CASE 19: a shim that fails to refuse is caught without the probe reaching the real go"
fi

# ---------------------------------------------------------------- CASE 20
# TRUNCATION MUST NEVER PRODUCE SILENT SUCCESS.
#
# The catastrophic mode for a PATH shim is not refusing too much -- it is
# exiting 0 without exec'ing, because then every build on the host reports
# success while producing nothing. A shim whose executable statements sit at
# top level reaches that state whenever the file is cut at a statement
# boundary: bash parses to EOF, runs the function definitions, and exits 0.
# Truncation at a statement boundary is NOT a syntax error, which is exactly
# where the original reasoning for this file was wrong.
#
# The invariant asserted here is the only one that matters: for EVERY prefix of
# the file, the shim either fails (any non-zero status) or reaches the real go.
# "Exited 0 having done nothing" is never allowed.
#
# The `main() { ...; }; main "$@"` idiom is NOT sufficient on its own -- a cut
# landing between the closing brace and the invocation still parses and exits 0.
# This case is what forced the wrapper to be a brace group opened on line 2, so
# that every cut after the shebang leaves it unterminated.
total_lines="$(wc -l <"$SHIM_SRC" | tr -d ' ')"
trunc_bad=0
trunc_checked=0
for n in $(seq 2 "$total_lines"); do
	cut="$WORK/truncated-go"
	head -n "$n" "$SHIM_SRC" >"$cut"
	rm -f "$ARGV"
	GC_GO_SHIM_REAL_GO="$FAKE_GO" FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" \
		bash "$cut" build ./... >/dev/null 2>&1
	rc=$?
	trunc_checked=$((trunc_checked + 1))
	if [ "$rc" -eq 0 ] && [ ! -f "$ARGV" ]; then
		fail "CASE 20: truncation at line $n exits 0 WITHOUT reaching the real go (silent success)"
		trunc_bad=$((trunc_bad + 1))
		[ "$trunc_bad" -lt 3 ] || break
	fi
done
[ "$trunc_bad" -eq 0 ] \
	&& pass "CASE 20: no truncation point ($trunc_checked checked) yields silent success"

# ---------------------------------------------------------------- CASE 21
# GOFLAGS carries flags into the subcommand that defines them, and `go clean`
# defines -cache. A decision that parses argv alone therefore has a hole:
# `GOFLAGS=-cache go clean -testcache` presents as an allowed invocation while
# cmd/go wipes the cache. Nobody sets that by accident -- but the guard's whole
# proposition is that it cannot be bypassed by accident, and a globally exported
# GOFLAGS would turn every `go clean` on the host into a wipe.
goflags_refused() {
	local label="$1" gf="$2"
	shift 2
	rm -f "$ARGV"
	GOFLAGS="$gf" GC_GO_SHIM_REAL_GO="$FAKE_GO" FAKE_GO_ARGV="$ARGV" \
		FAKE_GO_PID="$PIDFILE" bash "$SHIM_SRC" "$@" >"$OUT" 2>"$ERR"
	local rc=$?
	if [ "$rc" -eq 0 ]; then
		fail "$label: exited 0 (expected refusal)"
	elif [ -f "$ARGV" ]; then
		fail "$label: reached the real go with [$(argv_joined)]"
	else
		pass "$label"
	fi
}
goflags_refused "CASE 21a: GOFLAGS=-cache with a bare clean" "-cache" clean
goflags_refused "CASE 21b: GOFLAGS=-cache behind an allowed -testcache" "-cache" clean -testcache
goflags_refused "CASE 21c: GOFLAGS=--cache" "--cache" clean
goflags_refused "CASE 21d: GOFLAGS with -cache among other flags" "-mod=readonly -cache -x" clean
goflags_refused "CASE 21e: GOFLAGS=-cache=true" "-cache=true" clean

# GOFLAGS must not over-refuse: it only carries into the subcommand that defines
# the flag, so it is irrelevant to anything but `clean`, and a falsey or
# lookalike value is not the ban.
assert_allowed "CASE 21f: GOFLAGS is irrelevant to a build" build ./...
GOFLAGS="-cache" assert_allowed "CASE 21g: GOFLAGS=-cache does not block a build" build ./...
GOFLAGS="-testcache" assert_allowed "CASE 21h: GOFLAGS=-testcache is not the ban" clean -testcache
GOFLAGS="-modcache" assert_allowed "CASE 21i: GOFLAGS=-modcache is not the ban" clean
GOFLAGS="-cache=false" assert_allowed "CASE 21j: GOFLAGS=-cache=false is a no-op" clean

# ---------------------------------------------------------------- CASE 22
# A flag that takes a SEPARATE value must not end the argv scan.
#
# The scan stopped at the first non-flag token, and a value-taking flag puts a
# non-flag token in the middle of the flag list. So `go clean -tags foo -cache`
# was passed straight through -- while Go's flag package consumes "foo" as the
# value of -tags, keeps parsing, sees -cache, and cmd/go wipes the cache.
# Measured against the installed shim before the fix: refused `clean -cache`,
# passed `clean -tags foo -cache`.
assert_refused "CASE 22a: -cache behind -tags with a separate value" clean -tags foo -cache
assert_refused "CASE 22b: -cache behind -p with a separate value" clean -p 4 -cache
assert_refused "CASE 22c: -cache behind -gcflags with a separate value" clean -gcflags all=-N -cache
assert_refused "CASE 22d: -cache behind -toolexec" clean -toolexec /bin/echo -cache
assert_refused "CASE 22e: -cache after an =value flag still caught" clean -mod=vendor -cache
assert_refused "CASE 22f: value flag before -C-style global and the ban" -C / clean -tags x -cache

# The value itself may LOOK like the ban. `go clean -tags -cache` means
# tags="-cache": cmd/go does NOT wipe, so the shim must not refuse either.
# Skipping the value is what makes the shim agree with the runtime instead of
# over-refusing.
assert_allowed "CASE 22g: -tags -cache is a tag named -cache, not the ban" clean -tags -cache
assert_allowed "CASE 22h: a value flag with no -cache is not the ban" clean -tags foo -testcache
assert_allowed "CASE 22i: a package path still ends the scan" clean ./pkg

# ---------------------------------------------------------------- CASE 23
# The rollback path. A failed post-install probe must not leave a shim we have
# just proved untrustworthy ahead of the real toolchain -- and "restored" must
# mean USABLE, not merely present with the right bytes.
#
# The first version of this rollback restored the file non-executable: mktemp
# makes the backup slot 0600, `cp -f` leaves an existing destination's mode
# alone, and `mv` carries the source's mode. bash skips a non-executable PATH
# entry SILENTLY, so `go` resolved past the shim to the real toolchain -- the
# ban unenforced, with "rolled back: restored the previous ..." on stderr.
#
# So the assertions are the property, not the file: executable, and a PATH walk
# still resolves `go` TO the target. Asserting existence and content would have
# passed against the bug.
rb="$WORK/rollback"
mkdir -p "$rb/bin"

# A good shim first, so there is a prior copy to restore.
FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" PATH="$rb/bin:$WORK/fakego:$PATH" \
	bash "$INSTALLER" --dir "$rb/bin" >"$OUT" 2>"$ERR" \
	|| fail "CASE 23: could not install the good shim to set up the rollback"

good_sum="$(shasum -a 256 <"$rb/bin/go" | cut -d" " -f1)"

# Now reinstall from a sabotaged source so the refusal probe fails.
sab="$WORK/rollback-src"
mkdir -p "$sab"
cp -f "$INSTALLER" "$sab/"
sed 's/^if refuses_clean_cache "\$@"; then$/if false; then/' "$SHIM_SRC" >"$sab/go-clean-cache-shim.sh"

if FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" PATH="$rb/bin:$WORK/fakego:$PATH" \
	bash "$sab/install-go-clean-cache-shim.sh" --dir "$rb/bin" >"$OUT" 2>"$ERR"; then
	fail "CASE 23: installer reported success over a shim that does not refuse"
elif [ ! -x "$rb/bin/go" ]; then
	fail "CASE 23: rolled-back $rb/bin/go is NOT executable (mode $(ls -l "$rb/bin/go" | cut -c1-10)); bash will skip it and go resolves past the shim"
elif [ "$(shasum -a 256 <"$rb/bin/go" | cut -d" " -f1)" != "$good_sum" ]; then
	fail "CASE 23: rolled-back shim content differs from the good shim"
elif [ "$(PATH="$rb/bin:$WORK/fakego:$PATH" command -v go)" != "$rb/bin/go" ]; then
	fail "CASE 23: after rollback a PATH walk resolves go past the shim to $(PATH="$rb/bin:$WORK/fakego:$PATH" command -v go)"
else
	pass "CASE 23: a failed probe restores the prior shim, executable and still shadowing"
fi

# No prior install: rollback must REMOVE the new shim rather than leave a
# condemned one on PATH.
rb2="$WORK/rollback-fresh"
mkdir -p "$rb2/bin"
if FAKE_GO_ARGV="$ARGV" FAKE_GO_PID="$PIDFILE" PATH="$rb2/bin:$WORK/fakego:$PATH" \
	bash "$sab/install-go-clean-cache-shim.sh" --dir "$rb2/bin" >"$OUT" 2>"$ERR"; then
	fail "CASE 23b: installer reported success over a shim that does not refuse"
elif [ -e "$rb2/bin/go" ]; then
	fail "CASE 23b: a condemned shim was left at $rb2/bin/go with no prior copy to restore"
else
	pass "CASE 23b: with no prior install, a failed probe removes the shim"
fi

# The backup slot must not survive a successful install.
if ls "$rb/bin"/go.prior.* >/dev/null 2>&1; then
	fail "CASE 23c: a go.prior.* backup was left behind in a PATH directory"
else
	pass "CASE 23c: no backup slot left behind"
fi

# ------------------------------------------------------------------ verdict
if [ "$failures" -ne 0 ]; then
	echo "FAILED: $failures case(s)" >&2
	exit 1
fi
echo "all go-clean-cache shim cases passed"

#!/usr/bin/env bash
#
# go-clean-cache-shim -- a `go` wrapper that refuses `go clean -cache` and
# passes every other invocation straight through to the real toolchain.
#
# gocacheguard:allow-file  this file IS the enforcement; its refusal text
#                          necessarily names the command. That it contains no
#                          actual invocation is pinned separately by CASE 13 of
#                          scripts/test-go-clean-cache-shim.sh, which requires
#                          exactly one exec and requires it to be the passthrough.
#
# WHY THIS EXISTS
#
# AGENTS.md ("Build Cache Conventions") hard-bans `go clean -cache`. Until this
# shim, nothing enforced that ban: it is a rule about what a process DOES, and
# the repo's other guards only see what gets committed. On 2026-06-13 (bead
# vp-g96b) and again on 2026-09-05 something ran it against the shared GOCACHE
# and broke concurrent builds across the agent fleet -- not as cache misses but
# as hard failures, including on stdlib imports:
#
#     could not import slices (open .../bc/bc66f4...-d: no such file or directory)
#     link: cannot open file  .../74/74a81b...-d: no such file or directory
#
# `go clean -cache` RemoveAll's all 256 shard directories, hot entries and all.
# cmd/go's miss handling protects the LOOKUP, not the later open: OutputFile()
# hands a PATH to the compiler or linker, and an unlink landing in between is
# ENOENT at the tool. Neither the 2026-09-05 run nor its predecessor appeared
# in any shell history -- a process ran it, which is exactly the surface a
# commit-time lint cannot reach and this shim can.
#
# BLAST RADIUS -- READ BEFORE CHANGING ANYTHING BELOW
#
# Installed ahead of the real `go` on PATH, this file is in the path of EVERY
# Go build on the host. The refusal is the small part; the passthrough is the
# part that must be perfect. Three properties are load-bearing and are pinned
# by scripts/test-go-clean-cache-shim.sh:
#
#   1. Everything that is not the banned operation is `exec`ed, so there is no
#      extra process, no altered exit status, no swallowed signal, and no
#      buffered stream. CASE 8 proves the exec by pid identity.
#   2. The decision is a PARSE of the argument list, never a grep of the
#      command line -- otherwise `go build ./cmd/go-clean-cache` or
#      `go test -run 'go clean -cache'` would be refused (CASE 3).
#   3. Misconfiguration fails LOUD. A shim that cannot resolve the real go and
#      exits 0 turns every build on the host into a silent no-op, which is a
#      worse outage than the one being prevented (CASE 11).
#
# WHAT IS DELIBERATELY NOT BLOCKED
#
#   go clean -testcache   explicitly allowed by AGENTS.md; clears only the
#                         test-result cache and cannot corrupt a concurrent
#                         build
#   go clean -modcache    a different cache with a different failure mode
#   go clean -fuzzcache   likewise
#   go clean              bare; cleans build artifacts in package dirs
#   go clean -cache=false a no-op in cmd/go, so a no-op here
#
# Blocking any of those would be a false positive, and a guard that fires on
# legitimate work is a guard that gets deleted.
#
# ESCAPE HATCH
#
#   GC_ALLOW_GO_CLEAN_CACHE=1 go clean -cache
#
# Deliberate, per-invocation, and never silent -- the shim still says on stderr
# that it let it through, so the next cache-miss storm is traceable.
#
# INSTALL / UNINSTALL: scripts/install-go-clean-cache-shim.sh (which is also
# what substitutes REAL_GO_PINNED below). See
# engdocs/contributors/go-clean-cache-guard.md.
#
# This script deliberately sets neither GOCACHE nor TMPDIR nor GOTMPDIR.

# No `set -e`: the normal exit from this script is an exec, and an aborted
# script must never be mistaken for a refusal.
set -uo pipefail

# Absolute path to the real toolchain, substituted at install time. An
# unsubstituted value (anything not starting with "/") means this file is
# running uninstalled, and the shim refuses to guess rather than re-searching
# PATH -- a PATH search from inside a PATH shim is how you find yourself.
REAL_GO_PINNED='@REAL_GO@'

SELF="${BASH_SOURCE[0]}"

die() {
	printf 'go-clean-cache-shim: %s\n' "$*" >&2
	exit 3
}

# truthy -- shared by the -cache=VALUE parse and the override. Mirrors what
# Go's flag package accepts for a bool, so `-cache=false` is treated as the
# no-op cmd/go would treat it as.
truthy() {
	case "$1" in
	1 | t | T | true | TRUE | True) return 0 ;;
	*) return 1 ;;
	esac
}

# refuses_clean_cache <argv...> -- true when this invocation is the banned
# operation. A parse, not a grep:
#
#   * global flags before the subcommand are skipped, including `go -C dir`,
#     whose value is a separate argument;
#   * only the token that IS `-cache` / `--cache` counts, so `-testcache`,
#     `-modcache` and `-fuzzcache` cannot collide with it;
#   * scanning stops at the first non-flag argument, because from there on
#     cmd/go is reading package paths -- `go clean ./... -cache` cleans a
#     package named "-cache", it does not clear the build cache.
refuses_clean_cache() {
	local -a argv=("$@")
	local n=$# i=0 a

	# Skip anything before the subcommand.
	while [ "$i" -lt "$n" ]; do
		a="${argv[$i]}"
		case "$a" in
		-C)
			i=$((i + 2))
			continue
			;;
		-*)
			i=$((i + 1))
			continue
			;;
		*) break ;;
		esac
	done

	[ "$i" -lt "$n" ] || return 1
	[ "${argv[$i]}" = "clean" ] || return 1
	i=$((i + 1))

	while [ "$i" -lt "$n" ]; do
		a="${argv[$i]}"
		case "$a" in
		--) return 1 ;;
		-cache | --cache) return 0 ;;
		-cache=* | --cache=*) truthy "${a#*=}" && return 0 || return 1 ;;
		-*) i=$((i + 1)) ;;
		*) return 1 ;;
		esac
	done
	return 1
}

if refuses_clean_cache "$@"; then
	if truthy "${GC_ALLOW_GO_CLEAN_CACHE:-}"; then
		printf 'go-clean-cache-shim: GC_ALLOW_GO_CLEAN_CACHE is set — allowing `go clean -cache`.\n' >&2
		printf 'go-clean-cache-shim: every concurrent build on this host will now miss on everything.\n' >&2
	else
		cat >&2 <<'REFUSAL'
go: REFUSED — `go clean -cache` is banned on this host.

It removes all 256 shard directories of the shared build cache, hot entries
included. Concurrent builds do not merely miss: cmd/go resolves an entry, hands
the path to the compiler or linker, and the tool then opens a file that has
just been unlinked. That is a hard build failure, on stdlib imports included.
It has broken the fleet twice — bead vp-g96b (2026-06-13) and again 2026-09-05.

The rule: AGENTS.md, "Build Cache Conventions".

What you probably wanted instead:

  go clean -testcache                       clears test RESULTS only; explicitly
                                            allowed, and safe under concurrency
  scripts/trim-go-build-cache.sh --dry-run  bounded age-based trim of the shared
                                            cache — the supported way to reclaim
                                            space
  an isolated cold build                    point GOCACHE and TMPDIR at one
                                            throwaway dir under /var/tmp; see
                                            AGENTS.md, "If you truly need an
                                            isolated cold build"

If you really do mean to wipe the whole build cache, say so deliberately:

  GC_ALLOW_GO_CLEAN_CACHE=1 go clean -cache

REFUSAL
		exit 3
	fi
fi

REAL_GO="${GC_GO_SHIM_REAL_GO:-}"
if [ -z "$REAL_GO" ]; then
	case "$REAL_GO_PINNED" in
	/*) REAL_GO="$REAL_GO_PINNED" ;;
	*)
		die "no real go toolchain is configured (REAL_GO_PINNED is still the
install-time placeholder). This shim is running uninstalled. Install it with
scripts/install-go-clean-cache-shim.sh, or set GC_GO_SHIM_REAL_GO to the
absolute path of the real go. Refusing to search PATH from inside a PATH shim."
		;;
	esac
fi

[ -x "$REAL_GO" ] || die "configured real go is missing or not executable: $REAL_GO"
if [ "$REAL_GO" -ef "$SELF" ]; then
	die "configured real go resolves to this shim itself ($REAL_GO); refusing to
recurse. Point GC_GO_SHIM_REAL_GO at the real toolchain."
fi

exec "$REAL_GO" "$@"

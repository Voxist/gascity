#!/usr/bin/env bash
#
# Install (or remove) the `go clean -cache` shim from
# scripts/go-clean-cache-shim.sh.
#
# gocacheguard:allow-file  installing a guard against the command means naming
#                          it, in the post-install verification and in the
#                          operator instructions this prints.
#
# The shim only works if it is reached BEFORE the real toolchain on PATH, and
# the failure mode of getting that wrong is silent: a shim in a directory that
# PATH reaches after the real go never runs, and the operator believes the ban
# is enforced when it is not. On this host that is not hypothetical --
# ~/.gc/bin is LAST in the operator's PATH, behind /opt/homebrew/bin, so
# installing there would guard nothing. This installer refuses that case rather
# than manufacturing false confidence.
#
# It also:
#   * resolves the real go ONCE and bakes the absolute path into the installed
#     copy, so the shim never searches PATH at run time (a PATH search from
#     inside a PATH shim finds itself);
#   * resolves PAST any shim already on PATH, so two installed shims can never
#     be chained;
#   * verifies the installed shim both passes a build through and refuses the
#     ban before reporting success;
#   * on --uninstall, removes the file ONLY if it is this shim. Pointed at the
#     wrong directory the worst case is a no-op, never a deleted toolchain.
#
# Usage:
#   install-go-clean-cache-shim.sh [--dir DIR] [--dry-run]
#   install-go-clean-cache-shim.sh [--dir DIR] --uninstall
#
# Environment:
#   GC_GO_SHIM_DIR       default install directory (default $HOME/bin)
#   GC_GO_SHIM_REAL_GO   skip PATH resolution and pin this toolchain instead
#
# This script deliberately sets neither GOCACHE nor TMPDIR, and never runs
# `go clean` in any form.

set -uo pipefail
export LC_ALL=C

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SHIM_SRC="$SCRIPT_DIR/go-clean-cache-shim.sh"

# Marker used to recognise an installed copy. Kept in one place so the
# "is this a shim?" test and the uninstall safety check can never drift apart.
SHIM_MARKER='go-clean-cache-shim'

DIR="${GC_GO_SHIM_DIR:-$HOME/bin}"
UNINSTALL=0
DRY_RUN=0

die() {
	printf 'install-go-clean-cache-shim: %s\n' "$*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		[ $# -ge 2 ] || die "--dir needs a directory"
		DIR="$2"
		shift 2
		;;
	--dir=*)
		DIR="${1#--dir=}"
		shift
		;;
	--uninstall)
		UNINSTALL=1
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		sed -n '/^# Usage:/,/^# Environment:/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

[ -r "$SHIM_SRC" ] || die "shim source is missing: $SHIM_SRC"

# is_shim <file> -- true if the file is an installed copy of this shim. Read a
# bounded prefix so this can never be fooled into slurping a 100MB toolchain
# binary, and require the marker to appear in the shim's own header comment.
is_shim() {
	[ -f "$1" ] || return 1
	head -c 8192 "$1" 2>/dev/null | grep -q "$SHIM_MARKER"
}

TARGET="$DIR/go"

# ------------------------------------------------------------------ uninstall
if [ "$UNINSTALL" -eq 1 ]; then
	if [ ! -e "$TARGET" ]; then
		echo "nothing to uninstall: $TARGET does not exist"
		exit 0
	fi
	if ! is_shim "$TARGET"; then
		die "$TARGET is NOT this shim (no '$SHIM_MARKER' marker in its first 8KB).
Refusing to remove it — that is how you delete a real toolchain. If you are
certain, remove it by hand."
	fi
	if [ "$DRY_RUN" -eq 1 ]; then
		echo "would remove $TARGET (--dry-run; nothing was changed)"
		exit 0
	fi
	rm -f "$TARGET" || die "could not remove $TARGET"
	echo "removed $TARGET"
	exit 0
fi

# -------------------------------------------------------------- resolve real go
#
# PATH is walked by hand rather than using `command -v go`, for one reason: an
# already-installed shim must be stepped OVER, not pinned to. `command -v`
# would happily hand back the shim and produce a chain.
resolve_real_go() {
	local entry cand
	if [ -n "${GC_GO_SHIM_REAL_GO:-}" ]; then
		printf '%s' "$GC_GO_SHIM_REAL_GO"
		return 0
	fi
	local IFS=:
	for entry in $PATH; do
		[ -n "$entry" ] || entry=.
		cand="$entry/go"
		[ -x "$cand" ] && [ ! -d "$cand" ] || continue
		is_shim "$cand" && continue
		printf '%s' "$cand"
		return 0
	done
	return 1
}

REAL_GO="$(resolve_real_go)" || die "no real go toolchain found on PATH (every
'go' on PATH is already this shim, or there is none)."
[ -x "$REAL_GO" ] || die "resolved real go is not executable: $REAL_GO"
is_shim "$REAL_GO" && die "resolved real go is itself a shim: $REAL_GO"

# Absolute, symlink-resolved-at-the-directory-level path, so the baked value
# does not depend on the caller's cwd.
REAL_GO="$(cd "$(dirname "$REAL_GO")" && pwd -P)/$(basename "$REAL_GO")"
REAL_GO_DIR="$(dirname "$REAL_GO")"

# ------------------------------------------------------------- shadowing check
#
# The whole guarantee is "this runs instead of the real go". Verify it from
# PATH order rather than trusting the operator's mental model of their own
# PATH -- which is exactly what is wrong about ~/.gc/bin on this host.
path_index() {
	local want="$1" entry i=0
	local IFS=:
	for entry in $PATH; do
		[ -n "$entry" ] || entry=.
		if [ -d "$entry" ] && [ "$entry" -ef "$want" ]; then
			printf '%s' "$i"
			return 0
		fi
		i=$((i + 1))
	done
	return 1
}

[ -d "$DIR" ] || die "install directory does not exist: $DIR (create it first)"
DIR="$(cd "$DIR" && pwd -P)"
TARGET="$DIR/go"

dir_idx="$(path_index "$DIR")" || die "install directory is not on PATH: $DIR
A shim there would never be reached. Add it to PATH ahead of $REAL_GO_DIR
first, or pick a directory that already precedes it."
real_idx="$(path_index "$REAL_GO_DIR")" \
	|| die "cannot locate $REAL_GO_DIR on PATH; refusing to guess whether $DIR precedes it"

if [ "$dir_idx" -ge "$real_idx" ]; then
	die "$DIR is SHADOWED by $REAL_GO_DIR on PATH (position $dir_idx vs $real_idx).
A shim installed there would never run, and you would believe the ban was
enforced when it is not. Move $DIR ahead of $REAL_GO_DIR in PATH, or install
into a directory that already precedes it."
fi

# ---------------------------------------------------------------------- install
if [ "$DRY_RUN" -eq 1 ]; then
	printf 'dry-run: would install %s -> %s (real go: %s)\n' "$SHIM_SRC" "$TARGET" "$REAL_GO"
	exit 0
fi

if [ -e "$TARGET" ] && ! is_shim "$TARGET"; then
	die "$TARGET already exists and is NOT this shim. Refusing to overwrite it."
fi

# Atomic: write beside the target, then rename. A half-written `go` on a
# directory that precedes the toolchain would break every build on the host.
tmp="$(mktemp "$TARGET.XXXXXX")" || die "could not create a temp file in $DIR"
trap 'rm -f "$tmp"' EXIT

# Substitute only the pinned-path assignment. The single quotes are part of the
# replacement so the baked value survives a path containing shell metacharacters
# (it is only ever read, never re-expanded).
if ! sed "s|^REAL_GO_PINNED='@REAL_GO@'\$|REAL_GO_PINNED='$REAL_GO'|" "$SHIM_SRC" >"$tmp"; then
	die "could not render the shim"
fi
grep -q "^REAL_GO_PINNED='$REAL_GO'\$" "$tmp" \
	|| die "shim rendering did not substitute REAL_GO_PINNED (source changed shape?)"

chmod 755 "$tmp" || die "could not chmod the rendered shim"

# Keep the prior copy until the probes below have passed, so a failed
# verification can put it back rather than leaving this host with a shim we
# have just proved does not work. Only ever an older copy of this same shim --
# anything else was refused above.
prior_backup=""
if [ -e "$TARGET" ]; then
	prior_backup="$(mktemp "$TARGET.prior.XXXXXX")" \
		|| die "could not create a backup slot in $DIR"
	cp -f "$TARGET" "$prior_backup" || die "could not back up the existing $TARGET"
fi

mv -f "$tmp" "$TARGET" || die "could not install $TARGET"
trap - EXIT

# ------------------------------------------------------------------- verify
#
# Report success only after demonstrating both halves on the installed copy.
# `go version` is chosen for the passthrough probe because it is read-only.
# A failed probe must not leave a shim we have just proved untrustworthy ahead
# of the real toolchain on every build on this host. Roll back to whatever was
# at $TARGET before (usually nothing) and then die.
rollback() {
	if [ -n "${prior_backup:-}" ] && [ -f "$prior_backup" ]; then
		mv -f "$prior_backup" "$TARGET" 2>/dev/null \
			&& echo "rolled back: restored the previous $TARGET" >&2
	else
		rm -f "$TARGET" 2>/dev/null \
			&& echo "rolled back: removed $TARGET" >&2
	fi
}

if ! "$TARGET" version >/dev/null 2>&1; then
	rollback
	die "installed shim at $TARGET cannot run '$REAL_GO version' — install is broken"
fi
#
# The refusal probe is run with the real toolchain SWAPPED OUT for /usr/bin/true.
# Without that, this line asks a just-installed, not-yet-trusted shim to decide
# whether to wipe the shared build cache -- and if its refusal logic were broken
# in exactly the way this probe exists to detect, the probe itself would perform
# the wipe and cause the incident the shim exists to prevent. Pointing it at a
# no-op keeps the assertion (a broken shim execs /usr/bin/true, exits 0, and is
# caught) while making the failure path harmless. The passthrough probe above
# already proves the pinned toolchain is reachable, so nothing is lost.
# GC_ALLOW_GO_CLEAN_CACHE is unset for the probe, not merely left alone: it is
# the documented escape hatch, so it may well be exported in the very shell an
# operator used to wipe the cache before installing this. Inherited, it makes a
# WORKING shim pass the ban through, the probe then declares the install broken,
# and (before the rollback above) left the shim in place anyway.
if env -u GC_ALLOW_GO_CLEAN_CACHE GC_GO_SHIM_REAL_GO=/usr/bin/true \
	"$TARGET" clean -cache >/dev/null 2>&1; then
	rollback
	die "installed shim at $TARGET did NOT refuse 'go clean -cache' — install is broken"
fi

[ -n "$prior_backup" ] && rm -f "$prior_backup"

cat <<EOF
installed $TARGET
  real go:  $REAL_GO
  PATH:     $DIR (position $dir_idx) precedes $REAL_GO_DIR (position $real_idx)
  verified: 'go version' passes through; 'go clean -cache' is refused

Override for a deliberate wipe:  GC_ALLOW_GO_CLEAN_CACHE=1 go clean -cache
Uninstall:                       $0 --dir $DIR --uninstall
EOF

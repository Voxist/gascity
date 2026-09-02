#!/bin/sh
set -eu

# Refuse unexpanded Go template placeholders: a worktree must never be
# created from a literal "{{.AgentBase}}".
for arg in "$@"; do
  case "$arg" in
    *'{{'*) echo "worktree-setup.sh: refusing unexpanded template placeholder in argument: $arg" >&2; exit 64 ;;
  esac
done
# An empty expansion ({{.RigRoot}} is "" for city-scoped agents) collapses
# under sh -c and shifts every positional left, so "--sync" lands in $3.
# Refuse any of the three path/name positionals that looks like a flag.
for arg in "${1-}" "${2-}" "${3-}"; do
  case "$arg" in
    --*) echo "worktree-setup.sh: positional argument looks like a flag ($arg): an earlier placeholder expanded to nothing" >&2; exit 64 ;;
  esac
done

rig_root="${1:?rig root required}"
work_dir="${2:?work dir required}"
# Required, not defaulted: the flag check above only catches the collapse when
# a trailing flag shifts into $3. Without one ("... {{.RigRoot}} {{.WorkDir}}
# {{.AgentBase}}"), an empty rig root leaves $1=work_dir, $2=agent, $3 unset,
# both guards pass, and a default would silently create the worktree under the
# wrong name in the wrong place. The lifecycle variant of this script requires
# it for the same reason.
agent="${3:?agent name required (an earlier placeholder may have expanded to nothing)}"
mode="${4:---sync}"

mkdir -p "$(dirname "$work_dir")"

if [ -d "$work_dir/.git" ] || git -C "$work_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ "$mode" = "--sync" ]; then
    git -C "$work_dir" fetch --all --prune >/dev/null 2>&1 || true
  fi
  exit 0
fi

branch="gc/${agent}"
git -C "$rig_root" worktree add -B "$branch" "$work_dir" HEAD

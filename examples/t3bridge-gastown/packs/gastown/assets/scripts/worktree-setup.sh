#!/bin/sh
set -eu

# Fail closed on unexpanded Go template placeholders. If gc ever hands this
# script a literal "{{.AgentBase}}" (ga-iwz7u), creating a worktree from it
# would mint .gc/agents/{{.AgentBase}} on branch gc-{{.AgentBase}}-<hash>.
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
agent="${3:-agent}"
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

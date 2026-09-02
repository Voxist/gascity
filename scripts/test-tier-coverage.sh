#!/usr/bin/env bash
# test-tier-coverage.sh — asserts the local test tiers' UNION still covers
# every package in the module (ga-4h8bu).
#
# The fast tier deliberately excludes a few packages (cmd/gc runs in its own
# fast shards; two integration-weight packages plus the scripts package's
# integration-tagged pipeline tests run in the integration tier).
# The hazard is silent coverage rot: a package dropped from one tier without
# being picked up by another fails no build and no test — it just stops
# running anywhere, which is exactly how the 2026-07-15 resync shipped four
# runtime regressions past a green gate. This check makes that rot loud, on
# every fast run, for ~2 seconds of `go list`.
#
# Run as a direct shell job (not via a Go _test.go wrapper) for the same
# resourcecensus reason as test-push-gate-lock.sh: a Go test exec'ing this
# script would add an os/exec call to the never-shrinks subprocess audit.
#
# Mutation-checked: dropping a heavy package from the integration tier's set,
# or growing either exclude list without a corresponding home, fails this
# script (see TestingMD "Tier coverage guard").

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

tiers="$repo_root/scripts/test-tier-packages"
module="github.com/gastownhall/gascity"
fail=0

# Two individually-checked substitutions: in the combined form "$(a; b)" the
# exit status is b's alone, which would mask a printer failure.
unit_core_pkgs="$("$tiers" unit-core)"
unit_core_excl="$("$tiers" unit-core-excludes)"
all_pkgs="$(printf '%s\n%s\n' "$unit_core_pkgs" "$unit_core_excl")"
go_list="$(go list ./...)"

# 1. The fast tier accounts for EVERY package: unit-core plus its documented
#    exclude list must be exactly `go list ./...`. A new package can never be
#    silently absent from fast without landing in the exclude list, and a
#    stale exclude (package deleted/renamed) is flagged too.
if ! diff <(printf '%s\n' "$all_pkgs" | sort -u) <(printf '%s\n' "$go_list" | sort -u) >/dev/null; then
  echo "tier-coverage: fast tier (unit-core + excludes) does not equal 'go list ./...':" >&2
  diff <(printf '%s\n' "$all_pkgs" | sort -u) <(printf '%s\n' "$go_list" | sort -u) >&2 || true
  fail=1
fi

# 2. Every unit-core exclude has a HOME in another shard family. cmd/gc's home
#    is the fast unit-cmd-gc shards themselves; every other exclude must be in
#    the integration tier's packages-core set, or it runs NOWHERE.
integration_core="$("$tiers" integration-core)"
integration_excludes="$("$tiers" integration-core-excludes)"
while IFS= read -r pkg; do
  [[ "$pkg" == "${module}/cmd/gc" ]] && continue # fast unit-cmd-gc-N-of-6 shards
  if ! printf '%s\n' "$integration_core" | grep -qx "$pkg"; then
    # `go list ./...` can transiently OMIT a package with exit 0 while
    # sibling gauntlet jobs churn the worktree (the walk skips a directory it
    # momentarily cannot read, without an error). Before declaring a hole,
    # confirm against the package itself: it is a genuine hole only if the
    # package is deliberately excluded from integration-core, or does not
    # resolve at all.
    if printf '%s\n' "$integration_excludes" | grep -qx "$pkg"; then
      echo "tier-coverage: ${pkg} is excluded from fast unit-core AND from integration packages-core — it runs in NO tier" >&2
      fail=1
    elif ! go list "$pkg" >/dev/null 2>&1; then
      echo "tier-coverage: ${pkg} is excluded from fast unit-core but does not resolve as a package — it runs in NO tier" >&2
      fail=1
    else
      echo "tier-coverage: note: ${pkg} was missing from a transient 'go list ./...' but resolves directly; treating as covered" >&2
    fi
  fi
done < <("$tiers" unit-core-excludes)

# 3. The integration-core exclude list is pinned to exactly the three packages
#    that have dedicated shard families (test/integration, cmd/gc,
#    internal/runtime/tmux). A fourth exclusion sneaking in would silently
#    drop a package from the integration sweep with no dedicated home.
expected_integration_excludes="${module}/cmd/gc
${module}/internal/runtime/tmux
${module}/test/integration"
actual_integration_excludes="$("$tiers" integration-core-excludes | sort)"
if [[ "$actual_integration_excludes" != "$expected_integration_excludes" ]]; then
  echo "tier-coverage: integration-core exclude list changed; every entry needs a dedicated shard family and this guard updated deliberately:" >&2
  diff <(printf '%s\n' "$expected_integration_excludes") <(printf '%s\n' "$actual_integration_excludes") >&2 || true
  fail=1
fi

if [[ "$fail" -ne 0 ]]; then
  exit 1
fi
echo "tier-coverage: ok ($(printf '%s\n' "$go_list" | wc -l | tr -d ' ') packages accounted for across tiers)"

#!/usr/bin/env bash
#
# VENDORED from voxist-platform `scripts/ci/secret-scan-range.sh` (its PR
# #768), by way of livetranslate-webrtc `.github/ci/secret-scan-range.sh`.
# Kept byte-for-byte below the shebang so all three copies stay diffable;
# the original comments explain the reasoning and are worth reading in full.
# Added here for the gascity fork's betterleaks CI (bead vp-ty88y). Named
# voxist-secret-scan-range.sh, not secret-scan-range.sh, so a future upstream
# gastownhall/gascity commit at the default path never collides with this
# fork-only file on resync.
#
# Print the `git log` revision range that the current CI event introduces.
#
# Why this exists (bead vc-rq8b). `betterleaks git .` with no `--log-opts`
# scans `git log -p --all`: every ref the checkout fetched, which under
# `fetch-depth: 0` is the whole repository. The `secret-scan` required check
# therefore failed PRs for findings on *other people's branches*. Measured
# 2026-09-04: voxist-platform PR #766 went green -> red between d0a2bb81 and
# 770fb75a with no scan-relevant change; both findings were commits on PR
# #767's branch, ancestors of neither #766's head nor `origin/feature`.
#
# A required check must mean what its name says, and under ADR-0118 this one
# IS the review for content with no runtime reach. So the PR-triggered scan
# is scoped to the PR's own commits, and the push-triggered scan to the
# pushed range. Whole-repository history is still scanned once (see the PR
# description for the manual full-history triage) — see
# .github/workflows/secret-scan-full.yml in voxist-platform for the pattern,
# deliberately NOT a required check.
#
# Scoping changes only WHICH event a finding lands on, never whether it is
# found: every commit is in exactly one PR range and one push range.
#
# Reads the event from the environment so the workflow keeps the
# `${{ github.* }}` interpolation out of the shell body:
#   GITHUB_EVENT_NAME, GITHUB_SHA  — set by the runner
#   PR_BASE_SHA, PR_HEAD_SHA       — pull_request events
#   PUSH_BEFORE_SHA                — push events (github.event.before)
set -euo pipefail

ZERO=0000000000000000000000000000000000000000

have() { git cat-file -e "${1}^{commit}" 2>/dev/null; }

case "${GITHUB_EVENT_NAME:-}" in
  pull_request | pull_request_target)
    base="${PR_BASE_SHA:-}"
    head="${PR_HEAD_SHA:-}"
    # actions/checkout leaves us on GitHub's `refs/pull/N/merge` commit,
    # whose parent 1 is the base tip and parent 2 is the PR head. Those two
    # are always present locally; base.sha / head.sha are not, for a fork PR
    # or after a force-push to the base branch. Prefer the event payload,
    # fall back to the merge parents.
    { [ -n "$head" ] && have "$head"; } || head="$(git rev-parse HEAD^2)"
    { [ -n "$base" ] && have "$base"; } || base="$(git rev-parse HEAD^1)"
    # merge-base, not base itself: base has usually moved on since the PR
    # branched, and `base..head` would then replay unrelated trunk commits
    # into this PR's range — the same borrowed-blame defect, one branch over.
    #
    # Assigned on its own line, NOT inlined into the printf: `set -e` does not
    # fire for a command substitution in argument position (the command that
    # runs is `printf`, which succeeds). Inlined, a failing merge-base — two
    # histories with no common ancestor — would print `..$head`, which
    # betterleaks accepts and scans as ~0 bytes, and the required check would
    # go green having scanned nothing. In an assignment the substitution's
    # status IS the statement's status, so this fails closed.
    mb="$(git merge-base "$base" "$head")"
    printf '%s..%s\n' "$mb" "$head"
    ;;
  push)
    before="${PUSH_BEFORE_SHA:-}"
    sha="${GITHUB_SHA:?GITHUB_SHA is unset}"
    if [ -n "$before" ] && [ "$before" != "$ZERO" ] && have "$before" &&
      git merge-base --is-ancestor "$before" "$sha"; then
      printf '%s..%s\n' "$before" "$sha"
    else
      # A branch's first push sends before=000...0; a force-push sends an old
      # tip that may be unreachable or no longer an ancestor. Fall back to
      # "what this branch carries that the trunk does not".
      excl=""
      for trunk in origin/main; do
        have "$trunk" && excl="${excl} ^${trunk}"
      done
      if [ -z "$excl" ]; then
        # Refusing beats silently scanning everything: an empty exclusion
        # would make `git log -p $sha` walk all of history again, which is
        # exactly the defect this script exists to remove. Unreachable under
        # `fetch-depth: 0`, which always creates the remote-tracking refs.
        echo "voxist-secret-scan-range: no origin/main ref — cannot bound the scan" >&2
        exit 2
      fi
      printf '%s%s\n' "$sha" "$excl"
    fi
    ;;
  *)
    echo "voxist-secret-scan-range: unsupported event '${GITHUB_EVENT_NAME:-}'" >&2
    exit 2
    ;;
esac

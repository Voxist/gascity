# Release Gate Criteria Conventions

`release-gates/*.md` files record a reviewer's/agent's sign-off on a deploy
branch, one numbered criterion per row. This doc defines what the "Tests
pass" criterion must contain. No prior doc in the repo defined this — see
"Why this doc exists" below.

## The rule

"Tests pass" must name the specific CI jobs that `ci-required`
(`.github/workflows/ci.yml`) actually gates merge on for the paths the
change touches, and cite their real result — either an actual CI run on the
reviewed commit, or a local invocation that exercises the same coverage.

A criterion that only cites `make test-fast-parallel` and/or a package-
scoped `go test` is **not sufficient** whenever the change touches a path
covered by a job outside the fast tier. Find those jobs from the `changes`
job's path filters (same file): a filter matching the change's paths means
its job is a required, blocking check whenever it runs — not an optional
extra.

The most common miss: anything touching `cmd/gc/**`, `internal/**`, or
`examples/gastown/**` is covered by the `cmd_gc_process` filter, whose job
runs `TestTutorial01` (`cmd/gc/main_test.go`) under `GC_FAST_UNIT=0`
(`cmd/gc/fast_loop_helpers_test.go`). Every other default-tier entry point —
`make test`, `make test-fast-parallel`, bare `go test ./cmd/gc/`,
`make check` — sets `GC_FAST_UNIT` to `1` or leaves it unset, which skips
`TestTutorial01` entirely. Citing any of those alone, for a change in that
filter's scope, does not demonstrate `TestTutorial01` ran. Name
`make test-cmd-gc-process[-parallel]` (or the CI `cmd/gc process` job's
actual result) explicitly, or don't claim that criterion covers this path.

The general principle behind the example: "tests pass" must mean "the
gate's actual required checks passed," not "a command I chose passed."
Don't let convenience substitute for coverage.

## A previously red test cited as fixed

A green run is evidence only relative to the failure it claims to fix. When
a test failed and a change claims to fix it, one green re-run does not:
a nondeterministic failure passes about half the time with no fix at all.
Measured instance: PR #183 merged on a single green re-run of a test that
had gone red 37 minutes earlier; the mis-fix left the failure probability
at 50% and the regression landed on main (bead `vp-15qfy`, mechanism in
`vp-fyyz9`).

Before citing "Tests pass" for a fix of a red test, satisfy one of:

- **Deterministic red at the parent.** Run the regression test against the
  unfixed code (the fix branch's parent commit) and show it fails, then
  green at the fix commit. One red plus one green is then decisive: it
  demonstrates the test's red is reproducible and that this change is what
  turns it off.
- **Repeated greens where a red cannot be produced.** If the failure cannot
  be reproduced deterministically (environment gone, timing no longer
  observable), re-run the failing test at the fix commit at least 10
  consecutive times and cite the command. Ten greens of a 50/50 draw have
  p ≈ 0.1% — evidence of a real fix, unlike one.

State which of the two you did in the criterion. A single green re-run of a
test that went red earlier in the same PR is not acceptable evidence for a
release gate or a merge decision.

Related silent-gate class: a failure in a push-only lane (`Preflight /
unit cover`, `Integration / rest-full-*`) never gates a PR at all — it can
only turn main red post-merge. Main-red in a push-only lane is a release
blocker for the fleet binary roll; the CI Verdict Watchdog
(`.github/workflows/ci-verdict-watchdog.yml`) flags it as a failing
check-run on the head commit.

## Why this doc exists

Two independent gate files recorded "Tests pass: PASS" against suites
structurally incapable of reaching the regression they were meant to catch,
both citing `make test-fast-parallel` plus scoped/package-level commands
that leave `GC_FAST_UNIT` at `1` or unset:

- `release-gates/ga-bucf4p-live-session-workdir-isolation-gate.md`, on the
  branch of open PR #4735 (not yet in `main`): the cwd-collision guard
  change was signed off with a "Tests pass" row that never ran a pool
  scenario. Per the root-cause trace in bead `ga-9x4z1g`,
  `TestTutorial01/08-agent-pools` is the scenario that exercises that path.
- `release-gates/ga-7vhfyj-cwd-fallback-guard-gate.md` (PR #4738): recorded
  "The reviewer independently ran the full `cmd/gc` package: 8,030 PASS,
  0 FAIL, 96 SKIP" — `TestTutorial01` was inside the 96 `SKIP`. The change
  broke `TestTutorial01/01-hello-gas-city` and `TestTutorial01/session-fail`
  on CI shard 7.

Both gates were internally consistent (the cited commands really did pass)
and still missed a real regression, because nothing required the "Tests
pass" criterion to map to the CI jobs `ci-required` actually depends on for
the changed paths. This doc closes that gap for future gate authors; it
does not retroactively correct the two files above.

Full evidence and root-cause traces: bead `ga-9x4z1g` (Design field) and
`ga-9x4z1g.3` (notes).

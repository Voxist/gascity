# gc status: wrapper-level stale-while-revalidate to kill false 'partial status' — Plan v0.1
**Status:** Draft · **Date:** 2026-07-17 · **Author:** planner · **Rig:** voxist-platform

## Context
The `gc status` command has a hardcoded 50ms timeout in the status provider that fires while tmux StateCache runs a synchronous ps scan on loaded boxes. This causes `boundedStatusCall` to return a ZERO fallback (Running=false/empty) resulting in 'gc status' showing partial/dead status for LIVE sessions. The issue occurs because the 50ms bound fires first and discards last-known-good state that exists at the base level but only engages after refresh failure.

## Constraints
- Fix must be limited to read-only status providers, not control plane logic
- Must preserve existing behavior for cold-start scenarios
- Must maintain thread safety for concurrent status calls
- Changes only affect display/observability, not control decisions

## Proposed approach
Implement wrapper-level stale-while-revalidate (SWR) in status_provider.go by adding per-session last-known-good state for IsRunning/ProcessAlive/ObserveLiveness. When timeouts occur, serve the last-known-good instead of zero values. The original goroutine should still complete to populate last-good and base cache for subsequent calls.

## Micro-tasks
| id | description | acceptance | est | slings |
|---|---|---|---|---|
| T-001 | Write failing test that reproduces timeout returning zero instead of last-known-good | `TestStatusProviderServesLastKnownGoodOnTimeout` fails before fix, passes after | 4 | — |
| T-002 | Add per-session last-known-good maps to status provider | T-001 test still fails, but infrastructure is in place | 5 | — |
| T-003 | Implement boundedStatusCallSWR with last-known-good serving on timeout | T-001 test passes, timeout now returns last-known-good | 5 | — |
| T-004 | Ensure goroutines complete after timeout to update cache | Existing tests still pass, no race conditions introduced | 4 | — |
| T-005 | Update documentation for status provider behavior | Documentation reflects new SWR behavior | 3 | — |

## GDPR data-flow impact
### Data added / removed / relocated
none
### New cross-border transfers (or "none")
none
### Audit-log changes (or "none")
none

## MDR Class I traceability
Not applicable — not a clinical path.

## Acceptance criteria
- `gc status` command no longer shows false "partial status" for live sessions on loaded systems
- Tests pass including race detection
- Last-known-good is served during timeout scenarios
- Original goroutines complete to update cache after timeout
- Cold-start scenarios still return zero as expected

## Rollback plan
The git-level rollback would involve reverting the commits that introduce the SWR mechanism in status_provider.go. There is no data-level rollback needed since this only affects in-memory state for status display. We would trigger rollback if the SWR mechanism introduces new race conditions or degrades performance significantly beyond the original timeout issue.

## Open questions
[architect] Does the per-session last-known-good state need any cleanup mechanism to prevent memory growth?
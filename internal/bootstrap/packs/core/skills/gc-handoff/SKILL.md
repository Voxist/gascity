---
name: gc-handoff
description: Producing a complete context handoff — summarize state, reconcile beads, run gates, then gc handoff to mail it forward and restart the session
---

# Handoff

`gc handoff` is the runtime verb that carries a handoff: it mails your
work-state to your next context window and — for controller-restartable
sessions — asks the controller to restart you with that mail waiting. The
verb moves the bits. **This skill is the payload**: what to capture so the
next session resumes without re-deriving everything you already knew.

Run this procedure when you are **ending a session, sensing context fill,
or passing work to another session**.

> On `/compact` the PreCompact hook already fires `gc handoff --auto
> "context cycle"` — but that is a bare mail with an empty body. It proves
> the channel works; it does not carry your reasoning. This procedure is
> what makes a handoff worth reading.

## Procedure

1. **Summarize work-state.** A short brief covering:
   - what you were doing and **why** (the decision, not just the action)
   - in-flight beads (IDs + status), what is blocked and on what
   - the **next concrete step** for the resuming session
   - artifacts: branches, PRs, file paths, scratch dirs
2. **Reconcile beads** — the durable half of the handoff:
   ```
   gc bd close <id...>                       # finished work
   gc bd update <id> --note "state + next"   # in-flight: leave the trail
   gc bd create "follow-up..." --rig <rig>   # anything you discovered
   ```
   Beads outlive mail. A resuming session runs `bd ready` / `bd show`
   before it reads mail — put the truth there first.
3. **Quality gates** (only if code changed) — run tests/lint/build and
   record pass/fail in the summary. A handoff that hides a red build
   wastes the next session's first hour.
4. **Hand off** — carry the summary across:
   ```
   gc handoff "<subject>" "<the summary from step 1>"
   ```

## Modes

| Invocation | Effect | When |
|---|---|---|
| `gc handoff "<subj>" "<msg>"` | mail to self **+ controller restart** (blocks until the controller stops the session) | deliberate self-handoff / session end |
| `gc handoff --auto "<subj>"` | mail to self, **no restart** | PreCompact hooks (the provider owns the compaction lifecycle) |
| `gc handoff --target <alias> "<subj>" "<msg>"` | mail the target; **kill it** so the reconciler restarts it with the mail waiting | passing work to another session |

Self-handoff needs session context (`GC_ALIAS`/`GC_SESSION_ID` + city env);
remote handoff takes a session alias or ID. Subject is required unless
`--auto` is set.

## What good looks like

A resuming session should be able to act from the handoff alone:

> **subj:** route-reclaim OTLP — PR open, CI red on lint
> **body:** Wiring the shared-proxy path (be-pen9). PR #4 open on
> cstar/beads, CI red — gofmt on `proxy/endpoint.go` only, logic green.
> Next: gofmt + push. Rollback binary at `~/releases/bd-pre.bak`. Beads:
> be-pen9 in_progress (note carries the test matrix), be-dv63 open (the
> deploy runbook).

Vague ("continuing the work, see you next time") is the failure mode the
bare auto-mail already covers — do not reproduce it.

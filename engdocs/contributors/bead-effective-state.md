# Bead effective-state analysis

**Problem this solves.** A bead's `status` field (`open` / `in_progress` /
`closed` / …) does **not** tell you who owns the next action. The real,
decision-relevant state is a *composite* of status + dependencies + delivery
phase + session binding + labels + type + routing. Reconstructing it by hand —
"is this waiting on a human? on a review? on another bead? on a router that
never fired?" — is exactly the time-sink this process removes.

The answer is a single **effective state** per bead, each mapped to the **owner**
of the next action. It is always *derived live* (never a stored status file, per
the no-status-files principle) — re-run any time and it reflects reality.

Tool: [`scripts/bead-state.py`](../../scripts/bead-state.py) (self-tested:
`scripts/bead-state.py --selftest`). Future home: a `gc beads state` subcommand.

## The raw state signals

| Signal | Source | Values |
| --- | --- | --- |
| `status` | bd builtin | `open`, `in_progress`, `blocked`, `deferred`, `closed`, `pinned`, `hooked` |
| dependencies | `bd ready` / `bd blocked` | ready set vs blocked set (open blockers) |
| delivery phase | `gc.phase` metadata | `building`, `ci-pending`, `review-pending`, `rework`, `decision-pending`, `merge-pending`, `conflicted`, `merged`/`abandoned` (terminal) — see `internal/delivery/phase.go` |
| session binding | `gc.session_name` + `gc session list` | bound-to-live vs bound-to-dead (orphan) |
| routing | `gc.routed_to` | routed to a pool, or not |
| human gate | `human` label / `type=decision` / `gc.do_not_auto_route=1` | needs a person |
| work vs internal | `issue_type` + title | work types `{task,bug,feature,chore,epic,decision}`; everything else (session/convoy/step/…) and `^(nudge\|order\|wisp):` titles are gc-internal |

## The effective-state taxonomy

Each bead resolves to **exactly one** state. `owner` answers "who do I
nudge / unblock / wait on?".

| Effective state | Owner | Meaning / determined by |
| --- | --- | --- |
| `ready-unrouted` | **DISPATCHER** ⚠️ | open, ready, plannable, **not routed** — the router should have picked it up. If stale (> ~1d) this is a **routing gap** (the expensive-to-diagnose case). |
| `orphaned` | **RECLAIM** ⚠️ | `gc.session_name` set but that session is **dead** — work stranded; `open` ones are skipped by the work-query as "taken", `in_progress` ones are zombies. |
| `unknown` | **INVESTIGATE** ⚠️ | a work-typed bead that matched no rule — a **gap in the model**. Should be ~0; non-zero means extend the taxonomy. |
| `blocked-deps` | upstream beads | in the `bd blocked` set — waiting on other beads to close. |
| `waiting-human` | human | `human` label, `type=decision`, or `gc.do_not_auto_route=1`. |
| `waiting-review` | human | `gc.phase=review-pending` — a PR awaits human review. |
| `waiting-decision` | human | `gc.phase=decision-pending` — a merge/keep decision awaits a human. |
| `epic-triage` | human/planner | `type=epic` or `EPIC:` title — a container needing decomposition. |
| `routed-waiting` | agent-pool | routed via `gc.routed_to`, awaiting pool pickup (imminent). |
| `in-progress` | agent | `status∈{in_progress,hooked}` or bound to a **live** session. |
| `delivering` | agent | `gc.phase∈{building,ci-pending,rework,merge-pending,conflicted}`. |
| `orchestration` | controller | gc-internal runtime beads: session/convoy/step/wisp + `nudge:`/`order:`/`wisp:` markers + anything with `gc.kind`. Not human/agent work. |
| `deferred` | scheduler | `status=deferred`. |
| `pinned` | — | `status=pinned` — persistent by design. |
| `done` | — | `status=closed` or terminal phase (`merged`/`abandoned`). |

## The decision tree (precedence — FIRST match wins)

Order matters: a bead can satisfy several signals; the **most specific / most
blocking** one wins.

1. **terminal/frozen** — closed/terminal-phase → `done`; `deferred`; `pinned`
2. **orchestration** — non-work type, `-wisp-` id, `nudge:`/`order:`/`wisp:` title, or `gc.kind` set
3. **delivery phase** — `review-pending` → `waiting-review`; `decision-pending` → `waiting-decision`; agent phases → `delivering`
4. **active session** — `in_progress`/`hooked` or `gc.session_name` set → `in-progress`, **unless** the session is dead → `orphaned`
5. **blocked** — in `bd blocked` → `blocked-deps`
6. **human gate** — `human` label / `type=decision` / `do_not_auto_route` → `waiting-human`
7. **epic** — `type=epic`/`EPIC:` → `epic-triage`
8. **routed** — `gc.routed_to` set → `routed-waiting`
9. **ready + plannable + unrouted** → `ready-unrouted`
10. else → `unknown`

## Usage

```bash
# Full breakdown for the city in the current directory:
scripts/bead-state.py

# One rig, or one state with ids:
scripts/bead-state.py --rig voxist-api
scripts/bead-state.py --state ready-unrouted
scripts/bead-state.py --ids          # ids under every state
scripts/bead-state.py --json         # machine-readable

scripts/bead-state.py --selftest     # verify the taxonomy
```

## How to read the result — the three "you are losing time" signals

Investigate these **first**; they are why dispatch silently stalls:

- **`orphaned` > 0** — stranded work bound to dead sessions. The fix is to clear
  `gc.session_name` so it re-enters the queue (what a delivery-warden /
  orphan-reaper automates). Until then it consumes pool headroom and is invisible
  to the work-query.
- **`ready-unrouted` stale** — the router (unrouted-feeder) isn't placing ready
  work. Check feeder cadence, planner headroom, and that `gc hook` actually
  surfaces routed work (see `gc-hook-deaddrop` notes: a broken `bd context` or a
  rejected dolt mode makes every hook return `[]`).
- **`unknown` > 0** — a bead the model can't classify. Extend the taxonomy (and
  this doc + `bead-state.py` together).

Everything in `waiting-human` / `waiting-review` / `waiting-decision` /
`epic-triage` is **not** a fault — it is correctly parked on a person. A drained
backlog of only those states means the fleet is healthy and waiting on you, not
stuck.

## Keep in sync

The "work vs internal" rule mirrors the unrouted-feeder's `PLANNABLE_TYPES` +
`GC_INTERNAL_TITLE_RE`. If the feeder's definition of "work" changes, update both
it and `bead-state.py` so the classifier still agrees with the dispatcher.

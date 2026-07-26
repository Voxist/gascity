#!/usr/bin/env python3
"""bead-state — classify every bead into ONE effective state + owner.

The `status` field (open/in_progress/closed/...) does NOT tell you who owns the
next action on a bead. The real state is a composite of status + dependencies +
delivery phase + session binding + labels + type + routing. This tool collapses
all of those signals into a single, unambiguous "effective state" per bead, so
you never again have to hand-trace "is this waiting on a human? a review? another
bead? a routing gap?".

It is a DERIVED, live read (never a stored status file) — re-run it any time and
it reflects current reality. See engdocs/contributors/bead-effective-state.md for
the taxonomy + decision tree this implements.

Usage:
    scripts/bead-state.py                 # classify all rigs of the city in cwd
    scripts/bead-state.py --rig <rig-name>
    scripts/bead-state.py --state ready-unrouted   # only show one state
    scripts/bead-state.py --json          # machine-readable
    scripts/bead-state.py --ids           # print bead ids under each state

Exit status is 0 always; anomaly states (orphaned, ready-unrouted-stale, unknown)
are flagged in the report — they are the "you are losing time here" signals.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
from dataclasses import dataclass, field

# --- effective-state taxonomy (precedence order: FIRST match wins) -----------
# Each state maps to the OWNER of the next action. This is the whole point:
# "who do I wait on / nudge / unblock?".
OWNER = {
    "done": "—",
    "deferred": "scheduler",
    "pinned": "—",
    "orchestration": "controller",
    "delivering": "agent",
    "waiting-review": "human",
    "waiting-decision": "human",
    "in-progress": "agent",
    "orphaned": "RECLAIM",
    "blocked-deps": "upstream-beads",
    "waiting-human": "human",
    "epic-triage": "human/triage",
    "routed-waiting": "agent-pool",
    "ready-unrouted": "DISPATCHER",
    "unknown": "INVESTIGATE",
}
# Display order for the report (roughly: needs-attention first, terminal last).
ORDER = [
    "ready-unrouted", "orphaned", "unknown",         # anomalies / your-action
    "blocked-deps", "waiting-human", "waiting-review", "waiting-decision",
    "epic-triage", "routed-waiting",                  # waiting on someone/something
    "in-progress", "delivering",                      # active
    "orchestration", "deferred", "pinned", "done",    # runtime / frozen / terminal
]
# What counts as human-meaningful WORK (everything else type is gc-internal:
# session/convoy/step/workflow/scope/... — owned by the controller, not people).
WORK_TYPES = {"task", "bug", "feature", "chore", "epic", "decision"}
# gc-internal bookkeeping beads that masquerade as work types, keyed by title
# (e.g. type=chore "nudge:..." mail markers, type=task "order:..."/"wisp:...").
# Mirrors unrouted-feeder's GC_INTERNAL_TITLE_RE — keep in sync.
INTERNAL_TITLE_RE = re.compile(r"^\s*(nudge|order|wisp)\s*:", re.I)
# States that mean "a human must act" — the user's core pain point.
HUMAN_STATES = {"waiting-human", "waiting-review", "waiting-decision", "epic-triage"}
# States that are anomalies worth surfacing loudly.
ANOMALY_STATES = {"orphaned", "unknown"}

PLANNABLE_TYPES = {"task", "bug", "feature", "chore"}
DELIVERY_AGENT_PHASES = {"building", "ci-pending", "rework", "merge-pending", "conflicted"}
DELIVERY_TERMINAL_PHASES = {"merged", "abandoned"}
# A ready-unrouted bead older than this (days) is a routing-gap red flag, not
# just transient — the feeder should have picked it up by now.
READY_UNROUTED_STALE_DAYS = 1.0


def _run(args: list[str], beads_dir: str | None = None) -> tuple[int, str]:
    env = dict(os.environ)
    if beads_dir:
        env["BEADS_DIR"] = beads_dir
    try:
        p = subprocess.run(args, capture_output=True, text=True, env=env, timeout=60)
        return p.returncode, p.stdout
    except Exception:
        return 1, ""


def _bd_json(beads_dir: str, *args: str) -> list[dict]:
    rc, out = _run(["bd", *args, "--json"], beads_dir)
    if rc != 0 or not out.strip():
        return []
    # strip any non-JSON warning preamble
    s = out.find("[")
    o = out.find("{")
    start = min([i for i in (s, o) if i >= 0], default=-1)
    if start < 0:
        return []
    try:
        d = json.loads(out[start:])
    except Exception:
        return []
    if isinstance(d, dict):
        d = d.get("issues") or d.get("items") or []
    return d if isinstance(d, list) else []


def _rigs(city_path: str) -> list[dict]:
    rc, out = _run(["gc", "rig", "list", "--json"])
    if rc == 0 and out.strip():
        s = out.find("{")
        if s >= 0:
            try:
                return json.loads(out[s:]).get("rigs", [])
            except Exception:
                pass
    # fallback: city scope only
    return [{"name": os.path.basename(city_path), "path": city_path, "prefix": ""}]


def _live_sessions() -> set[str] | None:
    """Return the set of live (not-closed) session names, or None if unknown
    (so orphan detection degrades gracefully to 'in-progress')."""
    rc, out = _run(["gc", "session", "list", "--json"])
    if rc != 0 or not out.strip():
        return None
    s = out.find("[")
    o = out.find("{")
    start = min([i for i in (s, o) if i >= 0], default=-1)
    if start < 0:
        return None
    try:
        d = json.loads(out[start:])
    except Exception:
        return None
    items = d if isinstance(d, list) else d.get("sessions") or d.get("items") or []
    live = set()
    for it in items:
        if it.get("closed"):
            continue
        nm = it.get("name") or it.get("session_name")
        if nm:
            live.add(nm)
    return live


def _truthy(v) -> bool:
    return str(v).strip().lower() in {"1", "true", "yes"}


def classify(b: dict, ready: set[str], blocked: set[str], live: set[str] | None) -> str:
    md = b.get("metadata") or {}
    status = (b.get("status") or "").lower()
    typ = (b.get("issue_type") or b.get("type") or "").lower()
    labels = {str(x).lower() for x in (b.get("labels") or [])}
    title = (b.get("title") or "")
    phase = md.get("gc.phase")
    routed = md.get("gc.routed_to")
    session = md.get("gc.session_name")
    bid = b.get("id")

    # 1. terminal / frozen
    if status == "closed" or phase in DELIVERY_TERMINAL_PHASES:
        return "done"
    if status == "deferred":
        return "deferred"
    if status == "pinned":
        return "pinned"

    # 2. gc-internal / orchestration: session, convoy, molecule steps, wisps, and
    #    nudge:/order:/wisp: bookkeeping markers. Not work — owned by the controller.
    if typ not in WORK_TYPES or "-wisp-" in (bid or "") or INTERNAL_TITLE_RE.match(title) or md.get("gc.kind"):
        return "orchestration"

    # 3. delivery phase (most specific in-flight signal)
    if phase == "review-pending":
        return "waiting-review"
    if phase == "decision-pending":
        return "waiting-decision"
    if phase in DELIVERY_AGENT_PHASES:
        return "delivering"

    # 3. actively held by a session
    if status in {"in_progress", "hooked"} or session:
        if session and live is not None and session not in live:
            return "orphaned"
        return "in-progress"

    # 4. blocked by other beads
    if bid in blocked:
        return "blocked-deps"

    # 5. human-gated (the "waiting for human" bucket)
    if "human" in labels or typ == "decision" or _truthy(md.get("gc.do_not_auto_route")):
        return "waiting-human"

    # 6. epic container needing decomposition
    if typ == "epic" or title.strip().upper().startswith("EPIC"):
        return "epic-triage"

    # 7. routed to a pool, awaiting pickup
    if routed:
        return "routed-waiting"

    # 8. ready + plannable + unrouted -> the dispatcher should route it.
    #    If this lingers, it is a ROUTING GAP (the expensive-to-diagnose case).
    if status == "open" and bid in ready and typ in PLANNABLE_TYPES:
        return "ready-unrouted"

    # 9. nothing matched -> the model has a gap; surface it loudly.
    return "unknown"


def _age_days(b: dict) -> float:
    ts = b.get("updated_at") or b.get("created_at")
    if not ts:
        return 0.0
    for fmt in ("%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%dT%H:%M:%S%z"):
        try:
            return (time.time() - time.mktime(time.strptime(ts, fmt))) / 86400.0
        except Exception:
            continue
    return 0.0


@dataclass
class Bucket:
    ids: list[str] = field(default_factory=list)
    stale_ids: list[str] = field(default_factory=list)  # ready-unrouted that are old


def _selftest() -> int:
    """Exercise classify() across every state + key precedence rules. Runs with
    no infra (pure function), so it is the regression guard for the taxonomy."""
    R = {"R"}          # ready set
    B = {"B"}          # blocked set
    LIVE = {"sess-live"}

    def bead(**kw):
        md = kw.pop("md", {})
        b = {"id": kw.pop("id", "x-1"), "issue_type": kw.pop("type", "task"),
             "status": kw.pop("status", "open"), "title": kw.pop("title", "t"),
             "labels": kw.pop("labels", []), "metadata": md}
        b.update(kw)
        return b

    cases = [
        # state                bead
        ("done",            bead(status="closed")),
        ("done",            bead(md={"gc.phase": "merged"})),
        ("deferred",        bead(status="deferred")),
        ("pinned",          bead(status="pinned")),
        ("orchestration",   bead(type="session")),
        ("orchestration",   bead(type="convoy")),
        ("orchestration",   bead(id="vc-wisp-1")),
        ("orchestration",   bead(type="chore", title="nudge:nudge-abc")),
        ("orchestration",   bead(type="task", title="order:foo")),
        ("orchestration",   bead(md={"gc.kind": "scope"})),
        ("waiting-review",  bead(md={"gc.phase": "review-pending"})),
        ("waiting-decision",bead(md={"gc.phase": "decision-pending"})),
        ("delivering",      bead(md={"gc.phase": "building"})),
        ("in-progress",     bead(status="in_progress")),
        ("in-progress",     bead(md={"gc.session_name": "sess-live"})),
        ("orphaned",        bead(md={"gc.session_name": "sess-dead"})),
        ("blocked-deps",    bead(id="B")),
        ("waiting-human",   bead(labels=["human"])),
        ("waiting-human",   bead(type="decision")),
        ("waiting-human",   bead(md={"gc.do_not_auto_route": "1"})),
        ("epic-triage",     bead(type="epic")),
        ("epic-triage",     bead(title="EPIC: big thing")),
        ("routed-waiting",  bead(id="R", md={"gc.routed_to": "rig/agent"})),
        ("ready-unrouted",  bead(id="R")),
        ("unknown",         bead(type="bug")),  # work-typed, not ready/blocked/routed
        # precedence: orchestration (session) beats in_progress status
        ("orchestration",   bead(type="session", status="in_progress")),
        # precedence: delivery phase beats in_progress status
        ("waiting-review",  bead(status="in_progress", md={"gc.phase": "review-pending"})),
        # precedence: blocked beats human-gating
        ("blocked-deps",    bead(id="B", type="decision")),
    ]
    fails = 0
    for want, b in cases:
        got = classify(b, R, B, LIVE)
        if got != want:
            fails += 1
            print(f"  FAIL: want {want!r} got {got!r} for {b}")
    if fails:
        print(f"\n  {fails} selftest failure(s)")
        return 1
    print(f"  selftest OK ({len(cases)} cases)")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="Classify beads into effective states.")
    ap.add_argument("--selftest", action="store_true", help="run taxonomy self-tests and exit")
    ap.add_argument("--city-path", default=os.getcwd())
    ap.add_argument("--rig", help="restrict to one rig name")
    ap.add_argument("--state", help="only show this effective state")
    ap.add_argument("--ids", action="store_true", help="list bead ids under each state")
    ap.add_argument("--json", action="store_true", dest="as_json")
    args = ap.parse_args()

    if args.selftest:
        return _selftest()

    live = _live_sessions()
    rigs = _rigs(args.city_path)
    if args.rig:
        rigs = [r for r in rigs if r.get("name") == args.rig]

    buckets: dict[str, Bucket] = {s: Bucket() for s in ORDER}
    seen: set[str] = set()  # dedup federation-surfaced beads by id (home-rig wins)

    for r in rigs:
        path = r.get("path")
        prefix = (r.get("prefix") or "").lower()
        if not path:
            continue
        beads_dir = os.path.join(path, ".beads")
        ready = {b.get("id") for b in _bd_json(beads_dir, "ready")}
        blocked = {b.get("id") for b in _bd_json(beads_dir, "blocked")}
        allb = _bd_json(beads_dir, "list", "--status", "open,in_progress,blocked,deferred,hooked,pinned")
        if not allb:  # some bd versions reject comma list; fall back to plain
            allb = _bd_json(beads_dir, "list")
        for b in allb:
            bid = b.get("id") or ""
            # only this rig's own beads (skip federation echoes from other rigs)
            if prefix and not bid.lower().startswith(prefix + "-"):
                continue
            if bid in seen:
                continue
            seen.add(bid)
            st = classify(b, ready, blocked, live)
            buckets.setdefault(st, Bucket()).ids.append(bid)
            if st == "ready-unrouted" and _age_days(b) >= READY_UNROUTED_STALE_DAYS:
                buckets[st].stale_ids.append(bid)

    if args.as_json:
        out = {s: {"owner": OWNER[s], "count": len(buckets[s].ids), "ids": buckets[s].ids,
                   "stale_ids": buckets[s].stale_ids} for s in ORDER if buckets[s].ids}
        print(json.dumps(out, indent=2))
        return 0

    total = sum(len(buckets[s].ids) for s in ORDER)
    human = sum(len(buckets[s].ids) for s in HUMAN_STATES)
    print(f"\n  EFFECTIVE STATE OF {total} OPEN BEADS  (owner = who acts next)\n")
    print(f"  {'STATE':<16} {'OWNER':<16} {'N':>4}")
    print(f"  {'-'*16} {'-'*16} {'-'*4}")
    for s in ORDER:
        n = len(buckets[s].ids)
        if n == 0 or (args.state and s != args.state):
            continue
        flag = ""
        if s in ANOMALY_STATES:
            flag = "  <-- ANOMALY"
        elif s == "ready-unrouted" and buckets[s].stale_ids:
            flag = f"  <-- {len(buckets[s].stale_ids)} STALE = ROUTING GAP"
        print(f"  {s:<16} {OWNER[s]:<16} {n:>4}{flag}")
        if args.ids or args.state:
            print(f"      {', '.join(buckets[s].ids)}")
    print(f"\n  {human} bead(s) need a HUMAN ({', '.join(sorted(HUMAN_STATES))}).")
    anom = sum(len(buckets[s].ids) for s in ANOMALY_STATES) + sum(len(buckets['ready-unrouted'].stale_ids) for _ in [0])
    if anom:
        print(f"  {anom} bead(s) are anomalies/routing-gaps — investigate these FIRST.")
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())

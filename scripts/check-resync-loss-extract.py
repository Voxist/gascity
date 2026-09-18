#!/usr/bin/env python3
"""check-resync-loss-extract.py — Gate 2 for scripts/check-resync-loss.sh.

Ported from the ga-d32bn forensics prototype (validated against the real
2026-08-31 resync merge, 15913af6a). See that script's header for the full
mechanism this guards against.

This script computes BOTH directions of the same question in one pass over
the four trees (the tree parse is the expensive part; running it twice for
two scripts would double the cost for nothing).

Gate 2 (fork direction) asks: which (declaration, file) pairs did the fork
add that upstream never had an opinion on (present in OURS, absent from
that exact file in both BASE and THEIRS), and which of those are missing
from the MERGE result?

Gate 3 (upstream direction, bead ga-8gpw4) asks the mirror-image question:
which pairs did UPSTREAM add that the fork never had an opinion on (present
in THEIRS, absent from that exact file in both BASE and OURS), and which of
those are missing from the MERGE result? Gate 2 catches a `--theirs`-shaped
resolution discarding the fork's side; Gate 3 catches an `--ours`-shaped
resolution discarding upstream's. ga-d32bn was the first shape, so the gate
was built for it; the 2026-09-04 resync then hit the second shape
(internal/hooks/hooks_test.go resolved --ours, silently dropping all three
upstream-added cursor tests) with Gate 2 reporting a clean "0 missing" on
that exact tree. Gate 3's verdicts carry an `UPSTREAM-` prefix so a reader
(or a grep) can tell the two directions apart without knowing which section
a line came from.

Both directions are keyed by the (name, file) pair throughout, never by
bare name alone — a name that also exists somewhere unrelated in the two
other trees must not hide a real added declaration of the same name in a
different file. For each missing pair this script runs a plain
`git merge-file -p` over the three versions of the file that declared it
and checks whether the declaration's name still appears in that output.
The oracle is direction-agnostic: `git merge-file -p OURS BASE THEIRS` is
the text a plain 3-way merge would produce, and a name surviving outside
every conflict hunk in it would have been delivered unattended whichever
side contributed it:

  - present  -> RESOLUTION-BUG: a naive 3-way merge would have kept it: the
                loss was introduced by how THIS merge was hand-resolved, not
                by an unavoidable conflict.
  - absent   -> MERGE-OUTCOME: even a naive 3-way merge would not have kept
                it (the surrounding lines were rewritten by both sides), so
                the loss (if real) needs human triage rather than a
                mechanical fix.

A pair whose file carries the `linguist-generated` .gitattributes attribute
(AGENTS.md "Resync conventions" rule 1) is reported EXEMPT instead: that
file is licensed to be taken wholesale from upstream, so a "missing"
declaration there is the intended outcome, not a defect, and never fails
the gate.

The declaration extraction is a lightweight line-oriented scanner, not a Go
parser: it recognizes `func`, `func (recv)`, `type`, top-level `const`/`var`,
and names inside a grouped `const ( ... )` / `var ( ... )` / `type ( ... )`
block. That is sufficient to reproduce the counts in the ga-d32bn bead and
the resync-loss-mechanism bd memory; it is not a substitute for `go vet`.

Usage:
  check-resync-loss-extract.py BASE OURS THEIRS MERGE \
      --summary-out FILE --detail-out FILE \
      --upstream-report-out FILE --upstream-detail-out FILE

Prints one human-formatted report line per missing FORK-added declaration to
stdout, decl-count bookkeeping to stderr, a tiny `KEY=N` summary (no JSON,
no `tail` needed to read it) to --summary-out for the caller to gate on, and
the same per-declaration data as tab-separated `VERDICT\tNAME\tFILE` lines
to --detail-out for the caller to `cut -f`/`awk -F'\t'` instead of
re-parsing the human-formatted stdout report. The upstream direction's
report and detail go to their own two files rather than being interleaved
into stdout/--detail-out, so the caller renders the two gates as separate
sections and the fork-direction detail file keeps the exact format its
existing consumers (Gate 1's near-theirs corroboration) already parse.
"""
import argparse
import os
import re
import subprocess
import sys
import tempfile

RE_FUNC = re.compile(r"^func\s+([A-Za-z_][A-Za-z_0-9]*)\s*[\(\[]")
RE_METH = re.compile(r"^func\s+\([^)]*\)\s*([A-Za-z_][A-Za-z_0-9]*)\s*[\(\[]")
RE_TYPE = re.compile(r"^type\s+([A-Za-z_][A-Za-z_0-9]*)")
RE_CV = re.compile(r"^(?:const|var)\s+([A-Za-z_][A-Za-z_0-9]*)")
RE_OPEN = re.compile(r"^(?:const|var|type)\s*\($")
RE_GRP = re.compile(r"^\s+([A-Za-z_][A-Za-z_0-9]*)(?:\s*,\s*[A-Za-z_][A-Za-z_0-9]*)*\s*(?:[A-Za-z_\[\*].*)?=")
RE_GRP2 = re.compile(r"^\s+([A-Za-z_][A-Za-z_0-9]*)\s+[A-Za-z_\[\*][^=]*$")


def run(args, **kw):
    return subprocess.run(args, capture_output=True, check=True, **kw)


def decls(repo, ref):
    """Top-level Go declarations at `ref`: name -> sorted list of files."""
    out = {}
    files = run(["git", "-C", repo, "ls-tree", "-r", "--name-only", ref]).stdout.decode().split("\n")
    gofiles = [f for f in files if f.endswith(".go")]
    req = "".join(f"{ref}:{f}\n" for f in gofiles)
    proc = run(["git", "-C", repo, "cat-file", "--batch"], input=req.encode())
    data = proc.stdout
    pos = 0
    idx = 0
    while pos < len(data) and idx < len(gofiles):
        nl = data.index(b"\n", pos)
        parts = data[pos:nl].decode().split()
        if len(parts) < 3:
            pos = nl + 1
            idx += 1
            continue
        size = int(parts[2])
        body = data[nl + 1 : nl + 1 + size].decode("utf-8", "replace")
        pos = nl + 1 + size + 1
        fname = gofiles[idx]
        idx += 1
        ingroup = False
        for line in body.split("\n"):
            if RE_OPEN.match(line):
                ingroup = True
                continue
            if ingroup:
                if line.startswith(")"):
                    ingroup = False
                    continue
                m = RE_GRP.match(line) or RE_GRP2.match(line)
                if m:
                    out.setdefault(m.group(1), set()).add(fname)
                continue
            for pat in (RE_METH, RE_FUNC, RE_TYPE, RE_CV):
                m = pat.match(line)
                if m:
                    out.setdefault(m.group(1), set()).add(fname)
                    break
    return {k: sorted(v) for k, v in out.items()}


def blob_or_none(repo, ref, path):
    try:
        return run(["git", "-C", repo, "show", f"{ref}:{path}"]).stdout
    except subprocess.CalledProcessError:
        return None


def decl_pairs(d):
    """Flatten a name -> [files] map (as returned by decls()) into a set of
    (name, file) pairs."""
    return {(name, f) for name, files in d.items() for f in files}


def is_generated(repo, path):
    """True iff `path` carries the `linguist-generated` .gitattributes
    attribute — the same check scripts/check-resync-loss.sh's Gate 1
    is_exempt() makes, applied here so Gate 2 stops failing on declarations
    "lost" from a file AGENTS.md rule 1 already licenses taking wholesale
    from upstream (e.g. internal/api/genclient/client_gen.go)."""
    proc = subprocess.run(
        ["git", "-C", repo, "check-attr", "linguist-generated", "--", path],
        capture_output=True,
        check=False,
    )
    return proc.stdout.decode("utf-8", "replace").rstrip("\n").endswith(": linguist-generated: set")


# git merge-file's conflict markers: "<<<<<<< <ours-label>", "=======",
# ">>>>>>> <theirs-label>". Matched with DOTALL+MULTILINE so a conflict
# block spanning many lines is removed as one unit.
_CONFLICT_HUNK_RE = re.compile(
    r"^<<<<<<<[^\n]*\n.*?^=======\n.*?^>>>>>>>[^\n]*\n?",
    re.DOTALL | re.MULTILINE,
)


def strip_conflict_hunks(text):
    """Remove every conflict-marked block (both sides) from merge-file output.

    A name that appears only inside a conflict marker never reached the
    merge result on its own: a human had to make a judgment call for that
    hunk, and content on EITHER side of the conflict — ours' addition
    included — could have been discarded as part of resolving it. Content
    that survives stripping is content a plain, unattended 3-way merge
    would have delivered with no conflict at all.
    """
    return _CONFLICT_HUNK_RE.sub("", text)


def three_way_merge_text(repo, base, ours, theirs, path, cache):
    """git merge-file -p over the three blobs at `path`; memoized per path.

    Returns the text a plain, unattended 3-way merge would produce, with
    every conflict-marked hunk stripped (see strip_conflict_hunks) when
    `git merge-file` reported a conflict (non-zero exit — its exit code is
    the conflict count, not a boolean, so any non-zero value counts).
    """
    if path in cache:
        return cache[path]
    with tempfile.TemporaryDirectory() as td:
        ours_p = os.path.join(td, "ours")
        base_p = os.path.join(td, "base")
        theirs_p = os.path.join(td, "theirs")
        for dest, ref in ((ours_p, ours), (base_p, base), (theirs_p, theirs)):
            with open(dest, "wb") as fh:
                fh.write(blob_or_none(repo, ref, path) or b"")
        proc = subprocess.run(
            ["git", "merge-file", "-p", ours_p, base_p, theirs_p],
            capture_output=True,
            check=False,
        )
        raw_text = proc.stdout.decode("utf-8", "replace")
        clean_text = strip_conflict_hunks(raw_text) if proc.returncode != 0 else raw_text
    cache[path] = clean_text
    return clean_text


def analyze_direction(repo, base, ours, theirs, missing_pairs, cache, verdict_prefix):
    """Classify every missing (decl, file) pair; return counts and report lines.

    Shared by Gate 2 (verdict_prefix "") and Gate 3 (verdict_prefix
    "UPSTREAM-"): the classification is identical in both directions, only
    the set of pairs fed in and the label printed differ. `cache` is the
    per-path `git merge-file` memo, shared across both directions because a
    single file can lose declarations in both at once and re-merging it
    would be pure waste.

    Returns (bugs, outcomes, exempt, report_lines, detail_lines).
    """
    bug = 0
    outcome = 0
    exempt = 0
    report_lines = []
    detail_lines = []
    for name, fname in missing_pairs:
        if is_generated(repo, fname):
            # AGENTS.md rule 1 licenses taking this file wholesale from
            # upstream; Gate 1 already exempts the file itself, and a
            # declaration "lost" from it is that same licensed outcome,
            # not a resync-resolution defect. Reported for visibility,
            # not counted toward RESOLUTION-BUG/MERGE-OUTCOME, and never
            # fails the gate.
            exempt += 1
            verdict = verdict_prefix + "EXEMPT"
            report_lines.append(f"  {verdict:16s} {name}  ({fname})  (generated, AGENTS.md rule 1)")
            detail_lines.append(f"{verdict}\t{name}\t{fname}")
            continue
        clean_text = three_way_merge_text(repo, base, ours, theirs, fname, cache)
        pat = re.compile(r"\b" + re.escape(name) + r"\b")
        if pat.search(clean_text):
            # Survives outside every conflict-marked hunk (or there was
            # no conflict at all): a plain, unattended 3-way merge would
            # have delivered it. Losing it was a defect in how THIS
            # merge was hand-resolved, not an unavoidable conflict.
            verdict = verdict_prefix + "RESOLUTION-BUG"
            bug += 1
        else:
            # Either absent outright, or present only inside a conflict
            # marker a human had to resolve by hand — even git's own
            # automatic merge would not have delivered it standalone.
            verdict = verdict_prefix + "MERGE-OUTCOME"
            outcome += 1
        report_lines.append(f"  {verdict:16s} {name}  ({fname})")
        # Tab-separated for the shell caller to `cut -f`/`awk -F'\t'`
        # instead of re-parsing the human-formatted line above — a name
        # or file containing a space or paren would otherwise silently
        # corrupt that split.
        detail_lines.append(f"{verdict}\t{name}\t{fname}")
    return bug, outcome, exempt, report_lines, detail_lines


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("base")
    ap.add_argument("ours")
    ap.add_argument("theirs")
    ap.add_argument("merge")
    ap.add_argument("--repo", default=None, help="repo root (default: discover via git rev-parse)")
    ap.add_argument("--summary-out", required=True)
    ap.add_argument(
        "--detail-out",
        required=True,
        help="machine-parseable VERDICT\\tNAME\\tFILE, one line per missing fork-added declaration",
    )
    ap.add_argument(
        "--upstream-report-out",
        required=True,
        help="Gate 3 (upstream direction) human-formatted report",
    )
    ap.add_argument(
        "--upstream-detail-out",
        required=True,
        help="machine-parseable VERDICT\\tNAME\\tFILE, one line per missing upstream-added declaration",
    )
    args = ap.parse_args()

    repo = args.repo or run(["git", "rev-parse", "--show-toplevel"]).stdout.decode().strip()

    d_base = decls(repo, args.base)
    d_ours = decls(repo, args.ours)
    d_theirs = decls(repo, args.theirs)
    d_merge = decls(repo, args.merge)

    # Keyed by (name, file) pairs throughout, not by bare name: computing
    # "fork-added" as a set of bare names (set(d_ours) - set(d_base) -
    # set(d_theirs)) silently dropped any pair whose NAME exists anywhere
    # upstream, even in a wholly unrelated package/file — e.g. a fork-added
    # `sortedKeys` in cmd/gc/dolt_boot_drain.go was invisible merely because
    # some other, unrelated `sortedKeys` existed elsewhere in BASE or
    # THEIRS. Measured against the 2026-08-31 resync (15913af6a), that
    # bare-name exclusion hid 8 real losses the pair-keyed set below
    # catches, including the shared-`_test.go`-helper shape ga-d32bn exists
    # to catch in the first place.
    ours_pairs = decl_pairs(d_ours)
    base_pairs = decl_pairs(d_base)
    theirs_pairs = decl_pairs(d_theirs)
    merge_pairs = decl_pairs(d_merge)

    forkadded_pairs = ours_pairs - base_pairs - theirs_pairs
    missing_pairs = sorted(forkadded_pairs - merge_pairs)

    # Gate 3 (ga-8gpw4): the exact mirror image. Upstream added it, the fork
    # never had an opinion on it (absent from that same file in both BASE and
    # OURS), and the merge does not have it — an `--ours`-shaped resolution
    # discarded it.
    upstream_added_pairs = theirs_pairs - base_pairs - ours_pairs
    upstream_missing_pairs = sorted(upstream_added_pairs - merge_pairs)

    # A file BASE had that the fork deleted before the merge is a standing
    # fork decision, not a resync mishandling: upstream continuing to add
    # declarations to a file this fork no longer carries cannot be "lost" by
    # the merge. Reported in its own bucket so the operator still sees it,
    # never counted as a defect. Kept in lockstep with the identical skip in
    # check-resync-loss.sh's Gate 3 file-level loop — the two must agree, or
    # one level exempts what the other fails on.
    fork_deleted_files = set(
        f
        for f in run(["git", "-C", repo, "diff", "--name-only", "--diff-filter=D", args.base, args.ours])
        .stdout.decode()
        .split("\n")
        if f
    )
    upstream_fork_deleted_pairs = [p for p in upstream_missing_pairs if p[1] in fork_deleted_files]
    upstream_missing_pairs = [p for p in upstream_missing_pairs if p[1] not in fork_deleted_files]
    print(f"# fork-added (decl, file) pairs (ours - base - theirs): {len(forkadded_pairs)}", file=sys.stderr)
    print(f"# fork-added (decl, file) pairs still missing from merge: {len(missing_pairs)}", file=sys.stderr)
    print(
        f"# upstream-added (decl, file) pairs (theirs - base - ours): {len(upstream_added_pairs)}",
        file=sys.stderr,
    )
    print(
        f"# upstream-added (decl, file) pairs still missing from merge: {len(upstream_missing_pairs)} "
        f"(+{len(upstream_fork_deleted_pairs)} in files the fork had deleted)",
        file=sys.stderr,
    )

    # One merge-file memo shared by both directions: the same file can lose
    # declarations in both at once (a hand-resolved shared _test.go is the
    # canonical case), and re-merging it per direction would be pure waste.
    cache = {}

    bug, outcome, exempt, report_lines, detail_lines = analyze_direction(
        repo, args.base, args.ours, args.theirs, missing_pairs, cache, ""
    )
    for line in report_lines:
        print(line)
    with open(args.detail_out, "w", encoding="utf-8") as fh:
        for line in detail_lines:
            fh.write(line + "\n")

    up_bug, up_outcome, up_exempt, up_report_lines, up_detail_lines = analyze_direction(
        repo, args.base, args.ours, args.theirs, upstream_missing_pairs, cache, "UPSTREAM-"
    )
    for name, fname in upstream_fork_deleted_pairs:
        verdict = "UPSTREAM-FORK-DELETED"
        up_report_lines.append(f"  {verdict:16s} {name}  ({fname})  (fork deleted this file before the merge)")
        up_detail_lines.append(f"{verdict}\t{name}\t{fname}")
    with open(args.upstream_report_out, "w", encoding="utf-8") as fh:
        for line in up_report_lines:
            fh.write(line + "\n")
    with open(args.upstream_detail_out, "w", encoding="utf-8") as fh:
        for line in up_detail_lines:
            fh.write(line + "\n")

    with open(args.summary_out, "w", encoding="utf-8") as fh:
        fh.write(f"RESOLUTION_BUGS={bug}\n")
        fh.write(f"MERGE_OUTCOMES={outcome}\n")
        fh.write(f"EXEMPT={exempt}\n")
        fh.write(f"MISSING={len(missing_pairs)}\n")
        fh.write(f"UPSTREAM_RESOLUTION_BUGS={up_bug}\n")
        fh.write(f"UPSTREAM_MERGE_OUTCOMES={up_outcome}\n")
        fh.write(f"UPSTREAM_EXEMPT={up_exempt}\n")
        fh.write(f"UPSTREAM_MISSING={len(upstream_missing_pairs)}\n")
        fh.write(f"UPSTREAM_FORK_DELETED={len(upstream_fork_deleted_pairs)}\n")


if __name__ == "__main__":
    main()

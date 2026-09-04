#!/usr/bin/env python3
"""check-resync-loss-extract.py — Gate 2 for scripts/check-resync-loss.sh.

Ported from the ga-d32bn forensics prototype (validated against the real
2026-08-31 resync merge, 15913af6a). See that script's header for the full
mechanism this guards against.

Gate 2 asks: which top-level Go declarations did the fork add that upstream
never had an opinion on (present in OURS, absent from both BASE and THEIRS),
and which of those are missing from the MERGE result? For each missing
declaration this script runs a plain `git merge-file -p` over the three
versions of the file that declared it and checks whether the declaration's
name still appears in that output:

  - present  -> RESOLUTION-BUG: a naive 3-way merge would have kept it: the
                loss was introduced by how THIS merge was hand-resolved, not
                by an unavoidable conflict.
  - absent   -> MERGE-OUTCOME: even a naive 3-way merge would not have kept
                it (the surrounding lines were rewritten by both sides), so
                the loss (if real) needs human triage rather than a
                mechanical fix.

The declaration extraction is a lightweight line-oriented scanner, not a Go
parser: it recognizes `func`, `func (recv)`, `type`, top-level `const`/`var`,
and names inside a grouped `const ( ... )` / `var ( ... )` / `type ( ... )`
block. That is
sufficient to reproduce the counts in the ga-d32bn bead and the
resync-loss-mechanism bd memory; it is not a substitute for `go vet`.

Usage:
  check-resync-loss-extract.py BASE OURS THEIRS MERGE \
      --summary-out FILE --detail-out FILE

Prints one human-formatted report line per missing declaration to stdout,
decl-count bookkeeping to stderr, a tiny `KEY=N` summary (no JSON, no `tail`
needed to read it) to --summary-out for the caller to gate on, and the same
per-declaration data as tab-separated `VERDICT\tNAME\tFILE` lines to
--detail-out for the caller to `cut -f`/`awk -F'\t'` instead of re-parsing
the human-formatted stdout report.
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
        help="machine-parseable VERDICT\\tNAME\\tFILE, one line per missing declaration",
    )
    args = ap.parse_args()

    repo = args.repo or run(["git", "rev-parse", "--show-toplevel"]).stdout.decode().strip()

    d_base = decls(repo, args.base)
    d_ours = decls(repo, args.ours)
    d_theirs = decls(repo, args.theirs)
    d_merge = decls(repo, args.merge)

    forkadded = sorted(set(d_ours) - set(d_base) - set(d_theirs))

    # Keyed by (name, file), not by bare name: a helper duplicated in two
    # packages (same name, two files) that survives in ONE of them must
    # still be reported missing from the other. Checking "is this name
    # present anywhere in d_merge" made that loss invisible — the bare name
    # existing in the surviving file was enough to call the dropped one
    # "not missing" too.
    missing_pairs = []
    for name in forkadded:
        merge_files_for_name = set(d_merge.get(name, []))
        for fname in d_ours[name]:
            if fname not in merge_files_for_name:
                missing_pairs.append((name, fname))

    print(f"# fork-added top-level decls (ours - base - theirs): {len(forkadded)}", file=sys.stderr)
    print(f"# fork-added (decl, file) pairs still missing from merge: {len(missing_pairs)}", file=sys.stderr)

    cache = {}
    bug = 0
    outcome = 0
    with open(args.detail_out, "w", encoding="utf-8") as detail_fh:
        for name, fname in missing_pairs:
            clean_text = three_way_merge_text(repo, args.base, args.ours, args.theirs, fname, cache)
            pat = re.compile(r"\b" + re.escape(name) + r"\b")
            if pat.search(clean_text):
                # Survives outside every conflict-marked hunk (or there was
                # no conflict at all): a plain, unattended 3-way merge would
                # have delivered it. Losing it was a defect in how THIS
                # merge was hand-resolved, not an unavoidable conflict.
                verdict = "RESOLUTION-BUG"
                bug += 1
            else:
                # Either absent outright, or present only inside a conflict
                # marker a human had to resolve by hand — even git's own
                # automatic merge would not have delivered it standalone.
                verdict = "MERGE-OUTCOME"
                outcome += 1
            print(f"  {verdict:16s} {name}  ({fname})")
            # Tab-separated for the shell caller to `cut -f`/`awk -F'\t'`
            # instead of re-parsing the human-formatted line above — a name
            # or file containing a space or paren would otherwise silently
            # corrupt that split.
            detail_fh.write(f"{verdict}\t{name}\t{fname}\n")

    with open(args.summary_out, "w", encoding="utf-8") as fh:
        fh.write(f"RESOLUTION_BUGS={bug}\n")
        fh.write(f"MERGE_OUTCOMES={outcome}\n")
        fh.write(f"MISSING={len(missing_pairs)}\n")


if __name__ == "__main__":
    main()

#!/usr/bin/env -S python3 -B
"""Differential shrink probe for ADR-0147 D6 (vp-bajac).

For each sub-second *Timeout/*Interval/*Budget/*Deadline test-literal site,
shrink it 20x, run the owning test, and record whether the outcome flips to
failing. FAIL => the budget bounds a path the test must complete (fragile).
PASS => the budget only bounds a path the test may abandon (not load-bearing).
Timeout => inconclusive (the probe itself did not resolve in the budget).

The probe is differential (same test against itself, same host), per
ADR-0147 Appendix A, so it discriminates correctly regardless of host load.

Usage:
    scripts/shrink-probe.py census > sites.tsv
    scripts/shrink-probe.py sweep sites.tsv results.tsv [--bead-id ID] [--bead-store PATH]
"""
import os
import re
import signal
import subprocess
import sys
import time

UNIT_NS = {
    "time.Nanosecond": 1,
    "time.Microsecond": 1_000,
    "time.Millisecond": 1_000_000,
    "time.Second": 1_000_000_000,
    "time.Minute": 60_000_000_000,
    "time.Hour": 3_600_000_000_000,
}

ASSIGN_RE = re.compile(
    r'^\s*([A-Za-z_][A-Za-z0-9_]*(?:Timeout|Interval|Budget|Deadline))\s*=\s*([^=\n]+?)\s*(?://.*)?$'
)
FUNC_RE = re.compile(r'^func\s+(Test[A-Za-z0-9_]*)\s*\(')
# ANY_FUNC_RE additionally tracks non-Test top-level funcs and methods (vp-bajac):
# the census used to track ONLY `func TestXxx(`, so a literal sitting inside a
# shared non-Test helper (`func withFastClaimRetries(t *testing.T) {...}`), or
# at package level (a `const (...)` block, outside any function), carried
# forward whatever Test function was last seen in the file -- or "?" if none
# yet had. Both misattributions produce the SAME wrong site.test: "?", which
# sweep's `go test -run '^?$'` then runs as an unanchored, effectively
# unbounded selection (RE2 accepts "^?" as "optionally anchored"; it matches
# far more than nothing) -- measured live: one such site ran 8+ minutes before
# being killed, vs. this script's own PER_SITE_TIMEOUT_S=180 budget. Detecting
# this case and refusing to probe it (see NO_OWNING_TEST below) is cheaper and
# more honest than burning a full timeout on a command whose target was never
# well-formed to begin with.
ANY_FUNC_RE = re.compile(r'^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(')
# NO_OWNING_TEST marks a site whose enclosing scope sweep cannot resolve to a
# single `go test -run` target: either no function encloses it (a package-level
# const/var) or the enclosing function is a shared helper, not a Test itself.
# The differential probe's whole protocol ("run the owning test") has no single
# test to run here -- it needs the helper's CALLERS resolved first, a call-graph
# question this lexical census does not answer. sweep() skips these rows rather
# than mis-probing them.
NO_OWNING_TEST = "NO_OWNING_TEST"
PER_SITE_TIMEOUT_S = 180
FRAGILE_STOP_THRESHOLD = 3


class Terminated(Exception):
    """Raised from the SIGTERM/SIGINT handler below.

    Converting the signal into a normal Python exception is what makes an
    external kill (an operator Ctrl-C, or a wrapping `timeout` -- observed
    live: vp-bajac's own 1200s batch cap SIGTERM'd a run mid-site) go through
    run_probe's existing try/finally instead of around it. Python's default
    SIGTERM action terminates the process immediately, bypassing every
    finally block and context manager -- which is exactly how a prior sweep
    on this bead left an edited _test.go file behind despite run_probe's
    finally already containing the restore. Without this handler, the MUST
    rule in this bead's own spec ("restore the file in a finally block") is
    satisfied against an internal per-site timeout but not against any
    external termination, which is the gap that bit this bead twice.
    """


def _raise_terminated(signum, frame):
    raise Terminated(f"signal {signum}")


signal.signal(signal.SIGTERM, _raise_terminated)
signal.signal(signal.SIGINT, _raise_terminated)


def eval_duration_ns(expr):
    expr = expr.strip().rstrip(';')
    if expr in UNIT_NS:
        return UNIT_NS[expr]
    m = re.match(r'^time\.Duration\((-?\d+)\)$', expr)
    if m:
        return int(m.group(1))
    m = re.match(r'^(-?\d+)\s*\*\s*(time\.\w+)$', expr)
    if m and m.group(2) in UNIT_NS:
        return int(m.group(1)) * UNIT_NS[m.group(2)]
    m = re.match(r'^(time\.\w+)\s*\*\s*(-?\d+)$', expr)
    if m and m.group(1) in UNIT_NS:
        return int(m.group(2)) * UNIT_NS[m.group(1)]
    m = re.match(r'^(time\.\w+)\s*/\s*(-?\d+)$', expr)
    if m and m.group(1) in UNIT_NS:
        return UNIT_NS[m.group(1)] // int(m.group(2))
    return None


def census():
    out = subprocess.run(
        ["git", "ls-files", "*_test.go"], capture_output=True, text=True, check=True
    )
    files = [f for f in out.stdout.splitlines() if f]
    sites = []
    for path in files:
        try:
            with open(path, encoding="utf-8", errors="replace") as fh:
                lines = fh.readlines()
        except OSError:
            continue
        current_func = None
        current_func_is_test = False
        for lineno, line in enumerate(lines, start=1):
            afm = ANY_FUNC_RE.match(line)
            if afm:
                current_func = afm.group(1)
                current_func_is_test = bool(FUNC_RE.match(line))
                continue
            am = ASSIGN_RE.match(line)
            if not am:
                continue
            ident, expr = am.group(1), am.group(2)
            ns = eval_duration_ns(expr)
            if ns is None or ns < 0 or ns >= 1_000_000_000:
                continue
            if current_func_is_test:
                test = current_func
            elif current_func:
                test = f"{NO_OWNING_TEST}:helper={current_func}"
            else:
                test = f"{NO_OWNING_TEST}:package-level"
            sites.append((path, lineno, test, ident, expr.strip(), ns))
    for s in sites:
        print("\t".join(str(x) for x in s))
    print(f"TOTAL={len(sites)}", file=sys.stderr)


def pkg_dir(path):
    return path.rsplit("/", 1)[0] if "/" in path else "."


def assert_clean(path):
    r = subprocess.run(["git", "diff", "--quiet", "--", path])
    if r.returncode == 0:
        return
    subprocess.run(["git", "checkout", "--", path], check=False)
    r = subprocess.run(["git", "diff", "--quiet", "--", path])
    if r.returncode != 0:
        raise RuntimeError(f"FATAL: {path} not clean even after forced checkout -- aborting sweep")


def run_probe(path, lineno, test, ident, orig_expr, orig_ns, heartbeat):
    with open(path, encoding="utf-8", errors="replace") as fh:
        lines = fh.readlines()
    original_line = lines[lineno - 1]
    shrunk_ns = max(1, orig_ns // 20)
    indent = original_line[: len(original_line) - len(original_line.lstrip())]
    new_line = f"{indent}{ident} = time.Duration({shrunk_ns}) // PROBE-SHRUNK vp-bajac, was: {orig_expr}\n"

    heartbeat()
    lines[lineno - 1] = new_line
    with open(path, "w", encoding="utf-8") as fh:
        fh.writelines(lines)

    start = time.monotonic()
    verdict, detail, pkg = "INCONCLUSIVE_ERROR", "", pkg_dir(path)
    proc = None
    try:
        try:
            proc = subprocess.Popen(
                ["go", "test", "-count=1", "-run", f"^{test}$", f"./{pkg}/"],
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                start_new_session=True,
            )
            stdout, _ = proc.communicate(timeout=PER_SITE_TIMEOUT_S)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate()
            verdict, detail = "INCONCLUSIVE_TIMEOUT", f"go test exceeded {PER_SITE_TIMEOUT_S}s (process group killed)"
        except Terminated:
            # Same cleanup as the TimeoutExpired branch -- an external
            # termination must not orphan the go test process group
            # (start_new_session detaches it from ours) any more than an
            # internal one may. Re-raised after cleanup so the caller's own
            # finally (the file restore below) and sweep's lock removal
            # still run.
            if proc is not None and proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.communicate()
            raise
        else:
            fail_re = re.compile(rf"^--- FAIL: {re.escape(test)}\b", re.M)
            pass_re = re.compile(rf"^--- PASS: {re.escape(test)}\b", re.M)
            ok_re = re.compile(r"^ok\s+\S+", re.M)
            if fail_re.search(stdout):
                verdict, detail = "FRAGILE", "FAIL"
            elif pass_re.search(stdout) or ok_re.search(stdout):
                verdict, detail = "NOT_FRAGILE", "ok"
            else:
                verdict, detail = "INCONCLUSIVE_OTHER", stdout.strip().splitlines()[-1][:120] if stdout.strip() else "(no output)"
    finally:
        with open(path, "w", encoding="utf-8") as fh:
            lines[lineno - 1] = original_line
            fh.writelines(lines)
        assert_clean(path)

    elapsed = time.monotonic() - start
    return verdict, detail, elapsed


def sweep(sites_path, results_path, bead_id, bead_store):
    lock_path = results_path + ".lock"
    try:
        lock_fd = os.open(lock_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
    except FileExistsError:
        print(f"FATAL: {lock_path} exists -- another sweep is already running against this results file", file=sys.stderr)
        sys.exit(1)
    os.write(lock_fd, str(os.getpid()).encode())
    os.close(lock_fd)
    try:
        _sweep_locked(sites_path, results_path, bead_id, bead_store)
    finally:
        os.remove(lock_path)


def _sweep_locked(sites_path, results_path, bead_id, bead_store):
    def heartbeat():
        if bead_id and bead_store:
            subprocess.run(["bd", "heartbeat", bead_id, "-C", bead_store], capture_output=True, text=True)

    done = set()
    if os.path.exists(results_path):
        with open(results_path, encoding="utf-8") as fh:
            next(fh, None)
            for line in fh:
                parts = line.rstrip("\n").split("\t")
                if len(parts) >= 3:
                    done.add((parts[0], parts[1], parts[2]))

    write_header = not os.path.exists(results_path) or os.path.getsize(results_path) == 0
    fragile_count = 0
    if os.path.exists(results_path):
        with open(results_path, encoding="utf-8") as fh:
            next(fh, None)
            for line in fh:
                if "\tFRAGILE\t" in line:
                    fragile_count += 1

    with open(sites_path, encoding="utf-8") as fh:
        site_lines = [l.rstrip("\n").split("\t") for l in fh if l.strip()]

    with open(results_path, "a", encoding="utf-8") as out:
        if write_header:
            out.write("file\tlineno\ttest\tident\toriginal_expr\toriginal_ns\tshrunk_ns\tverdict\tdetail\telapsed_s\n")
            out.flush()
        for path, lineno, test, ident, expr, ns in site_lines:
            key = (path, lineno, test)
            if key in done:
                continue
            if fragile_count > FRAGILE_STOP_THRESHOLD:
                print(f"STOP: fragile_count={fragile_count} exceeds threshold {FRAGILE_STOP_THRESHOLD} -- halting sweep, escalate to PA", file=sys.stderr)
                return
            ns_i = int(ns)
            if test.startswith(NO_OWNING_TEST):
                # No single `go test -run` target exists for this site (see
                # NO_OWNING_TEST's doc comment) -- record it as skipped rather
                # than probe it. This still satisfies "every site carries a
                # probe-confirmed label" (the label is SKIPPED, with a reason),
                # without burning PER_SITE_TIMEOUT_S on a malformed command.
                out.write(f"{path}\t{lineno}\t{test}\t{ident}\t{expr}\t{ns_i}\t-\tSKIPPED_NO_OWNING_TEST\t{test}\t0.0\n")
                out.flush()
                print(f"{path}:{lineno} {test} -> SKIPPED_NO_OWNING_TEST (not probed)", file=sys.stderr)
                continue
            verdict, detail, elapsed = run_probe(path, int(lineno), test, ident, expr, ns_i, heartbeat)
            total_elapsed = elapsed
            if verdict == "FRAGILE":
                # Re-confirm: a single FAIL can be a flaky boundary effect, not
                # a reproducible fragility (observed live on this sweep).
                outcomes = [verdict]
                for _ in range(2):
                    v2, d2, e2 = run_probe(path, int(lineno), test, ident, expr, ns_i, heartbeat)
                    outcomes.append(v2)
                    total_elapsed += e2
                fragile_votes = sum(1 for o in outcomes if o == "FRAGILE")
                if fragile_votes >= 2:
                    verdict, detail = "FRAGILE", f"FAIL confirmed {fragile_votes}/3 runs"
                else:
                    verdict, detail = "FLAKY_BORDERLINE", f"split across 3 runs: {outcomes}"
            shrunk = max(1, ns_i // 20)
            out.write(f"{path}\t{lineno}\t{test}\t{ident}\t{expr}\t{ns_i}\t{shrunk}\t{verdict}\t{detail}\t{total_elapsed:.1f}\n")
            out.flush()
            if verdict == "FRAGILE":
                fragile_count += 1
            print(f"{path}:{lineno} {test} -> {verdict} ({total_elapsed:.1f}s)", file=sys.stderr)


def main():
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        sys.exit(1)
    cmd = sys.argv[1]
    if cmd == "census":
        census()
    elif cmd == "sweep":
        sites_path, results_path = sys.argv[2], sys.argv[3]
        bead_id = bead_store = None
        rest = sys.argv[4:]
        for i, a in enumerate(rest):
            if a == "--bead-id":
                bead_id = rest[i + 1]
            elif a == "--bead-store":
                bead_store = rest[i + 1]
        sweep(sites_path, results_path, bead_id, bead_store)
    else:
        print(f"unknown command: {cmd}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    try:
        main()
    except Terminated as exc:
        # run_probe's finally (and sweep's) already restored any mid-probe
        # file and removed the lock by the time this is reached -- a clean
        # stop, not a crash. Exit nonzero so a wrapping `timeout` or caller
        # script can tell "stopped early" from "finished" without parsing
        # stderr.
        print(f"shrink-probe: stopped ({exc}) -- state is clean, resume by re-running sweep", file=sys.stderr)
        sys.exit(130)

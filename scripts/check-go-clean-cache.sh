#!/usr/bin/env bash
#
# check-go-clean-cache.sh -- keep `go clean -cache` out of the codebase.
#
# gocacheguard:allow-file  this guard is the definition of the rule; its own
#                          patterns and messages necessarily name the command.
#
# WHAT THIS CATCHES, AND WHAT IT DOES NOT
#
# AGENTS.md ("Build Cache Conventions") hard-bans `go clean -cache`: run against
# the shared GOCACHE it removes all 256 shard directories, hot entries included,
# and concurrent builds then fail outright -- cmd/go resolves an entry, hands
# the path to the compiler or linker, and the tool opens a file that has just
# been unlinked. It broke the fleet on 2026-06-13 (bead vp-g96b) and again on
# 2026-09-05.
#
# This guard stops the command from entering the CODEBASE: a script, a Makefile
# recipe, a CI step, an order, an exec.Command in Go. That is worth having --
# it is how the ban would most plausibly become permanent and automated.
#
# It does NOT stop anyone from RUNNING the command, and it would not have
# prevented either incident. Nobody committed `go clean -cache` on either
# occasion; a process executed it, and neither run appeared in the operator's
# shell history. A commit-time lint cannot see that. The runtime half of the
# enforcement is scripts/go-clean-cache-shim.sh, a `go` wrapper installed ahead
# of the toolchain on PATH. Treat this file as regression prevention, not as
# coverage of the failure mode that motivated it.
#
# SURFACE
#
# Executable surfaces only. Prose is excluded BY CONSTRUCTION rather than by an
# allowlist: AGENTS.md, the engdocs handoffs and the release gates all state the
# ban by quoting it, and a guard that fired on the rule text would be unusable.
# Markdown and plain text are not executed, so they are not scanned at all.
#
# EXEMPTIONS
#
#   * a comment line (first non-blank characters are `#` or `//`) -- a comment
#     cannot execute, and warning about the command in a comment is exactly what
#     scripts/trim-go-build-cache.sh's header does;
#   * a line carrying `gocacheguard:allow` -- for a code line that must contain
#     the string, such as another static guard searching for it. For a command
#     spanning several physical lines (a backslash continuation, or a
#     gofmt-wrapped `exec.Command`), the marker counts on ANY line the command
#     spans;
#   * a file carrying `gocacheguard:allow-file` in its first 40 lines -- for a
#     file whose whole subject is the ban (this guard, the shim, their tests).
#
# Only a comment that OPENS its line is exempt. A trailing comment
# (`cmd  # go clean -cache`) and a C-style block comment (`/* ... */`) are both
# still flagged -- annotate those with `gocacheguard:allow`. This errs toward
# false positives on purpose: a false negative here is silent, and the multi-line
# gap this scanner used to have was silent in exactly the shapes ordinary
# formatting produces.
#
# Usage:
#   check-go-clean-cache.sh            scan every tracked file
#   check-go-clean-cache.sh --staged   scan the staged content only (pre-commit)
#
# Fails CLOSED: if the surface cannot be enumerated, that is a violation, not a
# pass. A guard that reports success when it could not evaluate manufactures the
# false confidence it exists to remove.
#
# This script sets neither GOCACHE nor TMPDIR, and runs no go command at all.

set -uo pipefail
export LC_ALL=C

MODE=full
case "${1:-}" in
--staged) MODE=staged ;;
"") ;;
-h | --help)
	sed -n '/^# Usage:/,/^# Fails CLOSED/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 0
	;;
*)
	echo "usage: $(basename "$0") [--staged]" >&2
	exit 2
	;;
esac

if ! command -v python3 >/dev/null 2>&1; then
	echo "check-go-clean-cache: BLOCKED — python3 is not on PATH; cannot run the scan (fail-closed)." >&2
	echo "  Install python3, or run the scan by hand: git grep -nE 'go[[:space:]]+clean([[:space:]]+-[^ ]+)*[[:space:]]+--?cache'" >&2
	exit 1
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "check-go-clean-cache: BLOCKED — not a git work tree; cannot enumerate the tracked surface (fail-closed)" >&2
	exit 1
fi

report="$(
	GCC_MODE="$MODE" python3 -c '
import os, re, subprocess, sys

MODE = os.environ["GCC_MODE"]

# Executable surfaces only. Anything whose content is prose, a fixture, or a
# vendored third party is out of scope; see the header.
SKIP_SUFFIX = (
    ".md", ".txt", ".golden", ".log", ".csv", ".jsonl", ".sum", ".lock",
    ".patch", ".diff", ".svg", ".png", ".jpg", ".pdf", ".ico", ".gz",
)
SKIP_PREFIX = ("docs/", "engdocs/", "plans/", "specs/", "release-gates/", "vendor/")
SKIP_COMPONENT = ("testdata", "vendor", "node_modules")

# An optional surrounding quote on the flag token, so `go clean "-cache"` and
# the single-quoted spelling are both caught. Assembled via chr(39) because this
# scanner is embedded in a single-quoted shell string and a literal apostrophe
# would terminate it.
QUOTE = "[" + chr(34) + chr(39) + "]?"

# The shell / Makefile / CI form. A parse of the flag list, not a substring
# match: the repetition group lets other flags sit on either side, while
# requiring the token to BE "-cache" keeps "-testcache", "-modcache" and
# "-fuzzcache" from colliding with it. The leading boundary excludes "cargo".
CMD = re.compile(
    r"(?:^|[^A-Za-z0-9_-])"
    r"(?:go|\$\(GO\)|\$\{GO\}|\$GO)"
    r"\s+clean"
    r"(?:\s+-[A-Za-z0-9_=.,-]+)*"
    r"\s+" + QUOTE + r"--?cache(?:=(?P<v>[A-Za-z0-9]+))?" + QUOTE +
    r"(?![A-Za-z0-9_=.-])"
)

# The exec-argv form, e.g. exec.Command("go", "clean", "-cache") in Go, or the
# same shape in a JSON/TOML command array.
# The leading \"go\" is optional but consumed when present, so the reported line
# is the one the CALL starts on rather than the orphaned \"clean\", line in the
# middle of a gofmt-wrapped argument list.
# SEP is whitespace that may also swallow a trailing // line comment. Without
# it, annotating the line the guard REPORTS (the one carrying the opening
# exec.Command) pushed the match start past the comment onto the next argument
# line: the report moved and the call stayed blocked, which is the behaviour
# that made the documented escape hatch look broken. Absorbing the comment puts
# the annotated line inside the match span, where the exemption test sees it.
# Only // is absorbed -- the argv form is Go-specific.
SEP = r"(?:\s|//[^\n]*)*"

ARGV = re.compile(
    r"(?:\"go\"" + SEP + r"," + SEP + r")?"
    r"\"clean\"" + SEP + r","
    r"(?:" + SEP + r"\"[^\"]*\"" + SEP + r",)*"
    + SEP + r"\"--?cache\""
)

# Mirrors Go flag package bool parsing, so "-cache=false" is the no-op cmd/go
# would treat it as -- and so this guard agrees with the runtime shim.
FALSEY = {"0", "f", "F", "false", "FALSE", "False"}

ALLOW_LINE = "gocacheguard" + ":allow"
ALLOW_FILE = ALLOW_LINE + "-file"


def in_surface(path):
    if path.endswith(SKIP_SUFFIX):
        return False
    if path.startswith(SKIP_PREFIX):
        return False
    parts = path.split("/")
    return not any(p in SKIP_COMPONENT for p in parts[:-1])


def git(*args):
    return subprocess.run(
        ["git"] + list(args), stdout=subprocess.PIPE, check=True
    ).stdout


def listing():
    if MODE == "staged":
        raw = git("diff", "--cached", "--name-only", "--diff-filter=ACM", "-z")
    else:
        raw = git("ls-files", "-z")
    return [p.decode("utf-8", "surrogateescape") for p in raw.split(b"\0") if p]


def content(path):
    if MODE == "staged":
        # The INDEX, not the worktree: the commit being made is what is staged,
        # and a file re-dirtied after `git add` must not decide this.
        r = subprocess.run(
            ["git", "show", ":" + path], stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
        return None if r.returncode else r.stdout
    try:
        with open(path, "rb") as fh:
            return fh.read()
    except OSError:
        return None


hits = []
for path in listing():
    if not in_surface(path):
        continue
    blob = content(path)
    if blob is None or b"\0" in blob[:8192]:
        continue
    try:
        text = blob.decode("utf-8")
    except UnicodeDecodeError:
        continue
    lines = text.split("\n")
    if any(ALLOW_FILE in ln for ln in lines[:40]):
        continue

    # Backslash continuations are joined before matching, and each logical line
    # keeps the line number of its FIRST physical line so the report points
    # where a reader will actually find the command. Without this a shell or
    # Makefile command split across lines was invisible to the scan.
    logical = []
    pending, start = "", None
    for n, line in enumerate(lines, 1):
        if start is None:
            start = n
        if line.endswith("\\"):
            pending += line[:-1] + " "
            continue
        logical.append((start, pending + line))
        pending, start = "", None
    if pending:
        logical.append((start, pending))

    def exempt(text_of_line):
        stripped = text_of_line.lstrip()
        if stripped.startswith("#") or stripped.startswith("//"):
            return True
        return ALLOW_LINE in text_of_line and ALLOW_FILE not in text_of_line

    def cmd_hit(line):
        m = CMD.search(line)
        return m is not None and (
            m.group("v") is None or m.group("v") not in FALSEY
        )

    # TWO passes, unioned and deduped by line.
    #
    # The physical pass is a FLOOR and must not be removed. Joining
    # continuations means a comment line ending in a backslash merges into the
    # next line, and the exemption test then sees a logical line beginning with
    # "#" -- so an executing command was silently exempted:
    #
    #     # see the note above \
    #     go clean -cache        <- executes in sh/bash; was MISSED
    #
    # In sh/bash a comment ends at the newline and the next line runs, backslash
    # or not. In make the continuation is real and the comment genuinely does
    # continue. Rather than become language-aware -- narrower, and more to get
    # wrong -- the guard keeps the physical pass for every surface, so the make
    # case is a loud false positive (annotatable) instead of a silent false
    # negative. A false negative here cannot be seen; a false positive can.
    found = {}
    for n, line in enumerate(lines, 1):
        if not exempt(line) and cmd_hit(line):
            found.setdefault(n, line.strip())
    for n, line in logical:
        if not exempt(line) and cmd_hit(line):
            found.setdefault(n, line.strip())
    for n in sorted(found):
        hits.append("%s:%d: %s" % (path, n, found[n]))

    # The argv form is matched against the WHOLE file: \s already spans
    # newlines, so exec.Command("go",\n\t"clean",\n\t"-cache") -- which is
    # exactly what gofmt produces once the call outgrows the line limit -- is a
    # match the moment it is not confined to a single line. The offset is
    # converted back to the line the call STARTS on. Exemption is decided by
    # that starting line, the same rule a one-line match gets.
    for m in ARGV.finditer(text):
        first = text.count("\n", 0, m.start()) + 1
        last = text.count("\n", 0, m.end()) + 1
        # Exemption is decided over EVERY physical line the match spans, the
        # same breadth the CMD path gets from its joined logical line. Deciding
        # it from the first line alone made the advice in the failure message
        # ("annotate that line") not work for a gofmt-wrapped call: the marker
        # took effect only on the middle argument line, and only by breaking
        # the regex rather than by exempting -- undiscoverable, so the reader
        # would give up and reach for allow-file or switch the guard off.
        # NOTE: no apostrophes in this embedded python; it lives inside a
        # single-quoted shell string and one would terminate it.
        if any(exempt(lines[i - 1]) for i in range(first, last + 1)):
            continue
        hits.append("%s:%d: %s" % (path, first, lines[first - 1].strip()))

for h in hits:
    print(h)
sys.exit(1 if hits else 0)
'
)"
status=$?

if [ "$status" -eq 0 ]; then
	exit 0
fi

if [ "$status" -ne 1 ]; then
	echo "check-go-clean-cache: BLOCKED — the scan itself failed (exit $status); treating as a violation (fail-closed)" >&2
	exit 1
fi

{
	echo "check-go-clean-cache: BLOCKED — \`go clean -cache\` must not enter the codebase."
	echo
	printf '%s\n' "$report" | sed 's/^/  /'
	echo
	cat <<'WHY'
Why: run against the shared GOCACHE it removes all 256 shard directories, hot
entries included. Concurrent builds do not merely miss — cmd/go resolves an
entry, hands the path to the compiler or linker, and the tool then opens a file
that has just been unlinked. That is a hard build failure, on stdlib imports
included. It broke the fleet on 2026-06-13 (bead vp-g96b) and again 2026-09-05.
The rule: AGENTS.md, "Build Cache Conventions".

Instead:
  go clean -testcache                       clears test RESULTS only; explicitly
                                            allowed, and safe under concurrency
  scripts/trim-go-build-cache.sh --dry-run  bounded age-based trim of the shared
                                            cache, the supported way to reclaim
                                            space

If a line genuinely must contain the string (a static guard searching for it,
say), annotate that line:

  ... # gocacheguard:allow

or, for a file whose whole subject is the ban, put `gocacheguard:allow-file` in
its first 40 lines with a reason.
WHY
} >&2
exit 1

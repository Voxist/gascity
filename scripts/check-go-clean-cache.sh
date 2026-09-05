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
#     the string, such as another static guard searching for it;
#   * a file carrying `gocacheguard:allow-file` in its first 40 lines -- for a
#     file whose whole subject is the ban (this guard, the shim, their tests).
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

# The shell / Makefile / CI form. A parse of the flag list, not a substring
# match: the repetition group lets other flags sit on either side, while
# requiring the token to BE "-cache" keeps "-testcache", "-modcache" and
# "-fuzzcache" from colliding with it. The leading boundary excludes "cargo".
CMD = re.compile(
    r"(?:^|[^A-Za-z0-9_-])"
    r"(?:go|\$\(GO\)|\$\{GO\}|\$GO)"
    r"\s+clean"
    r"(?:\s+-[A-Za-z0-9_=.,-]+)*"
    r"\s+--?cache(?:=(?P<v>[A-Za-z0-9]+))?"
    r"(?![A-Za-z0-9_=.-])"
)

# The exec-argv form, e.g. exec.Command("go", "clean", "-cache") in Go, or the
# same shape in a JSON/TOML command array.
ARGV = re.compile(r"\"clean\"\s*,(?:\s*\"[^\"]*\"\s*,)*\s*\"--?cache\"")

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


def violation(line):
    m = CMD.search(line)
    if m and (m.group("v") is None or m.group("v") not in FALSEY):
        return True
    return ARGV.search(line) is not None


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
    for n, line in enumerate(lines, 1):
        stripped = line.lstrip()
        if stripped.startswith("#") or stripped.startswith("//"):
            continue
        if ALLOW_LINE in line and ALLOW_FILE not in line:
            continue
        if violation(line):
            hits.append("%s:%d: %s" % (path, n, line.strip()))

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

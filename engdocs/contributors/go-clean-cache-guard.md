# Enforcing the `go clean -cache` ban

AGENTS.md ("Build Cache Conventions") hard-bans `go clean -cache`. Until this
work, nothing enforced it.

This page describes the two layers that now do, **what each one catches and
what it does not**, and how to install and remove the runtime layer.

## Why the ban exists

Run against the shared `GOCACHE`, `go clean -cache` `RemoveAll`s all 256 shard
directories — hot entries included. Concurrent builds do not merely miss:

```
could not import slices (open .../bc/bc66f4...-d: no such file or directory)
link: cannot open file  .../74/74a81b...-d: no such file or directory
```

Hard failures, on stdlib imports. `cmd/go`'s miss handling protects the
*lookup*, not the later *open*: `OutputFile()` returns a path, `cmd/go` hands
that path to the compiler or linker, and an unlink landing in between is ENOENT
at the tool. Go tolerates an entry it never found; it does not tolerate one
that disappears after it was found.

It has happened twice: bead **vp-g96b** (2026-06-13) and again on
**2026-09-05**, when the shared macOS cache went from ~157 GiB to 5.7 GB and
three agents plus a developer push failed mid-build.

## Layer 1 — the repo guard

`scripts/check-go-clean-cache.sh`, wired into `.githooks/pre-commit` (as
`--staged`), `make check-go-clean-cache`, `make check`, and the
`preflight-guards` CI job.

**Catches:** the command entering the codebase — a shell script, a Makefile
recipe (`go clean -cache` or `$(GO) clean -cache`), a CI step, an order TOML,
an `exec.Command("go", "clean", "-cache")` in Go.

**Does not catch:** anyone *running* the command. This is the important
limitation and it should not be glossed over: **neither incident involved a
commit.** In 2026-09-05 the command did not appear in the operator's shell
history at all — a process ran it. A commit-time lint is structurally incapable
of seeing that. Layer 1 is regression prevention; it is not coverage of the
failure mode that motivated it.

### Scanned surface

Executable surfaces only. Prose is excluded **by construction**, not by an
allowlist: AGENTS.md, the engdocs handoffs and the release gates all state the
ban by quoting it, and a guard that fired on the rule text would be unusable.
Markdown, plain text, goldens, `testdata/`, `vendor/`, `docs/`, `engdocs/`,
`plans/`, `specs/` and `release-gates/` are not scanned.

### Exemptions

| Exemption | Use for |
| --- | --- |
| a comment line (`#` or `//` first) | warning about the command, e.g. the header of `scripts/trim-go-build-cache.sh` |
| `gocacheguard:allow` on the line | a code line that must contain the string — another static guard searching for it |
| `gocacheguard:allow-file` in the first 40 lines | a file whose whole subject is the ban (the guard, the shim, their tests) |

A logical command split across physical lines is joined before matching, so a
backslash continuation and the wrapped `exec.Command("go",` / `"clean",` /
`"-cache")` form that **gofmt produces** once the call outgrows the line limit
are both caught. That gap was real until it was found in review: a guard defeated
by ordinary formatting is worse than one defeated by evasion, because the miss
correlates with routine maintenance rather than with somebody trying.

A **physical-line pass is kept as a floor** alongside the joined pass, unioned
and deduped. Joining alone let a comment ending in a backslash swallow the next,
executing line into an exempt logical line — in sh/bash a comment ends at the
newline and the next line runs regardless. In make the continuation is real, so
the floor is a deliberate false positive there: a false positive is loud and
annotatable, a false negative cannot be seen.

For a command spanning several physical lines, `gocacheguard:allow` counts on
**any** line the command spans, so the failure message's advice works wherever
the reader puts it.

Only a comment that *opens* its line is exempt. A trailing comment
(`cmd  # go clean -cache`) and a C-style block comment are both still flagged —
annotate those with `gocacheguard:allow`. This errs toward false positives on
purpose.

The guard deliberately fires on the string in a *message* or a *CI step name*
inside an executable file, not only on something that would execute. That is a
known friction; the fix is normally a two-word reword ("the build-cache ban
guard" rather than "the `go clean -cache` guard"). Narrowing it to
"only what looks like an invocation" was rejected because it reopens
`sh -c "go clean -cache"`.

## Layer 2 — the runtime shim

`scripts/go-clean-cache-shim.sh`, installed as `go` on a directory that
precedes the real toolchain on `PATH`. **This is the layer that would have
stopped both incidents.**

It refuses `go clean -cache` / `--cache` (including combined with other flags,
and behind `go -C dir`) and `exec`s everything else straight through.

**Deliberately not blocked** — blocking any of these would be a false positive,
and a guard that fires on legitimate work is a guard that gets deleted:

- `go clean -testcache` — explicitly allowed by AGENTS.md
- `go clean -modcache`, `go clean -fuzzcache`, bare `go clean`
- `go clean -cache=false` — a no-op in `cmd/go`, so a no-op here

The decision is a **parse of the argument list**, never a grep of the command
line, so `go build ./cmd/go-clean-cache` and `go test -run 'go clean -cache'`
pass through.

`GOFLAGS` is consulted too, but only once the subcommand is known to be `clean`.
`cmd/go` applies a `GOFLAGS` entry to any subcommand that *defines* that flag,
and `go clean` defines `-cache` — so `GOFLAGS=-cache go clean -testcache` would
otherwise present to an argv-only parse as an allowed invocation while `cmd/go`
wiped the cache. Nobody sets that by accident, but the guard's proposition is
that it cannot be bypassed *by* accident.

**The whole file is one brace group**, opened on line 2 and closed on the last
line. The worst state a PATH shim can reach is exiting 0 without `exec`ing —
every build on the host then reports success while producing nothing. A script
whose statements sit at top level reaches that state whenever it is *truncated
at a statement boundary*, which is not a syntax error: measured on the original
layout, a cut at line 100 landed among the function definitions and exited 0
silently. Bash parses a compound command in full before running any of it, so
every prefix of the file except the complete one is now an unterminated `{` —
exit 2, loud. The common `main() { …; }; main "$@"` idiom is **not** enough: a
cut between the closing brace and the invocation still parses and still exits 0.
CASE 20 asserts the invariant for every truncation point in the file. Do not add
statements after the closing brace.

### Blast radius

Installed ahead of the real `go`, this file is in the path of **every Go build
on the host**. The refusal is the small part; the passthrough is the part that
must be perfect. Three properties are load-bearing and are pinned by
`scripts/test-go-clean-cache-shim.sh`:

1. Everything that is not the banned operation is `exec`ed — no extra process,
   no altered exit status, no swallowed signal, no buffered stream. Proven by
   pid identity, not asserted from the source.
2. The decision is a parse, not a substring match.
3. Misconfiguration fails **loud**. A shim that cannot resolve the real `go`
   and exits 0 turns every build on the host into a silent no-op — a worse
   outage than the one it prevents.

### PATH placement — read before installing

The shim only works if `PATH` reaches it **before** the real toolchain, and
getting that wrong fails silently: the shim never runs and the operator
believes the ban is enforced.

On the macOS host (verified 2026-09-05) `~/.gc/bin` is **last** in the
operator's `PATH`, behind `/opt/homebrew/bin`. Installing there would guard
nothing. `~/bin` is second, ahead of `/opt/homebrew/bin`, and does work.

The installer refuses a shadowed directory rather than manufacturing false
confidence, and prints the two `PATH` positions it compared.

### Install

```bash
# see what it would do, and against which toolchain
scripts/install-go-clean-cache-shim.sh --dir ~/bin --dry-run

scripts/install-go-clean-cache-shim.sh --dir ~/bin
```

The installer resolves the real `go` once and bakes the absolute path into the
installed copy, so the shim never searches `PATH` at run time. It resolves
*past* any shim already on `PATH`, so two installed shims can never chain. It
reports success only after demonstrating on the installed copy that
`go version` passes through and that the ban is refused.

`--dir` defaults to `$GC_GO_SHIM_DIR`, else `~/bin`.

### Uninstall

```bash
scripts/install-go-clean-cache-shim.sh --dir ~/bin --uninstall
```

Removal happens **only** if the file carries the shim's marker. Pointed at the
wrong directory the worst case is a refusal, never a deleted toolchain.

### Escape hatch

```bash
GC_ALLOW_GO_CLEAN_CACHE=1 go clean -cache
```

Deliberate, per-invocation, and never silent — the shim still announces on
stderr that it let it through, so the next cache-miss storm is traceable.

### Residual gap if layer 2 is not installed

With layer 1 alone, nothing prevents a repeat of either incident. An agent or a
session that decides to run `go clean -cache` still can, and the first symptom
is still a fleet-wide build failure. The gap is not partially covered — it is
entirely uncovered.

Two narrower gaps remain even *with* the shim installed:

- a process that invokes the toolchain by absolute path (`/opt/homebrew/bin/go
  clean -cache`) bypasses `PATH` entirely;
- a process with a `PATH` that does not include the shim directory (a `launchd`
  job with a hard-coded `PATH`, a container) never sees it.

Both are narrower than the surface the shim covers, but they are real, and the
shim should not be described as making the operation impossible.

## Tests

`make check-go-clean-cache` runs all three suites and then the scan itself:

| Suite | Covers |
| --- | --- |
| `scripts/test-check-go-clean-cache.sh` | layer 1 — the flag parse, the surface, both exemptions, `--staged`, the pre-commit wiring end to end, and this repository's own baseline |
| `scripts/test-go-clean-cache-shim.sh` | layer 2 — refusal forms, every allowed form, argv/exit-status/stream/stdin fidelity, the exec, the override, and the installer |
| `scripts/test-trim-go-build-cache.sh` | `scripts/trim-go-build-cache.sh` — the alternative the refusal names: selector safety (it can never match a cache root or the depth-1 bookkeeping), the two-polarity `-newermt` preflight, re-stat immediately before unlink, and `--dry-run` |

They run from the Makefile target rather than from Go wrappers on purpose. A
`_test.go` wrapper per suite would add an `exec.Command` call site in a new
test file, and the P0.4 resource census (`test/test-resources.toml`) ratchets
untagged subprocess call and file totals as *anti-growth*: raising that ceiling
is an explicit policy change requiring council review, not something a new
guard should do as a side effect. `check-residency-boundary` runs its own
self-test the same way.

All three are hermetic. The shim suite drives a **fake** `go` on a temp
`PATH`, and the trim suite pins `GO_BUILD_CACHE_DIR` at a synthetic tree so the
real cache is never read or written:
`go clean -cache` is never executed anywhere in this repository's tests, which
is the entire point of the thing under test.

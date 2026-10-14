# Voxist fork maintainer guide

Fork-owned rules for the Voxist/gascity fork, moved verbatim out of the root
`AGENTS.md` (which upstream caps at 20000 bytes, `TestRootAgentsFileWithinBudget`).
The root file's "Read first" table points here. Read this before a resync,
before touching build caches or Git hooks on the shared macOS host, and before
reasoning about what this fork is for.

## Current integration mission

This fork is integrating **Gas City + T3 Code + a DoltLite-backed beads
store**. The target is not a permanent divergence from upstream Gas City.
The target is a maintainable integration branch whose useful changes can
track upstream easily and whose fork-specific behavior is isolated behind
small, obvious ownership boundaries.

When working here, assume three codebases matter:

- **This repo** (`/data/projects/gascity`): the Gas City SDK and the fork
  integration layer.
- **T3 Code** (`/data/projects/t3code` when present): the UI/runtime that
  hosts visible agent threads through the `t3bridge` runtime provider.
- **Beads / bd with DoltLite**: the work ledger backend. Gas City should use
  the normal beads abstractions and keep DoltLite-specific read/write behavior
  contained in beads/provider boundaries.

### Upstream alignment rules

- Keep `upstream/main` easy to merge. Prefer new files, small adapters,
  and fork-owned packages over broad edits to upstream-owned code.
- If upstream code must change, make the patch minimal and idiomatic so it
  can be rebased, dropped, or proposed upstream cleanly.
- Do not bury T3 Code or DoltLite assumptions in generic SDK paths. Put
  provider-specific behavior behind the existing runtime, config, or beads
  backend boundaries.
- Before rebuilding a missing feature from scratch, search history. This
  fork has repeatedly lost working code during branch churn; older branches
  and commits often already contain the fix.
- Treat archived plans and audits as evidence, not gospel. Confirm against
  current code and current upstream before porting.

### Feature archaeology workflow

Use git history deliberately when a feature appears missing or regressed:

In this fork the remotes are **inverted from the usual convention**: `origin`
is upstream (`gastownhall/gascity`) and the fork is `fork`/`Voxist`
(`Voxist/gascity`). There is no remote named `upstream`. The beads fork
follows the same shape: `origin` is `gastownhall/beads`, the fork is `bfork`.

```bash
git remote -v
git fetch --all --prune
git log --all --oneline --decorate --grep '<keyword>'
git log --all --oneline --decorate -- <path>
git show <commit>:<path>
git diff origin/main...HEAD -- <path>
git range-diff origin/main...HEAD
```

Useful search targets:

- T3 bridge/runtime: `t3bridge`, `T3Bridge`, `internal/runtime/t3bridge`,
  `cmd/gc/template_resolve_t3bridge.go`
- DoltLite/beads backend: `doltlite`, `DoltLite`, `internal/beads`,
  `providers.go`, `beads_provider_lifecycle`
- Prior parity work: `engdocs/archive/analysis/gastown-upstream-audit.md`,
  `engdocs/archive/analysis/feature-parity.md`,
  `engdocs/contributors/dolt-regression-audit.md`

If history contains working code, prefer porting the smallest proven slice
instead of inventing a parallel mechanism.

### Resync conventions

Every resync so far has produced a second wave of commits that exist only to
repair the first: `fix(resync)`, `merge-fix`, ratchet bumps, regenerated
artifacts, `test: unbreak CI`. As of 2026-08-30 there are **32 such commits**
in `origin/main..fork/main`, and 8 of them sit in the top 32 by conflict
weight — they cost more in the *next* merge than the fix they delivered.

Two rules keep that wave from forming.

**1. Generated artifacts are regenerated, never merged.** On a conflict in any
of these, take upstream wholesale and re-run the generator:

- `internal/api/openapi.json`, `docs/reference/schema/openapi.*`
- `docs/reference/schema/city-schema.*`, `docs/reference/schema/pack-schema.*`,
  `docs/reference/cli.md`, `docs/reference/config.md`
- `cmd/gc/productmetrics_command_census.json` — **NOT regenerable; merge it, see
  below** — `cmd/gc/metrics_census_gen.go`
- `internal/api/dashboardspa/**` generated TS and `dist/`
- `internal/api/genclient/client_gen.go`
- test ratchets and baselines: `internal/testpolicy/resourcecensus/census.go`
  (**NOT regenerable; merge it, see below**),
  `internal/testenv/testdata/*.golden`, `scripts/*baseline*`,
  `scripts/*manifest*`

**Two paths on that list are NOT regenerable, and taking them wholesale
destroys fork data. Merge them; never `--theirs` them:**

- `cmd/gc/productmetrics_command_census.json` is the census generator's
  hand-maintained **INPUT**, not its output. `cmd/gen-command-census/main.go`
  reads it as `paths.manifest` and derives `metrics_census_gen.go`,
  `internal/productmetrics/command_ids_gen.go` and
  `schemas/metrics/example/result.schema.json` *from* it. Taking upstream's
  copy deletes the fork's census commands, and regenerating afterwards does
  **not** restore them — the generator only re-derives from whatever manifest
  it is handed. (Observed once, during the 2026-09-06 resync: five fork commands
  were lost this way — beads-state, config-lint, provider-quota,
  provider-credentials, provider-rotate-key — and all five were restored
  before that merge landed. They are present today; this is the incident
  record, not a standing gap.)
- `internal/testpolicy/resourcecensus/census.go` is **hand-written Go** —
  package doc, imports, types, methods. It carries the attribute only to
  suppress the diff of a large bootstrap table. Taking it wholesale reverts
  fork ratchet rows (six, on the same merge).

This is the ga-d32bn family in a new shape: **exempted-but-not-regenerable.**
Gate 1 exempts both by attribute; Gate 2 sees no lost Go declaration (the
manifest is JSON; the ratchet rows are struct literals inside an existing
declaration); `go build`, `go vet` and the real suite are all blind to a missing
census entry. Gate 3 inherits Gate 1's exemption, so it does not close this
either.

One check does exist for the manifest, and it is worth knowing its exact scope:
`internal/commandcensus/generate.go`'s `ValidateEvolution` compares the manifest
against the committed `command_ids_gen.go` catalog and fails loudly — naming the
dropped ids — if a prior allocation was removed or remapped. It fires only when
the two **disagree**. Taking the manifest and the catalog from upstream
*together*, which is exactly what rule 1 instructs, keeps them consistent with
each other and silent about what the fork lost. The attribute answers "should GitHub collapse this
diff", which is not the same question as "can this be regenerated" — and rule 1
conflates them.

Every path above carries the `linguist-generated` attribute in
`.gitattributes`, which is the single enforced source of truth this list
mirrors: `scripts/check-resync-loss.sh`'s Gate 1 exemption
(`git check-attr linguist-generated`), `scripts/check-generated-docs-drift.sh`,
and GitHub's diff collapsing all read the same file. Add a path to
`.gitattributes` first, then update this list — a path exempted in one but
not the other is the bug this section exists to prevent (ga-d32bn).

Hand-merging these is what produces `chore(regen)` and `test: unbreak CI`
commits. The generator output is a *function of the merged source*; resolving
it by hand asserts a value the source does not yet imply, and the next resync
inherits both the wrong value and a conflicting commit.

**2. Do not rewrite published fork history to clean this up.** `fork/main`
carries ~490 remote branches and open PRs. Squashing debt commits out of it
would break every one, and the pathspec/force-push family has already cost
this fork a merged-but-empty PR (see the CHANGELOG rebase-clobber incident).

The debt commits do not need to be *removed* to stop conflicting — during a
merge they are resolved by rule 1, upstream-side. Retire them by
**resolution**, not by rewriting.

**3. `CHANGELOG.md` is union-merged**, both sides kept, ordered by date. Map
every conflicting path *before* resolving any of them; a resolver that assumes
CHANGELOG is the only conflict will silently mishandle a merge that also
touches code.

**4. Verify with the real suite.** `make test-local-full-parallel`, not
`go build` + `go vet`. The 2026-07-15 resync passed build and vet cleanly and
still shipped four runtime regressions.

**5. Rule 1's "take upstream wholesale" applies ONLY to the generated-artifact
list above.** A shared `_test.go` file is never on that list and is never
taken wholesale — it is merged, the same as any other hand-authored file. The
2026-08-31 resync applied rule 1's instinct to shared test files anyway and
silently dropped fork-added declarations (ga-d32bn): `go build`, `go vet`,
and rule 4's real-suite run are all blind to this class, because deleting a
test never fails a build — the suite passes precisely because the tests that
would have failed are the ones that got deleted. Every resync runs
`scripts/check-resync-loss.sh` on the merge commit before push to catch it;
`.githooks/pre-push` invokes it on every merge commit a push introduces, so
it is not something an operator has to remember.

Measured with the shipped gate, that merge lost **254 fork-added
(declaration, file) pairs** — 120 of them in `linguist-generated` files
(exempt under rule 1), leaving 134 real ones: 114 `RESOLUTION-BUG` (a plain
3-way merge would have kept them) and 20 `MERGE-OUTCOME`. Earlier prose in
this file and in ga-d32bn quoted 234 and 113; both were intermediate counts
from prototypes that keyed on bare declaration names and had no
generated-file exemption. 254/134/114 is the number the committed tooling
reproduces, and it is the one to cite.

**6. The loss is symmetric, and so is the gate.** Taking OURS wholesale on a
file upstream changed drops upstream-added code exactly the way rule 5's
took-theirs drops fork-added code, and it is just as invisible to build, vet
and the suite. The 2026-09-04 resync resolved `internal/hooks/hooks_test.go`
with `--ours` and silently dropped all three upstream-added cursor tests
while the fork-side gate reported "0 missing" on that exact tree (ga-8gpw4).
`check-resync-loss.sh` Gate 3 now asks the mirror-image question, at both
levels, and reports every verdict with an `UPSTREAM-` prefix: upstream-added
declarations absent from the merge, and — over `*.go` only — files the merge
dropped or took from the fork verbatim (`UPSTREAM-DROPPED-FILE`,
`UPSTREAM-PURE-LOSS`, `UPSTREAM-TOOK-OURS`).

The file-level half is not redundant with the declaration-level half. When
upstream MODIFIES an existing declaration rather than adding one, the
declaration set does not change, so a merge that takes the fork's blob
verbatim drops the change with the declaration check reporting nothing. Run
against the 2026-08-31 merge, Gate 3 finds 5 upstream-added declarations and
5 took-ours files that the fork-side gates never saw — and two of those
files, `cmd/gc/dolt_cleanup_drop.go` and
`internal/doctor/checks_order_firing_bounded_test.go`, are visible to
nothing else in the script.

Scoping the file-level half to `*.go` is deliberate. Over all paths a
took-ours check fires on every file this fork legitimately owns outright
(`AGENTS.md`, `CHANGELOG.md`, fork-only workflows), and a gate that cannot
be passed is worse than no gate.

## Build Cache Conventions

**Hard ban: never run `go clean -cache`** in any script, hook, or agent session.

Running `go clean -cache` against a shared `GOCACHE` (the default when
`$GOCACHE` is not overridden) corrupts the fleet-wide build cache for every
concurrent executor. Each executor that hits a missing cache entry then runs a
full rebuild, and any that calls `go clean -cache` mid-flight invalidates all
the others' in-progress caches. The incident (vp-g96b, 2026-06-13) produced
~10 cascading cache-miss errors across the executor pool.

**Just run `go build` / `make` — do NOT set `GOCACHE` yourself.** A warm shared
cache is faster and is never corrupted by a normal build. **How the sharing is
arranged differs by host, so verify before reasoning from either description:**

- **Linux fleet host** (`/data/projects/gascity`): a host `go` shim routes the
  default `GOCACHE` to a shared on-disk cache (`~/.cache/go-build`) and pins
  compile/link temp to disk (`GOTMPDIR=/var/tmp/gotmp`). *Inherited from the
  original authoring of this section; not re-verified from a macOS host.*
- **macOS host** (`/Users/Shared/Github/gascity`): `GOTMPDIR` is **unset**, and
  nothing routes `GOCACHE`. Sharing comes from configuration instead: every
  `GOCACHE` entry in `voxist-city/city.toml` (14 as of this writing) points
  every agent at
  `~/Library/Caches/go-build`. *Verified on this host, 2026-09-05.*

  There **is** a `go` shim at `~/bin/go`, but it does not touch `GOCACHE` — it
  exists only to enforce the ban above. Installed 2026-09-05 after a second
  `go clean -cache` incident, it sits ahead of `/opt/homebrew/bin` on both the
  operator's PATH and the supervisor's, so it intercepts fleet agents as well as
  interactive shells. It refuses `go clean -cache` / `--cache` — including
  combined with other flags and behind `go -C dir` — and `exec`s everything else
  through to `/opt/homebrew/bin/go` untouched. `go clean -testcache`,
  `-modcache`, `-fuzzcache` and a bare `go clean` are explicitly **not**
  blocked. It also refuses the flag when it arrives via the `GOFLAGS`
  environment variable. Deliberate override:
  `GC_ALLOW_GO_CLEAN_CACHE=1 go clean -cache`, which is never silent.
  Uninstall: `scripts/install-go-clean-cache-shim.sh --dir ~/bin --uninstall`,
  or `rm -f ~/bin/go`.

  Known gaps, stated because a guard that overstates its reach is worse than
  one that admits its edges: it does **not** cover an absolute-path invocation
  (`/opt/homebrew/bin/go clean -cache`), an IDE with a pinned SDK path, root,
  or `go env -w GOFLAGS=-cache` — which cmd/go reads from its own config file
  rather than the environment, so the shim never sees it (bead `ga-mzx61`).
  Layer 1, the repo scanner, catches the command entering the codebase and
  would not have prevented either incident, since nothing was committed on
  either occasion.
  Source and rationale: `engdocs/contributors/go-clean-cache-guard.md`.

The rules below hold on both, for the same underlying reason — one cache,
many concurrent writers — regardless of which mechanism puts them there.

**Never point `GOCACHE` (or `TMPDIR`) at `/tmp`.** On the Linux fleet host
`/tmp` is a size-capped RAM-backed tmpfs (61G) shared by the whole fleet,
including the harness's tool-output capture dir: one cold cache built there is
2-3GB, and a concurrent build wave fills tmpfs and ENOSPCs every agent on the
host (incident gm-tkz1r / ga-x9k9b9, 2026-07). A bare `mktemp -d` (no `-p` dir)
resolves against the unset `$TMPDIR`, which defaults to `/tmp`. Where the shim
exists it deliberately does **not** relocate a `GOCACHE` you set explicitly, so
an explicit `/tmp` path defeats it. (On macOS `/private/tmp` is disk-backed,
not tmpfs, so the ENOSPC mechanism does not apply — but `/tmp` is still the
wrong place for a build cache and the rule stands unchanged.)

**Cache growth is not self-limiting.** Go imposes no size cap — only a fixed
5-day trim applied opportunistically — and with the fleet building
continuously the cache outgrows that sweep. It reached 463GB on the macOS host
before a 3-day trim recovered 175GiB. Do not solve this with `go clean -cache`
(see the hard ban above); the maintained trim job is the supported route.

**If you truly need an isolated cold build** (a from-scratch compile without
`go clean -cache`), put the throwaway cache **on disk** and remove it
unconditionally with a `trap`, and redirect `TMPDIR` to the same dir so the
linker's own scratch also stays off tmpfs:

```bash
tmp=$(mktemp -d -p /var/tmp) && trap 'rm -rf "$tmp"' EXIT
GOCACHE="$tmp" TMPDIR="$tmp" go build ./cmd/gc/
```

**Exception:** `go clean -testcache` is explicitly allowed. It clears only the
test-result cache, not the compiled-object cache, and does not corrupt
concurrent builds.

**Hermetic Git test config is mirrored.** `Makefile`'s `TEST_ENV` and the
nested `env -i` wrappers in `scripts/test-local-parallel`,
`scripts/test-go-test-shard`, and `scripts/test-integration-shard` must all pin
`GIT_CONFIG_NOSYSTEM=1` and `GIT_CONFIG_GLOBAL=/dev/null`. Updating only the
Makefile is insufficient because each nested runner rebuilds the environment
and would otherwise restore user Git configuration through the preserved
`HOME`.

## Git hook ownership

**`.githooks` is the single owner of `core.hooksPath`.** Install it with
`make setup`; verify it with `make check-hooks`.

Only one directory can own `core.hooksPath`, and beads' installer claims it
for `.beads/hooks`. Those hooks exec `bd hooks run <hook>` without chaining
onward, so while beads owns the path every gate in `.githooks` is skipped:
pre-commit's `go clean -cache` guard, staged-Go formatting, nogo
`lint-changed` and the codegen+stage steps, and pre-push's resync-loss gate,
bead-ownership guard and push-time suite (`.githooks/lib/push-suite.sh`).
Nothing reports this: git simply
stops invoking the hooks, so commits look clean while spec-derived drift lands
on the mainline until a later suite failure surfaces the drift.

Reclaiming the path does not disable beads. Each `.githooks` hook forwards to
`.githooks/lib/beads-chain.sh`, which runs `bd hooks run <hook>` with the same
timeout and exit-code carve-outs beads' own integration block used. Adding a
hook that beads manages means adding its `.githooks` counterpart too —
`TestGitHooksCoverEveryBeadsManagedHook` in `scripts/` fails otherwise.

Beads' installer can reclaim `core.hooksPath` at any time. When it does,
`make check-hooks` fails and `make setup` puts it back.

The generated-artifact drift tests in `bazel test //...` are the backstop
for spec/client drift, but they only see work that reaches a CI run, and on
this fork that Bazel lane does not run (bazel.yml is upstream-only; fork CI
is fork-verify.yml) — run `make spec-ci` before pushing an API change, and
locally merged branches depend on the pre-commit gate actually running.

**Bazel-free hook modes on the macOS host.** Bazel's stock LLVM toolchain
cannot compile the dolthub cgo deps (ICU, zstd) on macOS, and every worktree
path gets its own Bazel output base — on a shared host with dozens of
worktrees and a near-full disk that is not affordable per commit. Set
`GC_PRECOMMIT_LINT=go` (pre-commit runs `go vet` over the staged packages
instead of nogo, and `make check-docs-go` instead of the Bazel docs check)
and `GC_PREPUSH_SUITE=go` (pre-push runs `make test-fast-parallel` instead of
`bazel test //...`). Both print a banner
saying they are not CI's gate; fork CI (fork-verify.yml) runs the Go-native
lanes.

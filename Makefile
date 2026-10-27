GOLANGCI_LINT_VERSION := 2.14.0
OASDIFF_VERSION := 1.33.0
BUILDX_VERSION := 0.21.2

# Detect OS and arch for binary download.
GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

BIN_DIR := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
OASDIFF := $(BIN_DIR)/oasdiff

BINARY     := gc
BUILD_DIR  := bin
INSTALL_DIR := $(BIN_DIR)

# Version metadata injected via ldflags. These run git in the build directory,
# so they resolve a worktree's `.git` gitdir pointer and describe the checkout
# actually being compiled. The Go toolchain's own buildvcs stamping does not:
# it identifies a repository by a `.git` *directory*, so from inside a worktree
# it keeps walking up and stamps whichever repository encloses it. That makes
# the toolchain stamp actively wrong for a worktree nested in another checkout
# — a polecat worktree under the city directory picks up the city's commit and
# the city's dirtiness (ga-u7fb) — so `build` below passes -buildvcs=false and
# these variables are the single source of truth.
VERSION    := $(shell tag=$$(git describe --tags --exact-match 2>/dev/null || true); if [ -n "$$tag" ]; then printf '%s' "$$tag" | sed 's/^v//'; else echo "dev"; fi)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DIRTY      := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo "-dirty" || true)
BUILD_TIME := $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)

LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT)$(DIRTY) \
           -X main.date=$(BUILD_TIME)

unique_words = $(if $1,$(firstword $1) $(call unique_words,$(filter-out $(firstword $1),$1)))

# macOS: icu4c (a transitive Dolt / go-icu-regex CGO build dependency) is
# keg-only under Homebrew, so its headers/libs are not on the default CGO
# search path. Point CGO at them when icu4c is present. This is a no-op on
# Linux and other platforms (where system ICU, e.g. libicu-dev, is found
# normally) and a no-op on macOS when icu4c is not installed.
#
# The prefix is accepted only when the ICU header is actually THERE.
# `brew --prefix <formula>` prints the formula's would-be prefix and exits 0
# whether or not it is installed, so its exit code cannot stand in for
# presence: believing it points CGO at a directory that does not exist and the
# build dies with "'unicode/regex.h' file not found" while the Makefile
# believes ICU is configured. That is how the Mac regression lane broke — the
# runner never installed icu4c (ga-mac-icu). Pinned by
# TestMakefileDarwinICUFlagsRequireTheHeaderToExist.
# `icu4c` is an ALIAS for the newest versioned formula (icu4c@78 today), and
# the alias can resolve to a version that is not the one installed — the macOS
# runner reports icu4c as installed via `brew list --formula icu4c` while
# $(brew --prefix icu4c)/include holds nothing. So every candidate prefix is
# considered and the first one that actually carries the header wins.
ifeq ($(shell uname),Darwin)
BREW_PREFIX := $(shell brew --prefix 2>/dev/null)
ICU_CANDIDATES := $(shell brew --prefix icu4c 2>/dev/null) $(sort $(wildcard $(BREW_PREFIX)/opt/icu4c@*)) $(sort $(wildcard $(BREW_PREFIX)/Cellar/icu4c*/*))
ICU_PREFIX := $(firstword $(foreach p,$(ICU_CANDIDATES),$(if $(wildcard $(p)/include/unicode/regex.h),$(p))))
ifneq ($(ICU_PREFIX),)
CGO_CPPFLAGS += -I$(ICU_PREFIX)/include
CGO_LDFLAGS += -L$(ICU_PREFIX)/lib
export CGO_CPPFLAGS
export CGO_LDFLAGS
endif
endif

# Nix/Flox: when the installed gc binary links ICU from the Nix store, system
# ICU headers in /usr/include may exist but link to the wrong libicuuc version,
# causing a __vdso_gettimeofday dlopen error at test/run time. Detect the ICU
# version the installed gc binary actually links (following symlinks to the real
# Nix store path), find the matching dev package, point CGO at it, disable the
# /usr/include fallback, and embed an rpath so the rebuilt binary finds its libs
# without LD_LIBRARY_PATH (important for the systemd supervisor service unit).
#
# Gate: _NIX_ICU_RT must resolve (after readlink -f) to a /nix/store path.
# This keeps the block inert in hermetic test environments where gc is not
# installed pointing at Nix ICU (CC being a temp-dir fake binary is not a
# reliable signal on Flox hosts where CC is system gcc but libs are Nix-managed).
ifeq ($(shell uname),Linux)
_GC_BIN         := $(shell command -v gc 2>/dev/null)
_NIX_ICU_RT_RAW := $(shell ldd $(_GC_BIN) 2>/dev/null | grep -m1 'libicuuc' | awk '{print $$3}')
_NIX_ICU_RT     := $(shell readlink -f '$(_NIX_ICU_RT_RAW)' 2>/dev/null)
ifneq ($(filter /nix/store/%,$(_NIX_ICU_RT)),)
_NIX_ICU_HASH   := $(shell printf '%s' "$(_NIX_ICU_RT)" | sed 's|/nix/store/\([^-]*\)-.*|\1|')
_NIX_ICU_LIBDIR := $(dir $(_NIX_ICU_RT))
_NIX_ICU_DEV    := $(shell for d in /nix/store/*-icu4c-*-dev; do grep -q "$(_NIX_ICU_HASH)" "$$d/nix-support/propagated-build-inputs" 2>/dev/null && printf '%s' "$$d" && break; done)
ifneq ($(_NIX_ICU_DEV),)
CGO_CPPFLAGS += -I$(_NIX_ICU_DEV)/include
CGO_LDFLAGS  += -L$(_NIX_ICU_LIBDIR) -Wl,-rpath,$(_NIX_ICU_LIBDIR)
export CGO_CPPFLAGS
export CGO_LDFLAGS
SYS_USR_CGO_FALLBACK := 0
$(info Nix/Flox ICU detected: -I$(_NIX_ICU_DEV)/include -L$(_NIX_ICU_LIBDIR) -rpath $(_NIX_ICU_LIBDIR); SYS_USR_CGO_FALLBACK disabled)
endif
endif
endif
# Linux: some non-system compilers (Nix, Flox, etc.) don't search /usr/include
# or /usr/lib by default. If system ICU headers exist but the compiler doesn't
# see them, intentionally let system paths participate in the whole CGO build.
# Set SYS_USR_CGO_FALLBACK=0 to disable this fallback for hermetic or cross-CGO
# builds. (Nix block above sets SYS_USR_CGO_FALLBACK=0 when Nix ICU is found.)
ifeq ($(shell uname),Linux)
SYS_USR_CGO_FALLBACK ?= 1
ifneq ($(SYS_USR_CGO_FALLBACK),0)
SYS_USR_INCLUDE ?= /usr/include
SYS_USR_LIB_ROOT ?= /usr/lib
SYS_USR_LIB64_ROOT ?= /usr/lib64
ifneq ($(wildcard $(SYS_USR_INCLUDE)/unicode/uregex.h),)
ifeq ($(shell $(CC) -E -Wp,-v -x c /dev/null 2>&1 | sed 's/^[[:space:]]*//' | grep -F -x -q "$(SYS_USR_INCLUDE)" && echo yes),)
SYS_USR_MULTIARCH_CANDIDATES := $(strip $(shell dpkg-architecture -q DEB_HOST_MULTIARCH 2>/dev/null) $(shell $(CC) -print-multiarch 2>/dev/null))
SYS_USR_LIB_CANDIDATES := $(foreach arch,$(SYS_USR_MULTIARCH_CANDIDATES),$(SYS_USR_LIB_ROOT)/$(arch)) $(SYS_USR_LIB64_ROOT) $(SYS_USR_LIB_ROOT)
SYS_USR_LIB_DIRS := $(strip $(call unique_words,$(strip $(foreach dir,$(SYS_USR_LIB_CANDIDATES),$(if $(wildcard $(dir)),$(dir))))))
$(info Linux system CGO fallback active: adding -I$(SYS_USR_INCLUDE) $(addprefix -L,$(SYS_USR_LIB_DIRS)); set SYS_USR_CGO_FALLBACK=0 to disable)
CGO_CPPFLAGS += -I$(SYS_USR_INCLUDE)
CGO_LDFLAGS += $(addprefix -L,$(SYS_USR_LIB_DIRS))
export CGO_CPPFLAGS
export CGO_LDFLAGS
endif
endif
endif
endif

.PHONY: build check check-all check-bd check-docker check-docs check-dolt check-hooks check-eventexport-isolation check-gomod-replace check-core-boundary check-go-clean-cache check-native-dependency-surface check-resync-loss check-routed-test-rows check-split-topology-rows check-version-tag lint lint-changed lint-affected lint-full lint-golangci vet-go fmt-check fmt-check-changed fmt vet test test-ci-policy test-mac test-coarse-clock test-fast-parallel test-fsys-darwin-compile test-herdr-live test-pack-registry-live test-native-doltlite-beads test-cmd-gc-process test-cmd-gc-process-shard test-cmd-gc-process-parallel test-productmetrics-testhook test-worker-core test-worker-core-phase2 test-worker-core-phase2-all test-worker-core-phase2-real-transport setup-worker-inference test-worker-inference test-worker-inference-phase3 test-acceptance test-beads-topology-matrix test-bd-cli-contract test-bd-cli-contract-home-isolation test-bd-conditional-release-contract test-acceptance-b test-acceptance-split-storage test-acceptance-c test-acceptance-all test-tutorial-goldens test-tutorial-regression test-tutorial test-integration test-integration-shards test-integration-shards-parallel test-integration-shards-cover test-integration-packages test-integration-packages-cover test-integration-review-formulas test-integration-review-formulas-cover test-integration-review-formulas-basic test-integration-review-formulas-basic-cover test-integration-review-formulas-retries test-integration-review-formulas-retries-cover test-integration-review-formulas-recovery test-integration-review-formulas-recovery-cover test-integration-bdstore test-integration-bdstore-cover test-integration-rest test-integration-rest-cover test-integration-rest-smoke test-integration-rest-smoke-cover test-integration-rest-full test-integration-rest-full-cover test-local-full-parallel test-mail-wisp-insert test-mcp-mail test-openclaw-bridge test-docker test-k8s test-cover test-cover-mac test-cover-noncmdgc test-cover-cmdgc-shard test-cover-noncmdgc-shard cover check-self-contained install install-tools install-buildx install-oasdiff openapi-breaking-check setup clean generate check-schema complexity complexity-diff complexity-check complexity-update docker-base docker-agent docker-controller docs-dev diagrams-excalidraw dashboard-build dashboard-build-npm dashboard-generate-client dashboard-lock dashboard-dev dashboard-check dashboard-check-npm dashboard-ci dashboard-smoke dashboard-e2e-go dashboard-e2e-play dashboard-e2e
.PHONY: check-release-dist-ignore
.PHONY: bazel-tmpdir test-go check-go check-all-go check-docs-go test-acceptance-go test-integration-go

# Build and test engine. Bazel is what CI gates on: .github/workflows/bazel.yml
# runs `bazel test` lanes on rbe-west, and nogo (lint + vet), formatting,
# generated-artifact drift and the policy guards are Bazel test targets there.
# The primary targets below (test, check, check-all, check-docs,
# test-acceptance, test-integration) run the same `bazel test` commands as
# those lanes, so a local run shares CI's action keys and its remote cache
# (TESTING.md "Building and testing"). Each keeps a plain-`go test` twin under
# an explicit -go name (test-go, check-go, ...) for offline work and hosts
# Bazel does not serve; that twin is a convenience, not what CI enforces.
# GitHub Actions jobs that still run Go-native suites call the -go names
# explicitly.
BAZEL ?= bazel
# Extra flags for every `bazel test` below: --config=fork-cache (contributors:
# the anonymous read-only cache) or --config=remote-exec (maintainers with an
# rbe-west certificate). Agent hosts whose ~/.bazelrc names the executor need
# neither. Better: put `build --config=...` in the gitignored .bazelrc.local.
BAZEL_FLAGS ?=
BAZEL_TEST = $(BAZEL) test $(BAZEL_FLAGS) --keep_going

# .bazelrc roots every locally run test's tmpdir at /tmp/bt (short unix-socket
# paths); nothing else creates it.
bazel-tmpdir:
	@mkdir -p /tmp/bt

## build: compile gc binary with version metadata
build:
	go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/gc
ifeq ($(shell uname),Darwin)
	@scripts/sign-darwin-local.sh $(BUILD_DIR)/$(BINARY)
endif

## artifact: build a provenance-named gc artifact (requires BASE_REF=<remote>/<branch>, e.g. Voxist/main)
## The filename is machine-derived from the ACTUAL build commit (rev-parse
## HEAD); the base branch's name is refused as the token unless HEAD is in
## BASE_REF's lineage (the gc-main-20260710-77916fc6c trap). The binary gets
## its commit and a main.buildBase lineage stamp via ldflags (visible in
## `gc version --long`), a .buildinfo.json manifest beside it, and its
## self-reported commit is verified against HEAD after the build. A dirty
## tree fails unless ALLOW_DIRTY=1, which names the artifact -dirty instead.
##
## Builds pass -buildvcs=false: Go's own VCS stamping is untrustworthy from
## linked worktrees — nested under this repo dir it embeds the MAIN
## checkout's HEAD/dirty state (how a worktree build at eb743642c once
## embedded 50e120757), outside it embeds nothing. The later -X main.commit
## wins over the $(COMMIT) one in $(LDFLAGS) and carries the full sha plus
## an explicit -dirty suffix.
ARTIFACT_DIR ?= $(BUILD_DIR)
.PHONY: artifact
artifact:
	@set -e; \
	if [ -z "$(BASE_REF)" ]; then \
		echo "ERROR: BASE_REF is required, e.g. 'make artifact BASE_REF=Voxist/main' — the lineage claim must name the remote (git remote -v)" >&2; \
		exit 2; \
	fi; \
	exports=$$(go run ./cmd/artifactname -repo . -base '$(BASE_REF)' -binary '$(BINARY)' $(if $(ALLOW_DIRTY),-allow-dirty,)) || exit $$?; \
	eval "$$exports"; \
	if [ -z "$$ARTIFACT_NAME" ] || [ -z "$$ARTIFACT_COMMIT_STAMP" ]; then \
		echo "ERROR: artifactname emitted no usable exports" >&2; \
		exit 1; \
	fi; \
	mkdir -p "$(ARTIFACT_DIR)"; \
	echo "building $(ARTIFACT_DIR)/$$ARTIFACT_NAME"; \
	go build -buildvcs=false -ldflags "$(LDFLAGS) -X main.commit=$$ARTIFACT_COMMIT_STAMP -X main.buildBase=$$ARTIFACT_BASE_STAMP" -o "$(ARTIFACT_DIR)/$$ARTIFACT_NAME" ./cmd/gc; \
	if [ "$$(uname)" = "Darwin" ]; then scripts/sign-darwin-local.sh "$(ARTIFACT_DIR)/$$ARTIFACT_NAME"; fi; \
	go run ./cmd/writebuildmanifest -binary "$(ARTIFACT_DIR)/$$ARTIFACT_NAME" -repo "$(CURDIR)"; \
	reported=$$("$(ARTIFACT_DIR)/$$ARTIFACT_NAME" version --json | sed -n 's/.*"commit":"\([^"]*\)".*/\1/p'); \
	if [ "$$reported" != "$$ARTIFACT_COMMIT_STAMP" ]; then \
		echo "ERROR: binary self-reports commit '$$reported', expected '$$ARTIFACT_COMMIT_STAMP' — refusing to trust this artifact" >&2; \
		exit 1; \
	fi; \
	"$(ARTIFACT_DIR)/$$ARTIFACT_NAME" version --long; \
	echo "OK: $(ARTIFACT_DIR)/$$ARTIFACT_NAME (self-reported commit verified == HEAD)"

## check-self-contained: assert the built gc binary is self-contained (Linux/Nix ICU rpath).
## Only enforced when the Nix/Flox ICU block above fired (_NIX_ICU_DEV set):
## on those hosts a binary without an ICU RUNPATH loads interactively (the
## shell has the Nix/Flox env) but silently boot-fails EVERY supervisor-spawned
## agent, which has no LD_LIBRARY_PATH -> town-wide stall. This gate makes
## that impossible to ship. On non-Nix hosts the target is a no-op.
check-self-contained: build
ifneq ($(_NIX_ICU_DEV),)
	@set -e; \
		bin="$(BUILD_DIR)/$(BINARY)"; \
		if command -v readelf >/dev/null 2>&1; then \
			if ! readelf -d "$$bin" 2>/dev/null | grep -qiE 'RUNPATH|RPATH'; then \
				echo "FATAL: $$bin has no RUNPATH/RPATH — NOT self-contained."; \
				echo "       Use 'make build' (bakes ICU -Wl,-rpath), not raw 'go build'."; \
				exit 1; \
			fi; \
		else \
			echo "WARN: readelf not found; skipping RUNPATH check (install discouraged)"; \
		fi; \
		if ! env -i HOME="$(HOME)" PATH=/usr/bin:/bin "$$bin" version >/dev/null 2>&1; then \
			echo "FATAL: $$bin failed clean-env boot (no LD_LIBRARY_PATH)."; \
			echo "       Supervisor-spawned agents will silently boot-fail on ICU."; \
			echo "       Build with 'make build' so the ICU rpath is embedded."; \
			exit 1; \
		fi; \
		echo "OK: $$bin self-contained (RUNPATH present + clean-env boot passes)"
endif

# Deploy channels: the PATH entries a running deployment can resolve `gc`
# through. They shadow each other -- which one wins differs per execution
# context (interactive shell, agent hook, supervisor unit) -- so they are only
# coherent while all of them name one binary.
#
# The precedence is not the obvious one. On the fleet host $(HOME)/.gc/bin
# precedes BOTH $(HOME)/.local/bin and $(BIN_DIR), so `command -v gc` resolves
# $(HOME)/.gc/bin/$(BINARY) -- not the path the supervisor's service unit
# names. Anyone diagnosing a `gc` version divergence who assumes .local/bin
# wins will reach the wrong conclusion.
#
# voxist-city ADR-0027 collapsed these three onto one artifact for exactly
# that reason, and named "go install recreates ~/go/bin/gc as a real file"
# as its re-evaluation trigger. `make deploy-fleet` is that collapse as a
# repeatable target; it is the sole Makefile writer of these paths.
# `install` reads them to warn, and never writes them.
#
# Note that installing still replaces $(BIN_DIR)/$(BINARY) itself -- that is
# its job. So `make install` re-splits the channels until `make deploy-fleet`
# runs. The two are a pair; running the first without the second leaves this
# host's PATH winner on the previous build.
GC_DEPLOY_DIR ?= $(HOME)/.gc/bin
GC_DEPLOY_BINARY ?= $(INSTALL_DIR)/$(BINARY)
GC_DEPLOY_CHANNELS ?= $(HOME)/.local/bin/$(BINARY) $(BIN_DIR)/$(BINARY) $(GC_DEPLOY_DIR)/$(BINARY)

## install: build and install gc to GOPATH/bin (same location as go install)
#
# Writes exactly one path: $(INSTALL_DIR)/$(BINARY). It deliberately does NOT
# relink any deploy channel -- two writers at one path, last one wins in
# silence, is how this machine ran a stale supervisor image for an unknown
# period (vc-lwif). To move a running deployment onto this build, run the
# explicit second step: `make deploy-fleet`.
install: check-self-contained
	@mkdir -p $(INSTALL_DIR)
	@# Fail CLOSED, before writing anything. A symlink at the install path
	@# pointing OUTSIDE $(INSTALL_DIR) is a deployed artifact, not a dev build,
	@# and overwriting it is never what `make install` meant to do -- it moves
	@# every context that resolves gc through that channel. A warning cannot
	@# help here: it can only print after the channel has already moved, and
	@# the failure this exists to prevent is precisely "nobody noticed".
	@# deploy-fleet refuses to move a channel until the binary has proved it
	@# runs; install is held to the same standard.
	@set -e; \
		if [ -z "$(GC_ALLOW_CHANNEL_OVERWRITE)" ] && [ -L "$(INSTALL_DIR)/$(BINARY)" ]; then \
			prior=$$(readlink "$(INSTALL_DIR)/$(BINARY)"); \
			case "$$prior" in \
				"$(INSTALL_DIR)"/*) ;; \
				*) echo "ERROR: $(INSTALL_DIR)/$(BINARY) is a deploy channel pointing at $$prior." >&2; \
				   echo "       Installing would move every context that resolves gc through it onto this build." >&2; \
				   echo "       Deliberate:  make install GC_ALLOW_CHANNEL_OVERWRITE=1 && make deploy-fleet" >&2; \
				   echo "       Elsewhere:   make install INSTALL_DIR=<dir>" >&2; \
				   exit 1;; \
			esac; \
		fi
	@set -e; \
		tmp="$(INSTALL_DIR)/.$(BINARY).tmp.$$$$"; \
		trap 'rm -f "$$tmp"' EXIT INT TERM HUP; \
		cp -f "$(BUILD_DIR)/$(BINARY)" "$$tmp"; \
		chmod 0755 "$$tmp"; \
		mv -f "$$tmp" "$(INSTALL_DIR)/$(BINARY)"; \
		trap - EXIT INT TERM HUP
	@# Deploy provenance: machine-derived build manifest next to the binary
	@go run ./cmd/writebuildmanifest -binary "$(INSTALL_DIR)/$(BINARY)" -repo "$(CURDIR)"
	@# A DIFFERENT channel that resolves to the install path also moved with
	@# this write. The fail-closed gate above covers the install path itself;
	@# this covers everything pointing at it. A dangling channel is reported
	@# too -- a broken channel is exactly what an operator wants to be told.
	@set -e; \
		for channel in $(GC_DEPLOY_CHANNELS); do \
			[ "$$channel" != "$(INSTALL_DIR)/$(BINARY)" ] || continue; \
			if [ -L "$$channel" ] && [ ! -e "$$channel" ]; then \
				echo "WARNING: $$channel is a dangling symlink -> $$(readlink "$$channel")"; \
				continue; \
			fi; \
			[ -e "$$channel" ] || continue; \
			[ "$$channel" -ef "$(INSTALL_DIR)/$(BINARY)" ] || continue; \
			echo "WARNING: $$channel resolves to $(INSTALL_DIR)/$(BINARY) -- this install moved it."; \
			echo "         Anything running that binary is now on this build."; \
		done
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"
	@# $(INSTALL_DIR)/$(BINARY) is itself a deploy channel, so claiming the
	@# channels were untouched would be false by this Makefile's own
	@# definitions. Name what was written instead (ga-lwif MEDIUM).
	@echo "$(INSTALL_DIR)/$(BINARY) is a deploy channel and was overwritten. The other channels were not."
	@echo "To move a deployment onto this build: make deploy-fleet"

## deploy-fleet: point every gc deploy channel at an installed build
#
# The second step `make install` no longer performs implicitly.
# GC_DEPLOY_BINARY defaults to what `make install` just wrote:
#
#   make install && make deploy-fleet
#
# To deploy a provenance-named artifact instead of a bare dev build:
#
#   make artifact BASE_REF=Voxist/main ARTIFACT_DIR=$$HOME/.gc/bin
#   make deploy-fleet GC_DEPLOY_BINARY=$$HOME/.gc/bin/gc-main-<date>-<sha>
#
# Every channel is SYMLINKED at the resolved real file, never copied.
# Copying a signed binary is how this fork previously produced one that died
# with SIGKILL 137; that did not reproduce for a linker-signed adhoc build
# when this target was verified, but it does bite when a stable signing
# identity is configured (see scripts/sign-darwin-local.sh). Linking sidesteps
# the question entirely, costs nothing for a 250MB binary, and keeps the
# deployed build legible -- `ls -l` on any channel names the artifact.
#
# The actual guard is the run-proof below: deploy-fleet executes the binary
# and refuses to move a single channel until it has, so a build that cannot
# run never reaches a deployment whatever the cause.
#
# A channel that already IS the build is kept, not linked. That test compares
# file IDENTITY (-ef: same device and inode after resolving symlinks), never
# path text: `ln -sfn x x` unlinks the real binary and leaves a
# self-referential loop, and the readlink verification below still passes --
# the link holds exactly what it was asked to hold -- so a textual compare
# exits 0 while reporting success over a destroyed, unrecoverable build. Any
# same-file/different-spelling pairing defeats a string compare: an interior
# "/./" in GC_DEPLOY_BINARY, a channel directory that is itself a symlink, a
# hard link, a relative path resolved against a different pwd, a symlinked
# $$HOME. The `[ -e ]` conjunct is load-bearing, not decorative: it keeps a
# DANGLING channel falling through to the link, which is what repairs it.
#
# Because readlink can only confirm what was just written, the loop is
# followed by an execution readback: every channel is run. That is the
# property this target owes its caller -- the pre-flight proves the ARTIFACT
# runs, and only the readback proves the CHANNELS do.
#
# All channels move together; repointing only some re-splits them.
.PHONY: deploy-fleet
deploy-fleet:
	@set -e; \
		target="$(GC_DEPLOY_BINARY)"; \
		if [ ! -e "$$target" ]; then \
			echo "ERROR: GC_DEPLOY_BINARY $$target does not exist -- run 'make install' first, or name a binary built by 'make artifact'" >&2; \
			exit 2; \
		fi; \
		case "$$target" in /*) ;; *) target="$$(pwd)/$$target";; esac; \
		hops=0; \
		while [ -L "$$target" ]; do \
			hops=$$((hops + 1)); \
			if [ "$$hops" -gt 40 ]; then \
				echo "ERROR: symlink loop resolving $(GC_DEPLOY_BINARY)" >&2; exit 1; \
			fi; \
			link=$$(readlink "$$target"); \
			case "$$link" in /*) target="$$link";; *) target="$$(dirname "$$target")/$$link";; esac; \
		done; \
		if [ ! -f "$$target" ] || [ ! -x "$$target" ]; then \
			echo "ERROR: $$target is not an executable file" >&2; exit 1; \
		fi; \
		if ! "$$target" version >/dev/null 2>&1; then \
			echo "ERROR: $$target does not run ('$$target version' failed) -- refusing to point a running deployment at it." >&2; \
			echo "       On macOS a damaged codesign gives SIGKILL 137: build in place, and prefer linking over copying a signed binary." >&2; \
			exit 1; \
		fi; \
		echo "Deploying $$target"; \
		moved=0; \
		for channel in $(GC_DEPLOY_CHANNELS); do \
			dir=$$(dirname "$$channel"); \
			if [ ! -d "$$dir" ]; then echo "  skip $$channel (no $$dir)"; continue; fi; \
			if [ -e "$$channel" ] && [ "$$channel" -ef "$$target" ]; then \
				echo "  keep $$channel (already the build)"; moved=$$((moved + 1)); continue; \
			fi; \
			pinned=; \
			if [ -e "$$channel" ] || [ -L "$$channel" ]; then \
				if [ "$$(uname)" = "Darwin" ]; then \
					case "$$(ls -ldO "$$channel" 2>/dev/null | awk '{print $$5}')" in \
						*uchg*) pinned="$$channel"; \
							trap 'chflags -h uchg "$$pinned" 2>/dev/null || true' EXIT INT TERM HUP; \
							chflags -h nouchg "$$channel";; \
					esac; \
				fi; \
			fi; \
			swap="$$channel.deploytmp.$$$$"; \
			ln -sfn "$$target" "$$swap"; \
			mv -f "$$swap" "$$channel"; \
			if [ "$$(readlink "$$channel")" != "$$target" ]; then \
				echo "ERROR: $$channel did not take the link" >&2; exit 1; \
			fi; \
			if [ -n "$$pinned" ]; then chflags -h uchg "$$channel"; trap - EXIT INT TERM HUP; fi; \
			moved=$$((moved + 1)); \
			echo "  $$channel -> $$target"; \
		done; \
		if [ "$$moved" -eq 0 ]; then \
			echo "ERROR: no deploy channel was written -- GC_DEPLOY_CHANNELS matched nothing on this machine." >&2; \
			echo "       Reporting success having moved nothing is the failure this target exists to prevent." >&2; \
			exit 1; \
		fi; \
		for channel in $(GC_DEPLOY_CHANNELS); do \
			[ -e "$$channel" ] || continue; \
			if ! "$$channel" version >/dev/null 2>&1; then \
				echo "ERROR: $$channel does not run after deploy -- the fleet would be dead." >&2; \
				exit 1; \
			fi; \
		done; \
		echo "Deploy channels now resolve to one binary, and every one of them ran."; \
		echo "Restart the supervisor so it execs the new image (e.g. gc service restart) -- a running process keeps the image it started with."

## generate: regenerate JSON schemas and reference docs
generate:
	go run ./cmd/genschema

## check-schema: verify generated docs are up to date
check-schema: generate
	@git diff --exit-code docs/reference/ || \
		(echo "Error: generated docs stale. Run 'make generate'" && exit 1)

## clean: remove build artifacts
clean:
	rm -f $(BUILD_DIR)/$(BINARY)

## complexity: print an advisory cyclomatic complexity report for shipped Go
complexity:
	@./scripts/ci/complexity.sh report

## complexity-diff: compare head with COMPLEXITY_BASE_REF (origin/main by default)
complexity-diff:
	@./scripts/ci/complexity.sh diff

## complexity-check: fail when complexity differs from the checked-in baseline
complexity-check:
	@./scripts/ci/complexity.sh check

## complexity-update: intentionally refresh the checked-in complexity snapshot
complexity-update:
	@./scripts/ci/complexity.sh update

## check: fast quality gates: the shell guards below plus `make test` (bazel test //...: nogo lint/vet, formatting, generated artifacts, unit tests)
check: check-release-dist-ignore check-go-clean-cache check-routed-test-rows check-split-topology-rows check-residency-boundary test

## check-go: the same gates without Bazel: golangci-lint fmt/lint, go vet, go test (offline convenience; CI does not run it)
check-go: fmt-check lint-golangci vet-go check-release-dist-ignore check-go-clean-cache check-routed-test-rows check-split-topology-rows check-residency-boundary test-go

## check-release-dist-ignore: keep GoReleaser output from marking release builds dirty
check-release-dist-ignore:
	@ tracked=$$(git ls-files -- dist); \
	if [ -n "$$tracked" ]; then \
		echo "ERROR: release output is tracked:" >&2; \
		echo "$$tracked" >&2; \
		exit 1; \
	fi; \
	if git check-ignore --no-index -q dist/metadata.json; then \
		echo "check-release-dist-ignore: OK (/dist/ is ignored)"; \
	else \
		echo "ERROR: dist/metadata.json is not ignored; GoReleaser builds will report vcs.modified=true" >&2; \
		exit 1; \
	fi

## check-routed-test-rows: enforce the six-row matrix on read-path routed tests
## Prevents per-file read-path migrations (ga-h6w) from regressing below the
## six mandatory rows (api-happy-path, api-cache-not-live, api-500-fallback,
## api-404-error, controller-down, escape-hatch).
check-routed-test-rows:
	./scripts/check-routed-test-rows.sh

## check-split-topology-rows: keep every split-store conformance invariant on both topologies
## The split-store bug class is a fix that is correct on one store arrangement and
## wrong on the other, so an invariant that runs on only one row is worse than no
## invariant: it reads as coverage. Enforces that every t.Run("I<n>...") in
## TestSplitTopologyConformance routes through forEachTopology/forEachTopologyWithRig
## and that the suite never constructs an env directly.
check-split-topology-rows:
	./scripts/check-split-topology-rows.sh

## check-residency-boundary: forbid a new store-enumeration site outside internal/storeref
## The lookup contract ("which stores answer this question, in what order, with what
## failure semantics") lives in internal/storeref. Every list assembled elsewhere
## re-derives the identity gate, the leg order, the dedupe rule and the fail-loud
## policy, and each restatement is a chance to get one clause wrong (the ~90-site
## census behind ga-axin6, ga-whzrt, ga-j4ob9). Shrink-only ratchet against
## scripts/residency-boundary-baseline.txt; the AST half runs as
## TestResidencyResolverBoundary in ./scripts.
check-residency-boundary:
	./scripts/check-residency-boundary.sh --self-test
	./scripts/check-residency-boundary.sh

## check-gomod-replace: block unreleased replace directives (pseudo-version, local path, git ref)
## Tripwire for the 2026-06-11 incident where PR #3489 shipped a pseudo-version replace
## (=> v1.0.5-0.20260611054652-dc0561af28e9) that violated the public-project release policy.
## Policy: only released semver tags allowed; human-operator bypass required for exceptions.
check-gomod-replace:
	bash scripts/check-gomod-replace.sh go.mod

## check-core-boundary: guard the open-core boundary (no commercial coupling in the OSS module)
check-core-boundary:
	bash scripts/check-core-boundary.sh

## check-native-dependency-surface: guard native beads dependency and binary growth
check-native-dependency-surface:
	bash scripts/check-native-dependency-surface.sh

## check-eventexport-isolation: keep the OSS event-export surface brand-free, single-sourced, and internal-free
check-eventexport-isolation:
	bash scripts/check-eventexport-isolation.sh

## check-go-clean-cache: keep `go clean -cache` out of the codebase (fork-maintainer-guide.md "Build Cache Conventions")
## Commit-time half of the ban only. It stops the command entering a script,
## Makefile recipe, CI step or exec.Command; it does NOT stop anyone running it
## (neither vp-g96b nor the 2026-09-05 recurrence was ever committed). The
## runtime half is scripts/go-clean-cache-shim.sh, and the supported way to
## reclaim space -- the alternative the refusal names -- is
## scripts/trim-go-build-cache.sh, whose suite runs here too.
## Self-tests run from here rather than from Go wrappers: a wrapper per suite
## would add an exec.Command call site in a new _test.go file, which the P0.4
## resource census ratchets as untagged subprocess growth. Same shape as
## check-residency-boundary above.
check-go-clean-cache:
	bash scripts/test-check-go-clean-cache.sh
	bash scripts/test-go-clean-cache-shim.sh
	bash scripts/test-trim-go-build-cache.sh
	bash scripts/check-go-clean-cache.sh

## check-resync-loss: detect Category-A resync merge loss (fork-added declarations silently dropped)
## Pass MERGE=<sha> to check a specific merge commit; defaults to HEAD. Every
## resync must run this on the merge commit before push (AGENTS.md "Resync
## conventions" rule 5; bead ga-d32bn).
check-resync-loss:
	./scripts/check-resync-loss.sh $(MERGE)

## check-bd: verify bd (beads CLI) is installed
check-bd:
	@command -v bd >/dev/null 2>&1 || \
		(echo "Error: bd not found. See docs/getting-started/installation.md" && exit 1)

## check-docker: verify docker and buildx are available
check-docker:
	@command -v docker >/dev/null 2>&1 || \
		(echo "Error: docker not found. Install: https://docs.docker.com/engine/install/" && exit 1)
	@docker buildx version >/dev/null 2>&1 || \
		(echo "Error: docker buildx not found. Run: make install-buildx" && exit 1)

## check-dolt: verify dolt is installed
check-dolt:
	@command -v dolt >/dev/null 2>&1 || \
		(echo "Error: dolt not found. See docs/getting-started/installation.md" && exit 1)

## check-version-tag: verify HEAD's release tag (if any) is a clean stable vX.Y.Z
## No-op on untagged HEADs, so safe to run on every checkout. Used by release.yml
## to reject pre-release tags (vX.Y.Z-rc1, -beta, etc.) — the release workflow
## publishes stable releases only.
check-version-tag:
	@TAG=$$(git describe --tags --exact-match HEAD 2>/dev/null || true); \
	if [ -z "$$TAG" ]; then \
		echo "check-version-tag: HEAD is not a release tag, skipping"; \
		exit 0; \
	fi; \
	if echo "$$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "check-version-tag: OK ($$TAG is a stable release tag)"; \
		exit 0; \
	fi; \
	if echo "$$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+-'; then \
		echo "ERROR: tag '$$TAG' has a pre-release suffix"; \
		echo "The release workflow publishes stable releases only."; \
		echo "Pre-release tags should not trigger release.yml."; \
		exit 1; \
	fi; \
	echo "ERROR: tag '$$TAG' is not a vX.Y.Z release tag"; \
	echo "Release tags must match vMAJOR.MINOR.PATCH exactly."; \
	exit 1

## check-all: make check plus the acceptance and integration Bazel suites
check-all: check test-acceptance test-integration

## check-all-go: check-go plus go test integration and docs sync, without Bazel
check-all-go: check-go check-bd check-dolt check-docker test-integration-go check-docs-go

LINT_CHANGED_REF ?= HEAD
LINT_CHANGED_SCOPE ?= worktree
LINT_FLAGS ?=
LINT_GOMEMLIMIT ?= 6GiB
LINT_ENV = GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GOMEMLIMIT=$(LINT_GOMEMLIMIT)
QUALITY_GATE_GOFLAGS = $$(go env GOFLAGS | sed -E 's/(^|[[:space:]])-mod=[^[:space:]]+//g') -mod=readonly
CI_STATIC_SELECT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))scripts/ci-static-select

# Lint and vet run as nogo (//tools/nogo): go vet's analyzers plus the
# linters .golangci.yml enables, validated beside every Go compile, so
# `bazel build`/`bazel test` (local or on rbe-west) fail on findings. Only the
# nogo output group is requested: analysis without linking any binary.
NOGO_BAZEL ?= bazel
NOGO_BUILD_FLAGS ?= --keep_going --output_groups=nogo_fix

## lint: run the nogo analyzers (go vet + golangci-lint's linters) over every Go package with Bazel
lint:
	$(NOGO_BAZEL) build $(NOGO_BUILD_FLAGS) //...

## lint-changed: run nogo over the Bazel packages of changed Go files (LINT_CHANGED_SCOPE=staged|tracked|worktree)
# testdata paths are dropped with sed, not a case statement: macOS /bin/sh
# (bash 3.2) cannot parse a case pattern's ')' inside $(...).
lint-changed:
	@case "$(LINT_CHANGED_SCOPE)" in \
		staged) \
			files="$$(git diff --cached --name-only --diff-filter=ACMRT -- '*.go')"; \
			;; \
		tracked) \
			files="$$(git diff --name-only --diff-filter=ACMRT "$(LINT_CHANGED_REF)" -- '*.go')"; \
			;; \
		worktree) \
			files="$$( \
				git diff --name-only --diff-filter=ACMRT "$(LINT_CHANGED_REF)" -- '*.go'; \
				git diff --cached --name-only --diff-filter=ACMRT -- '*.go'; \
				git ls-files --others --exclude-standard -- '*.go'; \
			)"; \
			;; \
		*) \
			echo "unknown LINT_CHANGED_SCOPE=$(LINT_CHANGED_SCOPE); expected staged, tracked, or worktree" >&2; \
			exit 2; \
			;; \
	esac; \
	if [ -z "$$files" ]; then \
		echo "lint-changed: no changed Go files"; \
		exit 0; \
	fi; \
	selected="$$(printf '%s\n' "$$files" | sed -e '/^$$/d' -e '/^testdata\//d' -e '/\/testdata\//d' | while IFS= read -r file; do \
		dir="$$(dirname "$$file")"; \
		if [ ! -f "$$dir/BUILD.bazel" ]; then echo "missing $$dir"; \
		elif [ "$$dir" = "." ]; then echo "//:all"; \
		else echo "//$$dir:all"; fi; \
	done | sort -u)"; \
	missing="$$(printf '%s\n' "$$selected" | sed -n 's/^missing //p')"; \
	if [ -n "$$missing" ]; then \
		printf 'lint-changed: %s has no BUILD.bazel; run '"'"'make bazel-sync'"'"'\n' $$missing >&2; \
		exit 1; \
	fi; \
	targets="$$(printf '%s\n' "$$selected" | sed '/^$$/d')"; \
	if [ -z "$$targets" ]; then \
		echo "lint-changed: no Bazel Go packages"; \
		exit 0; \
	fi; \
	echo "lint-changed: $$(printf '%s\n' "$$targets" | tr '\n' ' ')"; \
	$(NOGO_BAZEL) build $(NOGO_BUILD_FLAGS) $$targets

## vet: go vet's analyzers run inside nogo; same as lint
vet: lint

## lint-affected, lint-full: aliases of lint, kept for gate formulas (Bazel re-analyzes only packages whose inputs changed)
lint-affected lint-full: lint

## lint-golangci: golangci-lint outside Bazel, for hosts nogo does not cover (macOS jobs: darwin-only files)
lint-golangci: $(GOLANGCI_LINT)
	$(LINT_ENV) $(GOLANGCI_LINT) run $(LINT_FLAGS) ./...

## VET_TAG_SCOPES: build tags that gate test code the default vet never
## compiles, each with the package pattern that carries it. Vetted one by
## one so a file that only exists under one tag (test/acceptance/worker_inference
## is acceptance_c-only) cannot reach main uncompilable: a 2026-09-02 gofmt
## reshape broke two multi-value returns there and every fast-tier gate
## passed, because nothing before the nightly ever built that tag. Scoped to
## the carrying packages so the whole pass stays in the tens of seconds
## (vetting ./... per tag took 6 minutes).
VET_TAG_SCOPES ?= \
	acceptance_a=./test/acceptance/... \
	acceptance_b=./test/acceptance/... \
	acceptance_c=./test/acceptance/... \
	acceptance_bd_contract=./test/acceptance/... \
	chaos_dolt=./test/integration/... \
	loadharness=./test/loadharness/... \
	tmux_integration=./internal/runtime/tmux/... \
	liveprobe=./cmd/gc/... \
	productmetrics_testhook=./cmd/gc/...,./internal/productmetrics/... \
	gascity_native_beads=./cmd/gc/...,./internal/beads/...

## vet-go: plain `go vet` outside Bazel, for hosts nogo does not cover (macOS jobs), plus one scoped pass per build tag that gates test code
vet-go:
	GOFLAGS="$(QUALITY_GATE_GOFLAGS)" go vet ./...
	@for scope in $(VET_TAG_SCOPES); do \
		tag=$${scope%%=*}; pkgs=$$(printf '%s' "$${scope#*=}" | tr ',' ' '); \
		echo "vet -tags $$tag $$pkgs"; \
		GOFLAGS="$(QUALITY_GATE_GOFLAGS)" go vet -tags "$$tag" $$pkgs || exit 1; \
	done

## fmt-check: fail if formatting would change files
fmt-check: $(GOLANGCI_LINT)
	$(LINT_ENV) $(GOLANGCI_LINT) fmt --diff ./...

## fmt-check-changed: fail if formatting would change a regular changed Go file
fmt-check-changed: $(GOLANGCI_LINT)
	@$(LINT_ENV) "$(CI_STATIC_SELECT)" fmt-check-changed "$(GOLANGCI_LINT)"

## fmt: auto-fix formatting
fmt: $(GOLANGCI_LINT)
	GOMEMLIMIT=$(LINT_GOMEMLIMIT) $(GOLANGCI_LINT) fmt ./...

## TEST_ENV: env -i wrapper for `go test` invocations. Strips host env so
## agent-session vars (GC_CITY, GC_HOME, GC_SESSION_ID, ...) cannot leak into
## tests and corrupt live cities. Only the allowlist below survives. To opt
## extra vars through, set EXTRA_TEST_ENV='FOO=bar BAZ=qux' on the make line.
## See PR #746.
##
## Load-bearing: GC_DOLT_PORT and BEADS_DOLT_SERVER_PORT are deliberately NOT in
## the allowlist below. They point bd at the live shared city Dolt server, so
## letting them reach `go test` makes every bd-forking test write to PRODUCTION
## Dolt — 18+ parallel workers pegged the shared server and stalled bd writes
## city-wide (ga-w2kh1r). Do not add them. For a bare `go test` that bypasses
## this wrapper, internal/testenv scrubs these vars at test-binary init in every
## covered package (enforced by TestRequiresDedicatedTestenvImportFile).
##
## The pane shell is pinned to /bin/sh rather than forwarded. A test that opens
## a tmux or herdr pane runs that shell in it, and the invoking user's zsh under
## a fresh HOME (a release gate, CI) opens its new-user wizard, which swallows
## the typed text (ga-yghhjf). The nested env -i blocks in
## scripts/test-local-parallel, scripts/test-go-test-shard and
## scripts/test-integration-shard mirror the pin (see scripts/AGENTS.md).
GOPATH_VAL    := $(shell go env GOPATH)
GOCACHE_VAL   := $(shell go env GOCACHE)
GOMODCACHE_VAL := $(shell go env GOMODCACHE)
GOTMPDIR_VAL  := $(shell go env GOTMPDIR)
GOROOT_VAL    := $(shell go env GOROOT)
TEST_ENV = env -i \
	PATH="$$PATH" \
	HOME="$$HOME" \
	USER="$$USER" \
	LOGNAME="$$LOGNAME" \
	SHELL=/bin/sh \
	GIT_CONFIG_NOSYSTEM=1 \
	GIT_CONFIG_GLOBAL="$$(scripts/test-gitconfig-path)" \
	LANG="$$LANG" \
	TMPDIR="$${TMPDIR:-/var/tmp}" \
	OBSERVABLE_TEST_LOG="$${OBSERVABLE_TEST_LOG-}" \
	OBSERVABLE_FAILURE_LINES="$${OBSERVABLE_FAILURE_LINES-}" \
	GC_TEST_NO_SLICE="$${GC_TEST_NO_SLICE-}" \
	XDG_RUNTIME_DIR="$$XDG_RUNTIME_DIR" \
	GOPATH="$(GOPATH_VAL)" \
	GOCACHE="$(GOCACHE_VAL)" \
	GOMODCACHE="$(GOMODCACHE_VAL)" \
	GOTMPDIR="$(GOTMPDIR_VAL)" \
	GOROOT="$${GOROOT:-$(GOROOT_VAL)}" \
	GOENV="$${GOENV-}" \
	GOFLAGS="$${GOFLAGS-}" \
	GO111MODULE="$${GO111MODULE-}" \
	GOEXPERIMENT="$${GOEXPERIMENT-}" \
	GOPROXY="$${GOPROXY-}" \
	GOPRIVATE="$${GOPRIVATE-}" \
	GONOPROXY="$${GONOPROXY-}" \
	GONOSUMDB="$${GONOSUMDB-}" \
	GOSUMDB="$${GOSUMDB-}" \
	GOINSECURE="$${GOINSECURE-}" \
	GOVCS="$${GOVCS-}" \
	GOWORK="$${GOWORK-}" \
	ANTHROPIC_BASE_URL="$${ANTHROPIC_BASE_URL-}" \
	ANTHROPIC_API_KEY="$${ANTHROPIC_API_KEY-}" \
	ANTHROPIC_AUTH_TOKEN="$${ANTHROPIC_AUTH_TOKEN-}" \
	ANTHROPIC_DEFAULT_HAIKU_MODEL="$${ANTHROPIC_DEFAULT_HAIKU_MODEL-}" \
	ANTHROPIC_DEFAULT_SONNET_MODEL="$${ANTHROPIC_DEFAULT_SONNET_MODEL-}" \
	ANTHROPIC_DEFAULT_OPUS_MODEL="$${ANTHROPIC_DEFAULT_OPUS_MODEL-}" \
	CLAUDE_CODE_SUBAGENT_MODEL="$${CLAUDE_CODE_SUBAGENT_MODEL-}" \
	CLAUDE_CODE_EFFORT_LEVEL="$${CLAUDE_CODE_EFFORT_LEVEL-}" \
	CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="$${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC-}" \
	OLLAMA_API_KEY="$${OLLAMA_API_KEY-}" \
	XIAOMI_API_KEY="$${XIAOMI_API_KEY-}" \
	ZCODE_API_KEY="$${ZCODE_API_KEY-}" \
	ZCODE_CJS="$${ZCODE_CJS-}" \
	ZCODE_MODEL="$${ZCODE_MODEL-}" \
	ZCODE_BASE_URL="$${ZCODE_BASE_URL-}" \
	ZCODE_NODE_BIN="$${ZCODE_NODE_BIN-}" \
	ZCODE_STORAGE_DIR="$${ZCODE_STORAGE_DIR-}" \
	CGO_CPPFLAGS="$${CGO_CPPFLAGS-}" \
	CGO_LDFLAGS="$${CGO_LDFLAGS-}" \
	$(EXTRA_TEST_ENV)

## test-ci-policy: run the fast workflow-policy suite
test-ci-policy:
	$(TEST_ENV) PYTHONDONTWRITEBYTECODE=1 python3 -S -m unittest discover -s .github/workflows/scripts -p 'test_runner_policy.py'
	$(TEST_ENV) PYTHONDONTWRITEBYTECODE=1 python3 -S -m unittest discover -s .github/workflows/scripts -p 'test_ci_suite_coverage.py'
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 ./scripts/cipolicy
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 ./scripts/prwatchdog/...
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 -tags integration -run '^(TestLintAndVetRunAsNogoInBazel|TestLintChangedBuildsNogoForChangedBazelPackages|TestChangedFormattingScopesToTheDiff)$$' ./scripts
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 -run '^(TestBDVersionPins|TestDoltVersionPins)$$' ./scripts
	$(TEST_ENV) bash scripts/test-tier-coverage.sh

## test: bazel test //..., bazel.yml's unit lane: every untagged go_test plus nogo, format, generated-artifact and policy targets
test: bazel-tmpdir
	$(BAZEL_TEST) //...

## test-go: fast unit tests with plain go test (skip integration-tagged and GC_FAST_UNIT-gated process tests)
## The skipped cmd/gc process-backed scenarios remain covered by
## `make test-cmd-gc-process` locally and in CI by bazel.yml's integration-packages lane.
## Bound package parallelism so subprocess-heavy packages do not starve each
## other into false 5s probe/condition timeouts. Use -count=1 so pre-commit
## reports actual test results instead of hanging after PASS while Go computes
## cache input hashes over local working files.
## Wrapped in $(TEST_ENV) — see comment above for why.
test-go: test-fsys-darwin-compile
	$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GC_FAST_UNIT=1 scripts/go-test-observable test -- -p=4 -count=1 -timeout 15m ./...

## test-herdr-live: run the live herdr journeys against a real herdr server —
## the provider's own tier under internal/runtime/herdr, plus the controller's
## event-driven liveness journeys under cmd/gc. These drive panes, force
## agent-status reports and bounce the server, so they are opt-in rather than
## part of the fast unit sweep (see
## internal/runtime/herdr/herdrtest/livegate.go). Skips cleanly when herdr is
## absent. Wrapped in $(TEST_ENV), which is `env -i`, so the opt-in must be set
## inside it.
test-herdr-live:
	$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GC_HERDR_LIVE_TESTS=1 scripts/go-test-observable test -- -count=1 -timeout 10m ./internal/runtime/herdr/
	$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GC_HERDR_LIVE_TESTS=1 scripts/go-test-observable test -- -count=1 -timeout 10m -run LiveHerdr ./cmd/gc/

# MAC_UNIT_PKGS excludes cmd/gc from the Mac unit sweep; cmd/gc runs
# sharded via the mac-cmd-gc-process CI matrix job instead.
MAC_UNIT_PKGS = $(shell go list ./... | grep -v '/cmd/gc$$')

## test-mac-pkgs-guard: fail when MAC_UNIT_PKGS is empty. A failed go list
## expands it to nothing, and go test over no packages passes vacuously.
.PHONY: test-mac-pkgs-guard
test-mac-pkgs-guard:
	@test -n "$(strip $(MAC_UNIT_PKGS))" || { echo "test-mac: go list found no packages; refusing to pass over an empty sweep" >&2; exit 1; }

## test-mac: Mac unit sweep with cmd/gc excluded; cmd/gc covered by the Mac sharded job.
test-mac: test-mac-pkgs-guard test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 scripts/go-test-observable test-mac -- -p=4 -count=1 -timeout 15m $(MAC_UNIT_PKGS)

## test-coarse-clock: creation-order suites under a millisecond MemStore clock.
## The gc_coarse_clock tag reproduces on Linux the same-tick creates macOS's
## coarser wall clock hits; narrow it with COARSE_CLOCK_RUN='TestLane'.
COARSE_CLOCK_RUN ?= .
test-coarse-clock:
	$(TEST_ENV) GC_FAST_UNIT=0 scripts/go-test-observable test-coarse-clock -- -tags gc_coarse_clock -count=1 -timeout 60m -run '$(COARSE_CLOCK_RUN)' ./internal/beads/ ./cmd/gc/

LOCAL_TEST_JOBS ?= $(shell ./scripts/test-local-job-count)

# Deliberately empty: scripts/test-local-parallel owns the default per-package
# go test budget (ga-9au). Duplicating the number here is how the unit sweep and
# the cmd/gc shards drifted onto separate timeouts before. The assignment exists
# only so an operator override survives TEST_ENV's env -i, the way LOCAL_TEST_JOBS
# does; the sibling *-parallel targets below run without env -i and inherit an
# override from the environment directly.
GO_TEST_TIMEOUT ?=

## test-fast-parallel: run the default fast suite with cmd/gc sharded locally
test-fast-parallel:
	$(TEST_ENV) GC_PUSH_GATE_NO_CAP="$${GC_PUSH_GATE_NO_CAP-}" PUSH_GATE_MAX_CONCURRENT="$${PUSH_GATE_MAX_CONCURRENT-}" PUSH_GATE_MAX_WAIT_SECONDS="$${PUSH_GATE_MAX_WAIT_SECONDS-}" PUSH_GATE_POLL_SECONDS="$${PUSH_GATE_POLL_SECONDS-}" LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) GO_TEST_TIMEOUT=$(GO_TEST_TIMEOUT) ./scripts/test-local-parallel fast

## test-fsys-darwin-compile: cross-compile internal/fsys for macOS so
## unix.Stat_t field-type regressions fail in the default fast test path.
test-fsys-darwin-compile:
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GOOS=darwin GOARCH=arm64 go test -c -o "$$tmp/fsys.test" ./internal/fsys

## test-pack-registry-live: run the opt-in gascity-packs registry canary
test-pack-registry-live:
	@if [ -z "$${GC_TEST_GASCITY_PACKS_REGISTRY:-}" ]; then \
		echo "Set GC_TEST_GASCITY_PACKS_REGISTRY to main or a gascity-packs registry.toml source"; \
		echo "Example: GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live"; \
		exit 2; \
	fi
	@# Keep the live canary portable on runners and local machines that do not
	@# have the optional ICU C headers needed by the default CGO build path.
	$(TEST_ENV) CGO_ENABLED=0 GC_TEST_GASCITY_PACKS_REGISTRY="$${GC_TEST_GASCITY_PACKS_REGISTRY}" go test ./cmd/gc -run '^TestPackRegistryLiveGascityPacksCatalog$$' -count=1
	$(TEST_ENV) CGO_ENABLED=0 GC_TEST_GASCITY_PACKS_REGISTRY="$${GC_TEST_GASCITY_PACKS_REGISTRY}" go test -tags acceptance_a -timeout 10m ./test/acceptance -run '^TestPackRegistryLiveImportsEveryCatalogPack$$' -count=1

## check-embedded-pins: verify the bundled pack pin and bd/dolt pins are the latest releases
.PHONY: check-embedded-pins
check-embedded-pins:
	scripts/check-embedded-pins

## update-bundled-gastown-pack: pin the gastown module/constants/example to the latest registry release
update-bundled-gastown-pack:
	scripts/update-bundled-gastown-pack

## test-native-doltlite-beads: compile and run the native DoltLite read-store suite
test-native-doltlite-beads:
	$(TEST_ENV) CGO_ENABLED=0 go test -tags gascity_native_beads -run '^TestDoltlite' ./internal/beads -count=1

## sync-bd-corpus: vendor the bd contract corpus from a beads release (BD_CORPUS_TAG=vX.Y.Z)
sync-bd-corpus:
	scripts/sync-bd-corpus.sh

## test-cmd-gc-process: run the full non-short cmd/gc suite, including the
## process-backed lifecycle coverage routed out of the default fast loop
test-cmd-gc-process:
	$(TEST_ENV) GC_FAST_UNIT=0 scripts/go-test-observable test-cmd-gc-process -- -timeout 25m ./cmd/gc
	$(MAKE) test-productmetrics-testhook

## test-productmetrics-testhook: run the focused tagged product-metrics contracts
test-productmetrics-testhook:
	$(TEST_ENV) scripts/go-test-observable test-productmetrics-testhook -- -tags productmetrics_testhook -count=1 -run '^(TestProductMetricsTaggedBinaryProcessContracts|TestProductMetricsTesthookEndpointAcceptsOnlyLoopbackHTTPS|TestProductMetricsTaggedRunnerReadsInjectionOnlyAtInvocation|TestProductMetricsTesthookCAReadIsBounded|TestProductMetricsTaggedProcessFixtureIsEnabled|TestProductMetricsTestOnlyCensusEscapeIsNarrow)$$' ./cmd/gc

CMD_GC_PROCESS_SHARD ?= 1
CMD_GC_PROCESS_TOTAL ?= 6
# Soak knobs: run each shard's tests CMD_GC_PROCESS_COUNT times (go test
# -count) under a CMD_GC_PROCESS_TIMEOUT package timeout.
CMD_GC_PROCESS_COUNT ?= 1
CMD_GC_PROCESS_TIMEOUT ?= 20m
CMD_GC_COVER_TOTAL ?= 6
CMD_GC_COVER_SHARD ?= 1
test-cmd-gc-process-shard:
	$(TEST_ENV) GC_FAST_UNIT=0 GO_TEST_COUNT=$(CMD_GC_PROCESS_COUNT) GO_TEST_TIMEOUT=$(CMD_GC_PROCESS_TIMEOUT) ./scripts/test-go-test-shard ./cmd/gc $(CMD_GC_PROCESS_SHARD) $(CMD_GC_PROCESS_TOTAL)

## test-cmd-gc-process-parallel: run all cmd/gc process shards concurrently
test-cmd-gc-process-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel cmd-gc-process

## test-worker-core: run deterministic worker transcript and continuation conformance
test-worker-core:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest -run '^TestPhase1'

## test-worker-core-phase2: run deterministic phase-2 worker conformance coverage
test-worker-core-phase2:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest -run '^TestPhase2'
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/runtime/tmux -run '^TestPhase2'
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./cmd/gc -run '^TestPhase2(StartupMaterialization|InitialInputDelivery|InputResultFailureClassification)$$'

## test-worker-core-phase2-real-transport: run the live transport proof for phase 2
test-worker-core-phase2-real-transport:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 -tags integration ./cmd/gc -run '^TestPhase2WorkerCoreRealTransportProof$$'

## test-worker-core-phase2-all: run all phase-2 coverage while sharing package builds
test-worker-core-phase2-all:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest ./internal/runtime/tmux -run '^TestPhase2'
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 -tags integration ./cmd/gc -run '^TestPhase2(StartupMaterialization|InitialInputDelivery|InputResultFailureClassification|WorkerCoreRealTransportProof)$$'

WORKER_INFERENCE_PROFILE := $(if $(PROFILE),$(PROFILE),claude/tmux-cli)

## setup-worker-inference: install the provider CLI for PROFILE (default claude/tmux-cli)
setup-worker-inference:
	python3 scripts/worker_inference_setup.py install --profile "$(WORKER_INFERENCE_PROFILE)"

## test-worker-inference: run the live worker inference conformance package
##
## GC_HOME is passed through (and declared to internal/testenv, which scrubs it
## as a leak vector) because this suite edits city.toml IN-PROCESS, and
## config.ImplicitGCHome() returns "" inside a *.test binary unless GC_HOME is
## explicitly set. Without it every managed-city leg dies resolving the city's
## pack imports against the user-global repo cache. It is a cache path, not a
## credential; the isolation this suite relies on flows through the per-run
## GC_HOME it hands to the gc subprocesses it spawns.
test-worker-inference:
	$(TEST_ENV) GC_ACCEPTANCE_BD_BIN="$${GC_ACCEPTANCE_BD_BIN-}" GC_WORKER_INFERENCE_CURSOR_API_KEY="$${GC_WORKER_INFERENCE_CURSOR_API_KEY-}" GC_WORKER_INFERENCE_CURSOR_API_KEY_FILE="$${GC_WORKER_INFERENCE_CURSOR_API_KEY_FILE-}" CURSOR_API_KEY="$${CURSOR_API_KEY-}" PROFILE="$(WORKER_INFERENCE_PROFILE)" GC_WORKER_REPORT_DIR="$(GC_WORKER_REPORT_DIR)" GC_HOME="$${GC_HOME:-$$HOME/.gc}" GC_TESTENV_PASSTHROUGH=GC_HOME go test -count=1 -tags acceptance_c -timeout 45m -v ./test/acceptance/worker_inference

## test-worker-inference-phase3: alias for the live worker inference conformance package
test-worker-inference-phase3: test-worker-inference

## Tier A acceptance knobs (command-level PR gate) for test-acceptance-go.
## ACCEPTANCE_TIMEOUT overrides the go-test timeout. The unsharded local/CI
## target runs the command-heavy Tier A package serially; RC gate shards it.
##
## GC_ACCEPTANCE_BD_BIN selects the bd the beads topology tests drive. Without
## a bd that has --proxied-server they skip, which is why the default CI run is
## unaffected. GC_ACCEPTANCE_LEGACY_GC_BIN is a gc built before the ownership
## journal; only the legacy shape of the topology matrix needs it, and only
## that shape skips without it. TESTING.md documents both.
ACCEPTANCE_TIMEOUT ?= 15m
## ACCEPTANCE_GO_TEST_FLAGS passes extra `go test` flags through, which is how
## you narrow a run: ACCEPTANCE_GO_TEST_FLAGS='-run TestBeadsInitTopologyMatrix'
ACCEPTANCE_GO_TEST_FLAGS ?=
## TEST_ENV is `env -i` with a fixed allowlist, so every variable the acceptance
## tests read has to be named on the recipe line below or it is dropped and the
## test binary skips on it. That is how `make test-beads-topology-matrix` came
## to print `ok` in seconds having stood up zero shapes: the matrix opt-in never
## reached `go test`. Each of these defaults to the ambient value, so exporting
## it still works, and a target or the make line can override it.
ACCEPTANCE_TOPOLOGY_MATRIX ?= $(GC_ACCEPTANCE_TOPOLOGY_MATRIX)
ACCEPTANCE_REQUIRE_TOOLING ?= $(GC_REQUIRE_ACCEPTANCE_TOOLING)
ACCEPTANCE_REQUIRE_LEGACY_GC ?= $(GC_REQUIRE_ACCEPTANCE_LEGACY_GC)
## ACCEPTANCE_PERF turns on the proxied-native wall-clock gate in
## TestBeadsProxiedDefaultNativeLane (GC_ACCEPTANCE_PERF). Off by default: wall clock on a
## shared box is a statement about the box. The nightly perf lane sets it.
ACCEPTANCE_PERF ?= $(GC_ACCEPTANCE_PERF)
## test-acceptance: Tier A acceptance as bazel.yml's acceptance lane runs it (bazel test --config=acceptance)
test-acceptance: bazel-tmpdir
	$(BAZEL_TEST) --config=acceptance //test/acceptance:acceptance_test

## test-acceptance-go: Tier A acceptance with plain go test (honours the ACCEPTANCE_* knobs above)
test-acceptance-go:
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off GC_ACCEPTANCE_BEADS_PROVIDER="$${GC_ACCEPTANCE_BEADS_PROVIDER-}" GC_ACCEPTANCE_BD_BIN="$${GC_ACCEPTANCE_BD_BIN-}" GC_ACCEPTANCE_LEGACY_GC_BIN="$${GC_ACCEPTANCE_LEGACY_GC_BIN-}" GC_ACCEPTANCE_TOPOLOGY_MATRIX="$(ACCEPTANCE_TOPOLOGY_MATRIX)" GC_ACCEPTANCE_PERF="$(ACCEPTANCE_PERF)" GC_REQUIRE_ACCEPTANCE_TOOLING="$(ACCEPTANCE_REQUIRE_TOOLING)" GC_REQUIRE_ACCEPTANCE_LEGACY_GC="$(ACCEPTANCE_REQUIRE_LEGACY_GC)" go test -tags acceptance_a -timeout $(ACCEPTANCE_TIMEOUT) $(ACCEPTANCE_GO_TEST_FLAGS) ./test/acceptance/...

## test-beads-topology-matrix: run the init topology matrix on its own.
## Every supported way to initialise a beads scope — proxied-local, direct-local,
## direct- and proxied-external, the pre-journal GC-managed shape, doltlite, and
## a deferred GC_DOLT=skip init — walked through the same command list against a
## real bd and a real dolt. Needs a bd >= 1.3.0 in GC_ACCEPTANCE_BD_BIN and, for
## the legacy shape, a pre-journal gc in GC_ACCEPTANCE_LEGACY_GC_BIN. Eight
## shapes of real Dolt lifecycle take about an hour, hence the separate timeout.
##
## This target opts itself in to the matrix and turns a missing bd or dolt into
## a failure: a target that exists only to run the shapes has no honest way to
## report ok having run none of them. Pass BEADS_TOPOLOGY_MATRIX_REQUIRE_TOOLING=
## to get the old skip-on-missing-tooling behaviour back.
BEADS_TOPOLOGY_MATRIX_TIMEOUT ?= 90m
BEADS_TOPOLOGY_MATRIX_REQUIRE_TOOLING ?= 1
test-beads-topology-matrix:
	$(MAKE) test-acceptance-go ACCEPTANCE_TIMEOUT=$(BEADS_TOPOLOGY_MATRIX_TIMEOUT) \
		ACCEPTANCE_GO_TEST_FLAGS='-count=1 -run TestBeadsInitTopologyMatrix' \
		ACCEPTANCE_TOPOLOGY_MATRIX=1 \
		ACCEPTANCE_REQUIRE_TOOLING='$(BEADS_TOPOLOGY_MATRIX_REQUIRE_TOOLING)'

## test-bd-cli-contract: run only Gas City's external bd CLI compatibility contract.
## Keep this separate from hermetic Tier A so each supported bd version can run
## the same focused manifest without rebuilding gc or repeating unrelated flows.
BD_CLI_CONTRACT_TIMEOUT ?= 10m
test-bd-cli-contract:
	@command -v bd >/dev/null 2>&1 || (echo "Error: bd not found; cannot run external CLI contract" >&2; exit 1)
	$(TEST_ENV) go test -tags acceptance_bd_contract -timeout $(BD_CLI_CONTRACT_TIMEOUT) -count=1 \
		-run '^(TestBdBasicCRUD|TestBdDependencies|TestBdDestructive|TestBdWorkflow)$$' ./test/acceptance

## test-bd-cli-contract-home-isolation: HOME-isolation regression coverage for
## runBD (ga-1037rg / ga-yoxtux), kept as its own target rather than folded
## into test-bd-cli-contract's -run regex so that target's manifest stays
## exactly the focused 4-test external contract pinned by
## TestAcceptanceTargetsSeparateTierAFromExternalBdContracts
## (scripts/ci_critical_path_test.go). Run alongside test-bd-cli-contract,
## under the same acceptance_bd_contract tag and bd version on PATH, wherever
## that target runs in CI.
test-bd-cli-contract-home-isolation:
	@command -v bd >/dev/null 2>&1 || (echo "Error: bd not found; cannot run external CLI contract" >&2; exit 1)
	$(TEST_ENV) go test -tags acceptance_bd_contract -timeout $(BD_CLI_CONTRACT_TIMEOUT) -count=1 \
		-run '^TestRunBDIsolatesHOMEFromSharedServerConfig$$' ./test/acceptance

## test-bd-conditional-release-contract: run the ReleaseIfCurrent CAS contract
## against the bd on PATH. It is separate from test-bd-cli-contract because it
## is the only contract that needs a real CAS-capable bd. Every pinned bd
## (BD_PREV_VERSION, BD_VERSION and BD_CURRENT_REF) carries
## `--if-assignee`/`--if-status`, so the row's capability skip fires on none
## of them. CI runs the row only against the BD_CURRENT_REF bd (v1.3.2):
## strictly in //internal/beads:bd_conditional_release_contract_test, and in
## the integration-packages lane. A BD_PREV_VERSION cell is ga-7ryjc6.
## GC_REQUIRE_BD_CONDITIONAL_RELEASE=1 turns the row's capability skip into a
## failure, so a cell cannot pass while proving nothing.
##
## The existence preflight closes the other way this cell can pass having proven
## nothing: a `-run` selector that matches no test is not an error to `go test`
## — it prints "[no tests to run]" and exits 0 — so renaming the row, moving it
## to another package, or dropping its build tag would leave a permanently green
## cell guarding nothing. `go test -list` resolves the SAME name, package and
## tags the run below uses, from the same variables, and must print the name
## back. Deriving both from one variable is why this guard cannot rot into a
## false pass: there is no second copy of the selector to drift, and any change
## to `-list` output that broke the match would fail the cell loudly rather than
## silently stop catching (which is the failure mode of grepping `go test`'s
## human-facing "no tests to run" warning instead).
BD_CONDITIONAL_RELEASE_TIMEOUT ?= 10m
BD_CONDITIONAL_RELEASE_TEST = TestBdStoreReleaseIfCurrentAgainstRealBd
BD_CONDITIONAL_RELEASE_PKG = ./internal/beads
test-bd-conditional-release-contract:
	@command -v bd >/dev/null 2>&1 || (echo "Error: bd not found; cannot run the conditional-release contract" >&2; exit 1)
	@listed=$$($(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -tags integration \
		-list '^$(BD_CONDITIONAL_RELEASE_TEST)$$' $(BD_CONDITIONAL_RELEASE_PKG) 2>&1) || { \
		printf '%s\n' "$$listed" >&2; \
		echo "Error: could not list $(BD_CONDITIONAL_RELEASE_TEST) in $(BD_CONDITIONAL_RELEASE_PKG)" >&2; \
		exit 1; }; \
	printf '%s\n' "$$listed" | grep -qx '$(BD_CONDITIONAL_RELEASE_TEST)' || { \
		echo "Error: $(BD_CONDITIONAL_RELEASE_TEST) does not exist in $(BD_CONDITIONAL_RELEASE_PKG) under -tags integration." >&2; \
		echo "The -run selector below would match nothing, and this cell would pass having run no test." >&2; \
		echo "Point BD_CONDITIONAL_RELEASE_TEST/_PKG at the row's current name and home." >&2; \
		exit 1; }
	$(TEST_ENV) GC_REQUIRE_BD_CONDITIONAL_RELEASE=1 GOFLAGS= GOENV=off GOWORK=off \
		go test -tags integration -timeout $(BD_CONDITIONAL_RELEASE_TIMEOUT) -count=1 \
		-run '^$(BD_CONDITIONAL_RELEASE_TEST)$$' $(BD_CONDITIONAL_RELEASE_PKG)

## test-acceptance-b: run Tier B acceptance tests (lifecycle, ~5 min, nightly)
ACCEPTANCE_B_TIMEOUT ?= 10m
test-acceptance-b:
	$(TEST_ENV) GC_ACCEPTANCE_BD_BIN="$${GC_ACCEPTANCE_BD_BIN-}" GC_ACCEPTANCE_SPLIT_RC1_GC_BIN="$${GC_ACCEPTANCE_SPLIT_RC1_GC_BIN-}" GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC="$${GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC-}" go test -tags acceptance_b -timeout $(ACCEPTANCE_B_TIMEOUT) -v ./test/acceptance/tier_b/...

## test-acceptance-split-storage: run the split-storage end-to-end acceptance
## test (Tier B tag; #5987): a real controller and a scripted worker run a
## graph.v2 formula across `gc storage migrate`. Needs bd >= 1.3.0
## (GC_ACCEPTANCE_BD_BIN), dolt and jq. The upgraded-city scenario uses a
## v1.5.0-rc1 gc: GC_ACCEPTANCE_SPLIT_RC1_GC_BIN, or one the test builds from
## the v1.5.0-rc1 tag (git fetch --no-tags --depth=1 origin
## +refs/tags/v1.5.0-rc1:refs/tags/v1.5.0-rc1). It skips without either unless
## GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC is set.
SPLIT_STORAGE_ACCEPTANCE_TIMEOUT ?= 20m
test-acceptance-split-storage:
	$(TEST_ENV) GC_ACCEPTANCE_BD_BIN="$${GC_ACCEPTANCE_BD_BIN-}" GC_ACCEPTANCE_GC_BIN="$${GC_ACCEPTANCE_GC_BIN-}" GC_ACCEPTANCE_SPLIT_RC1_GC_BIN="$${GC_ACCEPTANCE_SPLIT_RC1_GC_BIN-}" GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC="$${GC_REQUIRE_ACCEPTANCE_SPLIT_RC1_GC-}" GC_REQUIRE_ACCEPTANCE_TOOLING="$(ACCEPTANCE_REQUIRE_TOOLING)" go test -tags acceptance_b -count=1 -timeout $(SPLIT_STORAGE_ACCEPTANCE_TIMEOUT) -v -run '^TestSplitStorageMigratedFormulaCompletes$$' ./test/acceptance/tier_b

## test-acceptance-c: run Tier C acceptance tests (real inference, ~30-40 min, manual/nightly)
test-acceptance-c:
	$(TEST_ENV) go test -tags acceptance_c -timeout 45m -v ./test/acceptance/tier_c/...

## test-acceptance-all: run all acceptance tiers
test-acceptance-all: test-acceptance test-bd-cli-contract test-acceptance-b test-acceptance-c

## test-integration: integration-tagged suites as bazel.yml's integration lanes run them (bazel test --config=integration)
test-integration: bazel-tmpdir
	$(BAZEL_TEST) --config=integration //test:integration_packages //test/integration:integration_test

## test-integration-go: run all tests including integration (tmux, etc.) with plain go test
test-integration-go:
	$(TEST_ENV) go test -tags integration -timeout 30m ./...

## test-integration-huma: run just the Huma binary smoke test
test-integration-huma:
	$(TEST_ENV) go test -tags integration -timeout 2m -run TestHumaBinary ./test/integration/

## test-integration-shards: run the CI integration shards sequentially
test-integration-shards: test-integration-packages test-integration-review-formulas test-integration-bdstore test-integration-rest-smoke test-integration-rest-full

## test-integration-shards-parallel: run the CI integration shards concurrently
test-integration-shards-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) ./scripts/test-local-parallel integration

## test-local-full-parallel: run fast unit, cmd/gc process, and integration shards concurrently
test-local-full-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel full

## test-integration-shards-cover: run the CI integration coverage shards sequentially
test-integration-shards-cover: test-integration-packages-cover test-integration-review-formulas-cover test-integration-bdstore-cover test-integration-rest-smoke-cover test-integration-rest-full-cover

## test-integration-packages: run all integration-tagged packages except ./test/integration
## cmd/gc package shards default to GC_FAST_UNIT=1; use test-cmd-gc-process for the slow process suite.
test-integration-packages:
	./scripts/test-integration-shard packages

## test-integration-packages-cover: run the packages shard with a CI coverage profile
test-integration-packages-cover:
	GO_TEST_COVERPROFILE=coverage.integration-packages.txt ./scripts/test-integration-shard packages

## test-integration-review-formulas: run the long-running workflow formula integration tests
test-integration-review-formulas:
	@status=0; \
	$(MAKE) test-integration-review-formulas-basic || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-retries || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-recovery || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-integration-review-formulas-cover: run the review-formulas shard with a CI coverage profile
test-integration-review-formulas-cover:
	@status=0; \
	$(MAKE) test-integration-review-formulas-basic-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-retries-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-recovery-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	if [ $$status -eq 0 ]; then \
		./scripts/merge-coverprofiles coverage.integration-review-formulas.txt \
			coverage.integration-review-formulas-basic.txt \
			coverage.integration-review-formulas-retries.txt \
			coverage.integration-review-formulas-recovery.txt; \
	fi; \
	exit $$status

## test-integration-review-formulas-basic: run the core happy-path review-formulas tests
test-integration-review-formulas-basic:
	./scripts/test-integration-shard review-formulas-basic

## test-integration-review-formulas-basic-cover: run the basic review-formulas shard with coverage
test-integration-review-formulas-basic-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-basic.txt ./scripts/test-integration-shard review-formulas-basic

## test-integration-review-formulas-retries: run the retry/soft-fail review-formulas tests
test-integration-review-formulas-retries:
	./scripts/test-integration-shard review-formulas-retries

## test-integration-review-formulas-retries-cover: run the retry/soft-fail review-formulas shard with coverage
test-integration-review-formulas-retries-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-retries.txt ./scripts/test-integration-shard review-formulas-retries

## test-integration-review-formulas-recovery: run the crash/recovery review-formulas test
test-integration-review-formulas-recovery:
	./scripts/test-integration-shard review-formulas-recovery

## test-integration-review-formulas-recovery-cover: run the crash/recovery review-formulas shard with coverage
test-integration-review-formulas-recovery-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-recovery.txt ./scripts/test-integration-shard review-formulas-recovery

## test-integration-bdstore: run the bd store conformance shard in isolation
test-integration-bdstore:
	GOFLAGS= GOENV=off GOWORK=off ./scripts/test-integration-shard bdstore

## test-integration-bdstore-cover: run the bdstore shard with a CI coverage profile
test-integration-bdstore-cover:
	GO_TEST_COVERPROFILE=coverage.integration-bdstore.txt ./scripts/test-integration-shard bdstore

## test-integration-rest-smoke: run the PR smoke subset of the remaining ./test/integration tests
test-integration-rest-smoke:
	./scripts/test-integration-shard rest-smoke

## test-integration-rest-smoke-cover: run the smoke rest shard with a CI coverage profile
test-integration-rest-smoke-cover:
	GO_TEST_COVERPROFILE=coverage.integration-rest-smoke.txt ./scripts/test-integration-shard rest-smoke

## test-integration-rest-full: run the heavier rest shard kept for nightly/RC and targeted PRs
test-integration-rest-full:
	./scripts/test-integration-shard rest-full

## test-integration-rest-full-cover: run the full rest shard with a CI coverage profile
test-integration-rest-full-cover:
	GO_TEST_COVERPROFILE=coverage.integration-rest-full.txt ./scripts/test-integration-shard rest-full

## test-integration-rest: run the combined rest smoke+full suite
test-integration-rest:
	@status=0; \
	$(MAKE) test-integration-rest-smoke || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-rest-full || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-integration-rest-cover: run the combined rest smoke+full coverage shards
test-integration-rest-cover:
	@status=0; \
	$(MAKE) test-integration-rest-smoke-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-rest-full-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-chaos-dolt: run the opt-in managed Dolt chaos integration test
## Set GC_DOLT_CHAOS_DURATION and GC_DOLT_CHAOS_SEED to control runtime and replay failures.
test-chaos-dolt:
	$(TEST_ENV) \
		GC_DOLT_CHAOS_DURATION=$${GC_DOLT_CHAOS_DURATION:-2m} \
		GC_DOLT_CHAOS_SEED="$${GC_DOLT_CHAOS_SEED-}" \
		go test -tags 'integration chaos_dolt' -timeout 45m -run 'TestManagedDoltChaos_CityAndRigCallersRemainConsistent' -count=1 ./test/integration


## test-tutorial-goldens: run tutorial golden acceptance tests (requires tmux, dolt, bd, claude auth)
## These exercise the published tutorial flow with real inference — run before each release.
test-tutorial-goldens:
	$(TEST_ENV) go test -tags acceptance_c -timeout 90m -v ./test/acceptance/tutorial_goldens/...

## test-tutorial: alias for tutorial goldens
test-tutorial: test-tutorial-goldens

## test-tutorial-regression: alias for tutorial goldens
test-tutorial-regression: test-tutorial-goldens

## check-docs: docs sync tests (bazel test //test/docsync:docsync_test, as the unit lane runs them)
check-docs: bazel-tmpdir
	$(BAZEL_TEST) //test/docsync:docsync_test

## check-docs-go: docs sync tests with plain go test
check-docs-go:
	$(TEST_ENV) go test ./test/docsync

# Packages for coverage — exclude noise:
#   session/tmux: integration-test-only, not meaningful for unit coverage
#   beadstest: conformance helper, runs under internal/beads coverage
# cmd/gc excluded: it runs sharded below in test-cover to stay under per-package timeout
UNIT_COVER_PKGS_NONCMDGC = $(shell go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v -e /session/tmux -e /beadstest -e '/cmd/gc$$')

## test-cover: run fast unit-test coverage without the integration-tagged package sweep.
## cmd/gc is sharded CMD_GC_COVER_TOTAL (default 6) ways via test-go-test-shard so each
## shard lands well under the per-package timeout; profiles are merged via merge-coverprofiles.
## The skipped cmd/gc process-backed scenarios remain covered by
## `make test-cmd-gc-process` locally and in CI by bazel.yml's integration-packages lane.
test-cover: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.noncmdgc.txt $(UNIT_COVER_PKGS_NONCMDGC)
	@rm -f coverage.cmdgc.*.txt
	@for s in $$(seq 1 $(CMD_GC_COVER_TOTAL)); do \
		$(TEST_ENV) GO_TEST_COVERPROFILE="coverage.cmdgc.$$s.txt" \
		GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=10m \
		./scripts/test-go-test-shard ./cmd/gc "$$s" $(CMD_GC_COVER_TOTAL) || exit 1; \
	done
	./scripts/merge-coverprofiles coverage.txt coverage.noncmdgc.txt coverage.cmdgc.*.txt

## test-cover-mac: Mac coverage sweep with cmd/gc excluded; cmd/gc runs via the Mac sharded job.
## Running the full test-cover cmd/gc shards sequentially on Mac would exceed the 25m job cap.
test-cover-mac: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.txt $(UNIT_COVER_PKGS_NONCMDGC)

## test-cover-noncmdgc: run unit coverage for all packages except cmd/gc, unsharded.
## CI runs the sharded variant below; this is the whole-list local equivalent.
test-cover-noncmdgc: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.noncmdgc.txt $(UNIT_COVER_PKGS_NONCMDGC)

## test-cover-noncmdgc-shard: run unit coverage for one non-cmd/gc shard
## (NONCMDGC_COVER_SHARD of NONCMDGC_COVER_TOTAL).
##
## The job this shards was the CI critical path: every preflight and integration
## job starts in parallel, and the run's wall time was set by this one job alone
## (14m, against 11m for the next longest). Its ~4.1m of fixed setup is paid per
## shard, so the shard count is deliberately small — the wall cannot drop below
## the ~11m band the other jobs occupy, and more shards past that buy runner
## minutes, not wall time.
NONCMDGC_COVER_SHARD ?= 1
NONCMDGC_COVER_TOTAL ?= 3
##
## The slice is assigned to a variable before go test runs. A command
## substitution used directly as an argument list discards the slicer's exit
## status, so a bad shard index would run `go test` with no packages, which
## succeeds having tested nothing; `set -e` does propagate the failure of an
## assignment.
test-cover-noncmdgc-shard: test-fsys-darwin-compile
	set -e; \
	pkgs="$$(printf '%s\n' $(UNIT_COVER_PKGS_NONCMDGC) | ./scripts/noncmdgc-shard-packages $(NONCMDGC_COVER_SHARD) $(NONCMDGC_COVER_TOTAL))"; \
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m \
		-coverprofile=coverage.noncmdgc.$(NONCMDGC_COVER_SHARD).txt \
		$$pkgs

## test-cover-cmdgc-shard: run unit coverage for one cmd/gc shard (CMD_GC_COVER_SHARD of CMD_GC_COVER_TOTAL).
test-cover-cmdgc-shard:
	$(TEST_ENV) GO_TEST_COVERPROFILE=coverage.cmdgc.$(CMD_GC_COVER_SHARD).txt GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=10m ./scripts/test-go-test-shard ./cmd/gc $(CMD_GC_COVER_SHARD) $(CMD_GC_COVER_TOTAL)

## cover: run tests and show coverage report
cover: test-cover
	go tool cover -func=coverage.txt

## install-tools: install pinned golangci-lint + oapi-codegen + oasdiff
install-tools: $(GOLANGCI_LINT) install-oapi-codegen install-oasdiff

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint v$(GOLANGCI_LINT_VERSION)..."
	@attempt=1; max_attempts=5; delay=2; \
	while [ $$attempt -le $$max_attempts ]; do \
		echo "golangci-lint install attempt $$attempt/$$max_attempts"; \
		if GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION); then \
			exit 0; \
		fi; \
		if [ $$attempt -lt $$max_attempts ]; then \
			echo "golangci-lint install failed; retrying in $${delay}s..." >&2; \
			sleep $$delay; \
		fi; \
		attempt=$$((attempt + 1)); \
		delay=$$((delay * 2)); \
	done; \
	echo "ERROR: failed to install golangci-lint v$(GOLANGCI_LINT_VERSION) after $$max_attempts attempts" >&2; \
	exit 1

## install-oapi-codegen: install pinned oapi-codegen so the spec→client drift
## test (TestGeneratedClientInSync) can regenerate client_gen.go without skipping.
.PHONY: install-oapi-codegen
install-oapi-codegen:
	@if ! command -v oapi-codegen >/dev/null; then \
		echo "Installing oapi-codegen..." >&2; \
		go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0; \
	fi

## install-oasdiff: install the pinned oasdiff used by openapi-breaking-check.
## Reinstalls when the binary on disk is a different version.
.PHONY: install-oasdiff
install-oasdiff:
	@if ! go version -m $(OASDIFF) 2>/dev/null | grep -Eq '^[[:space:]]+mod[[:space:]]+github.com/oasdiff/oasdiff[[:space:]]+v$(OASDIFF_VERSION)([[:space:]]|$$)'; then \
		echo "Installing oasdiff v$(OASDIFF_VERSION)..." >&2; \
		GOBIN=$(BIN_DIR) go install github.com/oasdiff/oasdiff@v$(OASDIFF_VERSION); \
	fi

## openapi-breaking-check: fail when internal/api/openapi.json breaks clients of
## the base spec (OPENAPI_BREAKING_BASE, default merge-base with origin/main):
## //cmd/openapi-breaking:openapi-breaking_test with that commit's spec as
## GC_OPENAPI_BREAKING_BASE_SPEC, as bazel.yml's unit lane runs it on PRs.
## Waive intentional breaks in internal/api/openapi-breaking.toml.
.PHONY: openapi-breaking-check
openapi-breaking-check: bazel-tmpdir
	@base="$${OPENAPI_BREAKING_BASE:-$$(git merge-base HEAD origin/main)}" || exit 1; \
	spec="$$(mktemp "$${TMPDIR:-/tmp}/openapi-base.XXXXXX")" || exit 1; \
	trap 'rm -f "$$spec"' EXIT; \
	git show "$$base:internal/api/openapi.json" > "$$spec" || exit 1; \
	echo "openapi-breaking-check: base $$base"; \
	GC_OPENAPI_BREAKING_BASE_SPEC="$$spec" $(BAZEL_TEST) //cmd/openapi-breaking:openapi-breaking_test

## openapi-breaking-check-go: the same gate with plain go test / go run and the
## Makefile's OASDIFF_VERSION.
.PHONY: openapi-breaking-check-go
openapi-breaking-check-go: install-oasdiff
	GC_REQUIRE_OASDIFF=1 OASDIFF=$(OASDIFF) go test -count=1 ./cmd/openapi-breaking
	OASDIFF=$(OASDIFF) go run ./cmd/openapi-breaking

## install-buildx: install docker buildx plugin
install-buildx:
	@mkdir -p $(HOME)/.docker/cli-plugins
	@case "$(GOOS)-$(GOARCH)" in \
		linux-amd64|linux-arm64) ;; \
		*) echo "Unsupported docker-buildx platform: $(GOOS)-$(GOARCH)" >&2; exit 1 ;; \
	esac; \
	tmp="$$(mktemp)"; \
	checksums="$$(mktemp)"; \
	trap 'rm -f "$$tmp" "$$checksums"' EXIT; \
	curl -sSfL "https://github.com/docker/buildx/releases/download/v$(BUILDX_VERSION)/checksums.txt" \
		-o "$$checksums"; \
	asset="buildx-v$(BUILDX_VERSION).$(GOOS)-$(GOARCH)"; \
	expected_sha="$$(awk -v asset="*$$asset" '$$2 == asset {print $$1}' "$$checksums")"; \
	if [ -z "$$expected_sha" ]; then echo "Missing checksum for $$asset" >&2; exit 1; fi; \
	curl -sSfL "https://github.com/docker/buildx/releases/download/v$(BUILDX_VERSION)/buildx-v$(BUILDX_VERSION).$(GOOS)-$(GOARCH)" \
		-o "$$tmp"; \
	echo "$$expected_sha  $$tmp" | sha256sum -c -; \
	install -m 0755 "$$tmp" $(HOME)/.docker/cli-plugins/docker-buildx
	@echo "Installed docker-buildx v$(BUILDX_VERSION)"

## test-mail-wisp-insert: run the beads version-skew regression tests for gc mail send (wisp_events INSERT path)
## Tripwire for the 2026-06-11 P0: covers both NativeDoltStore and BdStore → Dolt paths.
test-mail-wisp-insert:
	@echo "=== NativeDoltStore ephemeral mail (go.mod beads library) ==="
	$(TEST_ENV) go test -tags integration ./internal/beads/ -run TestNativeDoltStoreEphemeralMailSend -v -count=1
	@echo "=== BdStore mail wisp INSERT (bd CLI → Dolt SQL) ==="
	$(TEST_ENV) go test -tags integration ./test/integration/ -run TestBdStoreMailWispInsert -v -count=1

## test-mcp-mail: run mcp_agent_mail live conformance test (auto-starts server)
test-mcp-mail:
	$(TEST_ENV) GC_TEST_MCP_MAIL=1 go test ./internal/mail/exec/ -run TestMCPMailConformanceLive -v -count=1

## test-openclaw-bridge: run the contrib/openclaw-bridge Node test suite (rules_js)
test-openclaw-bridge:
	$(BAZEL_TEST) //contrib/openclaw-bridge/...

## test-docker: run the Docker session adapter against the emulated container host
test-docker:
	$(BAZEL_TEST) --config=integration --test_filter='^TestDockerSession' //test/containerhost:containerhost_test

## test-k8s: run the K8s session adapter against the emulated container host's pod API
test-k8s:
	$(BAZEL_TEST) --config=integration --test_filter='^TestK8sSession' //test/containerhost:containerhost_test

## setup: install tools and git hooks
## .githooks is the single core.hooksPath owner; its hooks chain every
## beads-managed hook through .githooks/lib/beads-chain.sh, so reclaiming the
## path from beads' installer does not disable beads.
setup: install-tools
	git config core.hooksPath .githooks
	@./scripts/check-githooks-owner.sh
	@echo "Done. Tools installed, pre-commit hook active."

## check-hooks: verify .githooks is this clone's active core.hooksPath
## The .githooks gates cannot report their own absence — when another installer
## claims core.hooksPath they simply never run. This is the external detector.
check-hooks:
	@./scripts/check-githooks-owner.sh
	@echo "core.hooksPath OK: .githooks gates are active."

## diagrams-excalidraw: render docs/diagrams/excalidraw/*.excalidraw to excalidraw-rendered/*.svg (idempotent)
diagrams-excalidraw:
	@set -e; \
	src_dir=docs/diagrams/excalidraw; \
	out_dir=docs/diagrams/excalidraw-rendered; \
	mkdir -p "$$out_dir"; \
	shopt -s nullglob 2>/dev/null || true; \
	rendered=0; \
	for f in "$$src_dir"/*.excalidraw; do \
		[ -e "$$f" ] || continue; \
		base=$$(basename "$$f" .excalidraw); \
		out="$$out_dir/$$base.svg"; \
		if [ ! -e "$$out" ] || [ "$$f" -nt "$$out" ]; then \
			echo "excalidraw -> $$out"; \
			npx -y @swiftlysingh/excalidraw-cli convert "$$f" --format svg --padding 16 --output "$$out"; \
			rendered=$$((rendered+1)); \
		fi; \
	done; \
	echo "excalidraw: rendered $$rendered file(s)"

## docs-dev: run the Mintlify docs locally
docs-dev:
	./mint.sh dev

# Dashboard SPA (internal/api/dashboardspa/web). Bazel builds and tests it
# hermetically (rules_js: pinned Node.js, npm packages from pnpm-lock.yaml),
# which is what CI gates on; the *-npm twins drive the same steps through a
# local npm install for dev servers and offline work.
DASHBOARD_WEB := internal/api/dashboardspa/web
DASHBOARD_BAZEL_TARGETS := //internal/api/dashboardspa/... //internal/api/dashboardbff/...

## dashboard-build: build the SPA bundle with Bazel and sync it into the committed dist/ (a Node-less `go build` embeds that copy)
dashboard-build:
	$(BAZEL) build $(BAZEL_FLAGS) --remote_download_outputs=toplevel //internal/api/dashboardspa:dist
	rm -rf internal/api/dashboardspa/dist
	cp -rfL bazel-bin/internal/api/dashboardspa/dist internal/api/dashboardspa/dist
	chmod -R a-x,u+w,a+rX internal/api/dashboardspa/dist

## dashboard-build-npm: the same bundle through a local npm install
dashboard-build-npm:
	cd $(DASHBOARD_WEB) && npm ci --silent && npm run build && rm -rf ../dist && cp -rf frontend/dist ../dist

## dashboard-generate-client: regenerate the typed API client from internal/api/openapi.json (openapi-ts under Bazel)
dashboard-generate-client:
	$(BAZEL) build $(BAZEL_FLAGS) --remote_download_outputs=toplevel //$(DASHBOARD_WEB):gc_supervisor_client
	rm -rf $(DASHBOARD_WEB)/shared/src/generated/gc-supervisor-client
	cp -rfL bazel-bin/$(DASHBOARD_WEB)/gc-supervisor-client $(DASHBOARD_WEB)/shared/src/generated/gc-supervisor-client
	chmod -R a-x,u+w,a+rX $(DASHBOARD_WEB)/shared/src/generated/gc-supervisor-client

## dashboard-lock: re-derive pnpm-lock.yaml (what Bazel installs) from package-lock.json after an npm dependency change
dashboard-lock:
	$(BAZEL) run $(BAZEL_FLAGS) -- @pnpm//:pnpm --dir "$(CURDIR)/$(DASHBOARD_WEB)" import

## dashboard-dev: Vite dev server (HMR) for SPA iteration
dashboard-dev:
	cd $(DASHBOARD_WEB) && npm run --workspace gas-city-dashboard-frontend dev

## dashboard-check: rebuild the committed dist/ (dashboard-build), then what CI
## gates on, under Bazel: typecheck (src + test + e2e specs, shared), vitest,
## the SPA build, the committed dist/, generated API client and pnpm-lock.yaml
## in sync, and the Go tests of the embedded handler + BFF
dashboard-check: dashboard-build bazel-tmpdir
	$(BAZEL_TEST) $(DASHBOARD_BAZEL_TARGETS)

## dashboard-check-npm: the same checks through a local npm install and go test
dashboard-check-npm: dashboard-build-npm
	cd $(DASHBOARD_WEB) && npm run typecheck && npm run --workspace gas-city-dashboard-frontend typecheck:test
	cd $(DASHBOARD_WEB) && npm run --workspace gas-city-dashboard-frontend typecheck:e2e
	cd $(DASHBOARD_WEB) && npm run --workspace gas-city-dashboard-frontend test
	$(TEST_ENV) go test ./internal/api/dashboardspa/... ./internal/api/dashboardbff/...

## dashboard-smoke: serve the built SPA bundle via Vite preview and verify it responds
dashboard-smoke: dashboard-build-npm
	@PORT=$$(python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'); \
	LOG=$$(mktemp); \
	( cd internal/api/dashboardspa/web/frontend && exec npm run preview -- --host 127.0.0.1 --strictPort --port $$PORT >"$$LOG" 2>&1 ) & \
	PID=$$!; \
	trap 'kill $$PID >/dev/null 2>&1 || true; wait $$PID >/dev/null 2>&1 || true; rm -f "$$LOG"' EXIT INT TERM; \
	for attempt in $$(seq 1 40); do \
		if curl -fsS "http://127.0.0.1:$$PORT/" >/dev/null; then \
			exit 0; \
		fi; \
		sleep 0.25; \
	done; \
	cat "$$LOG" >&2; \
	exit 1

## dashboard-e2e-go: Layer A of the dashboard e2e — serve the real supervisor
## stack (typed /v0 + host /api plane + embedded SPA) over a seeded event log +
## bead store via api.ServeSeededCity and assert each view's JSON projection.
## This is the run-view-break-catcher; it runs under the integration tier, not
## the fast unit baseline. Picked up automatically by the packages integration
## shard (go list ./...); this target runs it in isolation.
dashboard-e2e-go:
	$(TEST_ENV) go test -tags integration -timeout 10m ./test/dashport/...

## dashboard-e2e-play: Layer B of the dashboard e2e — the Playwright render smoke.
## Builds the SPA bundle (so the embedded dist/ the fakesupervisor serves is
## current), builds the seeded fakesupervisor binary with -tags integration,
## installs Chromium, and runs the render specs, which assert each view renders
## its seeded content with no React error boundary and no client-error POST. The
## Go webServer in playwright.config.ts launches the seeded fakesupervisor.
dashboard-e2e-play: dashboard-build-npm
	cd test/dashport/cmd/fakesupervisor && go build -tags integration -o fakesupervisor .
	cd internal/api/dashboardspa/web && npm ci --silent
	cd internal/api/dashboardspa/web/frontend && npm run test:e2e:install
	cd internal/api/dashboardspa/web/frontend && npm run test:e2e

## dashboard-e2e: run both dashboard e2e layers — the Go serve-level projection
## test (Layer A) and the Playwright browser render smoke (Layer B).
dashboard-e2e: dashboard-e2e-go dashboard-e2e-play

## dashboard-ci: alias of dashboard-check (its drift checks are Bazel diff tests now)
dashboard-ci: dashboard-check

## spec-ci: regenerate the OpenAPI spec + generated Go client, fail on drift,
## then run the OpenAPI breaking-change gate (openapi-breaking-check).
## Used by CI to enforce that internal/api/openapi.json, docs/reference/schema JSON
## artifacts, compatibility .txt mirrors, and internal/api/genclient/client_gen.go
## are all in lock-step with Huma.
spec-ci: install-oapi-codegen
	go run ./cmd/genspec
	go generate ./internal/api/genclient
	@if ! git diff --quiet -- internal/api/openapi.json docs/reference/schema/openapi.json docs/reference/schema/openapi.txt docs/reference/schema/events.json docs/reference/schema/events.txt internal/api/genclient/client_gen.go; then \
		echo "ERROR: spec/client artifacts drifted — run 'make spec-ci' locally and commit." >&2; \
		git --no-pager diff --stat -- internal/api/openapi.json docs/reference/schema/openapi.json docs/reference/schema/openapi.txt docs/reference/schema/events.json docs/reference/schema/events.txt internal/api/genclient/client_gen.go; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory openapi-breaking-check

## docker-base: build base image with system dependencies (~2.5 min, rebuild rarely)
docker-base: check-docker
	. ./deps.env && docker build -f contrib/k8s/Dockerfile.base \
		--build-arg DOLT_VERSION=$$DOLT_VERSION \
		-t gc-agent-base:latest .

## docker-agent: build base agent image (~5s on top of base). For prebaked images use: gc build-image
docker-agent: check-docker
	docker build -f contrib/k8s/Dockerfile.agent -t gc-agent:latest .
	@if kubectl config current-context 2>/dev/null | grep -q '^kind-'; then \
		cluster=$$(kubectl config current-context | sed 's/^kind-//'); \
		echo "Loading gc-agent:latest into kind cluster '$$cluster'..."; \
		kind load docker-image gc-agent:latest --name "$$cluster"; \
	fi

## docker-controller: build controller image for K8s deployment (~10s on top of agent)
docker-controller: check-docker
	docker build -f contrib/k8s/Dockerfile.controller -t gc-controller:latest .
	@if kubectl config current-context 2>/dev/null | grep -q '^kind-'; then \
		cluster=$$(kubectl config current-context | sed 's/^kind-//'); \
		echo "Loading gc-controller:latest into kind cluster '$$cluster'..."; \
		kind load docker-image gc-controller:latest --name "$$cluster"; \
	fi

## k8s-secret: create K8s secret with Claude credentials
## Usage: make k8s-secret CLAUDE_CONFIG_SRC=~/.claude [GC_K8S_NAMESPACE=gc]
## Source dir must contain .credentials.json (required) and optionally
## .claude.json (onboarding state) and settings.json.
k8s-secret:
	@if [ -z "$${CLAUDE_CONFIG_SRC:-}" ]; then \
		echo "Usage: make k8s-secret CLAUDE_CONFIG_SRC=<path-to-claude-config-dir>" >&2; \
		echo "  The directory must contain .credentials.json" >&2; \
		exit 1; \
	fi; \
	ns="$${GC_K8S_NAMESPACE:-gc}"; \
	src="$$CLAUDE_CONFIG_SRC"; \
	if [ ! -f "$$src/.credentials.json" ]; then \
		echo "Error: $$src/.credentials.json not found." >&2; \
		exit 1; \
	fi; \
	args="--from-file=.credentials.json=$$src/.credentials.json"; \
	[ -f "$$src/.claude.json" ] && args="$$args --from-file=.claude.json=$$src/.claude.json"; \
	[ -f "$$src/settings.json" ] && args="$$args --from-file=settings.json=$$src/settings.json"; \
	kubectl -n "$$ns" delete secret claude-credentials --ignore-not-found >/dev/null 2>&1; \
	kubectl -n "$$ns" create secret generic claude-credentials $$args; \
	echo "Secret 'claude-credentials' created in namespace '$$ns'"

## help: show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'

## bazel-sync: regenerate bazel BUILD files (gazelle) and the hermetic repo
## source tree used by whole-repo scan guards, apply the hermeticity
## ledger's go_test tags (test/bazel-hermeticity.toml), and regenerate the
## integration lane's package suite (//test:integration_packages). Run after
## adding packages.
.PHONY: bazel-sync
# BAZEL_SYNC_FLAGS: CI passes --config=remote-exec, so gazelle and its Go
# stdlib come from rbe-west's cache instead of 64 local compiles.
bazel-sync:
	bazel run $(BAZEL_SYNC_FLAGS) //:gazelle
	python3 tools/bazel/repo_tree.py
	python3 tools/bazel/hermetic_tags.py
	python3 tools/bazel/integration_suite.py

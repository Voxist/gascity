package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

func TestPreCommitFormatterPreservesFileMode(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fakeLint := filepath.Join(binDir, "golangci-lint")
	writeExecutable(t, fakeLint, `#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 2 ] || [ "$1" != "fmt" ] || [ "$2" != "--stdin" ]; then
  echo "unexpected golangci-lint args: $*" >&2
  exit 2
fi
cat
printf '\n'
`)

	source := filepath.Join(t.TempDir(), "needs_format.go")
	if err := os.WriteFile(source, []byte("package main"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "precommit-format-staged-go"))
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}
	cmd.Stdin = strings.NewReader(source + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("precommit formatter failed: %v\n%s", err, out)
	}

	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stat formatted source: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("formatted source mode = %o, want 644", got)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read formatted source: %v", err)
	}
	if string(content) != "package main\n" {
		t.Fatalf("formatted content = %q, want package main with newline", content)
	}
}

func TestTestFastParallelUsesSanitizedEnvironmentAndMachineAwareConcurrency(t *testing.T) {
	repoRoot := repoRoot(t)
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LOCAL_TEST_JOBS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CPUS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMORY_KIB=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMINFO=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_PROC_CGROUP=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CGROUP_ROOT=") ||
			strings.HasPrefix(entry, "GC_PUSH_GATE_NO_CAP=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_CONCURRENT=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_WAIT_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_POLL_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_UNRELATED_SENTINEL=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_LOADAVG=") {
			continue
		}
		baseEnv = append(baseEnv, entry)
	}
	tests := []struct {
		name      string
		cpus      string
		memoryKiB string
		makeArgs  []string
		wantJobs  string
		cgroup    string
		limit     string
		current   string
	}{
		{name: "large host uses automatic ceiling", cpus: "192", memoryKiB: "536870912", wantJobs: "16"},
		{name: "memory constrains fanout", cpus: "16", memoryKiB: "12582912", wantJobs: "3"},
		{name: "cpu constrains fanout", cpus: "2", memoryKiB: "67108864", wantJobs: "2"},
		{name: "small machine still runs one job", cpus: "8", memoryKiB: "2097152", wantJobs: "1"},
		{name: "unknown memory preserves safe fallback", cpus: "64", memoryKiB: "0", wantJobs: "3"},
		{name: "nested cgroup v2 ancestor constrains fanout", cpus: "16", wantJobs: "3", cgroup: "v2", limit: "12884901888", current: "0"},
		{name: "nested cgroup v1 ancestor constrains fanout", cpus: "16", wantJobs: "2", cgroup: "v1", limit: "8589934592", current: "0"},
		{name: "hybrid cgroup falls through to v1 memory controller", cpus: "16", wantJobs: "3", cgroup: "hybrid", limit: "12884901888", current: "0"},
		{name: "exhausted cgroup forces one job", cpus: "16", wantJobs: "1", cgroup: "v2", limit: "4294967296", current: "4294967296"},
		{name: "explicit override wins", cpus: "192", memoryKiB: "536870912", makeArgs: []string{"LOCAL_TEST_JOBS=7"}, wantJobs: "7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-n"}, tt.makeArgs...)
			args = append(args, "test-fast-parallel")
			cmd := exec.Command("make", args...)
			cmd.Dir = repoRoot
			// This table exercises the cpu/memory/cgroup axes only; pin loadavg=0
			// so a live host's real /proc/loadavg can't shrink the expected job
			// count out from under an unrelated case (ga-04m84s).
			cmd.Env = append(append([]string(nil), baseEnv...),
				"GC_TEST_LOCAL_CPUS="+tt.cpus,
				"GC_TEST_LOCAL_LOADAVG=0",
				"GC_PUSH_GATE_NO_CAP=1",
				"PUSH_GATE_MAX_CONCURRENT=7",
				"PUSH_GATE_MAX_WAIT_SECONDS=13",
				"PUSH_GATE_POLL_SECONDS=2",
				"PUSH_GATE_UNRELATED_SENTINEL=must-not-leak",
			)
			if tt.memoryKiB != "" {
				cmd.Env = append(cmd.Env, "GC_TEST_LOCAL_MEMORY_KIB="+tt.memoryKiB)
			}
			if tt.cgroup != "" {
				cmd.Env = append(cmd.Env, localTestCgroupEnv(t, tt.cgroup, tt.limit, tt.current)...)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n test-fast-parallel failed: %v\n%s", err, out)
			}
			command := string(out)
			if !strings.Contains(command, "env -i") {
				t.Fatalf("test-fast-parallel recipe should use TEST_ENV env -i wrapper:\n%s", command)
			}
			if !strings.Contains(command, "./scripts/test-local-parallel fast") {
				t.Fatalf("test-fast-parallel recipe should still dispatch the sharded fast runner:\n%s", command)
			}
			wantJobAssignment := " LOCAL_TEST_JOBS=" + tt.wantJobs + " CMD_GC_PROCESS_TOTAL="
			if !strings.Contains(command, wantJobAssignment) {
				t.Fatalf("test-fast-parallel job count should be %s:\n%s", tt.wantJobs, command)
			}
			for _, key := range []string{
				"GC_PUSH_GATE_NO_CAP",
				"PUSH_GATE_MAX_CONCURRENT",
				"PUSH_GATE_MAX_WAIT_SECONDS",
				"PUSH_GATE_POLL_SECONDS",
			} {
				wantForwarding := key + `="${` + key + `-}"`
				if !strings.Contains(command, wantForwarding) {
					t.Fatalf("test-fast-parallel should forward %s through TEST_ENV:\n%s", key, command)
				}
			}
			if strings.Contains(command, "PUSH_GATE_UNRELATED_SENTINEL") {
				t.Fatalf("test-fast-parallel must keep unrelated ambient variables out of TEST_ENV:\n%s", command)
			}
		})
	}
}

func localTestCgroupEnv(t *testing.T, version, limit, current string) []string {
	t.Helper()
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "cgroup")
	procCgroup := filepath.Join(root, "proc-self-cgroup")
	meminfo := filepath.Join(root, "meminfo")
	writeTestFile(t, meminfo, "MemAvailable: 67108864 kB\n")

	var controllerRoot, procLine, limitFile, currentFile string
	switch version {
	case "v2":
		controllerRoot = cgroupRoot
		procLine = "0::/parent/child\n"
		limitFile = "memory.max"
		currentFile = "memory.current"
	case "v1":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	case "hybrid":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "0::/unified/child\n5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	default:
		t.Fatalf("unsupported cgroup fixture version %q", version)
	}

	writeTestFile(t, procCgroup, procLine)
	if err := os.MkdirAll(filepath.Join(controllerRoot, "parent", "child"), 0o755); err != nil {
		t.Fatalf("create nested cgroup fixture: %v", err)
	}
	writeTestFile(t, filepath.Join(controllerRoot, "parent", limitFile), limit+"\n")
	writeTestFile(t, filepath.Join(controllerRoot, "parent", currentFile), current+"\n")

	return []string{
		"GC_TEST_LOCAL_MEMINFO=" + meminfo,
		"GC_TEST_LOCAL_PROC_CGROUP=" + procCgroup,
		"GC_TEST_LOCAL_CGROUP_ROOT=" + cgroupRoot,
	}
}

func TestPrePushUsesCanonicalMachineAwareConcurrency(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read pre-push hook: %v", err)
	}
	content := string(script)
	if strings.Contains(content, `LOCAL_TEST_JOBS="${LOCAL_TEST_JOBS:-3}"`) {
		t.Fatal("pre-push hook must not replace the canonical machine-aware default with a fixed three-job cap")
	}
	if !strings.Contains(content, "exec make test-fast-parallel") {
		t.Fatal("pre-push hook must continue delegating the unchanged fast-suite inventory to make test-fast-parallel")
	}
	for _, path := range []string{"Makefile", filepath.Join("scripts", "test-local-parallel")} {
		content, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(content), "scripts/test-local-job-count") {
			t.Fatalf("%s must use the canonical machine-aware job detector", path)
		}
	}
}

func TestPreCommitRegeneratesDashboardClientOnSpecChange(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	content := string(script)

	npmBlockStart := strings.Index(content, "if command -v npm")
	if npmBlockStart < 0 {
		t.Fatal("pre-commit hook must guard dashboard regeneration on npm availability")
	}
	npmBlock := content[npmBlockStart:]

	genClientIdx := strings.Index(npmBlock, "npm run generate:client")
	if genClientIdx < 0 {
		t.Fatal("pre-commit hook must run 'npm run generate:client' when internal/api/openapi.json changes — " +
			"make dashboard-check only builds and typechecks against whatever client is already on disk, it never " +
			"regenerates it (that's make dashboard-ci's job, which the hook never calls). A spec-only commit " +
			"currently ships a stale generated TS client (see PR #4627, #4607)")
	}

	dashboardCheckIdx := strings.Index(npmBlock, "make dashboard-check")
	if dashboardCheckIdx < 0 {
		t.Fatal("pre-commit hook must still run make dashboard-check dashboard-smoke")
	}
	if genClientIdx > dashboardCheckIdx {
		t.Fatal("pre-commit hook must regenerate the dashboard client BEFORE typecheck/build, so a client that " +
			"doesn't match the new spec fails typecheck immediately instead of silently building against stale types")
	}

	clientAddNeedle := "git add internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client"
	genClientAddIdx := strings.Index(npmBlock, clientAddNeedle)
	if genClientAddIdx < 0 {
		t.Fatal("pre-commit hook must stage the regenerated dashboard client so a spec-only commit includes it")
	}
	if genClientAddIdx < genClientIdx {
		t.Fatal("pre-commit hook must stage the generated client after regenerating it, not before")
	}

	if strings.Contains(content, "regenerate the TS types, typecheck, and rebuild") {
		t.Fatal("pre-commit hook's dashboard block comment must not claim it regenerates the TS types unless it " +
			"actually calls npm run generate:client")
	}

	if !strings.Contains(content, `echo "warning: npm not on PATH`) {
		t.Fatal("pre-commit hook must still warn and no-op cleanly when npm is not on PATH")
	}
}

func TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	clientPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "web", "shared", "src", "generated", "gc-supervisor-client")
	distPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "dist", "placeholder")

	// The hook resolves its beads chain relative to `git rev-parse
	// --show-toplevel`, which is this temp repo — install the real forwarder
	// there rather than re-implementing it.
	chain, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "lib", "beads-chain.sh"))
	if err != nil {
		t.Fatalf("read beads-chain.sh: %v", err)
	}
	chainPath := filepath.Join(tmpRepo, ".githooks", "lib", "beads-chain.sh")
	if err := os.MkdirAll(filepath.Dir(chainPath), 0o755); err != nil {
		t.Fatalf("mkdir .githooks/lib: %v", err)
	}
	writeExecutable(t, chainPath, string(chain))

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	writeTestFile(t, clientPath, "placeholder\n")
	writeTestFile(t, distPath, "placeholder\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- no .go, web-src, or doc files
	// are staged, matching the reviewer's criterion-2 repro scenario.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	binDir := t.TempDir()
	npmLog := filepath.Join(binDir, "npm.log")
	writeExecutable(t, filepath.Join(binDir, "npm"), `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "`+npmLog+`"
exit 0
`)
	// Stub make: this test verifies the control-flow reaches the dashboard
	// block at all (the reviewer's criterion-2 gap), not the real
	// dashboard-check/dashboard-smoke targets, which need the full repo.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env bash
exit 0
`)
	// Stub bd so the chained beads pre-commit hook is a no-op here; this test
	// is about the repo hook's own control flow.
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
exit 0
`)

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
	}

	logContent, readErr := os.ReadFile(npmLog)
	if readErr != nil {
		t.Fatalf("pre-commit hook exited early and never invoked npm when only internal/api/openapi.json was "+
			"staged -- the go/web/docs early guard must not skip a spec-only commit (hook output: %s)", out)
	}
	if !strings.Contains(string(logContent), "generate:client") {
		t.Fatalf("pre-commit hook must run 'npm run generate:client' when only internal/api/openapi.json is "+
			"staged, got npm invocations:\n%s", logContent)
	}
}

func TestPreCommitFailsClosedWhenSpecStagedButNpmAbsent(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")

	// pre-commit resolves beads-chain relative to this temp repo's toplevel.
	installBeadsChainForTempRepo(t, repoRoot, tmpRepo)

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- same repro shape as
	// TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged, but this
	// time npm itself is unreachable on PATH.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, nil),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pre-commit hook must fail when internal/api/openapi.json is staged and npm is not on PATH "+
			"-- the generated TS client can't be regenerated, so the commit would silently ship a stale "+
			"client with no enforcement until CI runs. Hook exited 0, output:\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci") || !strings.Contains(string(out), "generate:client") {
		t.Fatalf("pre-commit hook's npm-absent+spec-staged failure must name the exact recovery command "+
			"(cd internal/api/dashboardspa/web && npm ci && npm run generate:client), got:\n%s", out)
	}
}

func TestPreCommitFailsClosedWhenGoBlockStagesSpecAsSideEffectAndNpmAbsent(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	goFilePath := filepath.Join(tmpRepo, "main.go")
	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	formatStagedGoPath := filepath.Join(tmpRepo, "scripts", "precommit-format-staged-go")
	// Every path the Go block unconditionally `git add`s after each
	// generation step must already exist on disk, or that `git add` fails
	// closed under `set -euo pipefail` before the hook ever reaches the
	// npm-absent branch this test targets.
	generatedPaths := []string{
		specPath,
		filepath.Join(tmpRepo, "docs", "reference", "schema", "openapi.json"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "openapi.txt"),
		filepath.Join(tmpRepo, "internal", "api", "genclient", "client_gen.go"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "city-schema.json"),
		filepath.Join(tmpRepo, "docs", "reference", "schema", "city-schema.txt"),
		filepath.Join(tmpRepo, "docs", "reference", "config.md"),
		filepath.Join(tmpRepo, "docs", "reference", "cli.md"),
	}

	// pre-commit resolves beads-chain relative to this temp repo's toplevel.
	installBeadsChainForTempRepo(t, repoRoot, tmpRepo)

	runGit("init")
	writeTestFile(t, goFilePath, "package main\n\nfunc main() {}\n")
	for _, p := range generatedPaths {
		writeTestFile(t, p, "{}\n")
	}
	if err := os.MkdirAll(filepath.Dir(formatStagedGoPath), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", formatStagedGoPath, err)
	}
	writeExecutable(t, formatStagedGoPath, "#!/usr/bin/env bash\nexit 0\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a .go file -- internal/api/openapi.json is untouched by the
	// user's own `git add`. The hook's own Go block (staged_go_files branch)
	// regenerates and stages openapi.json as a SIDE EFFECT via
	// `go run ./cmd/genspec`, which is exactly the #4627/#4607 staleness
	// trap the npm-present branch re-reads for (fresh spec_changed) but
	// which the npm-absent fail-closed branch used to miss (ga-jg89a5): it
	// checked a snapshot taken before the hook ran at all, so it never saw
	// the spec this commit was actually about to ship.
	writeTestFile(t, goFilePath, "package main\n\nfunc main() { println(1) }\n")
	runGit("add", "main.go")

	goStub := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "run" ] && [ "$2" = "./cmd/genspec" ]; then
  printf '{"changed":true}\n' > internal/api/openapi.json
fi
exit 0
`

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": "#!/usr/bin/env bash\nexit 0\n",
			// Stands in for format/lint/genspec/genclient/genschema/vet.
			// Only `run ./cmd/genspec` has an observable side effect
			// (rewriting internal/api/openapi.json, which the hook's own
			// `git add` then stages), matching what the real cmd/genspec
			// does against a live Huma API -- the rest of the Go block is
			// exercised for control-flow only.
			"go": goStub,
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pre-commit hook must fail when its own Go block stages internal/api/openapi.json as a side "+
			"effect (go run ./cmd/genspec, triggered by staging a .go file) and npm is not on PATH -- the "+
			"generated TS client can't be regenerated, so the commit would silently ship a stale client with "+
			"no enforcement until CI runs. Hook exited 0, output:\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci") || !strings.Contains(string(out), "generate:client") {
		t.Fatalf("pre-commit hook's npm-absent+spec-staged-as-side-effect failure must name the exact "+
			"recovery command (cd internal/api/dashboardspa/web && npm ci && npm run generate:client), got:\n%s", out)
	}
}

func TestPreCommitWarnsOnlyWhenNpmAbsentAndSpecNotStaged(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	docPath := filepath.Join(tmpRepo, "README.md")

	// pre-commit resolves beads-chain relative to this temp repo's toplevel.
	installBeadsChainForTempRepo(t, repoRoot, tmpRepo)

	runGit("init")
	writeTestFile(t, docPath, "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage a docs-only change -- internal/api/openapi.json is untouched,
	// so npm's absence must stay a warning, not a hard failure. staged_docs
	// being non-empty also exercises `make check-docs`, so stub `make` as a
	// no-op; the fixture repo has none of the real doc-lint machinery.
	writeTestFile(t, docPath, "hello again\n")
	runGit("add", "README.md")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": "#!/usr/bin/env bash\nexit 0\n",
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook must still succeed (warn-only) when npm is absent and "+
			"internal/api/openapi.json is NOT staged -- contributors without Node tooling must not be "+
			"blocked on unrelated commits, got exit error: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "npm not on PATH") {
		t.Fatalf("pre-commit hook should still warn when npm is absent, got:\n%s", out)
	}
}

// restrictedPathWithoutNpm builds a PATH containing only symlinks to the
// real bash and git (plus any provided stub scripts), guaranteeing npm is
// unreachable regardless of what's installed on the test host -- falling
// back to the ambient PATH would make these tests flaky on any machine
// that actually has npm installed.
//
// A no-op `bd` stub is always installed so the beads-chain pre-commit
// forwarder does not fail closed when the ambient PATH has no `bd` (or has
// a real one that would try to talk to a database).
func restrictedPathWithoutNpm(t *testing.T, stubs map[string]string) string {
	t.Helper()
	binDir := t.TempDir()
	// sh+env are required for beads-chain.sh's `#!/usr/bin/env sh` shebang
	// under a restricted PATH (env is absolute in the shebang, but then looks
	// up `sh` on PATH). timeout is optional; without it the chain still runs.
	// python3 joins them because .githooks/pre-commit now runs
	// scripts/check-go-clean-cache.sh (the `go clean -cache` ban guard,
	// AGENTS.md "Build Cache Conventions"), whose scanner is python3. It is
	// already a hook-path dependency via check-resync-loss.sh in pre-push.
	for _, name := range []string{"bash", "sh", "env", "git", "xargs", "python3"} {
		realPath, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("resolve real %s on test host PATH: %v", name, err)
		}
		if err := os.Symlink(realPath, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	if stubs == nil {
		stubs = map[string]string{}
	}
	if _, ok := stubs["bd"]; !ok {
		stubs["bd"] = "#!/usr/bin/env bash\nexit 0\n"
	}
	for name, script := range stubs {
		writeExecutable(t, filepath.Join(binDir, name), script)
	}
	return binDir
}

// installBeadsChainForTempRepo copies the real beads-chain forwarder into a
// fixture repo. The real pre-commit hook resolves the chain via
// `git rev-parse --show-toplevel`, which is the temp repo, so the fixture
// must ship the lib dependency rather than re-implementing it.
func installBeadsChainForTempRepo(t *testing.T, repoRoot, tmpRepo string) {
	t.Helper()
	chain, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "lib", "beads-chain.sh"))
	if err != nil {
		t.Fatalf("read beads-chain.sh: %v", err)
	}
	chainPath := filepath.Join(tmpRepo, ".githooks", "lib", "beads-chain.sh")
	if err := os.MkdirAll(filepath.Dir(chainPath), 0o755); err != nil {
		t.Fatalf("mkdir .githooks/lib: %v", err)
	}
	writeExecutable(t, chainPath, string(chain))
}

func TestNativeDoltliteBeadsTargetRunsTaggedSuite(t *testing.T) {
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := validateNativeDoltliteMakefile(string(makefile)); err != nil {
		t.Fatalf("test-native-doltlite-beads recipe: %v", err)
	}

	cmd := exec.Command("make", "-n", "test-native-doltlite-beads")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-native-doltlite-beads failed: %v\n%s", err, out)
	}
	command := string(out)
	if err := validateNativeDoltliteDryRun(command); err != nil {
		t.Fatalf("make -n test-native-doltlite-beads output: %v", err)
	}
	for _, want := range []string{
		"CGO_ENABLED=0",
		"-tags gascity_native_beads",
		"-run '^TestDoltlite'",
		"./internal/beads",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("test-native-doltlite-beads recipe missing %q:\n%s", want, command)
		}
	}
	for _, banned := range []string{
		"CGO_ENABLED=1",
		"cgo,gascity_native_beads",
	} {
		if strings.Contains(command, banned) {
			t.Fatalf("test-native-doltlite-beads recipe must not contain %q (doltlite store now uses pure-Go modernc):\n%s", banned, command)
		}
	}
	assertNativeDoltliteBeadsSelectionMatchesTaggedOwners(t, repoRoot)
}

func TestLocalParallelAllowlistIncludesObservableEnv(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read test-local-parallel: %v", err)
	}
	content := string(script)
	for _, key := range []string{"OBSERVABLE_TEST_LOG", "OBSERVABLE_FAILURE_LINES"} {
		if !strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env should pass through %s", key)
		}
	}
	for _, key := range []string{"GC_CITY", "GC_HOME", "GC_SESSION_ID"} {
		if strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env must not pass through live session env %s", key)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- ga-w7nyj: --fix must never rewrite a negated type assertion ---
//
// golangci-lint 2.12.0's errorlint linter has a stock --fix that rewrites
// `if _, ok := err.(*T); ok { ... }`-shaped code to `errors.As`, and on the
// negated form -- `if _, ok := err.(*T); !ok { ... }` -- it silently drops
// the `!`, inverting the branch's meaning. .githooks/pre-commit used to run
// `make lint-changed` with --fix, which corrupted exactly this shape twice
// in this repo's own history before this guard existed (Voxist/gascity#219
// and #231's review fix-up commits both hit it). golangci-lint 2.12.0 has
// no per-linter fix control (confirmed against the official configuration
// docs: the only fix toggle is the global issues.fix / --fix, which applies
// to every linter that supports auto-fix — govet, errorlint, gocritic,
// misspell, and revive are all enabled in .golangci.yml and support it), so
// the fix is to drop --fix from the hook's invocation entirely: lint issues
// fail the commit instead of being silently rewritten.

// errorlintVulnerableSource is the exact shape golangci-lint's stock --fix
// corrupts. golangci-lint's report says "type assertion on error will fail
// on wrapped errors" and 2.12.0's stock fix drops the negation regardless of
// which specific error type or error-typed value is asserted on.
const errorlintVulnerableSource = `package main

import "os/exec"

func check(err error) string {
	if _, ok := err.(*exec.ExitError); !ok {
		return "not an exit error"
	}
	return "exit error"
}

func main() { _ = check(nil) }
`

// preCommitLintChangedFlags extracts the exact LINT_FLAGS string
// .githooks/pre-commit passes to `make lint-changed`, so this test tracks
// the real hook rather than a copy of it that could silently drift out of
// sync with what actually runs on every commit.
func preCommitLintChangedFlags(t *testing.T, repoRoot string) string {
	t.Helper()
	hook, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	re := regexp.MustCompile(`make lint-changed LINT_CHANGED_SCOPE=staged LINT_FLAGS="([^"]*)"`)
	m := re.FindStringSubmatch(string(hook))
	if m == nil {
		t.Fatal("pre-commit hook's `make lint-changed LINT_CHANGED_SCOPE=staged LINT_FLAGS=\"...\"` invocation " +
			"not found in the expected shape -- update this test's regex to match the hook's current form " +
			"(and re-check TestErrorlintStockFixDropsTypeAssertionNegation's premise still holds)")
	}
	return m[1]
}

func TestPreCommitLintChangedFlagsDoNotAutoFix(t *testing.T) {
	repoRoot := repoRoot(t)
	flags := preCommitLintChangedFlags(t, repoRoot)
	if strings.Contains(flags, "--fix") {
		t.Fatalf("pre-commit hook's lint-changed invocation passes --fix (flags=%q): golangci-lint 2.12.0 has "+
			"no per-linter fix control, and its stock errorlint --fix silently drops the negation on "+
			"`if _, ok := x.(*T); !ok`-shaped code (ga-w7nyj) -- --fix must stay out of any hook that runs "+
			"unattended against a developer's staged code", flags)
	}

	// The hook's own LINT_FLAGS is only one way --fix could come back.
	// Guard the other two places it could hide: hardcoded into the
	// Makefile's lint-changed recipe itself (bypassing LINT_FLAGS
	// entirely), or set as a standing default via .golangci.yml's `fix:`
	// config key (which applies even when no caller passes --fix at all).
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lintChangedRecipe := extractMakeRecipe(t, string(makefile), "lint-changed")
	if strings.Contains(lintChangedRecipe, "--fix") {
		t.Fatalf("Makefile's lint-changed recipe hardcodes --fix:\n%s\n"+
			"(ga-w7nyj: this would auto-fix every lint-changed caller regardless of what LINT_FLAGS the "+
			"pre-commit hook passes)", lintChangedRecipe)
	}

	golangciConfig, err := os.ReadFile(filepath.Join(repoRoot, ".golangci.yml"))
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	if regexp.MustCompile(`(?m)^\s*fix:`).Match(golangciConfig) {
		t.Fatalf(".golangci.yml sets a `fix:` key, which auto-fixes every golangci-lint invocation against this " +
			"config by default -- including the pre-commit hook's, regardless of what flags it passes (ga-w7nyj)")
	}
}

// extractMakeRecipe returns the recipe body (the tab-indented lines)
// following the first `<target>:` rule header in a Makefile, up to the
// first blank line or next rule header -- enough to check a specific
// recipe's own command text without parsing full Make syntax.
func extractMakeRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:[^\n]*\n((?:\t[^\n]*\n?)*)`)
	m := re.FindStringSubmatch(makefile)
	if m == nil {
		t.Fatalf("Makefile target %q not found in the expected `%s: ...` + tab-indented-recipe shape", target, target)
	}
	return m[1]
}

// newErrorlintFixtureModule writes a minimal, throwaway Go module containing
// errorlintVulnerableSource and returns its directory. No go.sum/vendor is
// needed: the fixture only imports the standard library.
func newErrorlintFixtureModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module errorlintfixture\n\ngo 1.24\n")
	writeTestFile(t, filepath.Join(dir, "main.go"), errorlintVulnerableSource)
	return dir
}

// golangciLintVersionPin extracts GOLANGCI_LINT_VERSION from the Makefile,
// so pinnedGolangciLintBin tracks the real pin rather than a copy of it
// that could silently drift.
func golangciLintVersionPin(t *testing.T, repoRoot string) string {
	t.Helper()
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	re := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION := (\S+)`)
	m := re.FindStringSubmatch(string(makefile))
	if m == nil {
		t.Fatal("Makefile's `GOLANGCI_LINT_VERSION := ...` pin not found in the expected shape")
	}
	return m[1]
}

// pinnedGolangciLintBin resolves the pinned golangci-lint binary the same
// way the Makefile does (GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint),
// not whatever "golangci-lint" happens to resolve to on PATH, and confirms
// its --version matches the Makefile's GOLANGCI_LINT_VERSION pin: this
// test's RED half asserts a specific known bug shape in a specific
// version's errorlint --fix, and a different version resolved from PATH
// would make that assertion meaningless.
//
// Missing the binary only skips locally. On CI it fails instead (CI=true,
// set by GitHub Actions on every job): preflight-static installs the
// pinned binary via its own `make lint-affected`/`make lint` step before
// this test runs there (ga-w7nyj), and that is the one lane meant to keep
// this guard honest on every PR, not just on push -- a silent skip there
// would hide exactly the regression this guard exists to catch.
func pinnedGolangciLintBin(t *testing.T, repoRoot string) string {
	t.Helper()
	gopath, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		t.Fatalf("go env GOPATH: %v", err)
	}
	bin := filepath.Join(strings.TrimSpace(string(gopath)), "bin", "golangci-lint")
	if _, err := os.Stat(bin); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("pinned golangci-lint binary not found at %s on CI: %v", bin, err)
		}
		t.Skip("golangci-lint not installed; run `make install-tools` to exercise this test locally")
	}

	wantVersion := golangciLintVersionPin(t, repoRoot)
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", bin, err, out)
	}
	if !strings.Contains(string(out), wantVersion) {
		t.Fatalf("%s --version = %q, want it to contain the Makefile's GOLANGCI_LINT_VERSION pin %q -- this "+
			"test's RED half asserts a specific known bug in a specific version's errorlint --fix", bin, out, wantVersion)
	}
	return bin
}

// runGolangciLintFixture runs the pinned golangci-lint against the fixture
// module using THIS repo's own .golangci.yml (so the fixture is linted
// under the exact settings and enabled-linter set production commits go
// through), with or without --fix, and returns the process's exit code and
// combined output. Deliberately unscoped (no --new-from-rev/--whole-files):
// those flags gate WHICH issues count as "new" for the hook's own cost/scope
// reasons, unrelated to whether --fix is safe, and this test's premise (the
// fixture module IS the whole diff) makes scoping moot -- every issue in it
// is "new" under any scoping.
//
// GOLANGCI_LINT_CACHE (golangci-lint's own result cache, not Go's build
// cache) is pointed at a fresh, empty directory per call, same as CI's own
// lint steps use. golangci-lint's result cache is keyed loosely enough that
// two fixtures with byte-identical content (this test's fixture always is)
// can collide across separate t.TempDir()s: a --fix run can silently no-op
// against a STALE cached absolute path from an earlier call's
// since-deleted temp dir, logging only a warning ("no such file or
// directory") rather than failing, which would make this test wrongly
// report the negation as "surviving" --fix. An isolated GOLANGCI_LINT_CACHE
// gives each call its own empty result cache without also forcing a cold
// GOCACHE (an isolated HOME would do both, and the Go-level rebuild alone
// costs ~17s per call).
func runGolangciLintFixture(t *testing.T, fixtureDir, repoRoot string, fix bool) (exitCode int, output string) {
	t.Helper()
	bin := pinnedGolangciLintBin(t, repoRoot)
	args := []string{"run", "-c", filepath.Join(repoRoot, ".golangci.yml")}
	if fix {
		args = append(args, "--fix")
	}
	args = append(args, ".")
	cmd := exec.Command(bin, args...)
	cmd.Dir = fixtureDir
	cmd.Env = append(os.Environ(), "GOLANGCI_LINT_CACHE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run golangci-lint: %v\n%s", err, out)
		}
		exitCode = exitErr.ExitCode()
	}
	return exitCode, string(out)
}

// TestErrorlintStockFixDropsTypeAssertionNegation is the RED/GREEN proof
// behind ga-w7nyj's fix, on THIS repo's exact .golangci.yml and the pinned
// golangci-lint binary (pinnedGolangciLintBin): the GREEN half is the actual guard (--fix
// absent: the vulnerable pattern survives lint untouched and the run fails,
// so the commit is blocked instead of silently rewritten); the RED half
// proves the danger this guard exists for is real on this exact toolchain,
// not just a delta reviewer's one-off observation -- if a future
// golangci-lint/errorlint release fixes this upstream, the RED assertion
// starts failing here first, not silently, and --fix could be reconsidered.
func TestErrorlintStockFixDropsTypeAssertionNegation(t *testing.T) {
	repoRoot := repoRoot(t)

	// GREEN: no --fix. errorlint specifically (not merely "some linter")
	// must flag the vulnerable pattern, and the negation must survive.
	// Asserting on "(errorlint)" rather than a bare nonzero exit matters:
	// this fixture also trips revive's package-comments (it has no package
	// doc comment), so a bare `code == 0` check could never go red even if
	// errorlint itself stopped flagging the pattern -- revive's unrelated
	// finding would keep the exit code nonzero regardless.
	fixture := newErrorlintFixtureModule(t)
	code, output := runGolangciLintFixture(t, fixture, repoRoot, false)
	if code == 0 {
		t.Fatal("golangci-lint without --fix reported no issues against errorlintVulnerableSource; " +
			"either errorlint no longer flags this pattern or the fixture/config drifted -- either way, " +
			"this test no longer proves what it claims to")
	}
	if !strings.Contains(output, "(errorlint)") {
		t.Fatalf("golangci-lint without --fix found issues, but none attributed to errorlint specifically -- "+
			"this test must prove errorlint flags the vulnerable pattern, not merely that some linter does "+
			"(e.g. revive's package-comments on this fixture):\n%s", output)
	}
	got, err := os.ReadFile(filepath.Join(fixture, "main.go"))
	if err != nil {
		t.Fatalf("read fixture after lint (no --fix): %v", err)
	}
	if string(got) != errorlintVulnerableSource {
		t.Fatalf("golangci-lint WITHOUT --fix rewrote the fixture; --fix must be the only thing that can "+
			"ever do that, and it was not passed:\n%s", got)
	}

	// RED: same config, same golangci-lint, --fix added back. Confirms the
	// bug this guard exists for is still reproducible -- specifically the
	// INVERTED rewrite (`if errors.As(...) {`, no negation), not just any
	// rewrite that mentions errors.As. A CORRECT upstream fix would rewrite
	// to `if !errors.As(...) {`, which also drops the literal substring
	// "!ok" and also contains "errors.As(" -- the prior version of these
	// assertions could not tell a correct fix from the bug, so it could
	// never go red even once upstream fixed this.
	fixture = newErrorlintFixtureModule(t)
	fixCode, fixOutput := runGolangciLintFixture(t, fixture, repoRoot, true)
	got, err = os.ReadFile(filepath.Join(fixture, "main.go"))
	if err != nil {
		t.Fatalf("read fixture after lint (--fix; exit %d): %v\n%s", fixCode, err, fixOutput)
	}
	// The negated shape is checked FIRST: "if !errors.As(" does not contain
	// "if errors.As(", so testing the unnegated shape first would report a
	// correct upstream rewrite as "bug not reproduced" and this message
	// could never fire.
	if strings.Contains(string(got), "!errors.As(") {
		t.Fatalf("golangci-lint --fix produced a NEGATED `if !errors.As(...) {` -- that is the CORRECT "+
			"rewrite, meaning upstream has already fixed the bug this guard exists for. "+
			"TestPreCommitLintChangedFlagsDoNotAutoFix is the only guard left, and re-enabling --fix (now "+
			"that it is provably safe again) could be reconsidered (--fix exit %d):\n%s\n--- lint output ---\n%s",
			fixCode, got, fixOutput)
	}
	if !strings.Contains(string(got), "if errors.As(") {
		t.Fatalf("expected golangci-lint --fix to reproduce the ga-w7nyj bug on this golangci-lint/errorlint "+
			"version: an UNNEGATED `if errors.As(...) {`, inverting the original branch's meaning "+
			"(--fix exit %d; a crash would also land here). Got:\n%s\n--- lint output ---\n%s",
			fixCode, got, fixOutput)
	}
}

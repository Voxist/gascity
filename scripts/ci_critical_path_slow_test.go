//go:build integration

// Extracted from ci_critical_path_test.go: this single test is ~43s of a file
// whose other 20 tests total <1s. It exercises a real make invocation and
// belongs to the integration tier (ga-4h8bu split: cheap guards stay in the
// push gate, slow pipelines move).

package scripts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cmdGCProcessExtraTestEnv = `GO_TEST_TIMING_FILE="$${GO_TEST_TIMING_FILE}" GO_TEST_TIMING_NAME="$${GO_TEST_TIMING_NAME}" GO_TEST_TIMING_VARIANT="$${GO_TEST_TIMING_VARIANT}" GO_TEST_RUNNER_LABEL="$${GO_TEST_RUNNER_LABEL}" GC_TEST_FAILURE_ARTIFACT_DIR="$${GC_TEST_FAILURE_ARTIFACT_DIR}" GITHUB_SHA="$${GITHUB_SHA}" GITHUB_WORKFLOW="$${GITHUB_WORKFLOW}" GITHUB_RUN_ID="$${GITHUB_RUN_ID}" GITHUB_RUN_ATTEMPT="$${GITHUB_RUN_ATTEMPT}" GITHUB_JOB="$${GITHUB_JOB}" RUNNER_NAME="$${RUNNER_NAME}" RUNNER_OS="$${RUNNER_OS}" RUNNER_ARCH="$${RUNNER_ARCH}"`

func TestCmdGCProcessTimingEnvCrossesMakeIsolation(t *testing.T) {
	fixture := newGoTestShardFixture(t)
	timingDir := filepath.Join(fixture.tmpDir, "timing artifacts")
	if err := os.Mkdir(timingDir, 0o755); err != nil {
		t.Fatalf("create timing directory: %v", err)
	}
	timingFile := filepath.Join(timingDir, "cmd gc process.json")

	cmd := makeCommand(
		"test-cmd-gc-process-shard",
		"CMD_GC_PROCESS_SHARD=1",
		"CMD_GC_PROCESS_TOTAL=2",
		"EXTRA_TEST_ENV="+cmdGCProcessExtraTestEnv,
	)
	cmd.Dir = fixture.repoRoot
	cmd.Env = []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + fixture.homeDir,
		"SHELL=/bin/sh",
		"LANG=C.UTF-8",
		"TMPDIR=" + fixture.tmpDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
		"GO_TEST_TIMING_FILE=" + timingFile,
		"GO_TEST_TIMING_NAME=cmd-gc-process-1-of-2",
		"GO_TEST_TIMING_VARIANT=linux default",
		"GO_TEST_RUNNER_LABEL=blacksmith 32 vcpu",
		"GO_TEST_RUNNER_CPU_COUNT=99",
		"GITHUB_SHA=abc123",
		"GITHUB_WORKFLOW=CI workflow with spaces",
		"GITHUB_RUN_ID=77",
		"GITHUB_RUN_ATTEMPT=2",
		"GITHUB_JOB=cmd gc process",
		"RUNNER_NAME=runner name with spaces",
		"RUNNER_OS=Linux",
		"RUNNER_ARCH=X64",
	}
	status, output := runShardCommand(t, cmd)
	if status == 0 || !strings.Contains(string(output), "Error 23") {
		t.Fatalf("make status = %d, want product failure 23 to remain authoritative\n%s", status, output)
	}

	data, err := os.ReadFile(timingFile)
	if err != nil {
		t.Fatalf("read timing artifact after Make isolation: %v\n%s", err, output)
	}
	var artifact observableTimingArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode timing artifact after Make isolation: %v\n%s", err, data)
	}
	if artifact.ShardID != "cmd-gc-process-1-of-2" || artifact.Variant != "linux default" {
		t.Fatalf("timing identity after Make isolation = shard %q variant %q", artifact.ShardID, artifact.Variant)
	}
	if artifact.CommitSHA != "abc123" || artifact.Workflow != "CI workflow with spaces" || artifact.RunID != "77" || artifact.RunAttempt != "2" || artifact.Job != "cmd gc process" {
		t.Fatalf("timing run metadata after Make isolation = %+v", artifact)
	}
	wantRunner := observableTimingRunner{
		Label: "blacksmith 32 vcpu", Name: "runner name with spaces", OS: "Linux", Arch: "X64", CPUCount: 16,
	}
	if artifact.Runner != wantRunner {
		t.Fatalf("timing runner after Make isolation = %+v, want %+v", artifact.Runner, wantRunner)
	}
}

// The fork's macOS process shard step hands its settings to
// scripts/test-go-test-shard through make's EXTRA_TEST_ENV (ga-05vt3.12).
// Run through the real make target with the value the step computes, the
// shard must come out in progress mode and run the prebuilt binary the build
// job compiled, past the Makefile's env -i, compiling nothing itself.
func TestMacProcessShardProgressCrossesMakeIsolation(t *testing.T) {
	fixture := newGoTestShardFixture(t)
	shardEnv := processShardStepValue(t, "Voxist/gascity", "1", "shard_env")
	if shardEnv == "" {
		t.Fatal("the fork's process shard step sets no shard_env")
	}
	runnerTemp := filepath.Join(fixture.tmpDir, "runner-temp")
	if err := os.MkdirAll(filepath.Join(runnerTemp, "cmd-gc-test"), 0o755); err != nil {
		t.Fatal(err)
	}
	prebuilt := strings.Replace(macCmdGCPrebuiltPath, macRunnerTemp, runnerTemp, 1)
	if err := os.WriteFile(prebuilt, []byte(readFixtureFile(t, fixture.fakeTestBinary)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.binaryExitFile, []byte("23"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := makeCommand(
		"test-cmd-gc-process-shard",
		"CMD_GC_PROCESS_SHARD=1",
		"CMD_GC_PROCESS_TOTAL=2",
		"EXTRA_TEST_ENV="+strings.ReplaceAll(shardEnv, macRunnerTemp, runnerTemp),
	)
	cmd.Dir = fixture.repoRoot
	cmd.Env = []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + fixture.homeDir,
		"SHELL=/bin/sh",
		"LANG=C.UTF-8",
		"TMPDIR=" + fixture.tmpDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
	}
	status, output := runShardCommand(t, cmd)
	if status == 0 || !strings.Contains(string(output), "Error 23") {
		t.Fatalf("make status = %d, want the prebuilt binary's failure 23 to remain authoritative\n%s", status, output)
	}
	for _, want := range []string{
		"progress on for ./cmd/gc shard 1 of 2: heartbeat every 120s, SIGQUIT to test binaries after 38m",
		"/cmd-gc-test/gc.test",
		"discovery: gc.test -test.list (runs TestMain)",
		"product run: gc.test -test.v",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("make-run shard output lacks %q:\n%s", want, output)
		}
	}
	if allArgs, err := os.ReadFile(fixture.allTestArgsFile); !os.IsNotExist(err) {
		t.Fatalf("the prebuilt shard ran go test (err %v):\n%s", err, allArgs)
	}
}

// make test-cmd-gc-process-build compiles cmd/gc's test binary once, through
// scripts/test-go-test-shard's own compile, to CMD_GC_PROCESS_BINARY, and runs
// no test; without CMD_GC_PROCESS_BINARY it refuses to run.
func TestMacProcessBuildCrossesMakeIsolation(t *testing.T) {
	fixture := newGoTestShardFixture(t)
	env := []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + fixture.homeDir,
		"SHELL=/bin/sh",
		"LANG=C.UTF-8",
		"TMPDIR=" + fixture.tmpDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
	}

	missing := makeCommand("test-cmd-gc-process-build")
	missing.Dir, missing.Env = fixture.repoRoot, env
	if status, output := runShardCommand(t, missing); status == 0 || !strings.Contains(string(output), "CMD_GC_PROCESS_BINARY") {
		t.Fatalf("make without CMD_GC_PROCESS_BINARY: status %d, want a failure naming it\n%s", status, output)
	}

	out := filepath.Join(fixture.tmpDir, "runner temp", "cmd-gc-test", "gc.test")
	cmd := makeCommand(
		"test-cmd-gc-process-build",
		"CMD_GC_PROCESS_BINARY="+out,
		"EXTRA_TEST_ENV=GO_TEST_SHARD_PROGRESS=1 GO_TEST_SHARD_PROGRESS_INTERVAL=60",
	)
	cmd.Dir, cmd.Env = fixture.repoRoot, env
	status, output := runShardCommand(t, cmd)
	if status != 0 {
		t.Fatalf("make status = %d, want 0\n%s", status, output)
	}
	wantCompile := "test\n-c\n-o\n" + out + "\n./cmd/gc\n"
	if got := readFixtureFile(t, fixture.allTestArgsFile); got != wantCompile {
		t.Fatalf("build go invocations:\n%s\nwant exactly the compile:\n%s", got, wantCompile)
	}
	for _, want := range []string{"compile: go test -c ./cmd/gc", "built ./cmd/gc test binary " + out + " (3 tests)"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("make-run build output lacks %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(fixture.productArgsFile); !os.IsNotExist(err) {
		t.Fatalf("the build ran a product (stat err %v)", err)
	}
}

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

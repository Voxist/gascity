// Helpers shared between fast-tier and integration-tagged test files in this
// package; must stay untagged so both build shapes see them (ga-4h8bu split).

package scripts_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func replaceScriptEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func scriptCommand(repoRoot, name string, args ...string) *exec.Cmd {
	return exec.Command(filepath.Join(repoRoot, "scripts", name), args...)
}

func assertFieldsInOrder(t *testing.T, fields string, want ...string) {
	t.Helper()
	allFields := strings.Fields(fields)
	next := 0
	for _, field := range allFields {
		if field == want[next] {
			next++
			if next == len(want) {
				return
			}
		}
	}
	t.Fatalf("fields are not in required order %v:\n%s", want, fields)
}

func countExactField(fields, want string) int {
	count := 0
	for _, field := range strings.Fields(fields) {
		if field == want {
			count++
		}
	}
	return count
}

type observableTimingArtifact struct {
	Schema     int                    `json:"schema"`
	ShardID    string                 `json:"shard_id"`
	Variant    string                 `json:"variant"`
	CommitSHA  string                 `json:"commit_sha"`
	Workflow   string                 `json:"workflow"`
	RunID      string                 `json:"run_id"`
	RunAttempt string                 `json:"run_attempt"`
	Job        string                 `json:"job"`
	Runner     observableTimingRunner `json:"runner"`
	Units      []observableTimingUnit `json:"units"`
}

type observableTimingRunner struct {
	Label    string `json:"label"`
	Name     string `json:"name"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUCount int    `json:"cpu_count"`
}

type observableTimingUnit struct {
	UnitID          string  `json:"unit_id"`
	Kind            string  `json:"kind"`
	Package         string  `json:"package"`
	Test            string  `json:"test"`
	Subtest         string  `json:"subtest"`
	Outcome         string  `json:"outcome"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func makeCommand(args ...string) *exec.Cmd {
	return testCommand("make", args...)
}

func testCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

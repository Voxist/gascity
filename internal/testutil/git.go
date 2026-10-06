package testutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// InitGitRepo creates a test-owned repository with one commit and returns its
// path and initial branch.
func InitGitRepo(t testing.TB) (string, string) {
	t.Helper()
	dir := t.TempDir()
	RunGit(t, dir, "init")
	RunGit(t, dir, "config", "user.email", "test@test.com")
	RunGit(t, dir, "config", "user.name", "Test")
	// Background maintenance (spawned detached after commit) briefly creates
	// objects/maintenance.lock, which races tests that snapshot the git dir
	// and diff it. Disable it before the first commit so none ever starts.
	RunGit(t, dir, "config", "maintenance.auto", "false")
	RunGit(t, dir, "config", "gc.auto", "0")
	RunGit(t, dir, "commit", "--allow-empty", "-m", "init")
	return dir, RunGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// RunGit runs git with repository-locating environment variables removed so
// ambient Git state cannot redirect a test command.
func RunGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return strings.TrimSpace(string(out))
}

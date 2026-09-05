package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTrimGoBuildCache runs the shell self-test for
// scripts/trim-go-build-cache.sh, the daily age-based trim of the shared Go
// build cache (AGENTS.md "Build Cache Conventions").
//
// The load-bearing assertion is that the trim's selector can never match a
// cache root. The prune script this replaces selected caches with the glob
// 'go-build-*', which also matches the empty suffix 'go-build-' -- the live
// shared cache root itself -- and would have deleted the entire fleet cache.
// The suite also pins that a find which does not honor -newermt is rejected
// rather than silently deleting nothing (bare 'find' on this host routes to
// bfs, which does exactly that), that Go's own trim bookkeeping at depth 1
// survives, that executable-cache directories are removed, and that the run
// aborts before deleting anything if any candidate path is not shaped like a
// cache entry.
//
// Hermetic: synthetic caches under t.TempDir()-equivalent scratch dirs only.
// The real cache at $HOME/Library/Caches/go-build is never read or written.
func TestTrimGoBuildCache(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-trim-go-build-cache.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-trim-go-build-cache.sh failed: %v\n%s", err, out)
	}
}

package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCheckGoCleanCache runs the shell self-test for
// scripts/check-go-clean-cache.sh, the commit-time half of the `go clean
// -cache` ban (AGENTS.md "Build Cache Conventions"; incident vp-g96b
// 2026-06-13, recurrence 2026-09-05).
//
// The guard keeps the command out of the codebase — a script, a Makefile
// recipe, a CI step, an order, an exec.Command. It does NOT prevent anyone
// running it, and would not have prevented either incident: nothing was
// committed on either occasion, a process executed the command. The runtime
// half is scripts/go-clean-cache-shim.sh, covered by
// TestGoCleanCacheShim.
//
// The suite pins the flag parse (`-testcache` / `-modcache` / `-fuzzcache` and
// a bare `go clean` must stay allowed, `cargo clean --cache` is not go, a
// package path containing the text is not a hit), that prose is out of the
// scanned surface by construction rather than by allowlist, both exemption
// markers, the --staged mode reading the index rather than the worktree, and
// the fail-closed path outside a git work tree. Its last case scans this
// repository itself.
//
// Hermetic: temp git repos only; no network, no go toolchain, and no cache is
// touched.
func TestCheckGoCleanCache(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-check-go-clean-cache.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-check-go-clean-cache.sh failed: %v\n%s", err, out)
	}
}

package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCheckResyncLoss runs the shell self-test for
// scripts/check-resync-loss.sh, the resync merge-loss gate (ga-d32bn and
// ga-8gpw4; AGENTS.md "Resync conventions" rules 5 and 6). It exercises the
// historical 2026-08-31 resync merge, synthetic-repo fixtures for
// DROPPED-FILE / the identical-change skip / conflict-hunk stripping /
// (name, file) keying, the matched-pair fixtures that prove each gate goes
// red on exactly one dropped declaration and green again when it is
// restored (ga-qq43h), and .githooks/pre-push wiring end to end against a
// real bare remote — in both the fork and the upstream direction.
// Hermetic: temp git repos and this repo's own already-fetched history
// only, no network/gh/model calls.
func TestCheckResyncLoss(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-check-resync-loss.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-check-resync-loss.sh failed: %v\n%s", err, out)
	}
}

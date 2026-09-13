package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPrePushLocalTestsAck runs the shell self-test for the LOCAL_TESTS_ACK
// escape on gate 3 of .githooks/pre-push (bead vc-dq0b).
//
// The property under test is that the ack is SCOPED, not merely that it
// works: it skips the fast-test tier and leaves the resync-loss gate
// (ga-d32bn) and the fail-closed bead-ownership guard (ga-fip9ps.1) armed.
// Before this ack the only bypass was `git push --no-verify`, which disarms
// all three — so every seat on a host where the tier cannot pass was pushing
// with the ownership guard silently disabled.
//
// Hermetic: temp git repos, a stub bd, and a Makefile whose tier target is a
// simulated failure. It never invokes the real Go suite and asserts no
// wall-clock budget.
func TestPrePushLocalTestsAck(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-pre-push-local-tests-ack.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-pre-push-local-tests-ack.sh failed: %v\n%s", err, out)
	}
}

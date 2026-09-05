package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGoCleanCacheShim runs the shell self-test for
// scripts/go-clean-cache-shim.sh and scripts/install-go-clean-cache-shim.sh,
// the runtime half of the `go clean -cache` ban (AGENTS.md "Build Cache
// Conventions"; incident vp-g96b 2026-06-13, recurrence 2026-09-05).
//
// Installed ahead of the real toolchain on PATH, the shim sits in front of
// every Go build on the host, so the passthrough matters more than the
// refusal. The suite pins argv fidelity, exit-status fidelity across several
// codes, stdout/stderr separation, stdin passthrough, and — by pid identity —
// that the passthrough is a real exec rather than a fork, so no wrapper
// process is left in the middle to swallow a signal. It also pins that the
// decision is a parse of the argument list rather than a grep of the command
// line (`go build ./cmd/go-clean-cache` and `go test -run 'go clean -cache'`
// must pass), that `-testcache`/`-modcache`/`-fuzzcache` stay allowed, that
// misconfiguration fails loud rather than open, the documented
// GC_ALLOW_GO_CLEAN_CACHE override, and the installer's refusal to write into
// a PATH-shadowed directory or to delete anything that is not this shim.
//
// Hermetic and safe: every case drives the shim against a FAKE go on a temp
// PATH. The real toolchain is never invoked and `go clean -cache` is never
// executed — which is the whole point of the thing under test.
func TestGoCleanCacheShim(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command(filepath.Join(root, "scripts", "test-go-clean-cache-shim.sh"))
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-go-clean-cache-shim.sh failed: %v\n%s", err, out)
	}
}

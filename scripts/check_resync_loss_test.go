package scripts_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	reportSkippedCases(os.Stderr, out)
}

// reportSkippedCases surfaces any case the shell suite skipped rather than
// ran, so a skip cannot masquerade as a pass.
//
// The historical layer needs commit 15913af6a, which a shallow checkout does
// not have — and CI's preflight-unit-cover-noncmdgc job uses
// actions/checkout at its default depth of 1, so that layer is skipped there
// BY DESIGN, every run. Without this the package just prints "ok" and a
// reader has no way to tell a deliberately-unreachable fixture from one that
// silently stopped running.
//
// Written straight to stderr, not t.Logf: `go test` only surfaces t.Log
// output for a failing test or under -v, and the CI shard runs neither. One
// line, and only when something actually skipped. The writer is a parameter
// so TestReportSkippedCases can prove both polarities against fixture text
// instead of the author eyeballing a real run (ga-qq43h).
func reportSkippedCases(w io.Writer, out []byte) {
	var skipped []string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "  skip "); ok {
			skipped = append(skipped, rest)
		}
	}
	if len(skipped) == 0 {
		return
	}
	// Blank-assigned: a write failure to the test log is not actionable,
	// and errcheck requires the intent to be explicit.
	_, _ = fmt.Fprintf(w, "TestCheckResyncLoss: %d case(s) skipped, not run:\n", len(skipped))
	for _, s := range skipped {
		_, _ = fmt.Fprintf(w, "  skip %s\n", s)
	}
}

// TestReportSkippedCases proves the skip notice fires on the exact output the
// shell suite produces for an unreachable historical fixture, and stays
// silent on an all-green run. Without the second half a reporter that
// printed unconditionally would look correct; without the first, one that
// never printed would.
func TestReportSkippedCases(t *testing.T) {
	const skipLine = "  skip historical fixture reachable — commit 15913af6a not found in this checkout (shallow clone?)"

	for _, tc := range []struct {
		name     string
		out      string
		wantEmit bool
	}{
		{
			name:     "shallow checkout skips the historical layer",
			out:      "== historical ==\n" + skipLine + "\n  ok   exits zero on a clean merge\n== summary: 24 passed, 0 failed, 1 skipped ==\n",
			wantEmit: true,
		},
		{
			name:     "all green emits nothing",
			out:      "  ok   exits zero on a clean merge\n== summary: 28 passed, 0 failed, 0 skipped ==\n",
			wantEmit: false,
		},
		{
			// "skipped" appears in the summary line of EVERY run, green
			// ones included. Matching it loosely would emit on every run
			// and train readers to ignore the notice.
			name:     "the summary line's own word is not a skipped case",
			out:      "== summary: 28 passed, 0 failed, 0 skipped ==\n",
			wantEmit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			reportSkippedCases(&buf, []byte(tc.out))
			if got := buf.Len() > 0; got != tc.wantEmit {
				t.Fatalf("emitted=%v, want %v; output was %q", got, tc.wantEmit, buf.String())
			}
			if tc.wantEmit && !bytes.Contains(buf.Bytes(), []byte("historical fixture reachable")) {
				t.Fatalf("notice does not name the skipped case: %q", buf.String())
			}
		})
	}
}

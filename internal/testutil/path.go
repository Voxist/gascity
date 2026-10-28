// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

const shortTempRootMaxLen = 40

// CanonicalPath returns the production path-normalized form used for
// comparisons. This keeps tests stable on macOS where /tmp and /var can be
// reported through /private aliases.
func CanonicalPath(path string) string {
	return pathutil.NormalizePathForCompare(path)
}

// AssertSamePath compares two filesystem paths after canonicalization.
func AssertSamePath(t *testing.T, got, want string) {
	t.Helper()
	got = CanonicalPath(got)
	want = CanonicalPath(want)
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// AssertCanonicalPathEquals compares a path a function under test RETURNED
// against an expectation, normalizing ONLY the expectation.
//
// Use this, not AssertSamePath, whenever the function under test is itself
// responsible for canonicalizing. AssertSamePath normalizes both sides, and
// CanonicalPath resolves symlinks — so it re-does the very work being asserted
// and the assertion becomes a tautology that an identity implementation passes.
// Normalizing only want keeps the darwin /private-alias spelling difference
// tolerated while still failing on a got that was never resolved.
func AssertCanonicalPathEquals(t *testing.T, got, want string) {
	t.Helper()
	if got != CanonicalPath(want) {
		t.Fatalf("path = %q, want %q (canonicalized from %q)", got, CanonicalPath(want), want)
	}
}

// ShortTempDir returns a test-owned directory under a short root.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root := shortTempRoot(os.TempDir(), os.Getenv("TEST_TMPDIR"))
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%q, %q): %v", root, prefix, err)
	}
	t.Cleanup(func() {
		SaveFailureDiagnostics(t, dir)
		_ = os.RemoveAll(dir)
	})
	return dir
}

func shortTempRoot(tempDir, testTempDir string) string {
	if runtime.GOOS == "windows" {
		return tempDir
	}
	canonicalRoot := CanonicalPath(tempDir)
	if len(tempDir) <= shortTempRootMaxLen && canonicalRoot != "/tmp" && !strings.HasPrefix(canonicalRoot, "/tmp/") {
		return tempDir
	}
	canonicalTestRoot := CanonicalPath(testTempDir)
	if canonicalTestRoot == "/tmp/bt" || strings.HasPrefix(canonicalTestRoot, "/tmp/bt/") {
		return "/tmp/bt"
	}
	return "/var/tmp"
}

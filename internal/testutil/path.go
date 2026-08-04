// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

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

// ShortTempDir returns a test-owned temporary directory rooted at a short path
// on macOS so Unix socket paths stay under the platform limit.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root := os.TempDir()
	if runtime.GOOS == "darwin" {
		root = "/tmp"
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%q, %q): %v", root, prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

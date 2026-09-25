//go:build integration

// Helpers whose only callers live in this package's integration-tagged files;
// kept behind the same tag so the default-tag build (and its `unused` lint)
// never sees an orphan.

package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func filteredMakefileCGOTestEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "CGO_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	// The Makefile's CGO probes invoke `go env`; with an isolated HOME that
	// would download a toolchain into t.TempDir(), whose read-only module
	// cache files then defeat TempDir cleanup. Pin the local toolchain and
	// keep any module-cache writes out of the temp home.
	return append(filtered,
		"GOTOOLCHAIN=local",
		"GOMODCACHE="+filepath.Join(os.TempDir(), "gc-makefile-cgo-gomodcache"),
	)
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("output contains %q:\n%s", needle, haystack)
	}
}

func lineWithPrefix(t *testing.T, output, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("output missing line prefix %q:\n%s", prefix, output)
	return ""
}

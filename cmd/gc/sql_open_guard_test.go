package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGCNonTestFilesUsePooledDoltConnections forbids raw sql.Open calls
// in cmd/gc and internal/api non-test files (style of
// TestGCNonTestFilesStayOnWorkerBoundary). Every Go-native Dolt
// connection must go through the shared internal/doltpool registry: one
// pooled *sql.DB per endpoint with bounded open/idle/lifetime caps.
// Per-call sql.Open+Close churns connections — the
// dolt_project_id.go pattern alone produced 2,618 TIME_WAIT sockets —
// and bypasses the connection budget the supervisor doctor enforces
// (city-scale plan items 1.2 and 2.7).
//
// If you need a deliberately fresh, unpooled connection (e.g. a health
// probe that must exercise new-connection setup), put it in a dedicated
// package outside cmd/gc and internal/api, or extend doltpool with an
// explicit fresh-dial API — do not add raw sql.Open here.
func TestGCNonTestFilesUsePooledDoltConnections(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	gcDir := filepath.Dir(currentFile)
	dirs := []string{
		gcDir,
		filepath.Join(gcDir, "..", "..", "internal", "api"),
	}
	// No files are currently allowed to call sql.Open directly.
	allowed := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if allowed[name] {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", path, err)
			}
			if strings.Contains(string(data), "sql.Open(") {
				t.Errorf("%s calls sql.Open directly; route the connection through internal/doltpool (shared pooled *sql.DB per endpoint)", path)
			}
		}
	}
}

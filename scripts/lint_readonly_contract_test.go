//go:build integration

package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFmtCheckDoesNotModifyGoSumWithAmbientWritableModuleMode(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"example.go": "package example\n\nfunc Value() int { return 1 }\n",
	})
	goSumPath := filepath.Join(fixture.repoRoot, "go.sum")
	writeTestFile(t, goSumPath, "example.com/dependency v1.0.0 h1:before\n")

	mutatingLint := filepath.Join(t.TempDir(), "golangci-lint")
	writeExecutable(t, mutatingLint, `#!/bin/sh
set -eu
case "${GOFLAGS-}" in
  *-tags=quality*) ;;
  *)
    echo "formatter lost non-module GOFLAGS" >&2
    exit 1
    ;;
esac
case "${GOFLAGS-}" in
  *-mod=readonly*) exit 0 ;;
esac
printf '%s\n' 'unexpected writable formatter resolution' >> go.sum
`)

	cmd := makeCommand(
		"--no-print-directory",
		"-f", fixture.productionMakefile,
		"GOLANGCI_LINT="+mutatingLint,
		"fmt-check",
	)
	cmd.Dir = fixture.repoRoot
	env := fixture.commandEnv()
	for index, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[index] = "GOFLAGS=-tags=quality -mod=mod"
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fmt-check failed: %v\n%s", err, output)
	}

	got, err := os.ReadFile(goSumPath)
	if err != nil {
		t.Fatalf("read go.sum after fmt-check: %v", err)
	}
	const want = "example.com/dependency v1.0.0 h1:before\n"
	if string(got) != want {
		t.Fatalf("fmt-check modified go.sum under ambient -mod=mod:\nwant: %q\n got: %q", want, got)
	}
}

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

func TestLintChangedFailsClosedWhenReadonlyMetadataIsStale(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
	})
	writeTestFile(t, filepath.Join(fixture.repoRoot, "go.sum"), "example.com/dependency v1.0.0 h1:before\n")
	writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

	goTool := filepath.Join(t.TempDir(), "go")
	writeExecutable(t, goTool, `#!/bin/sh
set -eu
case "${1-}" in
  env)
    if [ "${2-}" = "GOFLAGS" ]; then
      printf '%s\n' "${GOFLAGS-}"
    fi
    exit 0
    ;;
  list)
    case "${GOFLAGS-}" in
      *-mod=readonly*)
        echo "go: updates to go.sum needed; disabled by -mod=readonly" >&2
        exit 1
        ;;
    esac
    echo "unexpected writable module resolution" >> go.sum
    exit 0
    ;;
esac
echo "unexpected go invocation: $*" >&2
exit 1
`)

	before, err := os.ReadFile(filepath.Join(fixture.repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum before lint: %v", err)
	}
	fixture.resetCalls(t)
	cmd := makeCommand(
		"--no-print-directory",
		"-f", fixture.productionMakefile,
		"GOLANGCI_LINT="+fixture.fakeLint,
		"LINT_CHANGED_SCOPE=tracked",
		"LINT_CHANGED_REF=HEAD",
		"LINT_FLAGS=",
		"lint-changed",
	)
	cmd.Dir = fixture.repoRoot
	env := fixture.commandEnv()
	for index, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[index] = "GOFLAGS=-mod=mod"
		}
	}
	env = append(env, "PATH="+filepath.Dir(goTool)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("lint-changed succeeded with stale readonly metadata:\n%s", output)
	}
	if !strings.Contains(string(output), "updates to go.sum needed") {
		t.Fatalf("lint-changed error did not preserve the module failure:\n%s", output)
	}
	after, err := os.ReadFile(filepath.Join(fixture.repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum after lint: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("lint-changed modified go.sum under ambient -mod=mod:\nbefore: %q\nafter:  %q", before, after)
	}
	fixture.requireNoCalls(t)
}

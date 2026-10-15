package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// preCommitLintModeRepo builds a temp repo with one staged .go file and every
// path the hook's Go block `git add`s, and returns it with a runner that
// executes the real pre-commit hook there under the given GC_PRECOMMIT_LINT.
// `make` fails loudly (so reaching the nogo path is observable) and `go`
// records its argv into go-calls.log.
func preCommitLintModeRepo(t *testing.T) (string, func(mode string) (string, error)) {
	t.Helper()
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")
	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	installBeadsChainForTempRepo(t, repoRoot, tmpRepo)
	runGit("init")
	for _, p := range []string{
		"internal/api/openapi.json",
		"docs/reference/schema/openapi.json",
		"docs/reference/schema/openapi.txt",
		"internal/api/genclient/client_gen.go",
		"docs/reference/schema/city-schema.json",
		"docs/reference/schema/city-schema.txt",
		"docs/reference/config.md",
		"docs/reference/cli.md",
	} {
		writeTestFile(t, filepath.Join(tmpRepo, p), "{}\n")
	}
	if err := os.MkdirAll(filepath.Join(tmpRepo, "scripts"), 0o755); err != nil {
		t.Fatalf("create scripts dir: %v", err)
	}
	writeExecutable(t, filepath.Join(tmpRepo, "scripts", "precommit-format-staged-go"), "#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n")
	writeTestFile(t, filepath.Join(tmpRepo, "pkg", "a", "a.go"), "package a\n")
	// A nested module (like tools/nogo): its packages must be vetted from
	// inside that module, not from the repo root.
	writeTestFile(t, filepath.Join(tmpRepo, "tools", "sub", "go.mod"), "module example.com/sub\n")
	writeTestFile(t, filepath.Join(tmpRepo, "tools", "sub", "x", "x.go"), "package x\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")
	writeTestFile(t, filepath.Join(tmpRepo, "pkg", "a", "a.go"), "package a\n\nvar X = 1\n")
	writeTestFile(t, filepath.Join(tmpRepo, "tools", "sub", "x", "x.go"), "package x\n\nvar Y = 1\n")
	runGit("add", "pkg/a/a.go", "tools/sub/x/x.go")

	goLog := filepath.Join(tmpRepo, "go-calls.log")
	run := func(mode string) (string, error) {
		cmd := exec.Command("bash", hookPath)
		cmd.Dir = tmpRepo
		cmd.Env = []string{
			"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
				"make": "#!/usr/bin/env bash\necho \"make $*\" >&2\nexit 7\n",
				// `go -C <mod> list -e -f <tmpl> <pkgs...>` answers with its
				// package arguments (all buildable); every call is logged.
				"go": "#!/usr/bin/env bash\necho \"$*\" >> " + goLog + "\nif [ \"$3\" = list ]; then shift 6; printf '%s\\n' \"$@\"; fi\nexit 0\n",
			}),
			"HOME=" + t.TempDir(),
			"GC_PRECOMMIT_LINT=" + mode,
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return goLog, run
}

func TestPreCommitLintModeGoVetsStagedPackagesInsteadOfNogo(t *testing.T) {
	goLog, run := preCommitLintModeRepo(t)
	out, err := run("go")
	if err != nil {
		t.Fatalf("pre-commit with GC_PRECOMMIT_LINT=go must not reach `make lint-changed` (nogo via Bazel); err=%v output:\n%s", err, out)
	}
	if !strings.Contains(out, "NOT nogo") {
		t.Fatalf("GC_PRECOMMIT_LINT=go must say loudly that it is not CI's nogo gate, got:\n%s", out)
	}
	calls, readErr := os.ReadFile(goLog)
	if readErr != nil {
		t.Fatalf("read go stub log: %v", readErr)
	}
	if !strings.Contains(string(calls), "-C . vet ./pkg/a") {
		t.Fatalf("GC_PRECOMMIT_LINT=go must run go vet over the staged root-module package ./pkg/a, go calls:\n%s", calls)
	}
	if !strings.Contains(string(calls), "-C tools/sub vet ./x") {
		t.Fatalf("GC_PRECOMMIT_LINT=go must vet a nested module's package from inside that module, go calls:\n%s", calls)
	}
}

func TestPreCommitLintModeRejectsUnknownValue(t *testing.T) {
	_, run := preCommitLintModeRepo(t)
	out, err := run("golangci")
	var exitErr *exec.ExitError
	if err == nil || !errorsAsExit(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("an unknown GC_PRECOMMIT_LINT must fail the commit with exit 2, err=%v output:\n%s", err, out)
	}
	if !strings.Contains(out, "unknown GC_PRECOMMIT_LINT=golangci") {
		t.Fatalf("unknown-mode failure must name the value, got:\n%s", out)
	}
}

func errorsAsExit(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns *ExitError unwrapped
	if ok {
		*target = exitErr
	}
	return ok
}

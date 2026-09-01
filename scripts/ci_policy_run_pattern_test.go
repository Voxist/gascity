package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCIPolicyRunPatternNamesExistingTests guards the silent-green class in
// the Makefile's test-ci-policy target: it selects scripts tests by an
// anchored -run alternation, and a `go test -run` pattern that matches no
// test reports ok while running nothing. When the pr_static_scope contract
// file was moved behind -tags integration, the target kept matching the two
// static-scope tests by name without the tag — and CI stayed green. Every
// name in the alternation must be defined in this package (any build shape),
// and the line must carry -tags integration if any named test lives in a
// tagged file.
func TestCIPolicyRunPatternNamesExistingTests(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	target := shellFunctionBodyFromMakefile(t, string(makefile), "test-ci-policy")
	line := ""
	for _, l := range strings.Split(target, "\n") {
		if strings.Contains(l, "./scripts") && strings.Contains(l, "-run") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("test-ci-policy has no `go test -run ... ./scripts` line")
	}
	pat := regexp.MustCompile(`-run '\^\(([^)]+)\)\$\$?'`).FindStringSubmatch(line)
	if pat == nil {
		t.Fatalf("cannot parse the -run alternation from: %s", line)
	}
	names := strings.Split(pat[1], "|")

	files, err := filepath.Glob(filepath.Join(root, "scripts", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	tagged := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		isTagged := strings.Contains(string(src), "//go:build integration")
		for _, m := range regexp.MustCompile(`(?m)^func (Test\w+)\(`).FindAllStringSubmatch(string(src), -1) {
			defined[m[1]] = true
			if isTagged {
				tagged[m[1]] = true
			}
		}
	}
	needsTag := false
	for _, n := range names {
		if !defined[n] {
			t.Errorf("test-ci-policy -run names %s, which no scripts test file defines — the target matches nothing for it and reports ok", n)
		}
		if tagged[n] {
			needsTag = true
		}
	}
	if needsTag && !strings.Contains(line, "-tags integration") {
		t.Errorf("test-ci-policy -run names an integration-tagged test but the line lacks -tags integration:\n%s", line)
	}
}

// shellFunctionBodyFromMakefile returns the recipe lines of a Makefile target.
func shellFunctionBodyFromMakefile(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, target+":") {
			var body []string
			for _, r := range lines[i+1:] {
				if !strings.HasPrefix(r, "\t") {
					break
				}
				body = append(body, strings.TrimPrefix(r, "\t"))
			}
			return strings.Join(body, "\n")
		}
	}
	t.Fatalf("Makefile target %s not found", target)
	return ""
}

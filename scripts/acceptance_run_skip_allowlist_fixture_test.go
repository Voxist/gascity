package scripts_test

import (
	"strings"
	"testing"
)

// Fixture-based unit tests for parseSkipExpression, splitTopLevelAlternation,
// requiredJobEnv, and skipAllowlistProblems, all defined in
// acceptance_run_selection_test.go. These exercise the parser and the
// allowlist engine directly, without touching the real ci.yml -- the TDD
// probe ga-may9k asked for, kept as permanent regression coverage rather
// than a one-off manual check.

func TestParseSkipExpression(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		want    []string
		wantErr string // substring expected in the error, if non-empty
	}{
		{
			name: "bare test name",
			expr: "TestFoo",
			want: []string{"TestFoo"},
		},
		{
			name: "single subtest alternation",
			expr: "TestFoo/(bar)",
			want: []string{"TestFoo/bar"},
		},
		{
			name: "two-way subtest alternation, the real ci.yml shape",
			expr: "TestProxiedNativeLifecycle/(child-term-zombie|root-move)",
			want: []string{"TestProxiedNativeLifecycle/child-term-zombie", "TestProxiedNativeLifecycle/root-move"},
		},
		{
			name: "three-way subtest alternation",
			expr: "TestFoo/(a|b|c)",
			want: []string{"TestFoo/a", "TestFoo/b", "TestFoo/c"},
		},
		{
			name: "top-level alternation of two whole clauses",
			expr: "TestA|TestB/(x|y)",
			want: []string{"TestA", "TestB/x", "TestB/y"},
		},
		{
			name: "surrounding whitespace on clauses and alternatives is trimmed",
			expr: " TestFoo/( bar | baz ) ",
			want: []string{"TestFoo/bar", "TestFoo/baz"},
		},
		{
			name:    "empty expression",
			expr:    "",
			wantErr: "empty -skip expression",
		},
		{
			name:    "regex metacharacters are not expanded",
			expr:    "TestFoo.*",
			wantErr: "cannot expand",
		},
		{
			name:    "anchors are not expanded",
			expr:    "^TestFoo$",
			wantErr: "cannot expand",
		},
		{
			name:    "character classes inside the alternation are not expanded",
			expr:    "TestFoo/([abc])",
			wantErr: "cannot expand",
		},
		{
			name:    "nested groups are not expanded",
			expr:    "TestFoo/(bar|(baz|qux))",
			wantErr: "cannot expand",
		},
		{
			name:    "an empty alternative is not a plain name",
			expr:    "TestFoo/(bar|)",
			wantErr: "cannot expand",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSkipExpression(c.expr)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSkipExpression(%q) = %v, nil; want an error containing %q", c.expr, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("parseSkipExpression(%q) error = %q, want it to contain %q", c.expr, err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSkipExpression(%q) unexpected error: %v", c.expr, err)
			}
			if !equalStringSlices(got, c.want) {
				t.Fatalf("parseSkipExpression(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestSplitTopLevelAlternation(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []string
	}{
		{name: "no alternation", expr: "TestFoo", want: []string{"TestFoo"}},
		{name: "top-level only", expr: "TestA|TestB", want: []string{"TestA", "TestB"}},
		{
			name: "nested alternation stays inside its clause",
			expr: "TestA/(x|y)|TestB",
			want: []string{"TestA/(x|y)", "TestB"},
		},
		{
			name: "unbalanced closing paren does not underflow",
			expr: "TestA)|TestB",
			want: []string{"TestA)", "TestB"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitTopLevelAlternation(c.expr)
			if !equalStringSlices(got, c.want) {
				t.Fatalf("splitTopLevelAlternation(%q) = %v, want %v", c.expr, got, c.want)
			}
		})
	}
}

func TestRequiredJobEnv(t *testing.T) {
	on := map[string]any{"GC_REQUIRE_ACCEPTANCE_TOOLING": "1"}
	off := map[string]any{"GC_REQUIRE_ACCEPTANCE_TOOLING": "0"}
	empty := map[string]any{"GC_REQUIRE_ACCEPTANCE_TOOLING": ""}
	unset := map[string]any{}
	other := map[string]any{"SOMETHING_ELSE": "1"}

	cases := []struct {
		name string
		envs []map[string]any
		want bool
	}{
		{name: "on at any level", envs: []map[string]any{unset, on, unset}, want: true},
		{name: "unset everywhere", envs: []map[string]any{unset, unset}, want: false},
		{name: "explicit 0 is off", envs: []map[string]any{off}, want: false},
		{name: "empty string is off", envs: []map[string]any{empty}, want: false},
		{name: "unrelated key is off", envs: []map[string]any{other}, want: false},
		{name: "no envs at all", envs: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := requiredJobEnv(c.envs...); got != c.want {
				t.Fatalf("requiredJobEnv(%v) = %v, want %v", c.envs, got, c.want)
			}
		})
	}
}

// fixtureWorkflow builds a minimal ci.yml-shaped document with one job named
// "probe". required controls whether the job sets
// GC_REQUIRE_ACCEPTANCE_TOOLING; skip, when non-empty, becomes a
// `-skip '<skip>'` argument on the job's one `go test` step.
func fixtureWorkflow(required bool, skip string) []byte {
	env := ""
	if required {
		env = "    env:\n      GC_REQUIRE_ACCEPTANCE_TOOLING: \"1\"\n"
	}
	run := "go test -run 'TestFoo$' ./test/acceptance/"
	if skip != "" {
		run = "go test -run 'TestFoo$' \\\n            -skip '" + skip + "' \\\n            ./test/acceptance/"
	}
	return []byte("jobs:\n" +
		"  probe:\n" +
		env +
		"    steps:\n" +
		"      - name: run\n" +
		"        run: |\n" +
		"          " + run + "\n")
}

func TestSkipAllowlistProblems(t *testing.T) {
	allowlisted := []skipAllowlistEntry{
		{Row: "TestFoo/bar", Reason: "some reason", Bead: "ga-abc12"},
		{Row: "TestFoo/baz", Reason: "some reason", Bead: "ga-abc12"},
	}

	t.Run("unlisted skip in a required job is reported", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo/(bar|baz)")
		problems, err := skipAllowlistProblems(workflow, nil)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 2 {
			t.Fatalf("problems = %v, want exactly 2 (one per unlisted row)", problems)
		}
		for _, want := range []string{"TestFoo/bar", "TestFoo/baz"} {
			if !anyContains(problems, want) {
				t.Errorf("problems %v do not mention %q", problems, want)
			}
		}
	})

	t.Run("skip in a non-required job is ignored", func(t *testing.T) {
		workflow := fixtureWorkflow(false, "TestFoo/(bar|baz)")
		problems, err := skipAllowlistProblems(workflow, nil)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: a non-required job's -skip is not this guard's business", problems)
		}
	})

	t.Run("fully allowlisted skip is clean", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo/(bar|baz)")
		problems, err := skipAllowlistProblems(workflow, allowlisted)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: every deselected row is allowlisted", problems)
		}
	})

	t.Run("stale allowlist entry is reported", func(t *testing.T) {
		// Only "bar" is skipped now; "baz" in the allowlist is stale.
		workflow := fixtureWorkflow(true, "TestFoo/(bar)")
		problems, err := skipAllowlistProblems(workflow, allowlisted)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "TestFoo/baz") || !strings.Contains(problems[0], "stale") {
			t.Fatalf("problems = %v, want exactly one stale-entry problem naming TestFoo/baz", problems)
		}
	})

	t.Run("entry with empty reason or bead is reported even when otherwise matched", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo/(bar)")
		bad := []skipAllowlistEntry{{Row: "TestFoo/bar", Reason: "", Bead: ""}}
		problems, err := skipAllowlistProblems(workflow, bad)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if !anyContains(problems, "empty Reason") || !anyContains(problems, "empty Bead") {
			t.Fatalf("problems = %v, want complaints about both the empty Reason and the empty Bead", problems)
		}
	})

	t.Run("no skip at all is clean with an empty allowlist", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "")
		problems, err := skipAllowlistProblems(workflow, nil)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none", problems)
		}
	})

	t.Run("an unexpandable -skip in a required job errors instead of passing silently", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo.*")
		_, err := skipAllowlistProblems(workflow, nil)
		if err == nil {
			t.Fatal("expected an error for an unexpandable -skip pattern, got nil (this is the silent-pass hole ga-may9k closes)")
		}
		if !strings.Contains(err.Error(), "cannot expand") {
			t.Fatalf("error = %q, want it to mention the pattern could not be expanded", err.Error())
		}
	})

	t.Run("a workflow with no jobs is reported as broken, not vacuously clean", func(t *testing.T) {
		_, err := skipAllowlistProblems([]byte("jobs: {}\n"), nil)
		if err == nil {
			t.Fatal("expected an error for a workflow with no jobs")
		}
	})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func anyContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

package scripts_test

import (
	"strings"
	"testing"
)

// Fixture-based unit tests for parseSkipExpression, splitTopLevelAlternation,
// requiredJobEnv, checkSkipAmbiguity, and skipAllowlistProblems, all defined
// in acceptance_run_selection_test.go. These exercise the parser and the
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
			// LOW: plain Name/row without parentheses is accepted.
			name: "plain subtest form without parens",
			expr: "TestFoo/bar",
			want: []string{"TestFoo/bar"},
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
		{
			// LOW: an empty group is rejected, not silently coerced to the
			// bare-name form.
			name:    "an empty group is rejected",
			expr:    "TestFoo/()",
			wantErr: "cannot expand",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSkipExpressionRows(c.expr)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSkipExpressionRows(%q) = %v, nil; want an error containing %q", c.expr, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("parseSkipExpressionRows(%q) error = %q, want it to contain %q", c.expr, err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSkipExpressionRows(%q) unexpected error: %v", c.expr, err)
			}
			if !equalStringSlices(got, c.want) {
				t.Fatalf("parseSkipExpressionRows(%q) = %v, want %v", c.expr, got, c.want)
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

func TestCheckSkipAmbiguity(t *testing.T) {
	universe := acceptanceTestUniverse{
		TopLevel: []string{"TestProxiedNativeLifecycle", "TestProxiedNativeSafety"},
		Subtests: map[string][]string{
			"TestProxiedNativeLifecycle": {"child-term-zombie", "root-move", "root-move-after-crash"},
		},
	}

	t.Run("an anchored-in-substance single-match clause is clean", func(t *testing.T) {
		if err := checkSkipAmbiguity(skipClause{TestName: "TestProxiedNativeLifecycle"}, universe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("child-term-zombie matches exactly one subtest", func(t *testing.T) {
		clause := skipClause{TestName: "TestProxiedNativeLifecycle", Alts: []string{"child-term-zombie"}}
		if err := checkSkipAmbiguity(clause, universe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("bare top-level name unanchored matches two test functions", func(t *testing.T) {
		// The concrete review finding: "-skip 'TestProxiedNative'" (no
		// subtest group, no anchor) reaches BOTH TestProxiedNativeLifecycle
		// and TestProxiedNativeSafety via go test's unanchored regexp match.
		err := checkSkipAmbiguity(skipClause{TestName: "TestProxiedNative"}, universe)
		if err == nil {
			t.Fatal("expected an ambiguity error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous") ||
			!strings.Contains(err.Error(), "TestProxiedNativeLifecycle") ||
			!strings.Contains(err.Error(), "TestProxiedNativeSafety") {
			t.Fatalf("error = %q, want it to name both matched test functions", err.Error())
		}
	})

	t.Run("a subtest alternative unanchored matches a future sibling subtest", func(t *testing.T) {
		// The review's second concrete example: ".../(root-move)" also
		// matches a future "root-move-after-crash" subtest once one exists.
		clause := skipClause{TestName: "TestProxiedNativeLifecycle", Alts: []string{"root-move"}}
		err := checkSkipAmbiguity(clause, universe)
		if err == nil {
			t.Fatal("expected an ambiguity error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "root-move-after-crash") {
			t.Fatalf("error = %q, want it to name the unintended sibling subtest", err.Error())
		}
	})

	t.Run("a stale or misspelled pattern matches nothing", func(t *testing.T) {
		clause := skipClause{TestName: "TestProxiedNativeLifecycle", Alts: []string{"no-such-subtest"}}
		err := checkSkipAmbiguity(clause, universe)
		if err == nil {
			t.Fatal("expected an error for a pattern matching no known subtest, got nil")
		}
		if !strings.Contains(err.Error(), "matches no known") {
			t.Fatalf("error = %q, want it to say the pattern matches nothing known", err.Error())
		}
	})
}

// fixtureUniverse is the default acceptanceTestUniverse for
// TestSkipAllowlistProblems: one top-level test with two UNambiguous
// subtests, matching the "-skip 'TestFoo/(bar|baz)'" fixture shape used
// throughout. Individual ambiguity-focused subtests build their own.
func fixtureUniverse() acceptanceTestUniverse {
	return acceptanceTestUniverse{
		TopLevel: []string{"TestFoo"},
		Subtests: map[string][]string{"TestFoo": {"bar", "baz"}},
	}
}

// fixtureWorkflow builds a minimal ci.yml-shaped document with one job named
// "probe". required controls whether the job sets
// GC_REQUIRE_ACCEPTANCE_TOOLING; skip, when non-empty, becomes a
// `-skip '<skip>'` argument on the job's one `go test` step, in the one
// canonical spelling.
func fixtureWorkflow(required bool, skip string) []byte {
	run := "go test -run 'TestFoo$' ./test/acceptance/"
	if skip != "" {
		run = "go test -run 'TestFoo$' \\\n            -skip '" + skip + "' \\\n            ./test/acceptance/"
	}
	return fixtureWorkflowRaw(required, run)
}

// fixtureWorkflowRaw is fixtureWorkflow with the run: block's body supplied
// verbatim (each element becomes one line), for constructing the
// unsupported/malformed -skip spellings HIGH's fixtures need.
func fixtureWorkflowRaw(required bool, runLines ...string) []byte {
	env := ""
	if required {
		env = "    env:\n      GC_REQUIRE_ACCEPTANCE_TOOLING: \"1\"\n"
	}
	var run strings.Builder
	for _, line := range runLines {
		run.WriteString("          " + line + "\n")
	}
	return []byte("jobs:\n" +
		"  probe:\n" +
		env +
		"    steps:\n" +
		"      - name: run\n" +
		"        run: |\n" +
		run.String())
}

func TestSkipAllowlistProblems(t *testing.T) {
	allowlisted := []skipAllowlistEntry{
		{Row: "TestFoo/bar", Reason: "some reason", Bead: "ga-abc12"},
		{Row: "TestFoo/baz", Reason: "some reason", Bead: "ga-abc12"},
	}

	t.Run("unlisted skip in a required job is reported", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo/(bar|baz)")
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
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
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: a non-required job's -skip is not this guard's business", problems)
		}
	})

	t.Run("fully allowlisted skip is clean", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo/(bar|baz)")
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlisted, fixtureUniverse())
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
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlisted, fixtureUniverse())
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
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, bad, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if !anyContains(problems, "empty Reason") || !anyContains(problems, "empty Bead") {
			t.Fatalf("problems = %v, want complaints about both the empty Reason and the empty Bead", problems)
		}
	})

	t.Run("no skip at all is clean with an empty allowlist", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "")
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none", problems)
		}
	})

	t.Run("an unexpandable -skip in a required job errors instead of passing silently", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo.*")
		_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
		if err == nil {
			t.Fatal("expected an error for an unexpandable -skip pattern, got nil (this is the silent-pass hole ga-may9k closes)")
		}
		if !strings.Contains(err.Error(), "cannot expand") {
			t.Fatalf("error = %q, want it to mention the pattern could not be expanded", err.Error())
		}
	})

	t.Run("an ambiguous -skip in a required job errors instead of passing silently", func(t *testing.T) {
		workflow := fixtureWorkflow(true, "TestFoo")
		universe := acceptanceTestUniverse{TopLevel: []string{"TestFoo", "TestFooBar"}}
		_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, universe)
		if err == nil {
			t.Fatal("expected an ambiguity error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("error = %q, want it to say the pattern is ambiguous", err.Error())
		}
	})

	t.Run("a workflow with no jobs is reported as broken, not vacuously clean", func(t *testing.T) {
		_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": []byte("jobs: {}\n")}, nil, fixtureUniverse())
		if err == nil {
			t.Fatal("expected an error for a workflow with no jobs")
		}
	})

	t.Run("a row skipped only in nightly.yml is not mistaken for stale by ci.yml's entry", func(t *testing.T) {
		// LOW: multi-workflow scope. ci.yml has no skip at all; nightly.yml
		// has the allowlisted one. Scanning both must merge results before
		// judging staleness, not treat each file's absence as evidence the
		// row is gone.
		workflows := map[string][]byte{
			"ci.yml":      fixtureWorkflow(true, ""),
			"nightly.yml": fixtureWorkflow(true, "TestFoo/(bar|baz)"),
		}
		problems, err := skipAllowlistProblems(workflows, allowlisted, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: both allowlisted rows are skipped, just in a different file", problems)
		}
	})

	// HIGH: every unsupported -skip spelling must hard-error, never pass
	// silently. Each of these reproduces one form the review found passing
	// silently against the single-quoted-only parser.
	t.Run("unsupported -skip spellings all error instead of passing silently", func(t *testing.T) {
		cases := []struct {
			name    string
			run     []string
			wantErr string
		}{
			{
				name:    "equals form with quotes: -skip='...'",
				run:     []string{`go test -run 'TestFoo$' -skip='TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "skip-flag token",
			},
			{
				name:    "equals form, no quotes: -skip=X/a",
				run:     []string{`go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag token",
			},
			{
				name:    `double-quoted: -skip "X/a"`,
				run:     []string{`go test -run 'TestFoo$' -skip "TestFoo/bar" ./test/acceptance/`},
				wantErr: "skip-flag token",
			},
			{
				name:    "unquoted: -skip X/a",
				run:     []string{`go test -run 'TestFoo$' -skip TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag token",
			},
			{
				name:    "fully-qualified flag: -test.skip '...'",
				run:     []string{`go test -run 'TestFoo$' -test.skip 'TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "skip-flag token",
			},
			{
				name:    "double-dash: --skip '...' (functionally identical to -skip, still counted consistently)",
				run:     []string{`go test -run 'TestFoo$' --skip 'TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "",
			},
			{
				name:    "passed after -args: -args -test.skip=...",
				run:     []string{`go test -run 'TestFoo$' ./test/acceptance/ -args -test.skip=TestFoo/bar`},
				wantErr: "skip-flag token",
			},
			{
				name: "unresolvable shell variable: -skip \"$S\"",
				run: []string{
					`S='TestFoo/(bar|baz)'`,
					`go test -run 'TestFoo$' -skip "$S" ./test/acceptance/`,
				},
				wantErr: "skip-flag token",
			},
			{
				name:    "GOFLAGS inline assignment carries -skip",
				run:     []string{`GOFLAGS='-skip=TestFoo/bar' go test -run 'TestFoo$' ./test/acceptance/`},
				wantErr: "GOFLAGS",
			},
			{
				name:    "ACCEPTANCE_GO_TEST_FLAGS into make test-acceptance",
				run:     []string{`ACCEPTANCE_GO_TEST_FLAGS='-skip TestFoo/bar' make test-acceptance`},
				wantErr: "ACCEPTANCE_GO_TEST_FLAGS",
			},
			{
				// Control: the one recognized spelling, split across a
				// backslash-continued line, must NOT error.
				name: "backslash-continued -skip parses as the canonical form",
				run: []string{
					`go test -run 'TestFoo$' \`,
					`  -skip 'TestFoo/(bar|baz)' \`,
					`  ./test/acceptance/`,
				},
				wantErr: "",
			},
			{
				// Control: a comment mentioning "-skip" in prose -- exactly
				// how the real beads-proxied-native-acceptance step
				// documents its own deselection -- must not be counted as a
				// skip-flag token.
				name: "a -skip mentioned in a comment is not counted",
				run: []string{
					`# -skip excludes bar and baz for now, see some-bead`,
					`go test -run 'TestFoo$' \`,
					`  -skip 'TestFoo/(bar|baz)' \`,
					`  ./test/acceptance/`,
				},
				wantErr: "",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				workflow := fixtureWorkflowRaw(true, c.run...)
				_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlisted, fixtureUniverse())
				if c.wantErr == "" {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil (this is the silent-pass hole the review found)", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
				}
			})
		}
	})

	// GOFLAGS/ACCEPTANCE_GO_TEST_FLAGS set via a job or step env: map, not
	// inline in the run text.
	t.Run("GOFLAGS or ACCEPTANCE_GO_TEST_FLAGS set in job env errors", func(t *testing.T) {
		workflow := []byte("jobs:\n" +
			"  probe:\n" +
			"    env:\n" +
			"      GC_REQUIRE_ACCEPTANCE_TOOLING: \"1\"\n" +
			"      GOFLAGS: \"-skip=TestFoo/bar\"\n" +
			"    steps:\n" +
			"      - name: run\n" +
			"        run: go test -run 'TestFoo$' ./test/acceptance/\n")
		_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlisted, fixtureUniverse())
		if err == nil {
			t.Fatal("expected an error for GOFLAGS set in job env, got nil")
		}
		if !strings.Contains(err.Error(), "GOFLAGS") {
			t.Fatalf("error = %q, want it to name GOFLAGS", err.Error())
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

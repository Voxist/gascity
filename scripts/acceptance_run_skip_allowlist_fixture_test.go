package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture-based unit tests for parseSkipExpression, splitTopLevelAlternation,
// requiredJobEnv, checkSkipAmbiguity, and skipAllowlistProblems, all defined
// in acceptance_run_selection_test.go. These exercise the parser and the
// deny-by-default allowlist engine directly, without touching the real
// ci.yml -- the TDD probe ga-may9k asked for, kept as permanent regression
// coverage rather than a one-off manual check.

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
			name: "surrounding whitespace on the whole clause is trimmed",
			expr: "  TestFoo/(bar|baz)  ",
			want: []string{"TestFoo/bar", "TestFoo/baz"},
		},
		{
			// ga-1ebvc LOW: whitespace INSIDE an alternative used to be
			// trimmed here too, but go test's real -skip matching does not
			// trim the regex it builds from each alternative -- a literal
			// " bar " never matches a subtest actually named "bar", so
			// trimming it silently reported a row as skipped that go test
			// itself would never actually deselect. Reject it instead.
			name:    "whitespace inside an alternative is rejected, not trimmed",
			expr:    "TestFoo/( bar | baz )",
			wantErr: "not a plain name",
		},
		{
			// LOW: plain Name/row without parentheses is accepted.
			name: "plain subtest form without parens",
			expr: "TestFoo/bar",
			want: []string{"TestFoo/bar"},
		},
		{
			// The anchoring advice ("anchor it, e.g. ^Name$") must itself
			// parse -- a bare anchored name yields the same bare row.
			name: "an anchored bare test name is accepted, anchors stripped from the row",
			expr: "^TestFoo$",
			want: []string{"TestFoo"},
		},
		{
			name: "an anchored subtest alternative is accepted, anchors stripped from the row",
			expr: "TestFoo/(^bar$|baz)",
			want: []string{"TestFoo/bar", "TestFoo/baz"},
		},
		{
			name: "a fully anchored plain subtest form is accepted",
			expr: "^TestFoo$/^bar$",
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

// TestExtractAndBlankCanonicalSkips is the direct, parser-level regression
// for HIGH round 4's right-boundary fix: skipArgPattern must accept a
// canonical -skip whose closing quote is followed by end-of-text,
// whitespace, or one of `;&|)`, and must REJECT (not match at all) one
// glued to more text via another quote, a shell variable, or an escaped
// pipe -- the four concrete forms review probed. A rejection here means the
// text is left for skipFlagTokenPattern's fallback scan to catch as
// skip-flag-shaped, which TestSkipAllowlistProblems's "every probed bypass"
// table proves end to end; this test isolates the boundary decision itself.
func TestExtractAndBlankCanonicalSkips(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantExprs []string
		wantLeft  string // substring that must survive unblanked, "" to skip the check
	}{
		{
			name:      "canonical, followed by end-of-text",
			text:      "go test -skip 'TestFoo/bar'",
			wantExprs: []string{"TestFoo/bar"},
		},
		{
			name:      "canonical, followed by whitespace",
			text:      "go test -skip 'TestFoo/bar' ./test/acceptance/",
			wantExprs: []string{"TestFoo/bar"},
		},
		{
			name:      "canonical, followed by a semicolon",
			text:      "go test -skip 'TestFoo/bar';echo done",
			wantExprs: []string{"TestFoo/bar"},
			wantLeft:  ";echo done",
		},
		{
			name:      "canonical, followed by a closing paren",
			text:      "if true; then go test -skip 'TestFoo/bar'); fi",
			wantExprs: []string{"TestFoo/bar"},
		},
		{
			// review's glue form 1: two adjacent single-quoted shell
			// strings, which the shell concatenates into one -skip value
			// ("Allowed|TestOther"). The right-boundary fix must refuse to
			// treat "Allowed" as the whole expression.
			name:      "glued: adjacent single-quoted strings",
			text:      "go test -skip 'Allowed''|TestOther'",
			wantExprs: nil,
			wantLeft:  "-skip 'Allowed''|TestOther'",
		},
		{
			// review's glue form 2: a double-quoted continuation.
			name:      "glued: double-quoted continuation",
			text:      `go test -skip 'Allowed'"|TestOther"`,
			wantExprs: nil,
			wantLeft:  `-skip 'Allowed'"|TestOther"`,
		},
		{
			// review's glue form 3: a shell variable reference glued on.
			name:      "glued: shell variable glued on",
			text:      "go test -skip 'Allowed'$EXTRA",
			wantExprs: nil,
			wantLeft:  "-skip 'Allowed'$EXTRA",
		},
		{
			// review's glue form 4: an escaped pipe glued on.
			name:      `glued: escaped pipe glued on`,
			text:      `go test -skip 'Allowed'\|TestOther`,
			wantExprs: nil,
			wantLeft:  `-skip 'Allowed'\|TestOther`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exprs, blanked := extractAndBlankCanonicalSkips(c.text)
			if !equalStringSlices(exprs, c.wantExprs) {
				t.Fatalf("extractAndBlankCanonicalSkips(%q) exprs = %v, want %v", c.text, exprs, c.wantExprs)
			}
			if c.wantLeft != "" && !strings.Contains(blanked, c.wantLeft) {
				t.Fatalf("extractAndBlankCanonicalSkips(%q) blanked = %q, want it to still contain %q", c.text, blanked, c.wantLeft)
			}
			if len(c.wantExprs) > 0 && strings.Contains(blanked, c.wantExprs[0]) {
				t.Fatalf("extractAndBlankCanonicalSkips(%q) blanked = %q, a recognized expression must not survive in the blanked text", c.text, blanked)
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

// TestListAcceptanceWorkflowFiles is ga-1ebvc's LOW: the required-job scan
// used to read exactly ["ci.yml", "nightly.yml"], hardcoded. A required job
// (one setting GC_REQUIRE_ACCEPTANCE_TOOLING) added to any OTHER workflow
// file -- and this repo has several (fork-verify.yml, mac-regression.yml,
// ollama-acceptance-c.yml, rc-gate.yml, review-formulas.yml as of this
// writing) -- was outside this guard's scope no matter what its -skip said.
// listAcceptanceWorkflowFiles replaces the hardcoded pair with a glob of
// every .github/workflows/*.yml and *.yaml file.
func TestListAcceptanceWorkflowFiles(t *testing.T) {
	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir workflows dir: %v", err)
	}
	for _, name := range []string{"ci.yml", "nightly.yml", "extra.yaml", "not-a-workflow.md", "README"} {
		if err := os.WriteFile(filepath.Join(workflowsDir, name), []byte("jobs: {}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := listAcceptanceWorkflowFiles(dir)
	if err != nil {
		t.Fatalf("listAcceptanceWorkflowFiles: %v", err)
	}
	want := []string{"ci.yml", "extra.yaml", "nightly.yml"}
	if !equalStringSlices(got, want) {
		t.Fatalf("listAcceptanceWorkflowFiles = %v, want %v (only *.yml/*.yaml, sorted, no other extensions)", got, want)
	}
}

func TestCheckSkipAmbiguity(t *testing.T) {
	universe := acceptanceTestUniverse{
		TopLevel: []string{"TestProxiedNativeLifecycle", "TestProxiedNativeSafety"},
		Subtests: map[string][]string{
			"TestProxiedNativeLifecycle": {"child-term-zombie", "root-move", "root-move-after-crash"},
			"TestProxiedNativeSafety":    {"no-spawn-control", "no-spawn", "library-no-spawn-control"},
		},
	}

	t.Run("an unambiguous bare top-level clause is clean", func(t *testing.T) {
		clause := skipClause{TestName: "TestProxiedNativeLifecycle", TestNamePattern: "TestProxiedNativeLifecycle"}
		if _, err := checkSkipAmbiguity(clause, universe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("child-term-zombie matches exactly one subtest", func(t *testing.T) {
		clause := skipClause{
			TestName: "TestProxiedNativeLifecycle", TestNamePattern: "TestProxiedNativeLifecycle",
			Alts: []string{"child-term-zombie"}, AltPatterns: []string{"child-term-zombie"},
		}
		if _, err := checkSkipAmbiguity(clause, universe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("bare top-level name unanchored matches two test functions", func(t *testing.T) {
		// The concrete review finding: "-skip 'TestProxiedNative'" (no
		// subtest group, no anchor) reaches BOTH TestProxiedNativeLifecycle
		// and TestProxiedNativeSafety via go test's unanchored regexp match.
		clause := skipClause{TestName: "TestProxiedNative", TestNamePattern: "TestProxiedNative"}
		_, err := checkSkipAmbiguity(clause, universe)
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
		clause := skipClause{
			TestName: "TestProxiedNativeLifecycle", TestNamePattern: "TestProxiedNativeLifecycle",
			Alts: []string{"root-move"}, AltPatterns: []string{"root-move"},
		}
		_, err := checkSkipAmbiguity(clause, universe)
		if err == nil {
			t.Fatal("expected an ambiguity error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "root-move-after-crash") {
			t.Fatalf("error = %q, want it to name the unintended sibling subtest", err.Error())
		}
	})

	t.Run("a stale or misspelled pattern matches nothing", func(t *testing.T) {
		clause := skipClause{
			TestName: "TestProxiedNativeLifecycle", TestNamePattern: "TestProxiedNativeLifecycle",
			Alts: []string{"no-such-subtest"}, AltPatterns: []string{"no-such-subtest"},
		}
		_, err := checkSkipAmbiguity(clause, universe)
		if err == nil {
			t.Fatal("expected an error for a pattern matching no known subtest, got nil")
		}
		if !strings.Contains(err.Error(), "matches no known") || !strings.Contains(err.Error(), acceptanceUniverseScope) {
			t.Fatalf("error = %q, want it to say the pattern matches nothing known and name the scan scope", err.Error())
		}
	})

	t.Run("no-spawn-control is unanchored-ambiguous against its own longer sibling", func(t *testing.T) {
		// "no-spawn-control" is a SUFFIX of "library-no-spawn-control", so
		// the unanchored pattern reaches both.
		clause := skipClause{
			TestName: "TestProxiedNativeSafety", TestNamePattern: "TestProxiedNativeSafety",
			Alts: []string{"no-spawn-control"}, AltPatterns: []string{"no-spawn-control"},
		}
		_, err := checkSkipAmbiguity(clause, universe)
		if err == nil {
			t.Fatal("expected an ambiguity error, got nil")
		}
		if !strings.Contains(err.Error(), "library-no-spawn-control") {
			t.Fatalf("error = %q, want it to name the longer sibling subtest", err.Error())
		}
	})

	t.Run("anchoring no-spawn-control disambiguates it -- the anchoring advice actually works", func(t *testing.T) {
		// LOW: this is what checkPatternMatchesExactlyOne's own "anchor it"
		// advice recommends, and it must actually resolve the case above:
		// anchored, "no-spawn-control" only exact-matches itself.
		clause := skipClause{
			TestName: "TestProxiedNativeSafety", TestNamePattern: "TestProxiedNativeSafety",
			Alts: []string{"no-spawn-control"}, AltPatterns: []string{"^no-spawn-control$"},
		}
		if _, err := checkSkipAmbiguity(clause, universe); err != nil {
			t.Fatalf("anchoring should have disambiguated this pattern, got: %v", err)
		}
	})

	t.Run("a clause with alternatives still checks its own top-level fragment", func(t *testing.T) {
		// LOW: even when a clause scopes to a subtest, its own top-level
		// fragment is unanchored-matched too -- a future
		// TestProxiedNativeLifecycleV2 would make this ambiguous despite the
		// subtest alternative itself being unambiguous.
		universeWithFutureSibling := acceptanceTestUniverse{
			TopLevel: []string{"TestProxiedNativeLifecycle", "TestProxiedNativeLifecycleV2", "TestProxiedNativeSafety"},
			Subtests: universe.Subtests,
		}
		clause := skipClause{
			TestName: "TestProxiedNativeLifecycle", TestNamePattern: "TestProxiedNativeLifecycle",
			Alts: []string{"child-term-zombie"}, AltPatterns: []string{"child-term-zombie"},
		}
		_, err := checkSkipAmbiguity(clause, universeWithFutureSibling)
		if err == nil {
			t.Fatal("expected an ambiguity error for the top-level fragment, got nil")
		}
		if !strings.Contains(err.Error(), "TestProxiedNativeLifecycleV2") {
			t.Fatalf("error = %q, want it to name the future sibling top-level test", err.Error())
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

	t.Run("an unanchored prefix is tracked under the row it actually matches, not the literal text", func(t *testing.T) {
		// ga-1ebvc MEDIUM/LOW: the real subtest is "barbaz"; the -skip
		// pattern "bar" is an unanchored PREFIX that unambiguously matches
		// it via substring (checkPatternMatchesExactlyOne finds exactly one
		// candidate, so this is NOT the ambiguity case). go test's own
		// -skip matching is the same unanchored regexp match, so at
		// runtime "bar" really does deselect "barbaz" too. The allowlist
		// entry below is written against "TestFoo/barbaz" -- the row
		// actually skipped -- and must be recognized as covering it, not
		// reported as unlisted (recording "TestFoo/bar", the literal text,
		// instead of the matched candidate) or as stale (nothing recorded
		// under "TestFoo/barbaz").
		universe := acceptanceTestUniverse{
			TopLevel: []string{"TestFoo"},
			Subtests: map[string][]string{"TestFoo": {"barbaz"}},
		}
		allowlistForBarbaz := []skipAllowlistEntry{
			{Row: "TestFoo/barbaz", Reason: "some reason", Bead: "ga-abc12"},
		}
		workflow := fixtureWorkflow(true, "TestFoo/(bar)")
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlistForBarbaz, universe)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none: the allowlist entry for TestFoo/barbaz (the row \"bar\" actually "+
				"matches) should cover this skip", problems)
		}
	})

	t.Run("a required switch set via GITHUB_ENV still brings the job into scope", func(t *testing.T) {
		// ga-1ebvc LOW: the job's YAML has no env: block naming
		// GC_REQUIRE_ACCEPTANCE_TOOLING at all -- it sets the switch by
		// writing to $GITHUB_ENV at runtime, a common Actions idiom for a
		// LATER step (or job, via outputs) to pick up. requiredJobEnv only
		// reads the YAML env: maps, so a job that becomes required only
		// this way used to be invisible to this guard entirely -- its
		// -skip, however written, was never scanned or required to be
		// allowlisted.
		workflow := fixtureWorkflowRaw(false,
			`echo "GC_REQUIRE_ACCEPTANCE_TOOLING=1" >> "$GITHUB_ENV"`,
			`go test -run 'TestFoo$' -skip 'TestFoo/bar' ./test/acceptance/`,
		)
		problems, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "TestFoo/bar") {
			t.Fatalf("problems = %v, want exactly one problem naming the unlisted TestFoo/bar skip -- "+
				"the GITHUB_ENV-set required switch must still bring this job into scope", problems)
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

	// HIGH round 2: deny-by-default. Every one of these is a bypass the
	// review probed against round 1's spelling-enumeration parser; each must
	// still hard-error under the new "remove the canonical form, anything
	// skip-flag-shaped left over is an error" design. Two decoys and two
	// controls prove the design does not also over-trigger.
	t.Run("every probed bypass still errors instead of passing silently", func(t *testing.T) {
		cases := []struct {
			name    string
			run     []string
			wantErr string
		}{
			{
				name:    "the flag name itself quoted: '-skip' 'X'",
				run:     []string{`go test -run 'TestFoo$' '-skip' 'TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    `double-quoted flag and value: "-skip" "X"`,
				run:     []string{`go test -run 'TestFoo$' "-skip" "TestFoo/(bar|baz)" ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    `an elided empty string glued to the flag: -skip"" X`,
				run:     []string{`go test -run 'TestFoo$' -skip"" TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name: "a shell variable holding the flag: ARGS=-skip=X; go test $ARGS",
				run: []string{
					`ARGS=-skip=TestFoo/bar`,
					`go test -run 'TestFoo$' $ARGS ./test/acceptance/`,
				},
				wantErr: "skip-flag-shaped token",
			},
			{
				name: "a bash array holding the flag",
				run: []string{
					`args=(-skip=TestFoo/bar)`,
					`go test -run 'TestFoo$' "${args[@]}" ./test/acceptance/`,
				},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    "GOFLAGS appended to with +=",
				run:     []string{`export GOFLAGS+='-skip=TestFoo/bar'`, `go test -run 'TestFoo$' ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    "an unquoted, equals-form value: -skip=X/a",
				run:     []string{`go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    "fully-qualified flag: -test.skip '...'",
				run:     []string{`go test -run 'TestFoo$' -test.skip 'TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// go test treats -skip and --skip identically, but
				// skipArgPattern's left boundary (added this round to stop
				// a longer flag like "--not-skip 'X'" from being read as
				// canonical) means it only recognizes the single-hyphen
				// spelling: --skip is "not the one form" too, and must be
				// rewritten rather than silently accepted.
				name:    "double-dash --skip '...' is refused; the one spelling is single-hyphen -skip",
				run:     []string{`go test -run 'TestFoo$' --skip 'TestFoo/(bar|baz)' ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name:    "passed after -args: -args -test.skip=...",
				run:     []string{`go test -run 'TestFoo$' ./test/acceptance/ -args -test.skip=TestFoo/bar`},
				wantErr: "skip-flag-shaped token",
			},
			{
				name: "an unresolvable shell variable: -skip \"$S\"",
				run: []string{
					`S='TestFoo/(bar|baz)'`,
					`go test -run 'TestFoo$' -skip "$S" ./test/acceptance/`,
				},
				wantErr: "skip-flag-shaped token",
			},
			{
				// A comment containing a lone '#' preceded by a quote, not
				// whitespace, must NOT swallow the real -skip after it.
				name:    "echo '#'; go test -skip=X -- the '#' is quoted, not a comment start",
				run:     []string{`echo '#'; go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// $# (bash's positional-parameter count) must not be
				// mistaken for a comment start either; this line has no
				// -skip at all, so it must NOT error.
				name:    "echo $# -- not a comment start, and no -skip present",
				run:     []string{`echo $#; go test -run 'TestFoo$' ./test/acceptance/`},
				wantErr: "",
			},
			{
				// HIGH round 4: the concrete false match review found in the
				// previous (word-start) comment rule. A '#' preceded by
				// whitespace INSIDE a double-quoted string is not a shell
				// comment; the old rule fired on "step #1" anyway and
				// truncated the rest of the line, silently dropping the
				// real -skip=X that followed. The full-line-only rule must
				// leave this whole line alone, so the real -skip is still
				// visible (and still hard-errors, since -skip=X is not the
				// canonical single-quoted form).
				name:    `echo "step #1"; go test -skip=X -- a quoted '#' must not swallow the rest of the line`,
				run:     []string{`echo "step #1"; go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// Control: ci.yml has this exact shape today (an "## " lead-in
				// inside a double-quoted echo). It has no -skip on the line at
				// all, so the full-line-only rule -- which does not touch this
				// line either way -- must not error.
				name:    `echo "## Section banner" -- the real shape already in ci.yml, no -skip present`,
				run:     []string{`echo "## Section banner"`, `go test -run 'TestFoo$' ./test/acceptance/`},
				wantErr: "",
			},
			{
				// ga-1ebvc MEDIUM: the full-line-only rule from the round
				// above still had no memory of an open quote spanning
				// MULTIPLE lines. `true "` opens a double-quoted string on
				// line 1 that is not closed until the `"` on line 2 --
				// line 2's leading '#' is literal string content, not a
				// comment start, but the previous rule had no way to know
				// that and deleted the WHOLE line, including the real
				// -skip=X sitting right after the closing quote, with no
				// trace left for the fallback scan.
				name: `a '#' that begins inside a double quote opened on a PRIOR line is not a comment start`,
				run: []string{
					`true "`,
					`# " ; go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`,
				},
				wantErr: "skip-flag-shaped token",
			},
			{
				// Control: the same shape, but the quote from line 1 IS
				// closed before line 2 -- line 2's '#' is a real full-line
				// comment and must still be blanked exactly as before.
				name: `a '#' after a quote that closes on the SAME line is still a real comment`,
				run: []string{
					`true "closed"`,
					`# -skip 'TestFoo/bar' mentioned in prose only, not real code`,
					`go test -run 'TestFoo$' ./test/acceptance/`,
				},
				wantErr: "",
			},
			{
				// ga-1ebvc MEDIUM, heredoc form: a heredoc body line
				// starting with '#' (a shebang is the realistic case) is
				// literal body content, not a comment, and must not be
				// blanked -- though in this fixture that only matters if a
				// -skip were hiding there. Here it proves the heredoc's
				// own end delimiter is still recognized (the line AFTER
				// the heredoc body must be scanned normally): the real
				// -skip=X after the heredoc must still be caught.
				name: `a heredoc body line starting with '#' is not a comment, and the end delimiter still ends it`,
				run: []string{
					`cat <<'EOF' > script.sh`,
					`#!/bin/bash`,
					`EOF`,
					`go test -run 'TestFoo$' -skip=TestFoo/bar ./test/acceptance/`,
				},
				wantErr: "skip-flag-shaped token",
			},
			{
				// HIGH round 4, glue form 1: adjacent single-quoted strings
				// the shell concatenates into one value. Must not be read
				// as a clean "Allowed" expression with the rest ignored.
				name:    "glued: -skip 'Allowed''|TestOther' (adjacent single quotes)",
				run:     []string{`go test -run 'TestFoo$' -skip 'Allowed''|TestOther' ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// HIGH round 4, glue form 2: a double-quoted continuation.
				name:    `glued: -skip 'Allowed'"|TestOther" (double-quoted continuation)`,
				run:     []string{`go test -run 'TestFoo$' -skip 'Allowed'"|TestOther" ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// HIGH round 4, glue form 3: a shell variable glued on.
				name:    "glued: -skip 'Allowed'$EXTRA (shell variable glued on)",
				run:     []string{`go test -run 'TestFoo$' -skip 'Allowed'$EXTRA ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// HIGH round 4, glue form 4: an escaped pipe glued on.
				name:    `glued: -skip 'Allowed'\|TestOther (escaped pipe glued on)`,
				run:     []string{`go test -run 'TestFoo$' -skip 'Allowed'\|TestOther ./test/acceptance/`},
				wantErr: "skip-flag-shaped token",
			},
			{
				// Decoy: an unrelated flag that merely starts with "skip-"
				// must stay clear.
				name:    "decoy: --skip-foo is not the skip flag",
				run:     []string{`go test -run 'TestFoo$' --skip-foo=1 ./test/acceptance/`},
				wantErr: "",
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
				name: "a -skip mentioned in a real trailing comment is not counted",
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

	// HIGH round 2: an env value under ANY key name, not just GOFLAGS/
	// ACCEPTANCE_GO_TEST_FLAGS, is scanned.
	t.Run("an env value under any key name errors", func(t *testing.T) {
		cases := []struct {
			name   string
			envKey string
		}{
			{name: "GOFLAGS", envKey: "GOFLAGS"},
			{name: "ACCEPTANCE_GO_TEST_FLAGS into make test-acceptance (Makefile:906/921)", envKey: "ACCEPTANCE_GO_TEST_FLAGS"},
			{name: "an arbitrary key name the reviewer's HIGH named", envKey: "EXTRA"},
			{name: "TEST_FLAGS threaded through as $TEST_FLAGS", envKey: "TEST_FLAGS"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				workflow := []byte("jobs:\n" +
					"  probe:\n" +
					"    env:\n" +
					"      GC_REQUIRE_ACCEPTANCE_TOOLING: \"1\"\n" +
					"      " + c.envKey + ": \"-skip=TestFoo/bar\"\n" +
					"    steps:\n" +
					"      - name: run\n" +
					"        run: go test -run 'TestFoo$' ./test/acceptance/\n")
				_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, allowlisted, fixtureUniverse())
				if err == nil {
					t.Fatalf("expected an error for %s set in job env, got nil", c.envKey)
				}
				if !strings.Contains(err.Error(), c.envKey) {
					t.Fatalf("error = %q, want it to name %s", err.Error(), c.envKey)
				}
			})
		}
	})

	// Decoy: an env var whose NAME merely contains "skip" but whose VALUE
	// does not must stay clear -- only the value is scanned.
	t.Run("decoy: an env key named SKIP_X with an unrelated value stays clear", func(t *testing.T) {
		workflow := []byte("jobs:\n" +
			"  probe:\n" +
			"    env:\n" +
			"      GC_REQUIRE_ACCEPTANCE_TOOLING: \"1\"\n" +
			"      SKIP_X: \"hello\"\n" +
			"    steps:\n" +
			"      - name: run\n" +
			"        run: go test -run 'TestFoo$' ./test/acceptance/\n")
		_, err := skipAllowlistProblems(map[string][]byte{"ci.yml": workflow}, nil, fixtureUniverse())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
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

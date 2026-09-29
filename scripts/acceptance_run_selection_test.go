package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The proxied acceptance files are gated twice: by a build tag, and by the
// -run expression of whichever CI step selects them. The tag is easy to get
// right and easy to check; the selector is neither, and getting it wrong is
// silent in the worst possible way.
//
// It happened. TestProxiedNativeLifecycle (695 lines) and
// TestProxiedNativeSafety (550 lines) carried //go:build acceptance_a, sat in
// a directory the beads_topology path filter matches, and were named by no
// -run expression in any job — so the proxied-native lane's entire evidence
// base ran exactly once, on the author's box: the per-crash-shape ping and
// recover budgets, foreign-root's "0 pings, 0 dolt stop", the no-spawn
// positive control, both no-migrate rows (the BD_ALLOW_REMOTE_MIGRATE consent
// fence) and the author-at-commit pin. A regression in any of them would have
// landed green, and the two files would read as gates forever (council pr2
// C-F1).
//
// This is the guard for that. It lives in ./scripts rather than in ci.yml
// because ./scripts is inside UNIT_COVER_PKGS_NONCMDGC, which CI already runs
// as "Preflight / unit cover (noncmdgc)" — so it is enforced with no workflow
// edit and no shape-hash bump, the same route
// scripts/check_split_topology_rows_test.go established.
//
// It deliberately does NOT try to evaluate Go's -run grammar. It asserts the
// weaker, checkable thing: the function's name appears somewhere in a CI
// `go test` step's -run expression. A selector that names a function but
// cannot match it is a different bug, and one a green CI run makes visible;
// a selector that never mentions the function at all is invisible forever.
func TestProxiedAcceptanceFunctionsAreSelectedByCI(t *testing.T) {
	root := repoRoot(t)

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	runExpressions := strings.Join(collectRunExpressions(string(workflow)), "\n")
	if runExpressions == "" {
		t.Fatal("ci.yml contains no `go test -run` expressions at all; this guard would pass vacuously")
	}

	matches, err := filepath.Glob(filepath.Join(root, "test", "acceptance", "beads_proxied_*_test.go"))
	if err != nil {
		t.Fatalf("glob the proxied acceptance files: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no test/acceptance/beads_proxied_*_test.go files found; the guard has nothing to guard")
	}

	found := 0
	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, name := range topLevelTestFunctions(string(body)) {
			found++
			if !strings.Contains(runExpressions, name) {
				t.Errorf("%s: %s is named by no `go test -run` expression in ci.yml, so it runs in no job.\n"+
					"A build tag is not a gate: add the function to an existing step's -run, or give it one.\n"+
					"-run expressions currently in ci.yml:\n%s",
					filepath.Base(path), name, runExpressions)
			}
		}
	}
	if found == 0 {
		t.Fatal("the proxied acceptance files declare no top-level test functions; the scan is broken")
	}
}

// runPattern matches the body of a `-run '<expr>'` argument. CI writes every
// one of them single-quoted, which is what keeps this a text scan rather than
// a shell parser.
var runPattern = regexp.MustCompile(`-run\s+'([^']*)'`)

func collectRunExpressions(workflow string) []string {
	var out []string
	for _, match := range runPattern.FindAllStringSubmatch(workflow, -1) {
		out = append(out, match[1])
	}
	return out
}

// testFuncPattern matches a top-level Go test function declaration. The anchor
// is the line start, so a method or a nested closure cannot be mistaken for
// one.
var testFuncPattern = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(t \*testing\.T\)`)

func topLevelTestFunctions(body string) []string {
	var out []string
	for _, match := range testFuncPattern.FindAllStringSubmatch(body, -1) {
		out = append(out, match[1])
	}
	return out
}

// acceptanceWorkflowDoc is the part of a workflow the env-gate guard reads.
type acceptanceWorkflowDoc struct {
	Env  map[string]any `yaml:"env"`
	Jobs map[string]struct {
		Env   map[string]any `yaml:"env"`
		Steps []struct {
			Env map[string]any `yaml:"env"`
			Run string         `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestAcceptancePerfGateHasALane is round3 review (completeness).
//
// TestBeadsProxiedDefault gates the proxied-native lane's `gc status --json`
// wall clock only when GC_ACCEPTANCE_PERF is set, because wall clock on a
// shared PR runner is a statement about the runner. The plan leaves the number
// to "the GC_ACCEPTANCE_PERF nightly lane" — and nothing anywhere set the
// variable: no workflow, no Makefile target, and `make test-acceptance` runs
// under `env -i`, which dropped it even when a developer exported it. So a flag-on
// `gc status` ten times slower passed every job, nightly included.
//
// This is the same shape of guard as TestProxiedAcceptanceFunctionsAreSelectedByCI,
// one level finer: an env gate is a gate only if some job both sets it and
// selects the test that reads it.
func TestAcceptancePerfGateHasALane(t *testing.T) {
	const (
		gate     = "GC_ACCEPTANCE_PERF"
		testName = "TestBeadsProxiedDefault"
	)
	root := repoRoot(t)
	set := func(values ...map[string]any) bool {
		for _, env := range values {
			if value, ok := env[gate]; ok && strings.TrimSpace(fmt.Sprint(value)) != "" {
				return true
			}
		}
		return false
	}

	var lanes []string
	for _, workflow := range []string{"ci.yml", "nightly.yml"} {
		body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", workflow)) //nolint:gosec // a fixed path inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		var doc acceptanceWorkflowDoc
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parse %s: %v", workflow, err)
		}
		if len(doc.Jobs) == 0 {
			t.Fatalf("%s declares no jobs; the scan is broken", workflow)
		}
		for name, job := range doc.Jobs {
			for _, step := range job.Steps {
				if !set(doc.Env, job.Env, step.Env) {
					continue
				}
				for _, expr := range collectRunExpressions(step.Run) {
					if strings.Contains(expr, testName) {
						lanes = append(lanes, workflow+":"+name)
					}
				}
			}
		}
	}
	if len(lanes) == 0 {
		t.Errorf("no workflow job sets %s and runs %s, so the proxied-native `gc status` wall-clock gate "+
			"(assertProxiedNativePerfHeadroom) is enforced nowhere: add the variable to the job that is "+
			"meant to be its lane", gate, testName)
	}

	// And the local seam: TEST_ENV is `env -i`, so a variable the recipe does
	// not name never reaches `go test`.
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := ""
	lines := strings.Split(string(makefile), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "test-acceptance:") && i+1 < len(lines) {
			recipe = lines[i+1]
			break
		}
	}
	if recipe == "" {
		t.Fatal("the Makefile has no test-acceptance recipe; the scan is broken")
	}
	if !strings.Contains(recipe, gate+"=") {
		t.Errorf("`make test-acceptance` does not pass %s through its env -i allowlist, so exporting it "+
			"runs the suite with the perf gate silently off:\n%s", gate, recipe)
	}
}

// inRowSkipPattern matches a call that skips a test from inside it: t.Skip,
// t.Skipf, t.SkipNow on any receiver. A line whose code is commented out does
// not count.
var inRowSkipPattern = regexp.MustCompile(`\.Skip(f|Now)?\(`)

// TestProxiedAcceptanceRowsNeverSkipInRow is round4's missed completeness low.
//
// Every function in these files runs in a job that sets
// GC_REQUIRE_ACCEPTANCE_TOOLING — Beads / proxied-native acceptance is a
// required check — and that switch is what turns a missing precondition into
// a failure. An in-row t.Skip bypasses it: no-migrate-behind-ignored (the only
// acceptance proof that an exported BD_ALLOW_REMOTE_MIGRATE never reaches the
// library on the ignored lane) and dead-record-one-ping each carried one, and
// on a bd that produced their skip condition the job reported success with the
// row unrun, visible only in a step summary nothing gates on. A row whose
// precondition is missing calls helpers.MissingPrecondition (or
// MissingTooling), which skips locally and fails under the switch.
func TestProxiedAcceptanceRowsNeverSkipInRow(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "test", "acceptance", "beads_proxied_*_test.go"))
	if err != nil {
		t.Fatalf("glob the proxied acceptance files: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no test/acceptance/beads_proxied_*_test.go files found; the guard has nothing to guard")
	}
	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if inRowSkipPattern.MatchString(code) {
				t.Errorf("%s:%d skips from inside a row that runs in a required job under GC_REQUIRE_ACCEPTANCE_TOOLING:\n\t%s\n"+
					"call helpers.MissingPrecondition instead, so the job fails rather than passing with the row unrun",
					filepath.Base(path), n+1, strings.TrimSpace(line))
			}
		}
	}
}

// skipAllowlist is TestRequiredJobSkipsAreAllowlisted's ledger of every
// acceptance row a required ci.yml job's `go test -skip` is allowed to
// deselect. An in-row t.Skip is closed by TestProxiedAcceptanceRowsNeverSkipInRow
// above; this is the CI-selection door beside it (ga-may9k) -- a `-skip`
// deselects a row invisibly to both that guard and
// TestProxiedAcceptanceFunctionsAreSelectedByCI, which only checks that the
// function's name appears SOMEWHERE in a `-run` expression, never that
// `-skip` does not immediately take rows back out.
//
// Every entry needs a Reason (why the row cannot run today) and a Bead (the
// tracker that removes the entry when it can). TestRequiredJobSkipsAreAllowlisted
// fails on an unlisted deselection, a stale entry (nothing skips it anymore),
// or an entry missing either field.
var skipAllowlist = []skipAllowlistEntry{
	{
		Row:    "TestProxiedNativeLifecycle/child-term-zombie",
		Reason: "post-SIGTERM child-respawn recovery depends on a proxy-subsystem rewrite (internal/storage/dbproxy/proxy/) upstream shipped after this fork's bd pin, which no fork bd release carries yet",
		Bead:   "ga-w9xm9",
	},
	{
		Row:    "TestProxiedNativeLifecycle/root-move",
		Reason: "post-root-move recovery depends on the same proxy-subsystem rewrite as child-term-zombie",
		Bead:   "ga-w9xm9",
	},
}

// skipAllowlistEntry is one accepted `go test -skip` deselection.
type skipAllowlistEntry struct {
	Row    string // "TestName" or "TestName/subtest", exactly as -skip expands
	Reason string // why the row cannot run today
	Bead   string // the tracker that removes this entry when the row comes back
}

// skippedRow is one row a required job's `-skip` deselects.
type skippedRow struct {
	Row string
	Job string
}

// skipArgPattern matches the body of the ONE canonical `-skip '<expr>'`
// spelling this parser accepts: single-quoted, the same convention runPattern
// relies on for `-run`, with a left boundary that excludes it matching mid-
// identifier (an "...x-skip" tail cannot masquerade as the flag). go test's
// actual -skip flag also accepts --skip, -skip=X, -skip "X", an unquoted
// value, -test.skip/--test.skip, and reads GOFLAGS -- none of those are
// parsed here. Rather than enumerate and parse every one of them,
// requiredJobSkipsFromDoc DENIES BY DEFAULT: it removes every canonical
// occurrence this pattern finds, then hard-errors if anything matching
// skipFlagTokenPattern remains anywhere in the run script or any env value in
// scope. Unlike a missed `-run` (which a green CI run makes visible by never
// running), a missed `-skip` fails OPEN: the row runs -- or a shared resource
// the allowlist assumed was staying off does not.
var skipArgPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])-skip\s+'([^']*)'`)

// skipFlagTokenPattern finds anything that LOOKS like a go test skip flag, in
// any spelling and quoting: -skip/--skip/-test.skip/--test.skip, with or
// without '=', any quote style or none. The left boundary excludes identifier
// characters (so "x-skip" or "--skip-foo" cannot trigger it) as does the
// right boundary except '-' itself is excluded from the "clear" set (so
// "--skip-foo", a hypothetical unrelated flag, and "SKIP_X" as a bare word
// both stay clear, while "-skip=", "-skip'", "-skip\"", "-skip " all match).
// Deliberately wider than a real shell's tokenization -- it only ever
// OVER-detects, never under-detects, which is the fail-closed direction this
// guard wants.
var skipFlagTokenPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])--?(test\.)?skip([^A-Za-z0-9_-]|$)`)

// backslashContinuationPattern matches a shell line continuation, so a
// `-skip` split across a `\`-continued line for readability parses the same
// as one written on a single line.
var backslashContinuationPattern = regexp.MustCompile(`\\[ \t]*\n[ \t]*`)

// joinBackslashContinuations collapses `cmd \<newline>  arg` into `cmd arg`
// -- the shell's own continuation semantics -- before either pattern above
// sees the text.
func joinBackslashContinuations(s string) string {
	return backslashContinuationPattern.ReplaceAllString(s, " ")
}

// shellCommentStartPattern finds a '#' that starts a shell comment: at the
// beginning of a line, or immediately after whitespace -- the shell's own
// "comments begin a word" rule. This deliberately does not track quoting (an
// actually-quoted `"a # b"` still has its '#' preceded by a space and so is
// still treated as commented-out here), but it DOES fix the two concrete
// false matches review found with a naive first-'#'-on-the-line rule:
// `echo '#'; go test -skip 'X'` (the '#' is preceded by a quote, not
// whitespace, so it does not start a comment -- and must not swallow the
// real -skip after it) and `echo $#` (the '#' is the second character of the
// $# parameter, preceded by '$', not whitespace).
var shellCommentStartPattern = regexp.MustCompile(`(^|\s)#.*$`)

// stripShellComments removes shell comments by shellCommentStartPattern's
// word-start rule, so a comment that MENTIONS "-skip" in prose (exactly how
// the real beads-proxied-native-acceptance step documents its own
// deselection: "# -skip excludes child-term-zombie and root-move: ...") is
// not counted as a skip-flag token, while a '#' that is not actually a
// comment start does not swallow real content after it. Applied before
// joinBackslashContinuations: a real shell comment absorbs any trailing '\'
// too, so line-joining must not cross one.
func stripShellComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = shellCommentStartPattern.ReplaceAllString(line, "$1")
	}
	return strings.Join(lines, "\n")
}

// extractAndBlankCanonicalSkips finds every canonical -skip '<expr>'
// occurrence skipArgPattern recognizes, returning the expressions in
// left-to-right order and TEXT with those occurrences replaced (boundary
// character kept, the rest blanked) so a subsequent scan for anything
// skip-flag-shaped does not re-flag what this parser already accounted for.
func extractAndBlankCanonicalSkips(text string) (exprs []string, blanked string) {
	var b strings.Builder
	last := 0
	for _, m := range skipArgPattern.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:m[0]])
		if m[2] >= 0 {
			b.WriteString(text[m[2]:m[3]]) // keep the boundary character
		}
		b.WriteByte(' ')
		exprs = append(exprs, text[m[4]:m[5]])
		last = m[1]
	}
	b.WriteString(text[last:])
	return exprs, b.String()
}

// findSkipFlagToken reports a short window of text around the first
// skip-flag-shaped token remaining in s, if any -- enough to identify it in
// an error message without dumping the whole run script.
func findSkipFlagToken(s string) (window string, found bool) {
	loc := skipFlagTokenPattern.FindStringIndex(s)
	if loc == nil {
		return "", false
	}
	start, end := loc[0]-20, loc[1]+20
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	return strings.TrimSpace(s[start:end]), true
}

// bareOrAnchoredTestName and bareOrAnchoredSubtest are the fragment grammars
// this parser accepts, each optionally wrapped in a literal '^'/'$' pair --
// go test -skip matches each '/'-separated fragment as an independent,
// UNANCHORED regexp.MatchString, so anchoring a fragment is the real,
// working way to make it an exact match instead of a substring match. Without
// this, the parser's own ambiguity advice ("anchor it") named a form the
// parser could not then parse -- checkSkipAmbiguity's error message
// literally could not be followed.
const (
	bareOrAnchoredTestName = `\^?Test[A-Za-z0-9_]*\$?`
	bareOrAnchoredSubtest  = `\^?[A-Za-z0-9_-]+\$?`
)

// bareTestNamePattern matches a -skip clause naming a whole top-level test
// with no subtest scoping: "TestFoo", "^TestFoo$", ...
var bareTestNamePattern = regexp.MustCompile(`^(` + bareOrAnchoredTestName + `)$`)

// plainSubtestPattern matches a -skip clause naming one subtest without the
// parenthesized-alternation form: "TestFoo/bar", "TestFoo/^bar$", ...
var plainSubtestPattern = regexp.MustCompile(`^(` + bareOrAnchoredTestName + `)/(` + bareOrAnchoredSubtest + `)$`)

// groupedSubtestPattern matches a test name followed by a parenthesized
// `|`-separated alternation of subtest names -- go test -skip's
// `A/(b|c)` idiom. The group must be non-empty ("TestFoo/()" does not
// match); each alternative is validated against simpleSubtestNamePattern
// below, so metacharacters, nested groups, and character classes are
// rejected there rather than silently accepted, per the same "checkable, not
// a full regex evaluator" stance TestProxiedAcceptanceFunctionsAreSelectedByCI
// takes for `-run`.
var groupedSubtestPattern = regexp.MustCompile(`^(` + bareOrAnchoredTestName + `)/\((.+)\)$`)

// simpleSubtestNamePattern is what a single alternative inside the `(a|b|c)`
// group must look like to count as an explicit row name rather than more
// regex syntax this parser does not evaluate.
var simpleSubtestNamePattern = regexp.MustCompile(`^` + bareOrAnchoredSubtest + `$`)

// splitAnchors separates a fragment's bare name (anchors stripped, for the
// row/allowlist key -- an allowlist entry never spells "^root-move$") from
// the fragment exactly as written (anchors kept, for
// checkSkipAmbiguity's regexp compilation -- the anchors are the entire
// reason a fragment can disambiguate itself from a longer sibling).
func splitAnchors(fragment string) (bareName, pattern string) {
	pattern = fragment
	bareName = strings.TrimSuffix(strings.TrimPrefix(fragment, "^"), "$")
	return bareName, pattern
}

// skipClause is one `|`-separated piece of a -skip expression, carrying both
// the explicit row names it deselects (TestName/Alts, via rows()) and the
// fragment PATTERNS checkSkipAmbiguity compiles to check whether they reach
// further than that literal expansion claims: go test -skip matches each
// `/`-separated fragment via an UNANCHORED regexp.MatchString, not an
// exact-name comparison, so "TestProxiedNative" (no anchor, no subtest group)
// matches both TestProxiedNativeLifecycle and TestProxiedNativeSafety.
type skipClause struct {
	TestName        string   // the top-level test function name, anchors stripped
	TestNamePattern string   // the top-level fragment exactly as written, anchors kept
	Alts            []string // subtest name fragments, anchors stripped; nil means the clause targets the whole top-level test
	AltPatterns     []string // each Alts entry exactly as written, anchors kept
}

// rows returns the explicit row names this clause's LITERAL expansion
// deselects -- not accounting for the unanchored-matching ambiguity
// checkSkipAmbiguity checks separately.
func (c skipClause) rows() []string {
	if len(c.Alts) == 0 {
		return []string{c.TestName}
	}
	rows := make([]string, len(c.Alts))
	for i, alt := range c.Alts {
		rows[i] = c.TestName + "/" + alt
	}
	return rows
}

// parseSkipExpression expands a `go test -skip` regular expression into the
// clauses it names, or reports an error when a clause is not one of the
// shapes this parser understands. A `-skip` expression may itself be several
// clauses joined by top-level `|` (e.g. "TestA|TestB/(x|y)"); a `|` nested
// inside a clause's own `(...)` group is a subtest alternation, not a clause
// separator, so splitting happens at paren depth 0 only.
func parseSkipExpression(expr string) ([]skipClause, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty -skip expression")
	}
	var clauses []skipClause
	for _, raw := range splitTopLevelAlternation(expr) {
		clause := strings.TrimSpace(raw)
		if m := bareTestNamePattern.FindStringSubmatch(clause); m != nil {
			bare, pattern := splitAnchors(m[1])
			clauses = append(clauses, skipClause{TestName: bare, TestNamePattern: pattern})
			continue
		}
		if m := plainSubtestPattern.FindStringSubmatch(clause); m != nil {
			testBare, testPattern := splitAnchors(m[1])
			altBare, altPattern := splitAnchors(m[2])
			clauses = append(clauses, skipClause{
				TestName: testBare, TestNamePattern: testPattern,
				Alts: []string{altBare}, AltPatterns: []string{altPattern},
			})
			continue
		}
		if m := groupedSubtestPattern.FindStringSubmatch(clause); m != nil {
			testBare, testPattern := splitAnchors(m[1])
			alternation := m[2]
			var altBares, altPatterns []string
			for _, alt := range strings.Split(alternation, "|") {
				alt = strings.TrimSpace(alt)
				if !simpleSubtestNamePattern.MatchString(alt) {
					return nil, fmt.Errorf("cannot expand -skip clause %q: subtest alternative %q is not a plain name", clause, alt)
				}
				b, p := splitAnchors(alt)
				altBares = append(altBares, b)
				altPatterns = append(altPatterns, p)
			}
			clauses = append(clauses, skipClause{
				TestName: testBare, TestNamePattern: testPattern,
				Alts: altBares, AltPatterns: altPatterns,
			})
			continue
		}
		return nil, fmt.Errorf("cannot expand -skip clause %q into explicit row names "+
			"(supported shapes: TestName, TestName/subtest, or TestName/(alt1|alt2|...), each optionally wrapped in ^...$)", clause)
	}
	return clauses, nil
}

// parseSkipExpressionRows is parseSkipExpression flattened to row names, for
// callers (and fixture tests) that only need the literal expansion, not the
// clause structure checkSkipAmbiguity uses.
func parseSkipExpressionRows(expr string) ([]string, error) {
	clauses, err := parseSkipExpression(expr)
	if err != nil {
		return nil, err
	}
	var rows []string
	for _, c := range clauses {
		rows = append(rows, c.rows()...)
	}
	return rows, nil
}

// splitTopLevelAlternation splits expr on '|' at paren depth 0, so a `|`
// inside a clause's own "(a|b)" subtest group stays part of that clause.
func splitTopLevelAlternation(expr string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '|':
			if depth == 0 {
				parts = append(parts, expr[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, expr[start:])
	return parts
}

// requiredJobEnv reports whether GC_REQUIRE_ACCEPTANCE_TOOLING is on in the
// merged env of a workflow/job/step. This mirrors
// test/acceptance/helpers/tooling.go's requireSwitchOn exactly -- unset,
// empty, and "0" are off, anything else is on -- because that is the
// existing guard's own source for what a "required job" means
// (helpers.MissingTooling / helpers.MissingPrecondition fail rather than
// skip precisely when this switch is on), and this guard has to agree with
// it or it is checking the wrong jobs.
func requiredJobEnv(envs ...map[string]any) bool {
	const key = "GC_REQUIRE_ACCEPTANCE_TOOLING"
	for _, env := range envs {
		v, ok := env[key]
		if !ok {
			continue
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s != "" && s != "0" {
			return true
		}
	}
	return false
}

// subtestRunPattern finds a literal `t.Run("name", ...)` subtest name.
var subtestRunPattern = regexp.MustCompile(`t\.Run\("([^"]+)"`)

// acceptanceTestUniverse is the set of test/subtest names actually declared
// in the proxied acceptance files (test/acceptance/beads_proxied_*_test.go),
// used by checkSkipAmbiguity to catch a -skip PATTERN that reaches more (or
// fewer) tests than its literal expansion claims. go test -skip matches each
// `/`-separated fragment via an UNANCHORED regexp.MatchString, not an
// exact-name comparison -- "TestProxiedNative" matches both
// TestProxiedNativeLifecycle and TestProxiedNativeSafety -- so a clause this
// parser expands into one row name can silently deselect a sibling the
// allowlist never named.
//
// Scoped to the proxied acceptance files only, matching
// TestProxiedAcceptanceFunctionsAreSelectedByCI's own glob. A -skip could in
// principle also collide with an unrelated test elsewhere under
// test/acceptance/, which this check does not see -- the same scope
// TestProxiedAcceptanceRowsNeverSkipInRow and TestProxiedAcceptanceFunctionsAreSelectedByCI
// already commit to.
type acceptanceTestUniverse struct {
	TopLevel []string
	Subtests map[string][]string // parent test name -> its t.Run(...) literal subtest names
}

// loadAcceptanceTestUniverse scans test/acceptance/beads_proxied_*_test.go
// for their top-level test functions and, per function, its own t.Run(...)
// subtest names.
func loadAcceptanceTestUniverse(root string) (acceptanceTestUniverse, error) {
	matches, err := filepath.Glob(filepath.Join(root, "test", "acceptance", "beads_proxied_*_test.go"))
	if err != nil {
		return acceptanceTestUniverse{}, fmt.Errorf("glob the proxied acceptance files: %w", err)
	}
	universe := acceptanceTestUniverse{Subtests: map[string][]string{}}
	for _, path := range matches {
		body, err := os.ReadFile(path) //nolint:gosec // a path this glob found inside the repo
		if err != nil {
			return acceptanceTestUniverse{}, fmt.Errorf("read %s: %w", path, err)
		}
		text := string(body)
		universe.TopLevel = append(universe.TopLevel, topLevelTestFunctions(text)...)
		for name, funcBody := range splitByTopLevelTestFunction(text) {
			for _, m := range subtestRunPattern.FindAllStringSubmatch(funcBody, -1) {
				universe.Subtests[name] = append(universe.Subtests[name], m[1])
			}
		}
	}
	return universe, nil
}

// splitByTopLevelTestFunction returns, for every top-level
// `func TestX(t *testing.T) {` declaration in body, the text from that line
// up to (not including) the next one -- an approximation of the function's
// body that does not require brace matching, good enough to scope a
// t.Run(...) scan to roughly the right function. It reuses testFuncPattern,
// the same anchor topLevelTestFunctions is built on.
func splitByTopLevelTestFunction(body string) map[string]string {
	locs := testFuncPattern.FindAllStringSubmatchIndex(body, -1)
	out := map[string]string{}
	for i, loc := range locs {
		name := body[loc[2]:loc[3]]
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[name] = body[loc[0]:end]
	}
	return out
}

// acceptanceUniverseScope names where checkPatternMatchesExactlyOne's
// candidate pool comes from, for its own error messages.
const acceptanceUniverseScope = "test/acceptance/beads_proxied_*_test.go"

// checkSkipAmbiguity reports the first way clause's PATTERN reaches beyond
// what its literal expansion (clause.rows()) claims, per go test -skip's real
// unanchored per-fragment matching. It ALWAYS checks the top-level fragment
// (clause.TestNamePattern) against universe.TopLevel -- not only for a bare
// clause -- because a future sibling top-level test (a
// "TestProxiedNativeLifecycleV2") can make even a subtest-scoped clause's own
// top-level fragment newly ambiguous. When the clause has subtest
// alternatives, each is then checked against universe.Subtests[TestName] too.
//
// Zero matches means the pattern is stale or misspelled (nothing skips what
// the allowlist thinks it does); more than one means it silently reaches a
// sibling the allowlist never named -- the concrete shape of "TestProxiedNative"
// (no anchor) matching both TestProxiedNativeLifecycle and TestProxiedNativeSafety,
// or "root-move" also matching a future "root-move-after-crash" subtest.
func checkSkipAmbiguity(clause skipClause, universe acceptanceTestUniverse) error {
	if err := checkPatternMatchesExactlyOne(clause.TestNamePattern, clause.TestName, universe.TopLevel, "test function"); err != nil {
		return err
	}
	subtests := universe.Subtests[clause.TestName]
	for i, alt := range clause.Alts {
		pattern := clause.AltPatterns[i]
		if err := checkPatternMatchesExactlyOne(pattern, clause.TestName+"/"+alt, subtests, "subtest of "+clause.TestName); err != nil {
			return err
		}
	}
	return nil
}

func checkPatternMatchesExactlyOne(pattern, row string, candidates []string, what string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("-skip fragment %q (row %q) does not compile as a regexp: %w", pattern, row, err)
	}
	var matched []string
	for _, c := range candidates {
		if re.MatchString(c) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return fmt.Errorf("-skip fragment %q (row %q) matches no known %s in %s; it may be stale or misspelled",
			pattern, row, what, acceptanceUniverseScope)
	case 1:
		return nil
	default:
		return fmt.Errorf("-skip fragment %q (row %q) is ambiguous across %s: go test's unanchored matching also reaches %v; "+
			"anchor it (e.g. ^%s$) or write it to match only the intended %s", pattern, row, acceptanceUniverseScope, matched, pattern, what)
	}
}

// requiredJobSkipsFromDoc walks every step of every REQUIRED job (per
// requiredJobEnv) in one workflow document and expands each recognized
// `-skip` argument on its `go test` invocations into explicit row names,
// checking each against universe for the unanchored-matching ambiguity
// checkSkipAmbiguity guards.
//
// DENY BY DEFAULT, not an enumeration of known-bad spellings: for each
// step, extractAndBlankCanonicalSkips removes every canonical
// `-skip '<expr>'` occurrence from the (comment-stripped,
// continuation-joined) run text, and ANYTHING skip-flag-shaped
// (skipFlagTokenPattern) still present in what remains -- or in ANY env
// value in scope, workflow/job/step, under ANY key name -- is a hard error.
// This closes the class rather than chasing individual spellings: it also
// catches `'-skip' 'X'`, `-skip"" X`, an env var carrying `-skip=X` under a
// name that is not GOFLAGS or ACCEPTANCE_GO_TEST_FLAGS, and anything else
// that looks like the flag, without needing to have been told about it in
// advance. It returns an error -- not a partial result -- the moment
// anything about a step's -skip cannot be trusted: a leftover skip-flag-
// shaped token in the run script or an env value, an unparseable clause, or
// an ambiguous one.
func requiredJobSkipsFromDoc(workflowName string, doc acceptanceWorkflowDoc, universe acceptanceTestUniverse) ([]skippedRow, error) {
	var out []skippedRow
	for jobName, job := range doc.Jobs {
		for _, step := range job.Steps {
			if !requiredJobEnv(doc.Env, job.Env, step.Env) {
				continue
			}
			for _, env := range []map[string]any{doc.Env, job.Env, step.Env} {
				for key, value := range env {
					s := fmt.Sprint(value)
					if window, found := findSkipFlagToken(s); found {
						return nil, fmt.Errorf("%s job %q: env %s=%q contains a skip-flag-shaped token (%q) outside the "+
							"recognized -skip '<expr>' form; this guard cannot see inside an env value's content -- "+
							"rewrite the skip as an explicit -skip '<expr>' argument on the go test line instead",
							workflowName, jobName, key, s, window)
					}
				}
			}
			joined := joinBackslashContinuations(stripShellComments(step.Run))
			exprs, blanked := extractAndBlankCanonicalSkips(joined)
			if window, found := findSkipFlagToken(blanked); found {
				return nil, fmt.Errorf("%s job %q: run script contains a skip-flag-shaped token (%q) outside the "+
					"recognized -skip '<expr>' form; every -skip/-test.skip must be written exactly as "+
					"-skip '<expr>' (single-quoted) -- any other spelling fails open and is refused here",
					workflowName, jobName, window)
			}
			for _, expr := range exprs {
				clauses, err := parseSkipExpression(expr)
				if err != nil {
					return nil, fmt.Errorf("%s job %q: %w", workflowName, jobName, err)
				}
				for _, clause := range clauses {
					if err := checkSkipAmbiguity(clause, universe); err != nil {
						return nil, fmt.Errorf("%s job %q: %w", workflowName, jobName, err)
					}
					for _, row := range clause.rows() {
						out = append(out, skippedRow{Row: row, Job: jobName})
					}
				}
			}
		}
	}
	return out, nil
}

// skipAllowlistProblems is TestRequiredJobSkipsAreAllowlisted's engine,
// factored out as a pure function (workflow YAML + allowlist + test universe
// in, problem strings + a parse error out) so fixture-based unit tests can
// drive it against synthetic workflow YAML without touching the real ci.yml.
// workflows maps a display name (e.g. "ci.yml") to that workflow's raw YAML;
// every required job across every entry is checked and the results merged
// before allowlist staleness is judged, so a row skipped in one file is not
// mistaken for stale just because another file was checked first. A non-nil
// error means something about a -skip could not be trusted at all (see
// requiredJobSkipsFromDoc) -- a different failure mode from a policy
// mismatch, and one that must not be swallowed into an empty, all-clear
// problems slice.
func skipAllowlistProblems(workflows map[string][]byte, allowlist []skipAllowlistEntry, universe acceptanceTestUniverse) ([]string, error) {
	var skipped []skippedRow
	for name, workflow := range workflows {
		var doc acceptanceWorkflowDoc
		if err := yaml.Unmarshal(workflow, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if len(doc.Jobs) == 0 {
			return nil, fmt.Errorf("%s declares no jobs; the scan is broken", name)
		}
		rows, err := requiredJobSkipsFromDoc(name, doc, universe)
		if err != nil {
			return nil, err
		}
		skipped = append(skipped, rows...)
	}

	allowed := map[string]skipAllowlistEntry{}
	var problems []string
	for _, e := range allowlist {
		if strings.TrimSpace(e.Reason) == "" {
			problems = append(problems, fmt.Sprintf("skipAllowlist entry for %q has an empty Reason", e.Row))
		}
		if strings.TrimSpace(e.Bead) == "" {
			problems = append(problems, fmt.Sprintf("skipAllowlist entry for %q has an empty Bead id", e.Row))
		}
		if _, dup := allowed[e.Row]; dup {
			problems = append(problems, fmt.Sprintf("skipAllowlist has more than one entry for %q", e.Row))
		}
		allowed[e.Row] = e
	}

	seen := map[string]bool{}
	for _, row := range skipped {
		seen[row.Row] = true
		if _, ok := allowed[row.Row]; !ok {
			problems = append(problems, fmt.Sprintf(
				"job %q deselects %q via `go test -skip` with no entry in skipAllowlist; "+
					"add one with a Reason and the Bead tracking when the row comes back, or stop skipping it",
				row.Job, row.Row))
		}
	}
	for _, e := range allowlist {
		if !seen[e.Row] {
			problems = append(problems, fmt.Sprintf(
				"skipAllowlist entry %q (%s) is stale: no required job's `go test -skip` deselects it anymore; remove the entry",
				e.Row, e.Bead))
		}
	}
	return problems, nil
}

// TestRequiredJobSkipsAreAllowlisted is TestProxiedAcceptanceRowsNeverSkipInRow's
// CI-selection counterpart (ga-may9k). That test closes the in-row t.Skip
// door; this one closes the door beside it: a required job's own
// `go test -skip` deselecting a row from outside the row entirely, which
// neither that guard nor TestProxiedAcceptanceFunctionsAreSelectedByCI (which
// only checks a function is named by SOME `-run`) can see. -skip'ing
// TestProxiedNativeLifecycle/(child-term-zombie|root-move) in the
// beads-proxied-native-acceptance job (#215, ga-w9xm9) produces exactly the
// silent-pass-with-rows-unrun outcome TestProxiedAcceptanceRowsNeverSkipInRow
// exists to prevent, from a place it never looks.
//
// Scans ci.yml and nightly.yml (the only two workflows a required job -- one
// that sets GC_REQUIRE_ACCEPTANCE_TOOLING -- exists in today: ci.yml's
// beads-proxied-native-acceptance and beads-topology-acceptance, nightly.yml's
// beads-proxied-perf). A `-skip` in a required job in some OTHER workflow
// file is outside this guard's scope.
//
// Deny by default, not an enumeration: requiredJobSkipsFromDoc removes every
// canonical `-skip '<expr>'` occurrence and hard-errors on anything
// skip-flag-shaped left in the run script OR in any env value in scope
// (workflow, job, or step, under any key name -- not only GOFLAGS or
// ACCEPTANCE_GO_TEST_FLAGS). That closes `'-skip' 'X'`, `"-skip" "X"`,
// `-skip"" X`, an unquoted value, `-test.skip`, a shell variable holding the
// flag (`ARGS=-skip=X; go test $ARGS`), a bash array
// (`args=(-skip=X); go test "${args[@]}"`), `GOFLAGS+='-skip=X'`, and an env
// entry under any name (`env: EXTRA: -skip=X`) -- see
// acceptance_run_skip_allowlist_fixture_test.go for a fixture proving each
// one. stripShellComments' word-start rule keeps a `#` inside a quoted
// string (`echo '#'; go test -skip=X`) or a `$#` parameter (`echo $#`) from
// swallowing real content after it, which a naive first-'#' rule would have.
//
// Known remaining limits, stated rather than silently assumed: this guard
// reads `step.run:` text and job/step/workflow `env:` maps -- it does not
// execute shell, so it has no notion of what a script the run: text merely
// INVOKES (rather than inlines) does; nothing about invoking a checked-in
// script mentions "skip". stripShellComments' word-start rule is not
// quote-aware beyond that one case: a `#` after a shell metacharacter other
// than whitespace (e.g. `;#comment`) is not recognized as a comment start
// either, so it stays in the scanned text -- conservative (it can cause an
// over-detection, never a missed one) but still a known gap from real shell
// tokenization. GITHUB_ENV (a step appending to it for a LATER step to read)
// and a workflow_call input threaded through `${{ }}` interpolation both
// still reach this scan as the literal `${{ ... }}` text GitHub Actions has
// not yet resolved, which parseSkipExpression then correctly refuses to
// expand as an unrecognized -skip form -- caught, but for the wrong stated
// reason, which is worth knowing when reading that error.
func TestRequiredJobSkipsAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	workflows := map[string][]byte{}
	for _, name := range []string{"ci.yml", "nightly.yml"} {
		body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		workflows[name] = body
	}
	universe, err := loadAcceptanceTestUniverse(root)
	if err != nil {
		t.Fatalf("load acceptance test universe: %v", err)
	}
	problems, err := skipAllowlistProblems(workflows, skipAllowlist, universe)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

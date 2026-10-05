package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// The canonical -skip spelling is `-skip '<expr>'` (a plain single-quoted
// word, read from the shell AST by scanRunScriptSkips). go test's flag also
// accepts --skip, -skip=X, -skip "X", an unquoted value, -test.skip and
// GOFLAGS; none of those are parsed. requiredJobSkipsFromDoc DENIES BY
// DEFAULT: any other skip-shaped word is a hard error. Unlike a missed
// `-run` (which a green CI run makes visible by never running), a missed
// `-skip` fails OPEN.
//
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
				// ga-1ebvc LOW: no TrimSpace here, deliberately. go test's
				// real -skip matching does not trim the regex it builds
				// from each alternative -- " bar " never matches a subtest
				// actually named "bar" -- so trimming silently reported a
				// row as skipped that go test itself would never actually
				// deselect. simpleSubtestNamePattern already rejects
				// whitespace (it is not in bareOrAnchoredSubtest's
				// character class), so a padded alternative is refused
				// here rather than quietly normalized.
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

// setsRequiredSwitchViaGithubEnv reports whether run script text looks like
// it writes GC_REQUIRE_ACCEPTANCE_TOOLING to $GITHUB_ENV -- the standard
// Actions idiom (`echo "KEY=value" >> "$GITHUB_ENV"`) for setting an env var
// a LATER step (or, via outputs, a later job) picks up at runtime. ga-1ebvc
// LOW: requiredJobEnv only reads the YAML env: maps, so a job that becomes
// required exclusively this way was invisible to this guard -- its -skip,
// however written, was never scanned or required to be allowlisted. This is
// deliberately a cheap textual check (both substrings present on the run
// script, no attempt to parse which value is actually written or track it
// across steps/jobs) rather than a GITHUB_ENV interpreter: it can only
// over-trigger (mark a job required that was not, which is the safe
// direction for this guard), never under-trigger on the shape it targets.
func setsRequiredSwitchViaGithubEnv(run string) bool {
	// `${{ github.env }}` expands to the GITHUB_ENV path, so it is the same
	// target; it is invisible to the shell parser (replaced by a
	// placeholder), hence the raw-text check.
	if strings.Contains(run, "github.env") && scriptMentionsAll(run, "GC_REQUIRE_ACCEPTANCE_TOOLING") {
		return true
	}
	return scriptMentionsAll(run, "GC_REQUIRE_ACCEPTANCE_TOOLING", "GITHUB_ENV")
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
// checkSkipAmbiguity, on success, also returns the ROWS this clause actually
// deselects -- the real matched candidate names, not clause.rows()'s literal
// expansion of the -skip text. ga-1ebvc MEDIUM/LOW: an unanchored pattern
// that is a PREFIX of the real name (e.g. -skip 'TestFoo/(bar)' when the
// only real subtest is "barbaz") passes this check -- it matches exactly
// one candidate, so it is not ambiguous -- but the row it actually
// deselects, at runtime, under go test's own unanchored matching, is
// "barbaz", not the literal "bar" clause.rows() would report. Recording the
// literal text instead of the matched candidate meant an allowlist entry
// written against the REAL row ("TestFoo/barbaz") was reported both as
// covering an unlisted skip it did not (the guard recorded "TestFoo/bar")
// and as stale (nothing was recorded under its own name).
func checkSkipAmbiguity(clause skipClause, universe acceptanceTestUniverse) ([]string, error) {
	matchedTest, err := checkPatternMatchesExactlyOne(clause.TestNamePattern, clause.TestName, universe.TopLevel, "test function")
	if err != nil {
		return nil, err
	}
	if len(clause.Alts) == 0 {
		return []string{matchedTest}, nil
	}
	subtests := universe.Subtests[clause.TestName]
	rows := make([]string, len(clause.Alts))
	for i, alt := range clause.Alts {
		pattern := clause.AltPatterns[i]
		matchedAlt, err := checkPatternMatchesExactlyOne(pattern, clause.TestName+"/"+alt, subtests, "subtest of "+clause.TestName)
		if err != nil {
			return nil, err
		}
		rows[i] = matchedTest + "/" + matchedAlt
	}
	return rows, nil
}

// checkPatternMatchesExactlyOne returns the single candidate pattern
// matches, or an error if it matches zero or more than one.
func checkPatternMatchesExactlyOne(pattern, row string, candidates []string, what string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("-skip fragment %q (row %q) does not compile as a regexp: %w", pattern, row, err)
	}
	var matched []string
	for _, c := range candidates {
		if re.MatchString(c) {
			matched = append(matched, c)
		}
	}
	switch len(matched) {
	case 0:
		return "", fmt.Errorf("-skip fragment %q (row %q) matches no known %s in %s; it may be stale or misspelled",
			pattern, row, what, acceptanceUniverseScope)
	case 1:
		return matched[0], nil
	default:
		return "", fmt.Errorf("-skip fragment %q (row %q) is ambiguous across %s: go test's unanchored matching also reaches %v; "+
			"anchor it (e.g. ^%s$) or write it to match only the intended %s", pattern, row, acceptanceUniverseScope, matched, pattern, what)
	}
}

// findJobNode returns the raw YAML node for jobName under root's top-level
// "jobs:" mapping, or nil if root is nil or shaped in a way that could not
// be navigated (the typed decode elsewhere already rejects a workflow that
// does not parse as YAML at all, so this is a defensive nil, not a second
// error path).
func findJobNode(root *yaml.Node, jobName string) *yaml.Node {
	if root == nil {
		return nil
	}
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	jobsNode := mappingValue(doc, "jobs")
	if jobsNode == nil {
		return nil
	}
	return mappingValue(jobsNode, jobName)
}

// mappingValue returns the value node for key in a YAML mapping node, or
// nil if mapping is not a mapping node or has no such key.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// scanJobNodeForBypassSkips walks every scalar in a job's raw YAML node,
// except a steps[i] item's own run and name (steps[].run gets the canonical
// -skip '<expr>' treatment in requiredJobSkipsFromDoc's own loop), and hard-errors
// on anything skip-flag-shaped. ga-1ebvc MEDIUM: this is what closes the
// door acceptanceWorkflowDoc's typed decode leaves open on strategy.matrix,
// a step's own shell:, defaults.run.shell, container.env, with:, and
// anywhere else the struct has no field for. The run/name exemption is
// path-precise: a "run" key anywhere else (matrix.include, with:,
// container.env) is scanned.
func scanJobNodeForBypassSkips(workflowName, jobName string, jobNode *yaml.Node) error {
	return walkYAMLNode(jobNode, "", func(path, value string) error {
		if window, found := findSkipFlagToken(value); found {
			return fmt.Errorf("%s job %q: %s contains a skip-flag-shaped token (%q) outside the recognized "+
				"-skip '<expr>' run-script form; this guard only parses -skip inside steps[].run -- move it there",
				workflowName, jobName, path, window)
		}
		return nil
	})
}

// stepItemPathPattern matches the breadcrumb of a job's own steps[i] mapping
// (anchored at the job root), the only place a scalar "run" X
var stepItemPathPattern = regexp.MustCompile(`^steps\[\d+\]$`)

// walkYAMLNode recursively visits every scalar value under node, calling
// visit(path, value) for each one except a steps[i] item's own run and name
// scalars. path is a dotted/bracketed breadcrumb (e.g.
// "strategy.matrix.flags[0]") used only for error messages.
func walkYAMLNode(node *yaml.Node, path string, visit func(string, string) error) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			if err := walkYAMLNode(c, path, visit); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			value := node.Content[i+1]
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			// Only a "run" key whose OWN value is a scalar is the
			// canonical steps[].run shape this skips -- "run" as a
			// MAPPING key (e.g. defaults.run.shell) is a different field
			// entirely and must still be scanned. Suppressing only this
			// one child, rather than propagating a flag through the rest
			// of the subtree, is what keeps defaults.run.shell in scope
			// while steps[].run itself stays out of it.
			if value.Kind == yaml.ScalarNode && stepItemPathPattern.MatchString(path) && (key == "run" || key == "name") {
				continue
			}
			if err := walkYAMLNode(value, childPath, visit); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, c := range node.Content {
			if err := walkYAMLNode(c, fmt.Sprintf("%s[%d]", path, i), visit); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		return visit(path, node.Value)
	case yaml.AliasNode:
		return walkYAMLNode(node.Alias, path, visit)
	}
	return nil
}

// requiredJobSkipsFromDoc walks every step of every REQUIRED job (per
// requiredJobEnv) in one workflow document and expands each recognized
// `-skip` argument on its `go test` invocations into explicit row names,
// checking each against universe for the unanchored-matching ambiguity
// checkSkipAmbiguity guards.
//
// DENY BY DEFAULT, not an enumeration of known-bad spellings: each step's
// run script is parsed as bash (scanRunScriptSkips), the canonical
// `-skip '<expr>'` call arguments are taken from the AST, and ANYTHING
// else skip-flag-shaped in any word of the script -- or in ANY env value in
// scope, workflow/job/step, under ANY key name -- is a hard error. Jobs a
// required job transitively needs are scanned too, but may not use even the
// canonical form. It returns an error -- not a partial result -- the moment
// anything about a step's -skip cannot be trusted: a leftover skip-flag-
// shaped token, an unparseable script, an unparseable clause, or an
// ambiguous one.
func requiredJobSkipsFromDoc(workflowName string, doc acceptanceWorkflowDoc, root *yaml.Node, universe acceptanceTestUniverse) ([]skippedRow, error) {
	var out []skippedRow
	required := map[string]bool{}
	for jobName, job := range doc.Jobs {
		// LOW (ga-may9k round 4): a job is in scope if ANY of its steps sets
		// the required-tooling env, not only the specific step being
		// scanned right now. A step that itself carries no
		// GC_REQUIRE_ACCEPTANCE_TOOLING but runs later in a job another
		// step DID mark required is still part of a required job -- job and
		// workflow env apply uniformly to every step regardless of which
		// step's YAML happens to repeat them.
		jobRequired := requiredJobEnv(doc.Env, job.Env)
		for _, step := range job.Steps {
			if jobRequired {
				break
			}
			jobRequired = requiredJobEnv(step.Env) || setsRequiredSwitchViaGithubEnv(step.Run)
		}
		if jobRequired {
			required[jobName] = true
		}
	}
	if len(required) == 0 {
		return nil, nil
	}

	// A required job can consume values another job produced
	// (`${{ needs.gen.outputs.f }}` fed by `echo f=-skip=X >> $GITHUB_OUTPUT`),
	// so every job a required job transitively `needs:` is scanned for a
	// hiding -skip too. Only a REQUIRED job's own canonical skips count as
	// deselected rows.
	scope := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if scope[name] {
			return
		}
		scope[name] = true
		for _, dep := range jobNeeds(findJobNode(root, name)) {
			visit(dep)
		}
	}
	for name := range required {
		visit(name)
	}

	for jobName := range scope {
		job, ok := doc.Jobs[jobName]
		if !ok {
			continue
		}
		// ga-1ebvc MEDIUM: acceptanceWorkflowDoc's typed struct only has
		// fields for env: maps and steps[].run. A -skip hiding anywhere ELSE
		// in the job's YAML is scanned from the RAW node, except a job's own
		// steps[i].run and name (handled below with the shell parser).
		if err := scanJobNodeForBypassSkips(workflowName, jobName, findJobNode(root, jobName)); err != nil {
			return nil, err
		}
		envs := []map[string]any{doc.Env, job.Env}
		for _, step := range job.Steps {
			envs = append(envs, step.Env)
		}
		for _, env := range envs {
			for key, value := range env {
				v := fmt.Sprint(value)
				if window, found := findSkipFlagToken(v); found {
					return nil, fmt.Errorf("%s job %q: env %s=%q contains a skip-flag-shaped token (%q) outside the "+
						"recognized -skip '<expr>' form; this guard cannot see inside an env value's content -- "+
						"rewrite the skip as an explicit -skip '<expr>' argument on the go test line instead",
						workflowName, jobName, key, v, window)
				}
			}
		}
		for _, step := range job.Steps {
			exprs, flagged, err := scanRunScriptSkips(step.Run)
			if err != nil {
				return nil, fmt.Errorf("%s job %q: %w", workflowName, jobName, err)
			}
			if flagged != "" {
				return nil, fmt.Errorf("%s job %q: run script contains a skip-flag-shaped token (%q) outside the "+
					"recognized -skip '<expr>' form; every -skip/-test.skip must be written exactly as "+
					"-skip '<expr>' (single-quoted) -- any other spelling fails open and is refused here",
					workflowName, jobName, flagged)
			}
			if !required[jobName] {
				// A job that is only in scope because a required job needs it
				// may not skip anything, canonical form included: the guard
				// does not track which rows its skips would deselect.
				if len(exprs) > 0 {
					return nil, fmt.Errorf("%s job %q: -skip %q in a job a required job depends on (needs:); "+
						"only required jobs may use the canonical -skip form", workflowName, jobName, exprs[0])
				}
				continue
			}
			for _, expr := range exprs {
				clauses, err := parseSkipExpression(expr)
				if err != nil {
					return nil, fmt.Errorf("%s job %q: %w", workflowName, jobName, err)
				}
				for _, clause := range clauses {
					rows, err := checkSkipAmbiguity(clause, universe)
					if err != nil {
						return nil, fmt.Errorf("%s job %q: %w", workflowName, jobName, err)
					}
					for _, row := range rows {
						out = append(out, skippedRow{Row: row, Job: jobName})
					}
				}
			}
		}
	}
	// Workflow-level keys (defaults.run.shell, on.*.inputs.*.default, ...)
	// feed every job, so with any job required they are scanned too; jobs
	// are scanned above.
	top := root
	if top != nil && top.Kind == yaml.DocumentNode && len(top.Content) > 0 {
		top = top.Content[0]
	}
	if top != nil && top.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(top.Content); i += 2 {
			key := top.Content[i].Value
			if key == "jobs" {
				continue
			}
			if err := walkYAMLNode(top.Content[i+1], key, func(path, value string) error {
				if window, found := findSkipFlagToken(value); found {
					return fmt.Errorf("%s: workflow-level %s contains a skip-flag-shaped token (%q) that applies to required jobs; "+
						"this guard only parses -skip inside steps[].run", workflowName, path, window)
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// jobNeeds returns the job names listed in a job node's `needs:` (a scalar
// or a sequence).
func jobNeeds(jobNode *yaml.Node) []string {
	needs := mappingValue(jobNode, "needs")
	if needs == nil {
		return nil
	}
	switch needs.Kind {
	case yaml.ScalarNode:
		return []string{needs.Value}
	case yaml.SequenceNode:
		var names []string
		for _, c := range needs.Content {
			names = append(names, c.Value)
		}
		return names
	}
	return nil
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
		// ga-1ebvc MEDIUM: a second, raw parse alongside the typed one
		// above, so requiredJobSkipsFromDoc can also walk each required
		// job's full YAML subtree (scanJobNodeForBypassSkips) for a -skip
		// hiding in a field the typed struct has no field for.
		var root yaml.Node
		if err := yaml.Unmarshal(workflow, &root); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		rows, err := requiredJobSkipsFromDoc(name, doc, &root, universe)
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

// listAcceptanceWorkflowFiles returns the base names of every workflow file
// (*.yml, *.yaml) directly under root/.github/workflows, sorted. ga-1ebvc
// LOW: this replaces a hardcoded ["ci.yml", "nightly.yml"] pair that made a
// required job in any other workflow file invisible to this guard.
func listAcceptanceWorkflowFiles(root string) ([]string, error) {
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".yml", ".yaml":
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
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
// Scans every workflow file under .github/workflows (listAcceptanceWorkflowFiles),
// not a hardcoded pair -- ga-1ebvc LOW found that a required job (one that
// sets GC_REQUIRE_ACCEPTANCE_TOOLING) added to any file OTHER than the
// then-hardcoded ci.yml/nightly.yml was invisible to this guard no matter
// what its -skip said. As of this writing only ci.yml (beads-proxied-native-
// acceptance, beads-topology-acceptance) and nightly.yml (beads-proxied-perf)
// have a required job at all; the glob means a required job added to any
// other workflow file is in scope from the moment it exists, not from the
// next time someone remembers to extend a hardcoded list.
//
// Deny by default, not an enumeration: requiredJobSkipsFromDoc reads each
// run script with a real bash parser (mvdan.cc/sh), accepts exactly the
// canonical `-skip '<expr>'` call arguments, and hard-errors on any other
// skip-flag-shaped word, however quoted, escaped, ANSI-C encoded or glued
// to an expansion (`-skip=X`, `'-skip' 'X'`, `-test.skip`, `ARGS=-skip=X`,
// `GOFLAGS+='-skip=X'`, `-skip 'A'"|B"`, `-s\kip`, `$'\x2dskip'`, and a
// skip-shaped word in an env value under any name). Comments, quoting,
// heredocs, command substitution and line continuations are the parser's
// business, not this file's. Scalars elsewhere in a required job's YAML
// (matrix, with:, shell:, container, defaults, on.* inputs, needed jobs) are
// scanned as plain text for the same token.
//
// Known remaining limits, stated rather than claimed away:
//   - Nothing is executed. A script the run: text merely INVOKES, `eval`,
//     `bash -c` with a computed string, or a base64 round trip can build the
//     flag at run time from text that never spells it.
//   - `${{ }}` expressions are not evaluated. A skip-shaped token written
//     inside one, or glued to one, is caught; a flag assembled across a
//     `${{ }}` or parameter expansion is caught only when one character
//     stands in for the expansion (`${{ '-s' }}kip`, `-s${X:-k}ip`), not
//     when it expands to several or none.
//   - A flag split across two matrix values or env entries is invisible.
//   - A GITHUB_ENV write inside a composite action is out of scope.
//   - A heredoc body that merely prints `-skip` is flagged (fails closed).
//
// A related deselection vector is explicitly OUT of scope: `-run` can drop
// rows the same way `-skip` can (`-run 'TestX/(a|b)$'` in a required job
// runs only rows a and b, silently skipping every other subtest of TestX,
// with no allowlist and no error). ga-0xvbd (P2, discovered from ga-may9k)
// tracks reusing this parser against `-run`; until it ships, a `-run` that
// narrows a required job's subtests is not checked by anything in this
// file.
func TestRequiredJobSkipsAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	names, err := listAcceptanceWorkflowFiles(root)
	if err != nil {
		t.Fatalf("list workflow files: %v", err)
	}
	workflows := map[string][]byte{}
	for _, name := range names {
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

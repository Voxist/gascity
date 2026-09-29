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

// skipArgPattern matches the body of a `-skip '<expr>'` argument, the same
// single-quoted convention runPattern relies on for `-run`.
var skipArgPattern = regexp.MustCompile(`-skip\s+'([^']*)'`)

func collectSkipExpressions(runBody string) []string {
	var out []string
	for _, match := range skipArgPattern.FindAllStringSubmatch(runBody, -1) {
		out = append(out, match[1])
	}
	return out
}

// skipClausePattern matches one `-skip` clause this parser can expand:
// a bare test name, or a test name followed by a parenthesized `|`-separated
// alternation of subtest names -- go test -skip's `A/(b|c)` idiom. Anything
// with other regex metacharacters (`.*`, `^`, nested groups, character
// classes, ...) does not match and is reported as unexpandable rather than
// silently accepted, per the same "checkable, not a full regex evaluator"
// stance TestProxiedAcceptanceFunctionsAreSelectedByCI takes for `-run`.
var skipClausePattern = regexp.MustCompile(`^(Test[A-Za-z0-9_]*)(?:/\(([^()]*)\))?$`)

// simpleSubtestNamePattern is what a single alternative inside the `(a|b|c)`
// group must look like to count as an explicit row name rather than more
// regex syntax this parser does not evaluate.
var simpleSubtestNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseSkipExpression expands a `go test -skip` regular expression into the
// explicit row names ("TestName" or "TestName/subtest") it deselects, or
// reports an error when the expression is not one of the two shapes this
// parser understands. A `-skip` expression may itself be several clauses
// joined by top-level `|` (e.g. "TestA|TestB/(x|y)"); a `|` nested inside a
// clause's own `(...)` group is a subtest alternation, not a clause
// separator, so splitting happens at paren depth 0 only.
func parseSkipExpression(expr string) ([]string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("empty -skip expression")
	}
	var rows []string
	for _, clause := range splitTopLevelAlternation(expr) {
		clause = strings.TrimSpace(clause)
		m := skipClausePattern.FindStringSubmatch(clause)
		if m == nil {
			return nil, fmt.Errorf("cannot expand -skip clause %q into explicit row names "+
				"(supported shapes: TestName, or TestName/(alt1|alt2|...))", clause)
		}
		testName, alternation := m[1], m[2]
		if alternation == "" {
			rows = append(rows, testName)
			continue
		}
		for _, alt := range strings.Split(alternation, "|") {
			alt = strings.TrimSpace(alt)
			if !simpleSubtestNamePattern.MatchString(alt) {
				return nil, fmt.Errorf("cannot expand -skip clause %q: subtest alternative %q is not a plain name", clause, alt)
			}
			rows = append(rows, testName+"/"+alt)
		}
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

// requiredJobSkipsFromDoc walks every step of every REQUIRED job (per
// requiredJobEnv) and expands each `-skip` argument on its `go test`
// invocations into explicit row names. It returns an error -- not a partial
// result -- the moment any `-skip` expression cannot be expanded, so an
// unparseable pattern fails the check instead of silently passing it.
func requiredJobSkipsFromDoc(doc acceptanceWorkflowDoc) ([]skippedRow, error) {
	var out []skippedRow
	for jobName, job := range doc.Jobs {
		for _, step := range job.Steps {
			if !requiredJobEnv(doc.Env, job.Env, step.Env) {
				continue
			}
			for _, expr := range collectSkipExpressions(step.Run) {
				rows, err := parseSkipExpression(expr)
				if err != nil {
					return nil, fmt.Errorf("job %q: %w", jobName, err)
				}
				for _, row := range rows {
					out = append(out, skippedRow{Row: row, Job: jobName})
				}
			}
		}
	}
	return out, nil
}

// skipAllowlistProblems is TestRequiredJobSkipsAreAllowlisted's engine,
// factored out as a pure function (workflow bytes + allowlist in, problem
// strings + a parse error out) so fixture-based unit tests can drive it
// against synthetic workflow YAML without touching the real ci.yml. A
// non-nil error means a `-skip` expression could not be expanded at all --
// a different failure mode from a policy mismatch, and one that must not be
// swallowed into an empty, all-clear problems slice.
func skipAllowlistProblems(workflow []byte, allowlist []skipAllowlistEntry) ([]string, error) {
	var doc acceptanceWorkflowDoc
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if len(doc.Jobs) == 0 {
		return nil, fmt.Errorf("workflow declares no jobs; the scan is broken")
	}

	skipped, err := requiredJobSkipsFromDoc(doc)
	if err != nil {
		return nil, err
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
func TestRequiredJobSkipsAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	problems, err := skipAllowlistProblems(workflow, skipAllowlist)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

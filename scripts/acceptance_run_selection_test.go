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

// skipArgPattern matches the body of the ONE canonical `-skip '<expr>'`
// spelling this parser accepts: single-quoted, the same convention runPattern
// relies on for `-run`, with a left boundary that excludes it matching mid-
// identifier (an "...x-skip" tail cannot masquerade as the flag) AND a right
// boundary requiring the closing `'` be followed by end-of-text, whitespace,
// or one of `;&|)` -- go test's actual -skip flag also accepts --skip,
// -skip=X, -skip "X", an unquoted value, -test.skip/--test.skip, and reads
// GOFLAGS -- none of those are parsed here. Rather than enumerate and parse
// every one of them, requiredJobSkipsFromDoc DENIES BY DEFAULT: it removes
// every canonical occurrence this pattern finds, then hard-errors if anything
// matching skipFlagTokenPattern remains anywhere in the run script or any env
// value in scope. Unlike a missed `-run` (which a green CI run makes visible
// by never running), a missed `-skip` fails OPEN: the row runs -- or a shared
// resource the allowlist assumed was staying off does not.
//
// The right boundary closes a glued-suffix hole review found: without it,
// `-skip 'Allowed”|TestOther'` (two adjacent single-quoted shell strings,
// which the shell concatenates into one -skip 'Allowed|TestOther') matches
// this pattern as if "Allowed" were the whole expression, blanks exactly
// that much, and leaves the trailing `'|TestOther'` looking like ordinary
// quoted text -- not skip-flag-shaped, so it never reaches
// skipFlagTokenPattern's fallback scan either. TestOther silently skips with
// no allowlist entry and no error, from a spelling nobody was asked to
// recognize. `-skip 'Allowed'"|X"`, `-skip 'Allowed'$E`, and
// `-skip 'Allowed'\|X` are the same hole through a double-quoted glue, a
// shell variable, and an escaped pipe respectively. Go's regexp (RE2) has no
// lookahead, so the right boundary is a captured, consumed group like the
// left one; extractAndBlankCanonicalSkips re-emits it unchanged, the same
// way it already re-emits the left boundary character.
var skipArgPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])-skip\s+'([^']*)'($|[\s;&|)])`)

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

// fullLineCommentPattern matches a line whose first non-whitespace character
// is '#' -- a FULL-LINE shell comment, and only that, PROVIDED the scanner
// is not already inside a quote or heredoc body carried over from an
// earlier line (see shellLexState below). review's HIGH-2 found this
// guard's previous word-start rule ("'#' at line-start or after whitespace
// starts a comment") had a false match: a '#' preceded by whitespace INSIDE
// a double-quoted string is not a shell comment at all, so
// `echo "step #1"; go test -skip=X` had its word-start rule fire on the '#'
// in "step #1" and truncate the rest of the line -- including the real
// `; go test -skip=X` -- and ci.yml already has this shape (`echo "## ..."`).
// Restricting to whole-line comments fixed the SAME-line case, but
// review's ga-1ebvc MEDIUM found the multi-line version of the same hole:
// a quote OPENED on an earlier line and not yet closed makes a '#' at the
// START of a later line just as much NOT a comment, and the previous rule
// had no memory of that -- it deleted the whole line, real -skip included,
// with nothing left for the fallback scan to catch. shellLexState is the
// minimal state (open single/double quote, open heredoc) needed to answer
// "am I at a real command-start position" one line at a time.
var fullLineCommentPattern = regexp.MustCompile(`^\s*#`)

// heredocStartPattern finds a heredoc redirect (`<<`, `<<-`) and its
// delimiter, optionally quoted. It is only consulted on text known (by
// shellLexState) to be OUTSIDE any open quote already, so it cannot itself
// misfire on a `<<` that is merely quoted string content.
var heredocStartPattern = regexp.MustCompile(`<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// shellLexState is the minimal shell lexical state stripShellComments needs
// to track ACROSS lines: whether the scanner is inside an open single
// quote, an open double quote, or an open heredoc body, and (for a heredoc
// opened with `<<-`) whether the end delimiter's leading tabs are
// stripped. It is a state machine over QUOTING and HEREDOCS only -- not a
// shell parser.
//
// It tracks single, double and $'...' (ANSI-C) quotes, an unquoted trailing
// backslash (the next line is joined, so a leading '#' there is not a
// comment), and stops at an unquoted word-start '#'.
//
// Known gaps, stated rather than assumed away: it does not track
// command substitution $(...) or backtick nesting, a
// heredoc delimiter that itself needs more than one layer of quote
// removal, or `<<<` here-strings (harmless: they take no body). Each is a
// narrower vector than the multi-line-quote hole this fixes. These gaps do
// NOT all fail safe: a construct the scanner mis-reads can flip quote state
// and blank a line that is really code. Replace this tracker with an exact
// allowlist or mvdan.cc/sh if it keeps growing.
type shellLexState struct {
	singleQuoted bool
	doubleQuoted bool
	heredocEnd   string // non-empty while inside a heredoc body
	heredocStrip bool   // <<- : strip leading tabs from the end-delimiter line
	ansiQuoted   bool   // inside $'...' (backslash escapes, including \')
	continued    bool   // previous line ended in an unquoted '\': this line cannot start a comment
}

// scanLine updates st to reflect line's effect on shell quote/heredoc
// state. It never modifies line; stripShellComments decides separately,
// using the state as of the START of a line, whether that line is a
// comment to blank.
func (st *shellLexState) scanLine(line string) {
	st.continued = false
	i := 0
	for i < len(line) {
		c := line[i]
		switch {
		case st.ansiQuoted:
			if c == '\\' && i+1 < len(line) {
				i += 2
				continue
			}
			if c == '\'' {
				st.ansiQuoted = false
			}
			i++
		case st.singleQuoted:
			if c == '\'' {
				st.singleQuoted = false
			}
			i++
		case st.doubleQuoted:
			if c == '\\' && i+1 < len(line) {
				i += 2
				continue
			}
			if c == '"' {
				st.doubleQuoted = false
			}
			i++
		case c == '$' && i+1 < len(line) && line[i+1] == '\'':
			st.ansiQuoted = true
			i += 2
		case c == '\'':
			st.singleQuoted = true
			i++
		case c == '#' && (i == 0 || strings.ContainsRune(" \t;&|(", rune(line[i-1]))):
			// An unquoted word-start '#' begins a real comment: nothing after
			// it can open a quote or heredoc.
			return
		case c == '\\' && i+1 == len(line):
			st.continued = true
			i++
		case c == '"':
			st.doubleQuoted = true
			i++
		case c == '\\' && i+1 < len(line):
			i += 2
		case c == '<' && i+1 < len(line) && line[i+1] == '<':
			// A here-string (`<<<word`) does not match heredocStartPattern
			// (the identifier class excludes '<'), so it naturally falls
			// through to the i++ below and is never mistaken for a heredoc
			// -- it opens no body and needs no end-delimiter tracking.
			if m := heredocStartPattern.FindStringSubmatchIndex(line[i:]); m != nil && m[0] == 0 {
				st.heredocEnd = line[i+m[4] : i+m[5]]
				st.heredocStrip = strings.HasPrefix(line[i:], "<<-")
				i += m[1]
				continue
			}
			i++
		default:
			i++
		}
	}
}

// stripShellComments blanks every FULL-LINE shell comment, tracking quote
// and heredoc state across lines (shellLexState) so a '#' that only LOOKS
// like a line-start comment -- because it is inside a quote opened on an
// earlier line, or inside a heredoc body -- is left untouched instead of
// being deleted. A real full-line comment (outside any quote or heredoc,
// first non-whitespace character '#') lets a comment that MENTIONS "-skip"
// in prose (exactly how the real beads-proxied-native-acceptance step
// documents its own deselection: "# -skip excludes child-term-zombie and
// root-move: ...") not be counted as a skip-flag token. A '#' that shares a
// line with real content -- commented-out trailing code, or one that is
// merely inside a same-line quote -- is deliberately left untouched, same
// as before. Applied before joinBackslashContinuations: a real shell
// comment absorbs any trailing '\' too, so line-joining must not cross one.
func stripShellComments(s string) string {
	lines := strings.Split(s, "\n")
	var st shellLexState
	for i, line := range lines {
		if st.heredocEnd != "" {
			end := line
			if st.heredocStrip {
				end = strings.TrimLeft(end, "\t")
			}
			if end == st.heredocEnd {
				st.heredocEnd = ""
				st.heredocStrip = false
			}
			continue // heredoc body lines are never comments, never blanked
		}
		if !st.singleQuoted && !st.doubleQuoted && !st.ansiQuoted && !st.continued && fullLineCommentPattern.MatchString(line) {
			lines[i] = ""
			continue
		}
		st.scanLine(line)
	}
	return strings.Join(lines, "\n")
}

// extractAndBlankCanonicalSkips finds every canonical -skip '<expr>'
// occurrence skipArgPattern recognizes, returning the expressions in
// left-to-right order and TEXT with those occurrences replaced (both
// boundary characters kept, the rest blanked) so a subsequent scan for
// anything skip-flag-shaped does not re-flag what this parser already
// accounted for. The right boundary (end-of-text, whitespace, or one of
// `;&|)`) is consumed by the match the same way the left one is -- RE2 has
// no lookahead -- so it is re-emitted here exactly like the left boundary,
// rather than dropped: dropping it would silently delete a real separator
// (e.g. the `;` between two shell commands) from the blanked text a later
// scan still has to read correctly.
func extractAndBlankCanonicalSkips(text string) (exprs []string, blanked string) {
	var b strings.Builder
	last := 0
	for _, m := range skipArgPattern.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:m[0]])
		if m[2] >= 0 {
			b.WriteString(text[m[2]:m[3]]) // keep the left boundary character
		}
		b.WriteByte(' ')
		exprs = append(exprs, text[m[4]:m[5]])
		if m[6] >= 0 {
			b.WriteString(text[m[6]:m[7]]) // keep the right boundary character (empty at end-of-text)
		}
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
	run = stripShellComments(run)
	return strings.Contains(run, "GC_REQUIRE_ACCEPTANCE_TOOLING") && strings.Contains(run, "GITHUB_ENV")
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

// stepItemPathPattern matches the breadcrumb of a steps[i] mapping, the only
// place a scalar "run" (the script) or "name" is exempt from the raw scan.
var stepItemPathPattern = regexp.MustCompile(`(^|\.)steps\[\d+\]$`)

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
func requiredJobSkipsFromDoc(workflowName string, doc acceptanceWorkflowDoc, root *yaml.Node, universe acceptanceTestUniverse) ([]skippedRow, error) {
	var out []skippedRow
	anyRequired := false
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
		if !jobRequired {
			continue
		}
		anyRequired = true
		// ga-1ebvc MEDIUM: everything below this point (env: maps,
		// steps[].run) is exactly what acceptanceWorkflowDoc's typed
		// struct has fields for. A -skip hiding anywhere ELSE in this
		// job's YAML -- strategy.matrix, a step's own shell:,
		// defaults.run.shell, container.env, with: -- is invisible to all
		// of it, because the typed decode silently dropped that field.
		// scanJobNodeForBypassSkips walks the job's RAW node instead,
		// catching every scalar the typed struct does not, while leaving
		// "run" values (which are handled below, with the canonical
		// -skip '<expr>' parser) alone.
		if err := scanJobNodeForBypassSkips(workflowName, jobName, findJobNode(root, jobName)); err != nil {
			return nil, err
		}
		for _, step := range job.Steps {
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
	// Workflow-level keys (defaults.run.shell, ...) apply to every job, so
	// when any job is required they are scanned too; jobs are scanned above
	// and `on` carries no execution content.
	if anyRequired && root != nil {
		top := root
		if top.Kind == yaml.DocumentNode && len(top.Content) > 0 {
			top = top.Content[0]
		}
		if top.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(top.Content); i += 2 {
				key := top.Content[i].Value
				if key == "jobs" || key == "on" {
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
// Deny by default, not an enumeration: requiredJobSkipsFromDoc removes every
// canonical `-skip '<expr>'` occurrence and hard-errors on anything
// skip-flag-shaped left in the run script OR in any env value in scope
// (workflow, job, or step, under any key name -- not only GOFLAGS or
// ACCEPTANCE_GO_TEST_FLAGS). That closes `'-skip' 'X'`, `"-skip" "X"`,
// `-skip"" X`, an unquoted value, `-test.skip`, a shell variable holding the
// flag (`ARGS=-skip=X; go test $ARGS`), a bash array
// (`args=(-skip=X); go test "${args[@]}"`), `GOFLAGS+='-skip=X'`, a glued
// quote-concatenation (`-skip 'Allowed”|TestOther'`, `-skip 'Allowed'"|X"`,
// `-skip 'Allowed'$E`, `-skip 'Allowed'\|X`), and an env entry under any
// name (`env: EXTRA: -skip=X`) -- see acceptance_run_skip_allowlist_fixture_test.go
// for a fixture proving each one. stripShellComments only blanks a FULL-LINE
// comment (first non-whitespace character is `#`, outside any quote or line
// continuation); it never touches a `#`
// that shares a line with real content, so `echo "step #1"; go test -skip=X`
// (ci.yml has this exact shape: `echo "## ..."`) cannot have its trailing
// `-skip=X` swallowed by mistaking a quoted `#` for a comment start.
//
// Known remaining limits, stated rather than silently assumed, not silently
// claimed away:
//   - This guard reads `step.run:` text and job/step/workflow `env:` maps --
//     it does not execute shell, so it has no notion of what a script the
//     run: text merely INVOKES (rather than inlines) does; nothing about
//     invoking a checked-in script mentions "skip".
//   - `${{ }}` expansion is not resolved: `go test ${{ inputs.flags }}`, an
//     `env:` value like `F: ${{ vars.F }}`, and a `with:` input all reach
//     this scan as the literal, unresolved `${{ ... }}` text. A `-skip`
//     hiding inside one is not visible as `-skip` at all, canonical or
//     otherwise -- this is a true gap, not a caught-for-the-wrong-reason
//     case, because the literal text contains no skip-flag-shaped token to
//     trip skipFlagTokenPattern either.
//   - GITHUB_ENV: a step that appends to it for a LATER step to read moves
//     the value out of the `env:` maps this guard scans entirely.
//   - Runtime construction the scan cannot decode -- `printf`, ANSI-C
//     quoting (`$'\x2d skip'`), `eval`, or a base64/decode round trip --
//     can build the flag text at run time from characters that never spell
//     "-skip" in the checked-in YAML.
//   - Quote-splicing WITHOUT a glued right-hand alternation still parses
//     (`-s”kip 'X'`, `-sk\ip 'X'`, `"-sk""ip" 'X'`): the shell reassembles
//     these into `-skip 'X'`, but neither skipArgPattern nor
//     skipFlagTokenPattern's literal `-skip`/`--skip`/`-test.skip` spelling
//     recognizes the split form, so it is invisible to both the canonical
//     parser and its deny-by-default fallback -- not merely caught for the
//     wrong reason, but not caught at all.
//
// This list replaces an earlier, broader "never miss" claim this comment
// used to make; review's MEDIUM found it did not hold once quote-splicing
// and `${{ }}` were tried against the actual parser, and a limits section
// that overstates its own reach is worse than a shorter, accurate one.
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

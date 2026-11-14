package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The "Mac regression summary" check is meant to become a required check on
// main. These tests pin what that needs from mac-regression.yml: the summary
// fails closed on every tier the gate selected, no tier hides a failure, the
// integration tiers run whether or not the smoke tier passed, every non-docs
// pull request runs the smoke tier, and the check reports on merge-queue
// entries.

// ghNotCancelled is the step condition that runs unless the run was
// canceled, in GitHub Actions' spelling.
const ghNotCancelled = "${{ !cancelled() }}" //nolint:misspell // GitHub Actions spells it cancelled()

func macRegressionStep(t *testing.T, job, id string) ciCriticalPathStep {
	t.Helper()
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("mac-regression.yml has no %s job", job)
	}
	for _, step := range j.Steps {
		if step.ID == id || step.Name == id {
			return step
		}
	}
	t.Fatalf("mac-regression.yml %s has no step %q", job, id)
	return ciCriticalPathStep{}
}

// runMacGateStep runs the gate's decision script with env and returns its
// GITHUB_OUTPUT as a map.
func runMacGateStep(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	return runMacGateStepAs(t, "gastownhall/gascity", env)
}

// runMacGateStepAs runs the gate script as it would run in repo.
func runMacGateStepAs(t *testing.T, repo string, env map[string]string) map[string]string {
	t.Helper()
	step := macRegressionStep(t, "gate", "gate")
	script := strings.ReplaceAll(step.Run, "${{ github.repository }}", repo)
	if strings.Contains(script, "${{") {
		t.Fatalf("gate script has an expression this test does not substitute:\n%s", script)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "github_output")
	full := map[string]string{"GITHUB_OUTPUT": out}
	for k, v := range env {
		full[k] = v
	}
	if log, err := runWorkflowStepScript(t, dir, script, full); err != nil {
		t.Fatalf("gate script: %v\n%s", err, log)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	return got
}

func TestMacRegressionGateRunsSmokeOnEveryNonDocsPullRequest(t *testing.T) {
	sameRepoPR := map[string]string{
		"EVENT_NAME":     "pull_request",
		"PR_HEAD_REPO":   "gastownhall/gascity",
		"PR_DRAFT":       "false",
		"NEEDS_LABEL":    "false",
		"FILTER_OUTCOME": "success",
		"CODE_CHANGED":   "true",
	}
	with := func(over map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range sameRepoPR {
			env[k] = v
		}
		for k, v := range over {
			env[k] = v
		}
		return env
	}
	cases := []struct {
		name                  string
		env                   map[string]string
		smoke, full, formulas string
		reason                string
	}{
		{name: "code change", env: sameRepoPR, smoke: "true", full: "false", formulas: "false"},
		{name: "docs-only change", env: with(map[string]string{"CODE_CHANGED": "false"}), smoke: "false", full: "false", formulas: "false", reason: "docs-only"},
		{name: "diff not classified", env: with(map[string]string{"FILTER_OUTCOME": "failure", "CODE_CHANGED": ""}), smoke: "true", full: "false", formulas: "false"},
		{name: "needs-mac label", env: with(map[string]string{"NEEDS_LABEL": "true", "CODE_CHANGED": "false"}), smoke: "true", full: "true", formulas: "false"},
		{name: "draft", env: with(map[string]string{"PR_DRAFT": "true"}), smoke: "false", full: "false", formulas: "false", reason: "draft"},
		{name: "fork", env: with(map[string]string{"PR_HEAD_REPO": "someone/gascity"}), smoke: "false", full: "false", formulas: "false", reason: "fork"},
		{name: "merge group", env: map[string]string{"EVENT_NAME": "merge_group"}, smoke: "false", full: "false", formulas: "false", reason: "merge_group"},
		{name: "nightly", env: map[string]string{"EVENT_NAME": "schedule"}, smoke: "true", full: "true", formulas: "true"},
		{name: "dispatch full", env: map[string]string{"EVENT_NAME": "workflow_dispatch", "SUITE_INPUT": "full"}, smoke: "true", full: "true", formulas: "true"},
		{name: "dispatch needs-mac", env: map[string]string{"EVENT_NAME": "workflow_dispatch", "SUITE_INPUT": "needs-mac"}, smoke: "true", full: "true", formulas: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runMacGateStep(t, tc.env)
			if got["run_smoke"] != tc.smoke || got["run_full"] != tc.full || got["run_review_formulas"] != tc.formulas {
				t.Errorf("gate = smoke %q full %q review-formulas %q (%s), want %q %q %q", got["run_smoke"], got["run_full"], got["run_review_formulas"], got["reason"], tc.smoke, tc.full, tc.formulas)
			}
			if !strings.Contains(got["reason"], tc.reason) {
				t.Errorf("gate reason = %q, want it to mention %q", got["reason"], tc.reason)
			}
		})
	}
}

// The gate classifies a pull request's diff as docs-only or not; anything
// outside the docs trees (and the root's markdown files) runs the smoke tier.
// The cmd-gc process shards follow the smoke tier upstream. Off upstream the
// runner policy has no Blacksmith macOS pool, so the hosted runners take
// 20-30+ minutes a shard: there they run nightly, on dispatch, and on an
// explicit needs-mac request only (ga-05vt3.12).
func TestMacRegressionGateRoutesCmdGCProcessShards(t *testing.T) {
	pr := func(repo string, label string) map[string]string {
		return map[string]string{
			"EVENT_NAME":     "pull_request",
			"PR_HEAD_REPO":   repo,
			"PR_DRAFT":       "false",
			"NEEDS_LABEL":    label,
			"FILTER_OUTCOME": "success",
			"CODE_CHANGED":   "true",
		}
	}
	cases := []struct {
		name, repo string
		env        map[string]string
		want       string
	}{
		{name: "upstream pull request", repo: "gastownhall/gascity", env: pr("gastownhall/gascity", "false"), want: "true"},
		{name: "upstream docs-only", repo: "gastownhall/gascity", env: map[string]string{"EVENT_NAME": "pull_request", "PR_HEAD_REPO": "gastownhall/gascity", "PR_DRAFT": "false", "NEEDS_LABEL": "false", "FILTER_OUTCOME": "success", "CODE_CHANGED": "false"}, want: "false"},
		{name: "fork pull request", repo: "Voxist/gascity", env: pr("Voxist/gascity", "false"), want: "false"},
		{name: "fork pull request with needs-mac", repo: "Voxist/gascity", env: pr("Voxist/gascity", "true"), want: "true"},
		{name: "fork nightly", repo: "Voxist/gascity", env: map[string]string{"EVENT_NAME": "schedule"}, want: "true"},
		{name: "fork dispatch", repo: "Voxist/gascity", env: map[string]string{"EVENT_NAME": "workflow_dispatch", "SUITE_INPUT": "smoke"}, want: "true"},
		{name: "fork merge group", repo: "Voxist/gascity", env: map[string]string{"EVENT_NAME": "merge_group"}, want: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runMacGateStepAs(t, tc.repo, tc.env)
			if got["run_cmd_gc_process"] != tc.want {
				t.Errorf("run_cmd_gc_process = %q (smoke %q, %s), want %q", got["run_cmd_gc_process"], got["run_smoke"], got["reason"], tc.want)
			}
		})
	}
}

func TestMacRegressionGateClassifiesDocsOnlyDiffs(t *testing.T) {
	filter := macRegressionStep(t, "gate", "filter")
	if got := filter.With["predicate-quantifier"]; got != "every" {
		t.Errorf("gate filter predicate-quantifier = %q, want every (a file is code only when it matches every pattern)", got)
	}
	var filters map[string][]string
	if err := yaml.Unmarshal([]byte(filter.With["filters"]), &filters); err != nil {
		t.Fatalf("gate filters: %v", err)
	}
	want := []string{"**", "!docs/**", "!engdocs/**", "!*.md"}
	if got := filters["code"]; !slices.Equal(got, want) {
		t.Errorf("gate code filter = %q, want %q", got, want)
	}
	decide := macRegressionStep(t, "gate", "gate")
	for name, want := range map[string]string{
		"FILTER_OUTCOME": "${{ steps.filter.outcome }}",
		"CODE_CHANGED":   "${{ steps.filter.outputs.code }}",
	} {
		if got := decide.Env[name]; got != want {
			t.Errorf("gate decision env %s = %q, want %q", name, got, want)
		}
	}
}

// runMacSummary runs the summary script with the gate's selection and each
// tier's result, and reports whether it passed.
func runMacSummary(t *testing.T, env map[string]string) (bool, string) {
	t.Helper()
	step := macRegressionStep(t, "mac-regression-summary", "Summarize")
	if strings.Contains(step.Run, "${{") {
		t.Fatalf("summary script has an inline expression; pass it through env:\n%s", step.Run)
	}
	dir := t.TempDir()
	full := map[string]string{"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary")}
	for k, v := range env {
		full[k] = v
	}
	out, err := runWorkflowStepScript(t, dir, step.Run, full)
	return err == nil, out
}

func TestMacRegressionSummaryFailsClosed(t *testing.T) {
	smokeTiers := []string{"QUALITY", "UNIT", "CMD_GC", "ACCEPTANCE"}
	fullTiers := []string{"COVER", "INT_PACKAGES", "INT_BDSTORE", "INT_REST"}
	run := func(smoke, full, formulas bool, results map[string]string) map[string]string {
		env := map[string]string{
			"GATE_RESULT":         "success",
			"RUN_SMOKE":           map[bool]string{true: "true", false: "false"}[smoke],
			"RUN_FULL":            map[bool]string{true: "true", false: "false"}[full],
			"RUN_REVIEW_FORMULAS": map[bool]string{true: "true", false: "false"}[formulas],
			"REASON":              "test",
		}
		for _, tier := range append(append(slices.Clone(smokeTiers), fullTiers...), "REVIEW_FORMULAS") {
			env[tier] = "skipped"
		}
		if smoke {
			for _, tier := range smokeTiers {
				env[tier] = "success"
			}
		}
		if full {
			for _, tier := range fullTiers {
				env[tier] = "success"
			}
		}
		if formulas {
			env["REVIEW_FORMULAS"] = "success"
		}
		for k, v := range results {
			env[k] = v
		}
		return env
	}
	cases := []struct {
		name string
		env  map[string]string
		pass bool
	}{
		{name: "gate failed", env: map[string]string{"GATE_RESULT": "failure"}},
		{name: "nothing requested", env: run(false, false, false, nil), pass: true},
		{name: "smoke all green", env: run(true, false, false, nil), pass: true},
		{name: "full all green", env: run(true, true, false, nil), pass: true},
		{name: "everything green", env: run(true, true, true, nil), pass: true},
		{name: "required smoke tier skipped", env: run(true, false, false, map[string]string{"QUALITY": "skipped"})},
		{name: "required smoke tier canceled", env: run(true, false, false, map[string]string{"CMD_GC": "cancelled"})}, //nolint:misspell // GitHub Actions job result
		{name: "required smoke tier missing", env: run(true, false, false, map[string]string{"ACCEPTANCE": ""})},
		{name: "smoke tier failed", env: run(true, true, false, map[string]string{"UNIT": "failure"})},
		{name: "test-cover failed", env: run(true, true, false, map[string]string{"COVER": "failure"})},
		{name: "required full tier skipped", env: run(true, true, false, map[string]string{"INT_PACKAGES": "skipped"})},
		{name: "bdstore failed", env: run(true, true, false, map[string]string{"INT_BDSTORE": "failure"})},
		{name: "review-formulas requested and skipped", env: run(true, true, true, map[string]string{"REVIEW_FORMULAS": "skipped"})},
		{name: "review-formulas failed", env: run(true, true, true, map[string]string{"REVIEW_FORMULAS": "failure"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if pass, out := runMacSummary(t, tc.env); pass != tc.pass {
				t.Fatalf("summary passed = %v, want %v\n%s", pass, tc.pass, out)
			}
		})
	}
}

// No tier reports success over a failed step, and none hides behind
// continue-on-error: the summary reads needs.<job>.result for every tier.
func TestMacRegressionTiersDoNotMaskFailures(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	for name, job := range wf.Jobs {
		if job.ContinueOnError {
			t.Errorf("%s is continue-on-error", name)
		}
		for _, step := range job.Steps {
			if step.ContinueOnError && (name != "gate" || step.ID != "filter") {
				t.Errorf("%s step %q is continue-on-error", name, step.Name)
			}
		}
	}
	summarize := macRegressionStep(t, "mac-regression-summary", "Summarize")
	for name, value := range summarize.Env {
		if strings.Contains(value, "outputs.outcome") {
			t.Errorf("summary env %s = %q reads a step outcome; read the job result", name, value)
		}
	}
	for name, want := range map[string]string{
		"RUN_FULL":            "${{ needs.gate.outputs.run_full }}",
		"RUN_REVIEW_FORMULAS": "${{ needs.gate.outputs.run_review_formulas }}",
		"COVER":               "${{ needs.mac-cover.result }}",
		"INT_BDSTORE":         "${{ needs.mac-integration-bdstore.result }}",
		"REVIEW_FORMULAS":     "${{ needs.mac-integration-review-formulas.result }}",
	} {
		if got := summarize.Env[name]; got != want {
			t.Errorf("summary env %s = %q, want %q", name, got, want)
		}
	}
}

// The integration tiers run whether or not the smoke tier passed: a red smoke
// job must not skip (and so hide) the integration results, and the acceptance
// job's bd contract steps run after a Tier A failure.
func TestMacRegressionIntegrationTiersDoNotCascade(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	for _, name := range []string{"mac-integration-packages", "mac-integration-bdstore", "mac-integration-rest", "mac-integration-review-formulas", "mac-cover"} {
		job, ok := wf.Jobs[name]
		if !ok {
			t.Fatalf("mac-regression.yml has no %s job", name)
		}
		if want := []string{"runner-policy", "gate"}; !slices.Equal(job.Needs, want) {
			t.Errorf("%s needs = %v, want %v", name, job.Needs, want)
		}
	}
	for _, step := range wf.Jobs["mac-acceptance"].Steps {
		if strings.Contains(step.Run, "test-bd-cli-contract") && strings.TrimSpace(step.If) != ghNotCancelled {
			t.Errorf("acceptance step %q if = %q, want %s", step.Name, step.If, ghNotCancelled)
		}
	}
}

// A required check must report on merge-queue entries, or every entry waits
// out the queue's response timeout and fails.
func TestMacRegressionReportsOnMergeGroup(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On          map[string]yaml.Node `yaml:"on"`
		Concurrency struct {
			Group string `yaml:"group"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	var mg struct {
		Types []string `yaml:"types"`
	}
	node, ok := wf.On["merge_group"]
	if ok {
		if err := node.Decode(&mg); err != nil {
			t.Fatalf("on.merge_group: %v", err)
		}
	}
	if !ok || !slices.Equal(mg.Types, []string{"checks_requested"}) {
		t.Errorf("on.merge_group = %+v (present %v), want types [checks_requested]", mg, ok)
	}
	// The needs-mac label dispatches its own run for the same pull request.
	// Sharing a group with the pull_request run would cancel that run, whose
	// summary then reports failure on the PR head.
	if !strings.Contains(wf.Concurrency.Group, "github.event_name") {
		t.Errorf("concurrency group %q does not separate events", wf.Concurrency.Group)
	}
}

// make test-mac refuses to pass over an empty package list (a failed go list
// expands MAC_UNIT_PKGS to nothing, and go test then tests nothing).
func TestMakeTestMacRefusesAnEmptyPackageList(t *testing.T) {
	root := repoRoot(t)
	if out, err := runWorkflowStepScript(t, t.TempDir(), "make -s -C '"+root+"' test-mac-pkgs-guard MAC_UNIT_PKGS=\n", nil); err == nil {
		t.Fatalf("test-mac-pkgs-guard passed with no packages\n%s", out)
	}
	if out, err := runWorkflowStepScript(t, t.TempDir(), "make -s -C '"+root+"' test-mac-pkgs-guard MAC_UNIT_PKGS=./internal/fsys\n", nil); err != nil {
		t.Fatalf("test-mac-pkgs-guard failed with a package: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\ntest-mac: test-mac-pkgs-guard ") {
		t.Error("test-mac does not depend on test-mac-pkgs-guard")
	}
}

// macJobTimeoutMinutes returns the raw timeout-minutes of a mac-regression.yml
// job: a number, or a GitHub expression.
func macJobTimeoutMinutes(t *testing.T, job string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			TimeoutMinutes string `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	j, ok := wf.Jobs[job]
	if !ok || j.TimeoutMinutes == "" {
		t.Fatalf("mac-regression.yml job %s has no timeout-minutes", job)
	}
	return j.TimeoutMinutes
}

// macRepoTernary matches the one expression shape these budgets use:
// upstream's value, else the fork's.
var macRepoTernary = regexp.MustCompile(`^\$\{\{\s*github\.repository == 'gastownhall/gascity' && '?([0-9a-z]+)'? \|\| '?([0-9a-z]+)'?\s*\}\}$`)

// resolveMacRepoValue resolves value as GitHub would in repo: a literal is
// itself; the upstream-or-fork expression picks its side.
func resolveMacRepoValue(t *testing.T, value, repo string) string {
	t.Helper()
	if !strings.Contains(value, "${{") {
		return value
	}
	m := macRepoTernary.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		t.Fatalf("value %q is not the upstream-or-fork repository expression this test resolves", value)
	}
	if repo == "gastownhall/gascity" {
		return m[1]
	}
	return m[2]
}

func parseMinutes(t *testing.T, raw string) time.Duration {
	t.Helper()
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	d, err := time.ParseDuration(raw + "m")
	if err != nil {
		t.Fatalf("parse %q as minutes: %v", raw, err)
	}
	return d
}

// Mac Tier A acceptance keeps upstream's budget upstream (25 min job, 20m
// go test) and gets a larger one off upstream, where hosted macOS runners
// run it in one process: the 2026-11-09 resync's added acceptance tests ran
// past 18 minutes of go test there and the 25 min job was cancelled twice.
// On both, go test's own timeout fires inside the job budget, with room for
// the ~7 min setup and compile, so a hung test is named, not cut off.
func TestMacAcceptanceBudgetsByRepository(t *testing.T) {
	var tierA ciCriticalPathStep
	for _, step := range readCriticalPathWorkflow(t, "mac-regression.yml").Jobs["mac-acceptance"].Steps {
		if strings.TrimSpace(step.Run) == "make test-acceptance-go" {
			tierA = step
		}
	}
	if tierA.Run == "" {
		t.Fatal("mac-acceptance has no make test-acceptance-go step")
	}
	jobBudget := macJobTimeoutMinutes(t, "mac-acceptance")
	for _, tc := range []struct {
		repo           string
		job, goTimeout time.Duration
	}{
		{repo: "gastownhall/gascity", job: 25 * time.Minute, goTimeout: 20 * time.Minute},
		{repo: "Voxist/gascity", job: 45 * time.Minute, goTimeout: 35 * time.Minute},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			job := parseMinutes(t, resolveMacRepoValue(t, jobBudget, tc.repo))
			goTimeout := parseMinutes(t, resolveMacRepoValue(t, tierA.Env["ACCEPTANCE_TIMEOUT"], tc.repo))
			if job != tc.job || goTimeout != tc.goTimeout {
				t.Errorf("job %s, ACCEPTANCE_TIMEOUT %s; want %s and %s", job, goTimeout, tc.job, tc.goTimeout)
			}
			if job-goTimeout < 5*time.Minute {
				t.Errorf("job %s leaves under 5 min for setup and compile before ACCEPTANCE_TIMEOUT %s", job, goTimeout)
			}
		})
	}
}

// processShardGoTimeout runs the cmd-gc process shard step's timeout choice
// (everything before its make call) as repo with go_test_count count, and
// returns the go test timeout it picks.
func processShardGoTimeout(t *testing.T, repo, count string) time.Duration {
	t.Helper()
	return parseMinutes(t, processShardStepValue(t, repo, count, "timeout"))
}

// macRunnerTemp stands in for $RUNNER_TEMP when a test runs a step script.
const macRunnerTemp = "/runner-temp"

// processShardStepValue runs everything before the cmd-gc process shard
// step's make call as repo with go_test_count count, and returns the shell
// variable name as that leaves it.
func processShardStepValue(t *testing.T, repo, count, name string) string {
	t.Helper()
	step := macRegressionStep(t, "mac-cmd-gc-process", "Run cmd/gc process shard")
	var prefix []string
	for _, line := range strings.Split(step.Run, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "make ") {
			break
		}
		prefix = append(prefix, line)
	}
	script := strings.Join(prefix, "\n") + "\nprintf '%s' \"$" + name + "\"\n"
	if strings.Contains(script, "${{") {
		t.Fatalf("process shard step prelude has an expression this test does not substitute:\n%s", script)
	}
	out, err := runWorkflowStepScript(t, t.TempDir(), script, map[string]string{
		"GITHUB_REPOSITORY":   repo,
		"GO_TEST_COUNT_INPUT": count,
		"RUNNER_TEMP":         macRunnerTemp,
	})
	if err != nil {
		t.Fatalf("process shard step prelude: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// The cmd-gc process shards' go test timeout must fire, with its goroutine
// dump, before the job is cancelled, or a hung test goes unnamed. Off
// upstream the job gets 50 minutes and a shard spends ~6 minutes on setup and
// a cold compile before go test starts, so its go timeout is 30m: at 40m the
// fork's nightly lost 11 of 12 shards to the job cap with no dump
// (ga-05vt3.12). Upstream keeps 20m, and a soak (go_test_count above 1) 80m
// under its 90-minute job.
func TestMacProcessShardGoTimeoutFiresBeforeTheJobCap(t *testing.T) {
	jobCap := regexp.MustCompile(`github\.repository == 'gastownhall/gascity' && ([0-9]+) \|\| ([0-9]+)`).
		FindStringSubmatch(macJobTimeoutMinutes(t, "mac-cmd-gc-process"))
	if jobCap == nil {
		t.Fatalf("mac-cmd-gc-process timeout-minutes %q has no upstream-or-fork budget", macJobTimeoutMinutes(t, "mac-cmd-gc-process"))
	}
	for _, tc := range []struct {
		name, repo, count string
		want, job         time.Duration
	}{
		{name: "upstream", repo: "gastownhall/gascity", count: "1", want: 20 * time.Minute, job: parseMinutes(t, jobCap[1])},
		{name: "fork", repo: "Voxist/gascity", count: "1", want: 30 * time.Minute, job: parseMinutes(t, jobCap[2])},
		{name: "fork soak", repo: "Voxist/gascity", count: "3", want: 80 * time.Minute, job: 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := processShardGoTimeout(t, tc.repo, tc.count)
			if got != tc.want {
				t.Errorf("go test timeout = %s, want %s", got, tc.want)
			}
			if tc.job-got < 10*time.Minute {
				t.Errorf("go test timeout %s leaves under 10 min of the %s job for setup and compile, so the job is cancelled before go's goroutine dump", got, tc.job)
			}
		})
	}
}

// Off upstream a cmd-gc process shard streams its progress (ga-05vt3.12):
// every shard of the 2026-11-11 fork nightly reached the job cap still in
// discovery, before go's own timeout starts, and logged nothing. The step
// hands scripts/test-go-test-shard its settings through make's
// EXTRA_TEST_ENV, the one way past the Makefile's env -i: there, progress and
// the binary mac-cmd-gc-process-build compiled once, which the shard runs
// instead of compiling its own. Its SIGQUIT to a
// live test binary must come, a heartbeat late at worst, at least 8 minutes
// before the job cap (about 6 of setup precede the shard, then the dump is
// written), and never during a soak, which has its own 80m budget. Upstream
// runs the shard as before.
func TestMacProcessShardStreamsProgressOffUpstream(t *testing.T) {
	step := macRegressionStep(t, "mac-cmd-gc-process", "Run cmd/gc process shard")
	if !strings.Contains(step.Run, `EXTRA_TEST_ENV="$shard_env"`) {
		t.Fatalf("the process shard's make call does not pass EXTRA_TEST_ENV=\"$shard_env\":\n%s", step.Run)
	}
	jobCap := regexp.MustCompile(`github\.repository == 'gastownhall/gascity' && ([0-9]+) \|\| ([0-9]+)`).
		FindStringSubmatch(macJobTimeoutMinutes(t, "mac-cmd-gc-process"))
	if jobCap == nil {
		t.Fatalf("mac-cmd-gc-process timeout-minutes %q has no upstream-or-fork budget", macJobTimeoutMinutes(t, "mac-cmd-gc-process"))
	}
	forkJob := parseMinutes(t, jobCap[2])
	for _, tc := range []struct {
		name, repo, count string
		want              []string
	}{
		{name: "upstream", repo: "gastownhall/gascity", count: "1", want: nil},
		{name: "upstream soak", repo: "gastownhall/gascity", count: "3", want: nil},
		{name: "fork", repo: "Voxist/gascity", count: "1", want: []string{
			"GO_TEST_SHARD_PREBUILT=" + macCmdGCPrebuiltPath,
			"GO_TEST_SHARD_PROGRESS=1", "GO_TEST_SHARD_PROGRESS_INTERVAL=120", "GO_TEST_SHARD_PROGRESS_QUIT_AFTER=38m",
		}},
		{name: "fork soak", repo: "Voxist/gascity", count: "3", want: []string{
			"GO_TEST_SHARD_PREBUILT=" + macCmdGCPrebuiltPath,
			"GO_TEST_SHARD_PROGRESS=1", "GO_TEST_SHARD_PROGRESS_INTERVAL=120",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Fields(processShardStepValue(t, tc.repo, tc.count, "shard_env"))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("shard_env = %q, want %q", got, tc.want)
			}
			var interval, quitAfter time.Duration
			for _, assignment := range got {
				name, value, _ := strings.Cut(assignment, "=")
				switch name {
				case "GO_TEST_SHARD_PROGRESS_INTERVAL":
					seconds, err := time.ParseDuration(value + "s")
					if err != nil {
						t.Fatalf("GO_TEST_SHARD_PROGRESS_INTERVAL=%q: %v", value, err)
					}
					interval = seconds
				case "GO_TEST_SHARD_PROGRESS_QUIT_AFTER":
					quitAfter = parseMinutes(t, value)
				}
			}
			if quitAfter > 0 && forkJob-(quitAfter+interval) < 8*time.Minute {
				t.Errorf("SIGQUIT at %s plus a %s heartbeat leaves under 8 min of the %s job for setup and the dump", quitAfter, interval, forkJob)
			}
		})
	}
}

// macCmdGCPrebuiltPath is where a fork cmd-gc process shard finds the binary
// mac-cmd-gc-process-build compiled, with $RUNNER_TEMP as macRunnerTemp.
const macCmdGCPrebuiltPath = macRunnerTemp + "/cmd-gc-test/gc.test"

// Off upstream the cmd-gc process shards ran on 3-core, 7 GB hosted macOS
// runners, where compiling cmd/gc's test package (10-12 GB peak) swapped for
// 38-42 minutes in every shard, and the nightly lost all 12 to the 50-minute
// job cap before a test ran (ga-05vt3.12). There one job on a larger runner
// compiles the binary once, through the same make target and script the
// shards use, reports the compile's peak RSS and wall time, and uploads it;
// each shard downloads it and runs it in place of its own compile. Upstream,
// where Blacksmith compiles in minutes, the build job never runs and a shard
// runs exactly as before.
func TestMacCmdGCProcessBuildsOnceOffUpstream(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	build, ok := wf.Jobs["mac-cmd-gc-process-build"]
	if !ok {
		t.Fatal("mac-regression.yml has no mac-cmd-gc-process-build job")
	}
	if build.RunsOn != "macos-15-xlarge" {
		t.Errorf("build runs-on = %q, want the larger hosted runner macos-15-xlarge", build.RunsOn)
	}
	if want := "needs.gate.outputs.run_cmd_gc_process == 'true' && github.repository != 'gastownhall/gascity'"; strings.TrimSpace(build.If) != want {
		t.Errorf("build if = %q, want %q (fork-only, and only when the shards run)", build.If, want)
	}
	if want := []string{"runner-policy", "gate"}; !slices.Equal(build.Needs, want) {
		t.Errorf("build needs = %v, want %v", build.Needs, want)
	}
	if got := parseMinutes(t, macJobTimeoutMinutes(t, "mac-cmd-gc-process-build")); got > 45*time.Minute {
		t.Errorf("build timeout-minutes = %s, want at most 45", got)
	}

	var setup, compile, upload *ciCriticalPathStep
	for i := range build.Steps {
		step := &build.Steps[i]
		switch {
		case step.Uses == "./.github/actions/setup-gascity-macos":
			setup = step
		case strings.Contains(step.Run, "make test-cmd-gc-process-build"):
			compile = step
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			upload = step
		}
	}
	if setup == nil || compile == nil || upload == nil {
		t.Fatalf("build steps: setup %v, compile %v, upload %v; want the shared macOS setup, the make compile and an artifact upload", setup != nil, compile != nil, upload != nil)
	}
	for _, want := range []string{
		"/usr/bin/time -l",
		`CMD_GC_PROCESS_BINARY="$RUNNER_TEMP/cmd-gc-test/gc.test"`,
		"maximum resident set size",
	} {
		if !strings.Contains(compile.Run, want) {
			t.Errorf("build compile step lacks %q:\n%s", want, compile.Run)
		}
	}
	// The tagged product metrics profile is a second cmd/gc test compile of
	// the same size, so off upstream it runs here too, measured the same way
	// (TestProductMetricsTesthookProfileIsFocusedAndRequired pins that it
	// runs nowhere else there).
	var testhook *ciCriticalPathStep
	for i := range build.Steps {
		if strings.Contains(build.Steps[i].Run, "make test-productmetrics-testhook") {
			testhook = &build.Steps[i]
		}
	}
	if testhook == nil {
		t.Fatal("build job does not run make test-productmetrics-testhook")
	}
	for _, want := range []string{"/usr/bin/time -l", "maximum resident set size"} {
		if !strings.Contains(testhook.Run, want) {
			t.Errorf("build testhook step lacks %q:\n%s", want, testhook.Run)
		}
	}
	if upload.With["name"] != "cmd-gc-test-binary" || upload.With["path"] != "${{ runner.temp }}/cmd-gc-test/gc.test" ||
		upload.With["if-no-files-found"] != "error" || upload.With["retention-days"] != "1" {
		t.Errorf("build upload with = %v, want cmd-gc-test-binary from ${{ runner.temp }}/cmd-gc-test/gc.test, failing on no file, kept 1 day", upload.With)
	}

	shards := wf.Jobs["mac-cmd-gc-process"]
	if !slices.Contains(shards.Needs, "mac-cmd-gc-process-build") {
		t.Errorf("shard needs = %v, want mac-cmd-gc-process-build", shards.Needs)
	}
	var download, chmod *ciCriticalPathStep
	for i := range shards.Steps {
		step := &shards.Steps[i]
		switch {
		case strings.HasPrefix(step.Uses, "actions/download-artifact@"):
			download = step
		case strings.Contains(step.Run, "chmod +x"):
			chmod = step
		}
	}
	if download == nil || chmod == nil {
		t.Fatalf("shard steps: download %v, chmod %v; want the artifact download and its chmod (artifacts drop the mode bits)", download != nil, chmod != nil)
	}
	const forkOnly = "github.repository != 'gastownhall/gascity'"
	for _, step := range []*ciCriticalPathStep{download, chmod} {
		if strings.TrimSpace(step.If) != forkOnly {
			t.Errorf("shard step %q if = %q, want %q", step.Name, step.If, forkOnly)
		}
	}
	if download.With["name"] != upload.With["name"] || download.With["path"] != "${{ runner.temp }}/cmd-gc-test" {
		t.Errorf("shard download with = %v, want %s into ${{ runner.temp }}/cmd-gc-test", download.With, upload.With["name"])
	}
	if !strings.Contains(chmod.Run, `"$RUNNER_TEMP/cmd-gc-test/gc.test"`) {
		t.Errorf("shard chmod step does not mark the downloaded binary:\n%s", chmod.Run)
	}
	if got := processShardStepValue(t, "gastownhall/gascity", "1", "shard_env"); strings.Contains(got, "GO_TEST_SHARD_PREBUILT") {
		t.Errorf("upstream shard_env = %q, want no prebuilt binary", got)
	}
}

// The shards' if: runs them upstream as before (where the build job is
// skipped, which alone would skip a dependent job) and, off upstream, only
// once the build succeeded. It never revives a shard the gate routed off or a
// cancelled run.
func TestMacCmdGCProcessShardsRunWhenTheirBinaryIsReady(t *testing.T) {
	shards := readCriticalPathWorkflow(t, "mac-regression.yml").Jobs["mac-cmd-gc-process"]
	cond := strings.TrimSpace(shards.If)
	if inner, ok := strings.CutPrefix(cond, "${{"); ok {
		inner, ok = strings.CutSuffix(inner, "}}")
		if !ok {
			t.Fatalf("shard if %q opens ${{ without closing it", shards.If)
		}
		cond = strings.TrimSpace(inner)
	}
	cond = regexp.MustCompile(`\s+`).ReplaceAllString(cond, " ")
	for _, tc := range []struct {
		name, repo, route, build, cancelled string
		want                                bool
	}{
		{"upstream, build skipped", "gastownhall/gascity", "true", "skipped", "false", true},
		{"upstream, routed off", "gastownhall/gascity", "false", "skipped", "false", false},
		{"upstream, cancelled", "gastownhall/gascity", "true", "skipped", "true", false},
		{"fork, build succeeded", "Voxist/gascity", "true", "success", "false", true},
		{"fork, build failed", "Voxist/gascity", "true", "failure", "false", false},
		{"fork, build skipped", "Voxist/gascity", "true", "skipped", "false", false},
		{"fork, routed off", "Voxist/gascity", "false", "skipped", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr := cond
			for from, to := range map[string]string{
				"!cancelled()": map[string]string{"true": "false", "false": "true"}[tc.cancelled],
				"needs.gate.outputs.run_cmd_gc_process == 'true'":    strconv.FormatBool(tc.route == "true"),
				"github.repository == 'gastownhall/gascity'":         strconv.FormatBool(tc.repo == "gastownhall/gascity"),
				"needs.mac-cmd-gc-process-build.result == 'success'": strconv.FormatBool(tc.build == "success"),
			} {
				if !strings.Contains(expr, from) {
					t.Fatalf("shard if %q lacks %q", cond, from)
				}
				expr = strings.ReplaceAll(expr, from, to)
			}
			got, err := evalBoolExpr(expr)
			if err != nil {
				t.Fatalf("evaluate shard if %q (from %q): %v", expr, cond, err)
			}
			if got != tc.want {
				t.Errorf("shard if %q = %v, want %v", expr, got, tc.want)
			}
		})
	}
}

// evalBoolExpr evaluates an expression of true, false, &&, || and
// parentheses, with && binding tighter than ||, as GitHub's does.
func evalBoolExpr(expr string) (bool, error) {
	tokens := regexp.MustCompile(`true|false|&&|\|\||\(|\)|\S+`).FindAllString(expr, -1)
	pos := 0
	var parseOr func() (bool, error)
	parseAtom := func() (bool, error) {
		if pos >= len(tokens) {
			return false, fmt.Errorf("unexpected end")
		}
		tok := tokens[pos]
		pos++
		switch tok {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "(":
			v, err := parseOr()
			if err != nil {
				return false, err
			}
			if pos >= len(tokens) || tokens[pos] != ")" {
				return false, fmt.Errorf("unbalanced parenthesis")
			}
			pos++
			return v, nil
		}
		return false, fmt.Errorf("unexpected token %q", tok)
	}
	parseAnd := func() (bool, error) {
		v, err := parseAtom()
		for err == nil && pos < len(tokens) && tokens[pos] == "&&" {
			pos++
			var r bool
			r, err = parseAtom()
			v = v && r
		}
		return v, err
	}
	parseOr = func() (bool, error) {
		v, err := parseAnd()
		for err == nil && pos < len(tokens) && tokens[pos] == "||" {
			pos++
			var r bool
			r, err = parseAnd()
			v = v || r
		}
		return v, err
	}
	v, err := parseOr()
	if err == nil && pos != len(tokens) {
		err = fmt.Errorf("trailing token %q", tokens[pos])
	}
	return v, err
}

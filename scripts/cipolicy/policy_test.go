package cipolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type policyDocuments struct {
	ci      map[string]any
	nightly map[string]any
	action  map[string]any
}

func TestCurrentWorkflowsMatchPolicy(t *testing.T) {
	docs := loadPolicyDocuments(t)
	if err := validate(docs.ci, docs.nightly, docs.action); err != nil {
		t.Fatal(err)
	}
}

func TestMakeTestCIPolicyRunsStaticScopeContracts(t *testing.T) {
	// -tags integration is load-bearing: two of the named contracts live in
	// the integration-tagged pr_static_scope_contract_test.go (ga-4h8bu
	// scripts split — the 669s static-scope pipeline test cannot sit in the
	// fast push gate), and a -run alternation naming tests the build does
	// not compile matches nothing and reports ok. The scripts package's
	// TestCIPolicyRunPatternNamesExistingTests guards that class generally.
	const want = "\t$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 -tags integration -run '^(TestPreflightStaticScopesOrdinaryPRsWithoutWeakeningProtectedRuns|TestFullStaticLintExplicitlyOwnsConfiguredGolangCIGovet|TestChangedStaticTargetsScopeLintAndFormattingToTheDiff|TestCIStaticScopeClassifierFailsClosedOutsideValidatedPullRequestMerge)$$' ./scripts"

	makefilePath := filepath.Join("..", "..", "Makefile")
	body, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	_, rest, ok := strings.Cut(string(body), "\ntest-ci-policy:\n")
	if !ok {
		t.Fatal("Makefile has no test-ci-policy target")
	}
	recipe, _, _ := strings.Cut(rest, "\n\n")
	matches := 0
	for _, line := range strings.Split(recipe, "\n") {
		if line == want {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("test-ci-policy recipe must run the focused static-scope contracts with the exact hermetic command:\n%s", want)
	}
}

func TestDisplayLabelsDoNotAffectPolicy(t *testing.T) {
	docs := loadPolicyDocuments(t)
	docs.ci["name"] = "Renamed workflow"
	job(t, docs.ci, "preflight-acceptance")["name"] = "Renamed job"
	step(t, job(t, docs.ci, "preflight-acceptance"), 2)["name"] = "Renamed step"
	step(t, job(t, docs.ci, "preflight-static"), 2)["name"] = "GC_BEADS and test-integration-bdstore are display text only"
	docs.action["name"] = "Renamed action"
	docs.action["description"] = "Renamed action description"
	input(t, docs.action, "dolt-version")["description"] = "Renamed input description"

	if err := validate(docs.ci, docs.nightly, docs.action); err != nil {
		t.Fatalf("display-only rename changed policy: %v", err)
	}
}

func TestExecutionShapeMutationsFailPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, policyDocuments)
	}{
		{
			name: "needs",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "preflight-acceptance")["needs"] = []any{"changes"}
			},
		},
		{
			name: "if",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "integration-shards")["if"] = "false"
			},
		},
		{
			name: "runner",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "integration-shards")["runs-on"] = "ubuntu-latest"
			},
		},
		{
			name: "timeout",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "integration-shards")["timeout-minutes"] = 60
			},
		},
		{
			name: "environment",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "integration-shards")["env"].(map[string]any)["DOLT_VERSION"] = "latest"
			},
		},
		{
			name: "strategy",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "integration-shards")["strategy"].(map[string]any)["fail-fast"] = true
			},
		},
		{
			name: "nested execution field named name",
			mutate: func(t *testing.T, docs policyDocuments) {
				strategy := job(t, docs.ci, "integration-shards")["strategy"].(map[string]any)
				matrix := strategy["matrix"].(map[string]any)
				row := matrix["include"].([]any)[0].(map[string]any)
				row["name"] = "this is matrix data, not a display label"
			},
		},
		{
			name: "uses",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "preflight-acceptance"), 0)["uses"] = "actions/checkout@main"
			},
		},
		{
			name: "run",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "preflight-acceptance"), 2)["run"] = "make test-acceptance-all"
			},
		},
		{
			name: "with",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "integration-shards"), 1)["with"].(map[string]any)["install-claude-cli"] = "true"
			},
		},
		{
			name: "shell",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "preflight-acceptance"), 2)["shell"] = "bash {0}"
			},
		},
		{
			name: "error behavior",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "preflight-acceptance"), 2)["continue-on-error"] = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := loadPolicyDocuments(t)
			tt.mutate(t, docs)
			if err := validate(docs.ci, docs.nightly, docs.action); err == nil {
				t.Fatal("execution-affecting mutation unexpectedly passed")
			}
		})
	}
}

func TestTopologyAndProviderOwnershipMutationsFailPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, policyDocuments)
	}{
		{
			name: "PR trigger",
			mutate: func(t *testing.T, docs policyDocuments) {
				triggerMap(t, docs.ci)["pull_request"].(map[string]any)["types"] = []any{"opened", "synchronize"}
			},
		},
		{
			name: "required filter",
			mutate: func(t *testing.T, docs policyDocuments) {
				changeStep := step(t, job(t, docs.ci, "changes"), 1)
				filters := decodeYAMLMap(t, changeStep["with"].(map[string]any)["filters"].(string))
				filters["beads"] = removeValue(t, filters["beads"], "internal/beads/**")
				changeStep["with"].(map[string]any)["filters"] = encodeYAML(t, filters)
			},
		},
		{
			name: "changes action identity",
			mutate: func(t *testing.T, docs policyDocuments) {
				step(t, job(t, docs.ci, "changes"), 1)["uses"] = "dorny/paths-filter@main"
			},
		},
		{
			name: "changes outputs",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "changes")["outputs"].(map[string]any)["integration"] = "false"
			},
		},
		{
			name: "PR provider override",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "preflight-static")["env"] = map[string]any{"GC_BEADS": "sqlite"}
			},
		},
		{
			name: "wrapped duplicate proof",
			mutate: func(t *testing.T, docs policyDocuments) {
				steps := job(t, docs.ci, "preflight-static")["steps"].([]any)
				steps = append(steps, map[string]any{
					"run": "timeout 15m make test-integration-bdstore",
				})
				job(t, docs.ci, "preflight-static")["steps"] = steps
			},
		},
		{
			name: "quoted duplicate proof",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.ci, "preflight-static"), map[string]any{
					"run": `make test-integration-"bdstore"`,
				})
			},
		},
		{
			name: "backslash-obfuscated duplicate proof",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.ci, "preflight-static"), map[string]any{
					"run": `make test-integration-bd\store`,
				})
			},
		},
		{
			name: "line-continuation duplicate proof",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.ci, "preflight-static"), map[string]any{
					"run": "make test-integration-\\\nbdstore",
				})
			},
		},
		{
			name: "reusable workflow duplicate proof",
			mutate: func(_ *testing.T, docs policyDocuments) {
				docs.ci["jobs"].(map[string]any)["hidden-proof"] = map[string]any{
					"uses": "owner/repo/.github/workflows/proof.yml@0123456789abcdef",
					"with": map[string]any{"target": "test-integration-bdstore"},
				}
			},
		},
		{
			name: "nightly provider outside owner",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.nightly, "tier-b")["env"] = map[string]any{"GC_BEADS": "sqlite"}
			},
		},
		{
			name: "nightly workflow GC_BEADS inheritance",
			mutate: func(_ *testing.T, docs policyDocuments) {
				docs.nightly["env"].(map[string]any)["GC_BEADS"] = "sqlite"
			},
		},
		{
			name: "nightly workflow acceptance provider inheritance",
			mutate: func(_ *testing.T, docs policyDocuments) {
				docs.nightly["env"].(map[string]any)["GC_ACCEPTANCE_BEADS_PROVIDER"] = "sqlite"
			},
		},
		{
			name: "quoted nightly provider outside owner",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.nightly, "tier-b"), map[string]any{
					"run": `env GC_"BEADS"=sqlite true`,
				})
			},
		},
		{
			name: "backslash-obfuscated nightly provider outside owner",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.nightly, "tier-b"), map[string]any{
					"run": `env GC_\B\E\A\D\S=sqlite true`,
				})
			},
		},
		{
			name: "line-continuation nightly provider outside owner",
			mutate: func(t *testing.T, docs policyDocuments) {
				appendJobStep(t, job(t, docs.nightly, "tier-b"), map[string]any{
					"run": "env GC_\\\nBEADS=sqlite true",
				})
			},
		},
		{
			name: "composite action uses",
			mutate: func(t *testing.T, docs policyDocuments) {
				actionStep(t, docs.action, 1)["uses"] = "actions/setup-node@main"
			},
		},
		{
			name: "composite action step order",
			mutate: func(t *testing.T, docs policyDocuments) {
				steps := actionSteps(t, docs.action)
				steps[0], steps[1] = steps[1], steps[0]
			},
		},
		{
			name: "composite action step addition",
			mutate: func(t *testing.T, docs policyDocuments) {
				steps := actionSteps(t, docs.action)
				runs := docs.action["runs"].(map[string]any)
				runs["steps"] = append(steps, map[string]any{"run": "true", "shell": "bash"})
			},
		},
		{
			name: "composite action step removal",
			mutate: func(t *testing.T, docs policyDocuments) {
				steps := actionSteps(t, docs.action)
				docs.action["runs"].(map[string]any)["steps"] = steps[:len(steps)-1]
			},
		},
		{
			name: "composite action outputs",
			mutate: func(_ *testing.T, docs policyDocuments) {
				docs.action["outputs"] = map[string]any{
					"tool-path": map[string]any{
						"description": "Installed tool path",
						"value":       "${{ steps.install.outputs.path }}",
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := loadPolicyDocuments(t)
			tt.mutate(t, docs)
			if err := validate(docs.ci, docs.nightly, docs.action); err == nil {
				t.Fatal("topology mutation unexpectedly passed")
			}
		})
	}
}

func TestProviderSelectorDiscoveryIgnoresShellText(t *testing.T) {
	tests := []string{
		"echo 'GC_BEADS=sqlite'",
		"# GC_BEADS=sqlite",
		"unset GC_BEADS; true",
		"GC_BEADS_CONDITIONAL_WRITES=require go test ./...",
	}

	for _, run := range tests {
		t.Run(run, func(t *testing.T) {
			value := map[string]any{"steps": []any{map[string]any{"run": run}}}
			if match, ok := findJobProviderSelector(value, "job"); ok {
				t.Fatalf("shell text reported as provider selection: %+v", match)
			}
		})
	}
}

func TestProviderSelectorDiscoveryIgnoresMatrixDataNamedEnv(t *testing.T) {
	job := map[string]any{
		"strategy": map[string]any{
			"matrix": map[string]any{
				"include": []any{
					map[string]any{
						"os": "ubuntu-latest",
						"env": map[string]any{
							"GC_BEADS": "sqlite",
						},
					},
				},
			},
		},
	}

	if match, ok := findJobProviderSelector(job, "job"); ok {
		t.Fatalf("matrix data reported as provider selection: %+v", match)
	}
}

func TestProviderSelectorDiscoveryChecksEnvironmentPositions(t *testing.T) {
	tests := []struct {
		name     string
		job      map[string]any
		wantPath string
	}{
		{
			name:     "job",
			job:      map[string]any{"env": map[string]any{"GC_BEADS": "sqlite"}},
			wantPath: "job.env.GC_BEADS",
		},
		{
			name: "step",
			job: map[string]any{"steps": []any{
				map[string]any{"env": map[string]any{"GC_BEADS": "sqlite"}},
			}},
			wantPath: "job.steps[0].env.GC_BEADS",
		},
		{
			name: "container",
			job: map[string]any{"container": map[string]any{
				"env": map[string]any{"GC_BEADS": "sqlite"},
			}},
			wantPath: "job.container.env.GC_BEADS",
		},
		{
			name: "service",
			job: map[string]any{"services": map[string]any{
				"database": map[string]any{
					"env": map[string]any{"GC_BEADS": "sqlite"},
				},
			}},
			wantPath: "job.services.database.env.GC_BEADS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, ok := findJobProviderSelector(tt.job, "job")
			if !ok || match.path != tt.wantPath {
				t.Fatalf("match = %+v, found = %v, want path %q", match, ok, tt.wantPath)
			}
		})
	}
}

func TestReusableWorkflowFieldsAreExecutionFields(t *testing.T) {
	job := map[string]any{
		"name":    "display only",
		"uses":    "owner/repo/.github/workflows/proof.yml@0123456789abcdef",
		"with":    map[string]any{"target": "test-integration-bdstore"},
		"secrets": "inherit",
	}

	want := copyValue(job).(map[string]any)
	delete(want, "name")
	if got := projectJob(job); !reflect.DeepEqual(got, want) {
		t.Fatalf("reusable workflow projection = %#v, want %#v", got, want)
	}
}

func TestChangesFilterErrorsAreDeterministic(t *testing.T) {
	docs := loadPolicyDocuments(t)
	changeStep := step(t, job(t, docs.ci, "changes"), 1)
	filters := decodeYAMLMap(t, changeStep["with"].(map[string]any)["filters"].(string))
	filters["beads"] = removeValue(t, filters["beads"], "internal/beads/**")
	filters["mail"] = removeValue(t, filters["mail"], "internal/mail/**")
	changeStep["with"].(map[string]any)["filters"] = encodeYAML(t, filters)

	for attempt := 0; attempt < 100; attempt++ {
		err := validateChangesJob(docs.ci)
		if err == nil || !strings.Contains(err.Error(), `changes filter "beads"`) {
			t.Fatalf("attempt %d error = %v, want lexicographically first broken filter", attempt, err)
		}
	}
}

func TestProviderOwnershipErrorIdentifiesExecutionField(t *testing.T) {
	docs := loadPolicyDocuments(t)
	job(t, docs.nightly, "tier-b")["env"] = map[string]any{"GC_BEADS": "sqlite"}

	err := validate(docs.ci, docs.nightly, docs.action)
	if err == nil || !strings.Contains(err.Error(), "nightly.jobs.tier-b.env.GC_BEADS") {
		t.Fatalf("error = %v, want exact provider field path", err)
	}
}

func loadPolicyDocuments(t *testing.T) policyDocuments {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	return policyDocuments{
		ci:      readYAMLMap(t, filepath.Join(root, ".github", "workflows", "ci.yml")),
		nightly: readYAMLMap(t, filepath.Join(root, ".github", "workflows", "nightly.yml")),
		action: readYAMLMap(
			t,
			filepath.Join(root, ".github", "actions", "setup-gascity-ubuntu", "action.yml"),
		),
	}
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return decodeYAMLMap(t, string(data))
}

func decodeYAMLMap(t *testing.T, source string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := yaml.Unmarshal([]byte(source), &result); err != nil {
		t.Fatalf("decode YAML: %v", err)
	}
	return result
}

func encodeYAML(t *testing.T, value any) string {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatalf("encode YAML: %v", err)
	}
	return string(data)
}

func job(t *testing.T, workflow map[string]any, name string) map[string]any {
	t.Helper()
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		t.Fatal("workflow jobs are not a mapping")
	}
	value, ok := jobs[name].(map[string]any)
	if !ok {
		t.Fatalf("workflow job %q is not a mapping", name)
	}
	return value
}

func step(t *testing.T, job map[string]any, index int) map[string]any {
	t.Helper()
	steps, ok := job["steps"].([]any)
	if !ok || index < 0 || index >= len(steps) {
		t.Fatalf("job step %d is unavailable", index)
	}
	value, ok := steps[index].(map[string]any)
	if !ok {
		t.Fatalf("job step %d is not a mapping", index)
	}
	return value
}

func appendJobStep(t *testing.T, job map[string]any, value map[string]any) {
	t.Helper()
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatal("job steps are not a list")
	}
	job["steps"] = append(steps, value)
}

func triggerMap(t *testing.T, workflow map[string]any) map[string]any {
	t.Helper()
	value, ok := workflow["on"].(map[string]any)
	if !ok {
		t.Fatal("workflow triggers are not a mapping")
	}
	return value
}

func input(t *testing.T, action map[string]any, name string) map[string]any {
	t.Helper()
	inputs, ok := action["inputs"].(map[string]any)
	if !ok {
		t.Fatal("action inputs are not a mapping")
	}
	value, ok := inputs[name].(map[string]any)
	if !ok {
		t.Fatalf("action input %q is not a mapping", name)
	}
	return value
}

func actionSteps(t *testing.T, action map[string]any) []any {
	t.Helper()
	runs, ok := action["runs"].(map[string]any)
	if !ok {
		t.Fatal("action runs are not a mapping")
	}
	steps, ok := runs["steps"].([]any)
	if !ok {
		t.Fatal("action steps are not a list")
	}
	return steps
}

func actionStep(t *testing.T, action map[string]any, index int) map[string]any {
	t.Helper()
	steps := actionSteps(t, action)
	if index < 0 || index >= len(steps) {
		t.Fatalf("action step %d is unavailable", index)
	}
	value, ok := steps[index].(map[string]any)
	if !ok {
		t.Fatalf("action step %d is not a mapping", index)
	}
	return value
}

func removeValue(t *testing.T, value any, remove string) []any {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatal("filter paths are not a list")
	}
	result := make([]any, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); !ok || text != remove {
			result = append(result, item)
		}
	}
	if len(result) == len(values) {
		t.Fatalf("filter path %q was not present", remove)
	}
	return result
}

func TestPolicyErrorsIdentifyTheBrokenContract(t *testing.T) {
	docs := loadPolicyDocuments(t)
	job(t, docs.ci, "integration-shards")["runs-on"] = "ubuntu-latest"

	err := validate(docs.ci, docs.nightly, docs.action)
	if err == nil || !strings.Contains(err.Error(), "integration-shards") {
		t.Fatalf("error = %v, want integration-shards context", err)
	}
}

// TestLintDoesNotRunBehindTheBoundaryGuards pins the ga-dejjh split. The
// boundary guards and the native-DoltLite suite are ~2 minutes; golangci-lint
// is 13-15 on a PR run and is the whole critical path. Running them in one
// job put the cheap work in series ahead of the expensive work for no reason.
// A future edit that moves lint back alongside the guards — or drops either
// job from the gates — silently restores that, so both halves are pinned here:
// they are separate jobs, and both are listed in the needs of check and
// ci-preflight. (ci-required does not name them directly; it reaches them
// through ci-preflight, which is why the loop below asserts the two jobs that
// do list them.)
func TestLintDoesNotRunBehindTheBoundaryGuards(t *testing.T) {
	docs := loadPolicyDocuments(t)

	guards, err := workflowJob(docs.ci, "preflight-guards")
	if err != nil {
		t.Fatalf("preflight-guards: %v", err)
	}
	guardSteps, err := mappingSlice(guards["steps"], "preflight-guards steps")
	if err != nil {
		t.Fatal(err)
	}
	static, err := workflowJob(docs.ci, "preflight-static")
	if err != nil {
		t.Fatalf("preflight-static: %v", err)
	}
	staticSteps, err := mappingSlice(static["steps"], "preflight-static steps")
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range splitViolations(guardSteps, staticSteps) {
		t.Error(violation)
	}

	// Splitting a job is only safe if both halves still gate.
	for _, gate := range []string{"check", "ci-preflight"} {
		job, err := workflowJob(docs.ci, gate)
		if err != nil {
			t.Fatalf("%s: %v", gate, err)
		}
		needs, ok := job["needs"].([]any)
		if !ok {
			t.Fatalf("%s needs is not a list: %T", gate, job["needs"])
		}
		for _, want := range []string{"preflight-static", "preflight-guards"} {
			found := false
			for _, n := range needs {
				if text, ok := n.(string); ok && text == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s does not require %s; a failure there would not fail the gate", gate, want)
			}
		}
	}
}

// lintRunSteps and guardRunSteps are the COMPLETE command sets each job owns.
// A partial list lets a partial re-merge through: an edit that moved only
// `make fmt-check-changed` back into the guards job would re-serialize that
// work while a list naming just `make fmt-check` stayed green.
var (
	lintRunSteps = []string{
		"make lint", "make lint-affected",
		"make fmt-check", "make fmt-check-changed",
		"make vet", "make check-docs",
	}
	guardRunSteps = []string{
		"make test-ci-policy",
		"make check-gomod-replace",
		"make check-native-dependency-surface",
		"make check-eventexport-isolation",
		"make check-core-boundary",
		"make test-native-doltlite-beads",
	}
)

// countSteps counts steps whose field equals value. findStep cannot be used
// for an ABSENCE assertion: it returns -1 both when a step is missing and when
// it appears more than once (its ambiguity guard), so `findStep(...) >= 0`
// reads a DUPLICATED step as absent.
//
// No run string is duplicated in ci.yml today, and
// TestPreflightStaticScopesOrdinaryPRsWithoutWeakeningProtectedRuns pins the
// scope pair to one step each — the pair runs DIFFERENT commands
// (make lint-affected vs make lint, make fmt-check-changed vs make fmt-check).
// The duplicate shape is what a bad merge produces, which is exactly when this
// tripwire has to bite, so it is guarded rather than assumed away.
// TestCountStepsSeesDuplicates covers it.
func countSteps(steps []map[string]any, field, value string) int {
	n := 0
	for _, step := range steps {
		if step[field] == value {
			n++
		}
	}
	return n
}

// splitViolations reports run strings that landed in the wrong half of the
// ga-dejjh split: lint commands in the guards job, guard commands in the static
// job. It takes the step lists rather than reading ci.yml so that
// TestSplitViolationsSeeDuplicatedSteps can hand it the shape a bad merge
// makes, which the real workflow never has.
func splitViolations(guardSteps, staticSteps []map[string]any) []string {
	var violations []string
	for _, run := range lintRunSteps {
		if countSteps(guardSteps, "run", run) > 0 {
			violations = append(violations, fmt.Sprintf("preflight-guards runs %q; the lint work belongs in preflight-static so it does not queue behind the guards", run))
		}
	}
	for _, run := range guardRunSteps {
		if countSteps(staticSteps, "run", run) > 0 {
			violations = append(violations, fmt.Sprintf("preflight-static runs %q; the guards belong in preflight-guards so lint is not serialized behind them", run))
		}
	}
	return violations
}

// TestSplitViolationsSeeDuplicatedSteps is the regression guard for the swap
// this PR makes: the split check must catch a forbidden step that a merge left
// in TWICE, which `findStep(...) >= 0` read as absent.
//
// The real ci.yml holds no duplicate, so without a fixture the two call sites
// could be reverted to findStep and the whole suite would stay green — the
// regression could silently re-land. Each case below fails if that revert
// happens.
func TestSplitViolationsSeeDuplicatedSteps(t *testing.T) {
	for _, tc := range []struct {
		name         string
		guardSteps   []map[string]any
		staticSteps  []map[string]any
		wantContains string
	}{
		{
			name: "lint duplicated into the guards job",
			guardSteps: []map[string]any{
				{"run": "make test-ci-policy"},
				{"run": "make lint"},
				{"run": "make check-gomod-replace"},
				{"run": "make lint"},
			},
			wantContains: "preflight-guards runs \"make lint\"",
		},
		{
			name: "a guard duplicated into the static job",
			staticSteps: []map[string]any{
				{"run": "make check-core-boundary"},
				{"run": "make lint"},
				{"run": "make check-core-boundary"},
			},
			wantContains: "preflight-static runs \"make check-core-boundary\"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitViolations(tc.guardSteps, tc.staticSteps)
			if len(got) == 0 {
				t.Fatalf("a duplicated step was reported as absent; findStep's -1 ambiguity is back")
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.wantContains) {
				t.Errorf("violations = %q, want one mentioning %s", got, tc.wantContains)
			}
		})
	}

	// The single-step case must still be caught, and a clean split must stay
	// silent, or the guard would be trivially satisfied.
	if got := splitViolations([]map[string]any{{"run": "make vet"}}, nil); len(got) != 1 {
		t.Errorf("a single misplaced step produced %d violations, want 1", len(got))
	}
	if got := splitViolations([]map[string]any{{"run": "make test-ci-policy"}}, []map[string]any{{"run": "make lint"}}); len(got) != 0 {
		t.Errorf("a correct split reported %q, want no violations", got)
	}
}

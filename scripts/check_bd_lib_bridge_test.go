package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	bridgeTestLibRef    = "3c205e0a41e42c01be5dd2c1df6bf870b6104482"
	bridgeTestBridgeRef = "872c78ec288f2540672e60fd9926c1490e0732ca"
	bridgeTestBase      = "125c62063406d064d8392f3245920b47e9c81f85"
	bridgeTestHead      = "4af8ad95e8e35907994c4bee62e27ee4b2371703"

	// bridgeTestPRDiff is a pinned upstream PR range changing one production
	// file and one test file.
	bridgeTestPRDiff = `diff --git a/proxy_data.go b/proxy_data.go
--- a/proxy_data.go
+++ b/proxy_data.go
@@ -1,2 +1,3 @@
 package beads
+func DialProxyData() {}
-// old
diff --git a/internal/creds/command_test.go b/internal/creds/command_test.go
--- a/internal/creds/command_test.go
+++ b/internal/creds/command_test.go
@@ -1,2 +1,3 @@
 {key: "A"},
+{key: "BEADS_DOLT_SERVER_PROXY_ROOT"},
`
	// bridgeTestOnlyProd is the range without its test hunk: what a
	// cherry-pick onto an older base carries when the test table is not
	// there yet.
	bridgeTestOnlyProd = `diff --git a/proxy_data.go b/proxy_data.go
--- a/proxy_data.go
+++ b/proxy_data.go
@@ -1,2 +1,3 @@
 package beads
+func DialProxyData() {}
-// old
`
	bridgeTestAnnotation = "::warning title=Beads library bridge::"
	// bridgeTestUnreachable refuses every connection at once: a network
	// lookup that fails, without a network.
	bridgeTestUnreachable = "http://127.0.0.1:1"
)

func bridgeTestDepsEnv(base, head string) string {
	return "BD_REPO=Voxist/beads\nBD_LIB_REF=" + bridgeTestLibRef +
		"\nBD_LIB_BRIDGE_REF=" + bridgeTestBridgeRef +
		"\nBD_LIB_BRIDGE_UPSTREAM_PR=gastownhall/beads#8412" +
		"\nBD_LIB_BRIDGE_UPSTREAM_BASE=" + base +
		"\nBD_LIB_BRIDGE_UPSTREAM_HEAD=" + head + "\n"
}

// runBridgeCheck runs scripts/check-bd-lib-bridge against a fabricated
// deps.env, as GitHub Actions would, and returns its output and error.
func runBridgeCheck(t *testing.T, depsEnv string, args ...string) (string, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "deps.env"), []byte(depsEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "check-bd-lib-bridge"), append([]string{"--repo-root", root}, args...)...)
	cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=true", "GITHUB_TOKEN=", "GH_TOKEN=",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeBridgeTestFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diff")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckBDLibBridge drives scripts/check-bd-lib-bridge's network-side rules
// over fabricated lookups, so each refusal and each warning is proven to fire.
func TestCheckBDLibBridge(t *testing.T) {
	bridged := bridgeTestDepsEnv(bridgeTestBase, bridgeTestHead)
	for _, tc := range []struct {
		name       string
		depsEnv    string
		parents    string
		bridgeDiff string
		state      string
		head       string
		want       string // "" means the check must pass
		// wantWarn, when set, is a warning the passing check must print.
		wantWarn string
	}{
		{name: "the pinned range on BD_LIB_REF", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "open", head: bridgeTestHead},
		{name: "the pinned range less a test hunk", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestOnlyProd, state: "open", head: bridgeTestHead},
		{name: "no bridge declared", depsEnv: "BD_REPO=Voxist/beads\nBD_LIB_REF=" + bridgeTestLibRef + "\n"},
		{name: "a bridge on another parent", depsEnv: bridged, parents: strings.Repeat("a", 40), bridgeDiff: bridgeTestPRDiff, state: "open", head: bridgeTestHead, want: "want exactly BD_LIB_REF"},
		{name: "a bridge with two parents", depsEnv: bridged, parents: bridgeTestLibRef + " " + strings.Repeat("b", 40), bridgeDiff: bridgeTestPRDiff, state: "open", head: bridgeTestHead, want: "want exactly BD_LIB_REF"},
		{
			name: "a fork-only line", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", head: bridgeTestHead, want: "the pinned PR range does not carry",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/proxy_data.go b/proxy_data.go\n+// fork only\n",
		},
		{
			name: "a fork-only file", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", head: bridgeTestHead, want: "fork-only delta",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/cmd/bd/fork.go b/cmd/bd/fork.go\n--- a/cmd/bd/fork.go\n+++ b/cmd/bd/fork.go\n+package main\n",
		},
		{
			name: "a production hunk dropped", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", head: bridgeTestHead, want: "drops 1 line",
			bridgeDiff: strings.Replace(bridgeTestPRDiff, "-// old\n", "", 1),
		},
		// A merged or closed PR (maintainers often absorb a PR into their own
		// commit and close it unmerged) is the bridge owner's next change, not
		// a failure of whatever PR happens to run CI.
		{
			name: "the PR merged", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "merged", head: bridgeTestHead,
			wantWarn: "carries gastownhall/beads#8412, which is merged, not open: drop the bridge",
		},
		{
			name: "the PR closed unmerged", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "closed", head: bridgeTestHead,
			wantWarn: "carries gastownhall/beads#8412, which is closed, not open: drop the bridge",
		},
		// A push to the PR moves its head, not the pinned range the bridge is
		// checked against: a warning to rebuild, never a failure.
		{
			name: "the PR head moved past the pinned one", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "open", head: strings.Repeat("c", 40),
			wantWarn: "head moved from the pinned 4af8ad95e8e3 to cccccccccccc; rebuild the bridge",
		},
		// The structural checks stay hard failures whatever the PR's state.
		{name: "a merged PR does not excuse another parent", depsEnv: bridged, parents: strings.Repeat("a", 40), bridgeDiff: bridgeTestPRDiff, state: "merged", head: bridgeTestHead, want: "want exactly BD_LIB_REF"},
		{
			name: "a moved head does not excuse a fork-only line", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", head: strings.Repeat("c", 40), want: "the pinned PR range does not carry",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/proxy_data.go b/proxy_data.go\n+// fork only\n",
		},
		{name: "a malformed upstream PR", depsEnv: strings.Replace(bridged, "gastownhall/beads#8412", "Voxist/beads#80", 1), want: "want gastownhall/beads#<n>"},
		{name: "an unpinned PR head", depsEnv: bridgeTestDepsEnv(bridgeTestBase, ""), want: "BD_LIB_BRIDGE_UPSTREAM_HEAD = ''"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runBridgeCheck(t, tc.depsEnv,
				"--bridge-parents", tc.parents,
				"--bridge-diff", writeBridgeTestFile(t, tc.bridgeDiff),
				"--pr-diff", writeBridgeTestFile(t, bridgeTestPRDiff),
				"--pr-state", tc.state,
				"--pr-head", tc.head,
			)
			assertBridgeCheck(t, out, err, tc.want, tc.wantWarn)
			if tc.wantWarn != "" && !strings.Contains(out, "BD_LIB_BRIDGE") {
				t.Fatalf("the warning does not name the step:\n%s", out)
			}
		})
	}
}

// TestCheckBDLibBridgeNetworkFailuresWarn proves a lookup that cannot reach
// GitHub is a warning naming what went unchecked, never an uncaught error that
// fails an unrelated PR. Each lookup is pointed at a port that refuses.
func TestCheckBDLibBridgeNetworkFailuresWarn(t *testing.T) {
	bridged := bridgeTestDepsEnv(bridgeTestBase, bridgeTestHead)
	for _, tc := range []struct {
		name     string
		args     []string
		wantWarn string
	}{
		{
			name: "the bridge fetch", wantWarn: "could not fetch the bridge commit",
			args: []string{"--github-url", bridgeTestUnreachable, "--pr-state", "open", "--pr-head", bridgeTestHead},
		},
		{
			name: "the pinned range fetch", wantWarn: "could not fetch the pinned gastownhall/beads#8412 range",
			args: []string{
				"--github-url", bridgeTestUnreachable, "--bridge-parents", bridgeTestLibRef,
				"--bridge-diff", writeBridgeTestFile(t, bridgeTestPRDiff), "--pr-state", "open", "--pr-head", bridgeTestHead,
			},
		},
		{
			name: "the PR state lookup", wantWarn: "could not look up gastownhall/beads#8412",
			args: []string{
				"--api-url", bridgeTestUnreachable, "--bridge-parents", bridgeTestLibRef,
				"--bridge-diff", writeBridgeTestFile(t, bridgeTestPRDiff), "--pr-diff", writeBridgeTestFile(t, bridgeTestPRDiff),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runBridgeCheck(t, bridged, tc.args...)
			assertBridgeCheck(t, out, err, "", tc.wantWarn)
			if !strings.Contains(out, "unchecked:") {
				t.Fatalf("the summary does not name what went unchecked:\n%s", out)
			}
			if strings.Contains(out, "Traceback") {
				t.Fatalf("the network failure escaped as an exception:\n%s", out)
			}
		})
	}
}

// TestCheckBDLibBridgeVerifiesThePinnedRange runs the range check over real
// git: fixture repositories stand in for BD_REPO and gastownhall/beads, served
// through file:// with --github-url. The bridge carries the pinned range; a
// base that is not the head's ancestor is a hard failure.
func TestCheckBDLibBridgeVerifiesThePinnedRange(t *testing.T) {
	hosting := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(dir, file, body, msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git(dir, "add", "-A")
		git(dir, "commit", "--quiet", "-m", msg)
		return git(dir, "rev-parse", "HEAD")
	}
	for _, repo := range []string{"Voxist/beads", "gastownhall/beads"} {
		dir := filepath.Join(hosting, repo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		git(dir, "init", "--quiet", "--initial-branch=main")
		git(dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	}
	upstream := filepath.Join(hosting, "gastownhall/beads")
	libRef := commit(upstream, "proxy_data.go", "package beads\n", "lib ref")
	base := commit(upstream, "other.go", "package beads\n", "pr base")
	head := commit(upstream, "proxy_data.go", "package beads\nfunc DialProxyData() {}\n", "the PR")
	unrelated := git(upstream, "commit-tree", "-m", "unrelated", git(upstream, "rev-parse", "HEAD^{tree}"))
	git(upstream, "update-ref", "refs/heads/unrelated", unrelated)

	fork := filepath.Join(hosting, "Voxist/beads")
	git(fork, "fetch", "--quiet", upstream, libRef)
	git(fork, "checkout", "--quiet", "-B", "bridge", libRef)
	bridge := commit(fork, "proxy_data.go", "package beads\nfunc DialProxyData() {}\n", "bridge")

	pins := func(repo, bridgeRef, base, head string) string {
		return "BD_REPO=" + repo + "\nBD_LIB_REF=" + libRef + "\nBD_LIB_BRIDGE_REF=" + bridgeRef +
			"\nBD_LIB_BRIDGE_UPSTREAM_PR=gastownhall/beads#8412\nBD_LIB_BRIDGE_UPSTREAM_BASE=" + base +
			"\nBD_LIB_BRIDGE_UPSTREAM_HEAD=" + head + "\n"
	}
	depsEnv := func(base string) string { return pins("Voxist/beads", bridge, base, head) }
	url := "file://" + hosting
	const missing = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	t.Run("the bridge carries the pinned range", func(t *testing.T) {
		out, err := runBridgeCheck(t, depsEnv(base), "--github-url", url, "--pr-state", "open", "--pr-head", head)
		assertBridgeCheck(t, out, err, "", "")
	})
	t.Run("a base that is not the head's ancestor", func(t *testing.T) {
		out, err := runBridgeCheck(t, depsEnv(unrelated), "--github-url", url, "--pr-state", "open", "--pr-head", head)
		assertBridgeCheck(t, out, err, "is not an ancestor of BD_LIB_BRIDGE_UPSTREAM_HEAD", "")
	})
	// A remote that ANSWERS and refuses is not an outage. Before, these warned
	// "unchecked: diff" and exited 0, so an unfetchable pin (a force-pushed
	// and collected PR head) switched the diff check off.
	t.Run("an unfetchable pinned head fails", func(t *testing.T) {
		out, err := runBridgeCheck(t, pins("Voxist/beads", bridge, base, missing), "--github-url", url, "--pr-state", "open", "--pr-head", missing)
		assertBridgeCheck(t, out, err, "range (BD_LIB_BRIDGE_UPSTREAM_BASE..HEAD) cannot be fetched", "")
		if !strings.Contains(out, "not our ref") {
			t.Fatalf("the refusal does not carry git's own answer:\n%s", out)
		}
	})
	t.Run("a refused bridge ref fails", func(t *testing.T) {
		out, err := runBridgeCheck(t, pins("Voxist/beads", missing, base, head), "--github-url", url, "--pr-state", "open", "--pr-head", head)
		assertBridgeCheck(t, out, err, "the bridge commit BD_LIB_BRIDGE_REF "+missing+" cannot be fetched", "")
	})
	t.Run("a bridge repository that is not there fails", func(t *testing.T) {
		out, err := runBridgeCheck(t, pins("Voxist/no-such-beads", bridge, base, head), "--github-url", url, "--pr-state", "open", "--pr-head", head)
		assertBridgeCheck(t, out, err, "cannot be fetched from Voxist/no-such-beads", "")
	})
}

// TestCheckBDLibBridgeClassifiesFetchFailures pins the classifier over the
// stderr git really prints (captured from git 2.x against GitHub, file:// and a
// closed port; every one exits 128, so the exit code cannot tell them apart).
// Only a failure that never got an answer may warn; anything else fails.
func TestCheckBDLibBridgeClassifiesFetchFailures(t *testing.T) {
	cases := map[string]string{
		"fatal: remote error: upload-pack: not our ref deadbeefdeadbeefdeadbeefdeadbeefdeadbeef":                                    "refused",
		"fatal: git upload-pack: not our ref deadbeef":                                                                              "refused",
		"fatal: couldn't find remote ref refs/heads/gone":                                                                           "refused",
		"ERROR: Repository not found.\nfatal: Could not read from remote repository.":                                               "refused",
		"fatal: '/x/nope' does not appear to be a git repository":                                                                   "refused",
		"fatal: unable to access 'http://127.0.0.1:1/x/': Failed to connect to 127.0.0.1:1 after 4 ms: Could not connect to server": "transport",
		"fatal: unable to access 'https://no-such-host.invalid/x/': Could not resolve host: no-such-host.invalid":                   "transport",
		"fatal: unable to access 'https://github.com/x/': The requested URL returned error: 502":                                    "transport",
		"fatal: unable to access 'https://github.com/x/': SSL certificate problem: unable to get local issuer certificate":          "transport",
		"fatal: unable to access 'https://github.com/x/': Operation timed out after 30000 milliseconds":                             "transport",
		"error: something nobody has seen before":                                                                                   "unknown",
	}
	script := "import importlib.util, json, sys\n" +
		"from importlib.machinery import SourceFileLoader\n" +
		"loader = SourceFileLoader('bridge', sys.argv[1])\n" +
		"spec = importlib.util.spec_from_loader('bridge', loader)\n" +
		"mod = importlib.util.module_from_spec(spec)\n" +
		"loader.exec_module(mod)\n" +
		"cases = json.loads(sys.stdin.read())\n" +
		"print(json.dumps({k: mod.classify_fetch_failure(k) for k in cases}))\n"
	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", "-c", script, filepath.Join(repoRoot(t), "scripts", "check-bd-lib-bridge"))
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("classify: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	for stderr, want := range cases {
		if got[stderr] != want {
			t.Errorf("classify_fetch_failure(%q) = %q, want %q", stderr, got[stderr], want)
		}
	}
}

func assertBridgeCheck(t *testing.T, out string, err error, want, wantWarn string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("the check refused a valid bridge: %v\n%s", err, out)
	case want != "" && err == nil:
		t.Fatalf("the check passed; want a refusal naming %q\n%s", want, out)
	case want != "" && !strings.Contains(out, want):
		t.Fatalf("the refusal does not name %q:\n%s", want, out)
	}
	annotated := strings.Contains(out, bridgeTestAnnotation)
	switch {
	case wantWarn != "" && (!annotated || !strings.Contains(out, wantWarn)):
		t.Fatalf("the check did not warn %q as a GitHub annotation:\n%s", wantWarn, out)
	case wantWarn == "" && want == "" && annotated:
		t.Fatalf("the check warned with nothing to warn about:\n%s", out)
	}
}

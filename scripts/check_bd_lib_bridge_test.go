package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	bridgeTestLibRef    = "3c205e0a41e42c01be5dd2c1df6bf870b6104482"
	bridgeTestBridgeRef = "872c78ec288f2540672e60fd9926c1490e0732ca"

	// bridgeTestPRDiff is an upstream PR changing one production file and one
	// test file.
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
	// bridgeTestOnlyProd is the PR without its test hunk: what a cherry-pick
	// onto an older base carries when the test table is not there yet.
	bridgeTestOnlyProd = `diff --git a/proxy_data.go b/proxy_data.go
--- a/proxy_data.go
+++ b/proxy_data.go
@@ -1,2 +1,3 @@
 package beads
+func DialProxyData() {}
-// old
`
)

// TestCheckBDLibBridge drives scripts/check-bd-lib-bridge's network-side rules
// over fabricated lookups, so each refusal is proven to fire.
func TestCheckBDLibBridge(t *testing.T) {
	bridged := "BD_REPO=Voxist/beads\nBD_LIB_REF=" + bridgeTestLibRef +
		"\nBD_LIB_BRIDGE_REF=" + bridgeTestBridgeRef + "\nBD_LIB_BRIDGE_UPSTREAM_PR=gastownhall/beads#8412\n"
	for _, tc := range []struct {
		name       string
		depsEnv    string
		parents    string
		bridgeDiff string
		state      string
		want       string // "" means the check must pass
		// wantWarn, when set, is a warning the passing check must print.
		wantWarn string
	}{
		{name: "the PR on BD_LIB_REF", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "open"},
		{name: "the PR less a test hunk", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestOnlyProd, state: "open"},
		{name: "no bridge declared", depsEnv: "BD_REPO=Voxist/beads\nBD_LIB_REF=" + bridgeTestLibRef + "\n"},
		{name: "a bridge on another parent", depsEnv: bridged, parents: strings.Repeat("a", 40), bridgeDiff: bridgeTestPRDiff, state: "open", want: "want exactly BD_LIB_REF"},
		{name: "a bridge with two parents", depsEnv: bridged, parents: bridgeTestLibRef + " " + strings.Repeat("b", 40), bridgeDiff: bridgeTestPRDiff, state: "open", want: "want exactly BD_LIB_REF"},
		{
			name: "a fork-only line", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", want: "the PR does not carry",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/proxy_data.go b/proxy_data.go\n+// fork only\n",
		},
		{
			name: "a fork-only file", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", want: "fork-only delta",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/cmd/bd/fork.go b/cmd/bd/fork.go\n--- a/cmd/bd/fork.go\n+++ b/cmd/bd/fork.go\n+package main\n",
		},
		{
			name: "a production hunk dropped", depsEnv: bridged, parents: bridgeTestLibRef, state: "open", want: "drops 1 line",
			bridgeDiff: strings.Replace(bridgeTestPRDiff, "-// old\n", "", 1),
		},
		// A merged or closed PR (maintainers often absorb a PR into their own
		// commit and close it unmerged) is the bridge owner's next change, not
		// a failure of whatever PR happens to run CI: a warning naming the
		// bridge and how to drop it.
		{
			name: "the PR merged", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "merged",
			wantWarn: "carries gastownhall/beads#8412, which is merged, not open: drop the bridge",
		},
		{
			name: "the PR closed unmerged", depsEnv: bridged, parents: bridgeTestLibRef, bridgeDiff: bridgeTestPRDiff, state: "closed",
			wantWarn: "carries gastownhall/beads#8412, which is closed, not open: drop the bridge",
		},
		// The structural checks stay hard failures whatever the PR's state.
		{name: "a merged PR does not excuse another parent", depsEnv: bridged, parents: strings.Repeat("a", 40), bridgeDiff: bridgeTestPRDiff, state: "merged", want: "want exactly BD_LIB_REF"},
		{
			name: "a closed PR does not excuse a fork-only line", depsEnv: bridged, parents: bridgeTestLibRef, state: "closed", want: "the PR does not carry",
			bridgeDiff: bridgeTestPRDiff + "diff --git a/proxy_data.go b/proxy_data.go\n+// fork only\n",
		},
		{name: "a malformed upstream PR", depsEnv: strings.Replace(bridged, "gastownhall/beads#8412", "Voxist/beads#80", 1), want: "want gastownhall/beads#<n>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, content string) string {
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			}
			write("deps.env", tc.depsEnv)
			args := []string{
				"--repo-root", root,
				"--bridge-parents", tc.parents,
				"--bridge-diff", write("bridge.diff", tc.bridgeDiff),
				"--pr-diff", write("pr.diff", bridgeTestPRDiff),
				"--pr-state", tc.state,
			}
			cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "check-bd-lib-bridge"), args...)
			cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=true")
			out, err := cmd.CombinedOutput()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("the check refused a valid bridge: %v\n%s", err, out)
			case tc.want != "" && err == nil:
				t.Fatalf("the check passed; want a refusal naming %q\n%s", tc.want, out)
			case tc.want != "" && !strings.Contains(string(out), tc.want):
				t.Fatalf("the refusal does not name %q:\n%s", tc.want, out)
			}
			annotated := strings.Contains(string(out), "::warning title=Beads library bridge::")
			switch {
			case tc.wantWarn != "" && (!annotated || !strings.Contains(string(out), tc.wantWarn)):
				t.Fatalf("the check did not warn %q as a GitHub annotation:\n%s", tc.wantWarn, out)
			case tc.wantWarn != "" && !strings.Contains(string(out), "delete BD_LIB_BRIDGE_REF and BD_LIB_BRIDGE_UPSTREAM_PR"):
				t.Fatalf("the warning does not name the step that drops the bridge:\n%s", out)
			case tc.wantWarn == "" && annotated:
				t.Fatalf("the check warned with nothing to warn about:\n%s", out)
			}
		})
	}
}

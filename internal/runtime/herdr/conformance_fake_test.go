package herdr

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// TestMain installs a fake `herdr` CLI at the front of $PATH and pre-binds the
// shared session-server's unix socket, so TestHerdrConformance can run the
// full runtimetest.RunProviderTests suite against the production New(...)
// entrypoint with no real herdr binary and no live-journey skip.
//
// The PATH change is process-wide (os.Setenv), which only this test binary
// (one process per Go package under test) ever sees — but it still must not
// change what the opt-in live tier (TestHerdrConformance_Live,
// herdrtest.RequireLive) does when a human has actually opted into it: that
// tier's whole point is exercising a REAL herdr, and a fake shadowing it on
// PATH mid-run would silently defang `make test-herdr-live`. So this setup is
// skipped outright whenever the live opt-in env vars are set — mirroring
// herdrtest.SkipReason's own gate (herdrtest is a separate package this test
// does not import, so the condition is restated here rather than shared).
//
// Deliberately does NOT touch $XDG_CONFIG_HOME/$HOME: several other tests in
// this package (panebinding_provider_test.go, startup_delivery_confirm_test.go,
// kindpath_live_test.go) bind their OWN real unix sockets under
// herdrConfigDir()'s AMBIENT resolution, keyed by long per-test session names
// ("gctest-pb-<pid>-<n>"); redirecting that root to a fresh os.MkdirTemp
// directory made every one of those paths exceed the platform's unix-socket
// path-length limit (bind: invalid argument on darwin) even though nothing
// about this test touched them. The pre-bound socket below lives at the
// session=="default" path precisely because it is the SHORTEST form
// (herdrConfigDir()/herdr/herdr.sock, no "sessions/<name>/" segment) and
// therefore the one least likely to tip over that limit regardless of where
// the ambient config dir happens to be.
func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	if strings.TrimSpace(os.Getenv("GC_HERDR_LIVE_TESTS")) == "1" || strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "0" {
		return m.Run()
	}
	if goruntime.GOOS == "windows" {
		// The fake herdr CLI below is a POSIX shell script; conformance
		// against it is skipped by TestHerdrConformance itself on windows via
		// the same check, so there is nothing to set up.
		return m.Run()
	}

	binDir, err := os.MkdirTemp("", "herdr-fake-bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: create fake herdr bin dir: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1
	}
	defer os.RemoveAll(binDir) //nolint:errcheck // best-effort cleanup

	binPath := filepath.Join(binDir, "herdr")
	if err := os.WriteFile(binPath, []byte(fakeHerdrCLIScript), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: write fake herdr CLI: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1
	}
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: prepend fake herdr bin dir to PATH: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1
	}

	sockPath := fakeHerdrDefaultSessionSocketPath()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: create fake herdr socket dir: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1
	}
	// A prior crashed run of this same test binary can leave the socket inode
	// behind (net.Listen refuses to bind over an existing path, stale or not —
	// unlike TCP there is no port to time out). Clear it first, mirroring
	// client.removeStaleSocket's own guard for the identical problem.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: bind fake herdr session-server socket: %v\n", err) //nolint:errcheck // best-effort diagnostic
		return 1
	}
	defer ln.Close()          //nolint:errcheck // best-effort cleanup
	defer os.Remove(sockPath) //nolint:errcheck // best-effort cleanup; leaving the inode behind only costs the NEXT run a removeStaleSocket-style clear
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed at process exit
			}
			conn.Close() //nolint:errcheck // satisfying client.serverAlive()'s dial-and-close probe only
		}
	}()

	return m.Run()
}

// fakeHerdrDefaultSessionSocketPath replicates client.go's herdrConfigDir()
// and socketPath() for the session=="default" branch exactly (without
// mutating any environment variable — see TestMain's doc comment on why),
// so the listener TestMain binds sits exactly where every conformance
// session's ConfigureServer→serverAlive dial will look.
// conformanceHerdrSession names the literal this must track.
func fakeHerdrDefaultSessionSocketPath() string {
	configDir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			configDir = filepath.Join(home, ".config")
		} else if cd, err := os.UserConfigDir(); err == nil {
			configDir = cd
		}
	}
	return filepath.Join(configDir, "herdr", "herdr.sock")
}

// conformanceHerdrSession is the shared outer herdr session every
// TestHerdrConformance factory call constructs its Provider against — see
// workspaceTabFor/New: the per-agent name passed to Start (not this) is what
// drives workspace/tab placement, so sharing one herdr "session" across
// factory calls mirrors production's one-session-per-city model and keeps
// every call resolving the same pre-bound socket path above.
const conformanceHerdrSession = "default"

// fakeHerdrCLIScript is a stand-in `herdr` CLI for the full runtimetest
// conformance suite driven through a RAW/shell-mode session (no cfg.Command,
// so Start never issues a kind launch): every op resolves through the pane
// binding Start persists locally (panebinding.go), not through herdr's own
// agent registry, so the registry verbs below always answer "no such agent"
// and the pane verbs always succeed — matching a session nobody ever
// registered a named agent into. Stateless and PID-keyed (no shared files to
// race): concurrent Start/Stop/List calls from runtimetest's concurrent
// subtests each get their own process and their own synthesized pane/tab id,
// and nothing here needs to recognize a previously-seen id again.
const fakeHerdrCLIScript = `#!/bin/sh
shift 2 2>/dev/null
v1="$1"
v2="$2"
case "$v1 $v2" in
"workspace list")
  printf '{"result":{"workspaces":[]}}' ;;
"workspace create"|"tab create")
  printf '{"result":{"tab":{"tab_id":"tab-%s"},"root_pane":{"pane_id":"pane-%s"}}}' "$$" "$$" ;;
"tab list")
  printf '{"result":{"tabs":[]}}' ;;
"tab rename"|"tab close")
  : ;;
"agent get")
  printf '{"error":{"code":"agent_not_found","message":"no such agent"}}' ;;
"agent list")
  printf '{"result":{"agents":[]}}' ;;
"agent prompt")
  printf '{"result":{"type":"agent_prompted"}}' ;;
"agent start")
  printf '{"result":{"agent":{"agent":"%s","pane_id":"pane-%s","agent_status":"idle"}}}' "$3" "$$" ;;
"agent wait")
  printf '{"result":{"agent":{"agent":"%s","agent_status":"idle"}}}' "$3" ;;
"pane process-info")
  printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}' ;;
"pane read")
  printf 'conformance-pane-contents\n' ;;
"pane send-keys"|"pane run"|"pane close")
  : ;;
*)
  : ;;
esac
exit 0
`

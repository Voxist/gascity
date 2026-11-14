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

// conformanceHerdrSession is the herdr session every TestHerdrConformance
// factory call builds its Provider against. It carries the test binary's pid,
// so its socket directory belongs to this process alone: it never names a real
// herdr session, and two test binaries running at once never share it.
var conformanceHerdrSession = fmt.Sprintf("gcconf-%d", os.Getpid())

// TestMain lets TestHerdrConformance run the full runtime.Provider contract
// against the production New entrypoint with no herdr binary: it puts a fake
// herdr CLI first on PATH and binds the conformance session's server socket,
// so Start's ConfigureServer finds a live server and launches nothing. The
// listener only accepts and closes: serverAlive dials it, and nothing else in
// the suite talks to it.
//
// When a human opts into the live tier (GC_HERDR_LIVE_TESTS=1 or
// GC_FAST_UNIT=0), the fake would shadow the real herdr those journeys exist
// to exercise, so nothing is installed; TestHerdrConformance then runs against
// the real herdr, and TestMain stops the server it started.
func TestMain(m *testing.M) {
	sockDir := filepath.Dir(newClient(conformanceHerdrSession, "").socketPath())
	if strings.TrimSpace(os.Getenv("GC_HERDR_LIVE_TESTS")) == "1" || strings.TrimSpace(os.Getenv("GC_FAST_UNIT")) == "0" {
		code := m.Run()
		_ = newClient(conformanceHerdrSession, "").stopServer()
		_ = os.RemoveAll(sockDir)
		os.Exit(code)
	}

	binDir, err := installFakeHerdrCLI(sockDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: %v\n", err)
		os.Exit(1)
	}
	ln, err := net.Listen("unix", newClient(conformanceHerdrSession, "").socketPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: binding the conformance server socket: %v\n", err)
		_ = os.RemoveAll(binDir)
		_ = os.RemoveAll(sockDir)
		os.Exit(1)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	code := m.Run()
	_ = ln.Close()
	_ = os.RemoveAll(sockDir)
	_ = os.RemoveAll(binDir)
	os.Exit(code)
}

// installFakeHerdrCLI writes fakeHerdrCLIScript as `herdr` in a new temp
// directory, puts that directory first on PATH, and prepares an empty sockDir
// for the conformance server socket. It returns the bin directory.
func installFakeHerdrCLI(sockDir string) (string, error) {
	if goruntime.GOOS == "windows" {
		return "", fmt.Errorf("the fake herdr CLI is a POSIX shell script")
	}
	binDir, err := os.MkdirTemp("", "herdr-fake-bin-")
	if err != nil {
		return "", fmt.Errorf("creating the fake herdr bin dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(fakeHerdrCLIScript), 0o755); err != nil {
		_ = os.RemoveAll(binDir)
		return "", fmt.Errorf("writing the fake herdr CLI: %w", err)
	}
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		_ = os.RemoveAll(binDir)
		return "", fmt.Errorf("putting the fake herdr CLI on PATH: %w", err)
	}
	// sockDir is named for this pid, so anything already there is left over
	// from an earlier binary that had the same pid and crashed.
	if err := os.RemoveAll(sockDir); err != nil {
		_ = os.RemoveAll(binDir)
		return "", fmt.Errorf("clearing the conformance socket dir: %w", err)
	}
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		_ = os.RemoveAll(binDir)
		return "", fmt.Errorf("creating the conformance socket dir: %w", err)
	}
	return binDir, nil
}

// fakeHerdrCLIScript stands in for the herdr CLI under the conformance suite.
// The suite starts every session with an empty cfg.Command, so Start never
// launches a named agent: every lifecycle op resolves through the pane binding
// Start persists in the provider's meta dir. The registry verbs therefore
// always answer "no such agent" and the pane verbs always succeed. It keeps no
// state, so the suite's concurrent subtests cannot race through it; each
// invocation names its tab and pane after its own pid.
const fakeHerdrCLIScript = `#!/bin/sh
shift 2
case "$1 $2" in
"workspace list")
  printf '{"result":{"workspaces":[]}}' ;;
"workspace create"|"tab create")
  printf '{"result":{"tab":{"tab_id":"tab-%s"},"root_pane":{"pane_id":"pane-%s"}}}' "$$" "$$" ;;
"tab list")
  printf '{"result":{"tabs":[]}}' ;;
"agent get")
  printf '{"error":{"code":"agent_not_found","message":"no such agent"}}' ;;
"agent list")
  printf '{"result":{"agents":[]}}' ;;
"pane process-info")
  printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}' ;;
"pane read")
  printf 'conformance-pane-contents\n' ;;
*)
  : ;;
esac
exit 0
`

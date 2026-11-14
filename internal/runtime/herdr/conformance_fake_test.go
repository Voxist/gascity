package herdr

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// conformanceHerdrSession is the herdr session every TestHerdrConformance
// factory call builds its Provider against. The fake herdr CLI answers only
// for this session and hands every other invocation to the real herdr.
const conformanceHerdrSession = "gcconf"

// realHerdrPath is where the real herdr executable was on PATH before
// TestMain put the fake in front of it, or empty when there is none. The live
// tier gates on it (requireLiveHerdr), since a PATH lookup now finds the fake.
var realHerdrPath string

// fakeHerdrStateDir holds the fake herdr CLI's per-pane markers.
var fakeHerdrStateDir string

// TestMain lets TestHerdrConformance run the full runtime.Provider contract
// against the production New entrypoint in every lane, with or without a real
// herdr. It runs before any test:
//
//   - It points XDG_CONFIG_HOME, the root of every herdr socket path, at a new
//     short directory under /tmp, so no test in this binary binds a socket
//     under the user's ~/.config/herdr, socket paths stay under the sun_path
//     limit, and whatever a panic or timeout leaves behind is under /tmp.
//   - It puts a fake herdr CLI first on PATH. The fake answers invocations for
//     conformanceHerdrSession and execs the real herdr, found before the PATH
//     change, for every other session, or exits 127 when there is none.
//   - It binds conformanceHerdrSession's server socket, so Start's
//     ConfigureServer finds a live server and launches nothing. The listener
//     only accepts and closes: serverAlive dials it, and nothing else in the
//     suite talks to it.
func TestMain(m *testing.M) {
	if goruntime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "herdr tests: the fake herdr CLI is a POSIX shell script")
		os.Exit(1)
	}
	root, err := os.MkdirTemp("/tmp", "hdr")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: creating the test root: %v\n", err)
		os.Exit(1)
	}
	if err := installFakeHerdrCLI(root); err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: %v\n", err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	ln, err := net.Listen("unix", newClient(conformanceHerdrSession, "").socketPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr tests: binding the conformance server socket: %v\n", err)
		_ = os.RemoveAll(root)
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
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// installFakeHerdrCLI sets the process up for the conformance fake under root:
// it records the real herdr's path, points XDG_CONFIG_HOME at root/config,
// writes the fake as root/bin/herdr and puts root/bin first on PATH, and
// creates the fake's state directory and the conformance session's socket
// directory.
func installFakeHerdrCLI(root string) error {
	if path, err := exec.LookPath("herdr"); err == nil {
		realHerdrPath = path
	}
	if err := os.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config")); err != nil {
		return fmt.Errorf("pointing XDG_CONFIG_HOME at the test root: %w", err)
	}
	fakeHerdrStateDir = filepath.Join(root, "state")
	binDir := filepath.Join(root, "bin")
	sockDir := filepath.Dir(newClient(conformanceHerdrSession, "").socketPath())
	for _, dir := range []string{fakeHerdrStateDir, binDir, sockDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte(fakeHerdrCLIScript(realHerdrPath, fakeHerdrStateDir)), 0o755); err != nil {
		return fmt.Errorf("writing the fake herdr CLI: %w", err)
	}
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return fmt.Errorf("putting the fake herdr CLI on PATH: %w", err)
	}
	return nil
}

// fakeHerdrCLIScript renders the stand-in herdr CLI. Every client call is
// `herdr --session <session> <verb...>`, so $2 is the session.
//
// For conformanceHerdrSession it answers the verbs the conformance suite
// reaches. The suite starts every session with an empty cfg.Command, so Start
// never launches a named agent and every lifecycle op resolves through the
// pane binding Start persists in the provider's meta dir: the registry verbs
// always answer "no such agent". Each invocation names its tab and pane after
// its own pid. `pane close` leaves a per-pane marker in state, and a closed
// pane answers pane_not_found afterwards, as herdr does; one file per pane
// keeps the suite's concurrent subtests from racing through shared state.
//
// For any other session it execs the real herdr, or exits 127 like a missing
// executable when there is none.
func fakeHerdrCLIScript(realHerdr, state string) string {
	return `#!/bin/sh
if [ "$2" != ` + shellquote.Quote(conformanceHerdrSession) + ` ]; then
  REAL=` + shellquote.Quote(realHerdr) + `
  if [ -n "$REAL" ]; then exec "$REAL" "$@"; fi
  echo 'herdr: not installed (only the conformance fake is on PATH)' >&2
  exit 127
fi
STATE=` + shellquote.Quote(state) + `
shift 2
closed() { [ -e "$STATE/closed-$1" ]; }
gone() {
  printf '{"error":{"code":"pane_not_found","message":"pane %s not found"}}' "$1" >&2
  exit 1
}
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
  if closed "$4"; then gone "$4"; fi
  printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}' ;;
"pane read")
  if closed "$3"; then gone "$3"; fi
  printf 'conformance-pane-contents\n' ;;
"pane close")
  if closed "$3"; then gone "$3"; fi
  : > "$STATE/closed-$3" ;;
*)
  : ;;
esac
exit 0
`
}

// TestHerdrStopClosesTheSessionPane pins what the conformance suite cannot
// see: Stop clears the session's metadata, so after it no Provider method
// reaches the pane again, and a Stop that left the pane open would still pass
// the contract. The fake's close marker shows whether the pane was closed.
func TestHerdrStopClosesTheSessionPane(t *testing.T) {
	p := New(conformanceHerdrSession, t.TempDir(), t.TempDir(), 0, 0)
	const name = "stop-closes-pane"
	if err := p.Start(context.Background(), name, runtime.Config{WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pane, err := p.GetMeta(name, metaBoundPane)
	if err != nil || pane == "" {
		t.Fatalf("bound pane after Start = %q, %v; want the pane Start placed", pane, err)
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fakeHerdrStateDir, "closed-"+pane)); err != nil {
		t.Fatalf("Stop left pane %s open: %v", pane, err)
	}
	if p.IsRunning(name) {
		t.Fatalf("IsRunning(%q) after Stop = true", name)
	}
}

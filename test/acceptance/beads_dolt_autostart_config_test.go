//go:build acceptance_a

// ga-xwn1k / ga-rpgvw: pin the fix carried by the bd v1.92.0 repin
// (Voxist/beads#56, #57) -- a workspace's own `dolt.auto-start: false`
// must gate every IMPLICIT dolt sql-server auto-start, not just the
// BEADS_DOLT_AUTO_START env var.
//
// The real shape, per the beads-side investigation on ga-rpgvw: a
// SERVER-mode workspace (metadata.json dolt_mode=server, not an embedded
// `bd init`), no server ever started for it, reached through an IMPLICIT
// path -- `bd dolt start` is deliberately ungated by design and proves
// nothing here.
//
// The bug is NOT observable through the `bd` CLI binary itself: every
// reachable cobra command path resolves the workspace via
// beads.FindBeadsDir() and then calls config.Initialize() for that exact
// directory before touching the store, so global viper always sees the
// workspace's own config.yaml and the reader bug this fix closes never
// fires. (Confirmed empirically while building this test: `bd list`,
// `bd doctor`, and `bd list` under a mismatched BEADS_DIR all refused
// correctly on 1.91.0 too.) The bug bites LIBRARY CONSUMERS instead --
// any caller of internal/doltserver.EnsureRunningDetailed that never runs
// bd's own config.Initialize() first, which is exactly what tripped
// ga-rpgvw's incident (an unmanaged dolt sql-server spawned against a
// workspace whose config.yaml plainly said not to) and exactly what
// Voxist/beads' own regression test
// (TestIsAutoStartDisabledForHonoursWorkspaceConfigWithoutGlobalInit)
// pins from inside the beads module.
//
// gascity cannot import beads' internal packages (different module,
// internal/ visibility), and gc's own linked beads library
// (deps.env BD_LIB_REF) does not carry this fix regardless of this repin
// -- the fork-first bridge deliberately keeps BD_LIB_REF on the upstream
// ancestor. So the only way to prove this from gascity is to build the
// PINNED bd SOURCE (deps.env BD_SOURCE_REF) and run a small probe test
// against its own internal/doltserver package, matching Voxist/beads'
// own regression shape: a server-mode workspace, config.yaml with a flat
// `dolt.auto-start: false` (the exact spelling bd itself writes),
// BEADS_DOLT_AUTO_START unset, and a call to EnsureRunningDetailed with
// no prior config.Initialize() -- the library-consumer path.
//
// Proven red-at-1.91.0 / green-at-1.92.0 by hand while writing this test
// (not re-verified on every CI run -- that would double the network/build
// cost of every row for a fixed historical fact): built bd from
// Voxist/beads 2498618eb (deps.env's pin before this repin) and got
//
//	RED: EnsureRunningDetailed started/adopted a server (port=50497
//	startedByUs=true) despite dolt.auto-start:false ...
//
// against the identical probe, an unmanaged `dolt sql-server` process and
// all. The same probe against Voxist/beads dbf278efb (this repin's pin)
// refused:
//
//	GREEN: EnsureRunningDetailed refused: Dolt server unreachable (port 0)
//	and auto-start is disabled (dolt.auto-start: false in config.yaml or
//	BEADS_DOLT_AUTO_START=0). ...
//
// This test pins the CURRENT deps.env pin's (green) behaviour going
// forward, so a future accidental downgrade of BD_SOURCE_REF -- or a
// beads regression on the same commit -- fails it.
package acceptance_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// autoStartProbeTestGo is copied verbatim into the pinned bd source tree's
// internal/doltserver package and run with `go test` there. It exercises
// EnsureRunningDetailed exactly the way a library consumer that never calls
// bd's own config.Initialize() would -- the path ga-rpgvw's fix closes.
const autoStartProbeTestGo = `package doltserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGascityProbeEnsureRunningDetailedLibraryConsumer is injected by
// gascity's test/acceptance/beads_dolt_autostart_config_test.go
// (ga-xwn1k / ga-rpgvw). See that file for what this proves and why it has
// to live here rather than in gascity's own module.
func TestGascityProbeEnsureRunningDetailedLibraryConsumer(t *testing.T) {
	for _, k := range []string{
		"BEADS_DOLT_AUTO_START", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT", "BEADS_DIR",
	} {
		t.Setenv(k, "")
	}
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The real shape: server mode, and the flat dotted spelling bd itself
	// writes to config.yaml -- the nested-only reader ga-rpgvw's fix
	// replaces could not see this form.
	cfg := "issue_prefix: probe\ndolt.auto-start: false\ndolt.mode: server\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := ` + "`" + `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"probe","project_id":"p"}` + "`" + `
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	// No server was ever started for this workspace -- EnsureRunningDetailed
	// is reached implicitly, never via the ungated 'bd dolt start'.
	serverDir := resolveServerDir(beadsDir)
	t.Cleanup(func() {
		if state, err := IsRunning(serverDir); err == nil && state != nil && state.Running {
			t.Logf("a server was started at %s (pid %d, port %d) despite auto-start being disabled; killing it", serverDir, state.PID, state.Port)
			if stopErr := StopWithForce(serverDir, true); stopErr != nil {
				t.Errorf("FAILED TO KILL the leaked server (pid %d): %v -- kill it by hand", state.PID, stopErr)
			}
		}
	})

	port, startedByUs, err := EnsureRunningDetailed(beadsDir)
	if err == nil {
		t.Fatalf("EnsureRunningDetailed started or adopted a server (port=%d startedByUs=%v) despite the workspace disabling auto-start; it must refuse", port, startedByUs)
	}
	if !strings.Contains(err.Error(), "auto-start is disabled") {
		t.Fatalf("refusal must name the policy; got: %v", err)
	}
}
`

// depsEnvBDPins reads BD_REPO/BD_SOURCE_REF/BD_SOURCE_SHA256 from the
// module root's deps.env. Mirrors test/integration's
// bridgeIntegrationBDPins, duplicated rather than shared because the two
// packages (integration, acceptance_test) do not import each other.
func depsEnvBDPins(t *testing.T) (repo, ref, sha256sum string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(helpers.FindModuleRoot(), "deps.env"))
	if err != nil {
		t.Fatalf("read deps.env: %v", err)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, found := strings.Cut(line, "="); found {
			vals[k] = v
		}
	}
	if vals["BD_SOURCE_REF"] == "" {
		t.Fatal("deps.env has no BD_SOURCE_REF; this test targets the fork-first bridge pin")
	}
	repo = vals["BD_REPO"]
	if repo == "" {
		repo = "gastownhall/beads"
	}
	return repo, vals["BD_SOURCE_REF"], vals["BD_SOURCE_SHA256"]
}

// fetchAndVerifyBDSource downloads the BD_REPO@ref source tarball, verifies
// it against wantSHA256 (when non-empty), and extracts it into dir. Mirrors
// install-bd-archive.sh's and test/integration's bridge-mode download, so a
// bad deps.env pin fails the same way here as it would in CI or the image
// build.
func fetchAndVerifyBDSource(t *testing.T, repo, ref, wantSHA256, dir string) {
	t.Helper()
	tarPath := filepath.Join(t.TempDir(), "bd-source.tar.gz")
	url := fmt.Sprintf("https://github.com/%s/archive/%s.tar.gz", repo, ref)
	dl := exec.Command("curl", "-fsSL", "--retry", "3", url, "-o", tarPath)
	if out, err := dl.CombinedOutput(); err != nil {
		helpers.MissingTooling(t, "download %s: %v\n%s", url, err, out)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("read downloaded bd source tarball: %v", err)
	}
	if wantSHA256 != "" {
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wantSHA256 {
			t.Fatalf("bd source tarball sha256 = %s, deps.env BD_SOURCE_SHA256 = %s", got, wantSHA256)
		}
	}
	if out, err := exec.Command("tar", "-xzf", tarPath, "--strip-components=1", "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("extract bd source tarball: %v\n%s", err, out)
	}
}

// TestBeadsDoltAutoStartConfigHonouredByLibraryConsumer pins the fixed
// behaviour of deps.env's CURRENT bd pin (v1.92.0, Voxist/beads dbf278efb
// as of this writing): a server-mode workspace's own
// `dolt.auto-start: false` must refuse an implicit auto-start for a
// library consumer that never ran bd's own config.Initialize(). See the
// file-level doc comment for the red-at-1.91.0/green-at-1.92.0 evidence
// and why this cannot be proven through the bd CLI binary itself.
func TestBeadsDoltAutoStartConfigHonouredByLibraryConsumer(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		helpers.MissingTooling(t, "go toolchain not on PATH: %v", err)
	}
	repo, ref, wantSHA256 := depsEnvBDPins(t)

	srcDir := t.TempDir()
	fetchAndVerifyBDSource(t, repo, ref, wantSHA256, srcDir)

	probePath := filepath.Join(srcDir, "internal", "doltserver", "zz_gascity_autostart_probe_test.go")
	if err := os.WriteFile(probePath, []byte(autoStartProbeTestGo), 0o644); err != nil {
		t.Fatalf("write probe test into pinned bd source: %v", err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-run", "TestGascityProbeEnsureRunningDetailedLibraryConsumer", "-v", "./internal/doltserver/")
	cmd.Dir = srcDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %s (%s@%s) failed to honour dolt.auto-start:false for an implicit, no-config-init auto-start -- this is the ga-rpgvw regression:\n%s", ref, repo, ref, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("expected the injected probe test to report PASS; got:\n%s", out)
	}
}

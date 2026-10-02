//go:build integration

package integration

// TestCleanInstallTutorialPath is a regression test for GitHub issue #1670:
// "Tutorial 01: gc sling fails on clean install — issue_prefix not seeded into
// Dolt DB by gc init / gc rig add."
//
// After gc init + gc rig add the rig's Dolt database must be seeded with
// issue_prefix so that `bd config get issue_prefix` returns the derived prefix
// and rig-scoped bead creation works without a "database not initialized" error.
//
// This test passes against current main (post-#1477) and guards against future regressions of #1670.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// redactedMetadataJSONKeyNeedles is the set of substrings a metadata.json key
// must not contain in a diagnostics dump. Mirrors
// cmd/gc/internal/beads/contract/preflight.go's own redaction needle list
// (password, passwd, secret, token, key) so the two stay in agreement about
// what counts as sensitive in this file.
var redactedMetadataJSONKeyNeedles = []string{"password", "passwd", "secret", "token", "key"}

// dumpScopeMetadataJSON reads a scope's .beads/metadata.json and renders it
// with any sensitive-looking field (anything matching
// redactedMetadataJSONKeyNeedles, case-insensitively -- metadata.json can
// carry dolt_password for an external endpoint) replaced with "[redacted]"
// rather than printed verbatim into a CI log.
func dumpScopeMetadataJSON(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("%v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		// Not valid JSON (or not an object) -- still worth seeing fewer than
		// the raw bytes is unhelpful, but printing raw bytes risks a secret
		// that didn't parse as expected. Report the shape only.
		return fmt.Sprintf("(unparseable as a JSON object: %v)", err)
	}
	for key := range fields {
		lower := strings.ToLower(key)
		for _, needle := range redactedMetadataJSONKeyNeedles {
			if strings.Contains(lower, needle) {
				fields[key] = "[redacted]"
				break
			}
		}
	}
	redacted, err := json.Marshal(fields)
	if err != nil {
		return fmt.Sprintf("(failed to re-marshal after redaction: %v)", err)
	}
	return string(redacted)
}

// dumpRigDoltDiagnostics summarizes the on-disk state that distinguishes a
// classic gc-managed rig (sharing the city's one Dolt server) from a
// provider-owned one (its own, separately-seeded server) at failure time. It
// reads best-effort: a missing file is reported as such rather than failing
// the dump itself, since the diagnostic's whole point is to explain a prior
// failure, not introduce a new one.
//
// Three files answer "which server, if any, was this rig pointed at, and did
// the city think it owned that server itself": .beads/dolt-server.port
// (which port a scope's own bd process bound, when it ran one),
// .beads/metadata.json (backend/dolt_mode, which distinguishes classic
// "server" mode from an embedded or provider-owned scope -- redacted, since
// it can carry dolt_password for an external endpoint), and
// .gc/scope-ownership.json (the journal that actually tells gc-managed from
// provider-owned, read by cityScopeProviderOwned/scopeProviderOwned -- the
// ownership VALUE both metadata.json and dolt-server.port are only indirect
// evidence of). ga-wuda3's root cause was exactly this: a rig silently got
// its own unseeded provider-owned Dolt server instead of sharing the city's.
func dumpRigDoltDiagnostics(cityDir, rigDir string) string {
	var b strings.Builder
	dumpPortFile := func(label, path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "  %s (%s): %v\n", label, path, err)
			return
		}
		fmt.Fprintf(&b, "  %s (%s): %s\n", label, path, strings.TrimSpace(string(data)))
	}
	dumpMetadata := func(label, path string) {
		fmt.Fprintf(&b, "  %s (%s): %s\n", label, path, dumpScopeMetadataJSON(path))
	}
	fmt.Fprintf(&b, "city dir: %s\n", cityDir)
	dumpPortFile("city dolt-server.port", filepath.Join(cityDir, ".beads", "dolt-server.port"))
	dumpMetadata("city metadata.json", filepath.Join(cityDir, ".beads", "metadata.json"))
	dumpPortFile("city scope-ownership.json", filepath.Join(cityDir, ".gc", "scope-ownership.json"))
	fmt.Fprintf(&b, "rig dir: %s\n", rigDir)
	dumpPortFile("rig dolt-server.port", filepath.Join(rigDir, ".beads", "dolt-server.port"))
	dumpMetadata("rig metadata.json", filepath.Join(rigDir, ".beads", "metadata.json"))
	return b.String()
}

// bdDoltInRig runs the bd binary in rigDir using the managed Dolt endpoint
// from cityDir. The rig and the city share the same Dolt server; the database
// name is read from the rig's .beads/metadata.json by bd.
//
// Uses runCommandStdout, not runCommand: callers parse the returned string as
// a value (an issue prefix, a bead ID), and bd's own stderr diagnostics must
// not be able to corrupt that value (ga-rsktma).
func bdDoltInRig(cityDir, rigDir string, args ...string) (string, error) {
	env := commandEnvForDir(cityDir, true)
	if port, ok := ensureManagedDoltPortForTest(cityDir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
	}
	return runCommandStdout(rigDir, env, integrationBDCommandTimeout, bdBinary, args...)
}

func TestCleanInstallTutorialPath(t *testing.T) {
	requireDoltIntegration(t)

	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)
	registerIntegrationDoltSQLServerCleanup(t, cityDir)

	// --- Step 1: gc init (clean install, no --file) ---
	// Wrapped in retryOnDoltDirtyTableMigrationRace: under the full parallel
	// sweep, bd's schema-migration bootstrap can transiently race a
	// still-settling prior schema state (gastownhall/beads#4566, ga-38xsx4).
	// The retry is narrowly scoped to that one known signature — see the
	// predicate's doc comment — so it never masks a real gc-init failure.
	out, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
		return runGCDoltWithEnv(env, "", "init",
			"--skip-provider-readiness",
			"--providers", "codex",
			"--default-provider", "codex",
			cityDir,
		)
	})
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	// --- Step 2: gc rig add (simulate `gc rig add ~/tutorial-rig-alpha`) ---
	// Three-part name so DeriveBeadsPrefix produces a 3-char prefix ("tra") that
	// can never equal the 2-char prefix derived from the random city name.
	rigName := "tutorial-rig-alpha"
	rigDir := filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	// Same beads#4566 exposure as Step 1: gc rig add seeds a second, separate
	// Dolt DB (the rig's own) and can hit the identical transient race.
	out, err = retryOnDoltDirtyTableMigrationRace(func() (string, error) {
		return gcDolt(cityDir, "rig", "add", rigDir)
	})
	if err != nil {
		t.Fatalf("gc rig add failed: %v\noutput: %s", err, out)
	}

	wantPrefix := config.DeriveBeadsPrefix(rigName) // "tra"

	// --- Assertion 1: bd config get issue_prefix returns the derived prefix ---
	// Regression: rig Dolt DB was never seeded so this returned "" before the fix.
	prefixOut, err := bdDoltInRig(cityDir, rigDir, "config", "get", "issue_prefix")
	if err != nil {
		t.Fatalf("bd config get issue_prefix in rig failed: %v\noutput: %s\n(regression: issue #1670 — rig Dolt DB not seeded during gc rig add)\n%s", err, prefixOut, dumpRigDoltDiagnostics(cityDir, rigDir))
	}
	gotPrefix := strings.TrimSpace(prefixOut)
	if gotPrefix != wantPrefix {
		t.Errorf("bd config get issue_prefix = %q, want %q\n(regression: issue #1670 — rig Dolt DB not seeded during gc rig add)\n%s", gotPrefix, wantPrefix, dumpRigDoltDiagnostics(cityDir, rigDir))
	}

	// --- Assertion 2: rig-scoped bead creation succeeds ---
	// If the DB is not initialized, bd create returns a "database not initialized"
	// (or equivalent) error instead of a bead ID.
	beadTitle := fmt.Sprintf("tutorial regression test bead (%s)", wantPrefix)
	beadOut, err := bdDoltInRig(cityDir, rigDir, "create", beadTitle)
	if err != nil {
		if strings.Contains(strings.ToLower(beadOut+err.Error()), "not initialized") {
			t.Fatalf("bd create in rig failed with database-not-initialized (regression issue #1670): %v\noutput: %s\n%s", err, beadOut, dumpRigDoltDiagnostics(cityDir, rigDir))
		}
		t.Fatalf("bd create in rig failed: %v\noutput: %s\n%s", err, beadOut, dumpRigDoltDiagnostics(cityDir, rigDir))
	}
	// Bead ID should carry the rig prefix, e.g. "mp-abc".
	if !strings.Contains(beadOut, wantPrefix+"-") {
		t.Errorf("bd create output = %q, want bead ID containing prefix %q", strings.TrimSpace(beadOut), wantPrefix+"-")
	}
}

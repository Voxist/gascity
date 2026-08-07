package productmetrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/gchome"
)

// TestMain scrubs the ambient metrics kill switches before any test runs.
//
// Several tests assert Status/RecordingPermit outcomes that presuppose no
// environment-level opt-out — e.g. TestOpenProductionAndPreparationAreLazy-
// AndNonCreating wants (pending-notice, preference-unset). On any machine
// where the operator has set GC_DISABLE_USAGE_METRICS=1 or DO_NOT_TRACK=1
// (a reasonable default for a dev or fleet box), those tests read the
// AMBIENT environment and fail with (environment-disabled, ...) — asserting
// against the host's preferences rather than the code.
//
// WHAT THIS SCRUB TAKES AWAY, AND WHERE IT IS GIVEN BACK. The package's other
// kill-switch tests inject a fake getenv (mapGetenv) and never touch the real
// environment, so before this file the ONLY coverage of the production
// os.Getenv wiring with a kill switch actually present was ACCIDENTAL — an
// opted-out dev box going red. The scrub removes that accident, so
// TestKillSwitchesDisableThroughTheRealEnvironment below restores it
// deliberately: real env var, real OpenProduction, no injected deps.
//
// The scrub also runs in the package's helper re-execs of the test binary
// (marker/spool/storage protocol helpers); none of them exercise the kill
// switches, and a child test that needs one sets it via t.Setenv, which is
// applied after TestMain runs.
func TestMain(m *testing.M) {
	// POSIX unsetenv fails only for invalid names (empty or containing '=');
	// these compile-time constants never are, so there is no error to handle.
	_ = os.Unsetenv(envDisableUsageMetrics) //nolint:errcheck
	_ = os.Unsetenv(envDoNotTrack)          //nolint:errcheck
	os.Exit(m.Run())
}

// TestKillSwitchesDisableThroughTheRealEnvironment pins the production env
// wiring end to end: a kill switch present in the REAL process environment
// must disable metrics through OpenProduction's default dependencies
// (service.go: getenv: os.Getenv), with no injected fakes anywhere.
//
// This is the coverage the TestMain scrub above removes. Without this test, a
// regression in the default dependency wiring — getenv stubbed out, the
// real-env read path bypassed — would pass every test on every machine, and a
// released gc would silently record usage metrics from users who explicitly
// opted out.
func TestKillSwitchesDisableThroughTheRealEnvironment(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
		reason StateReason
	}{
		{"gc disable", envDisableUsageMetrics, ReasonGCDisable},
		{"do not track", envDoNotTrack, ReasonDoNotTrack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Same trusted-home shape as TestOpenProductionAndPreparationAre-
			// LazyAndNonCreating: a private parent and a never-created home keep
			// the home-stability gate satisfied so the ENV gate is what decides.
			parent := t.TempDir()
			// Canonicalize: on darwin TMPDIR sits under /var -> /private/var, and
			// the trust inspection rejects a home with a symlinked ancestor
			// ("path component /var is a symlink"). The env gate can only be
			// reached through a home the inspection accepts.
			parent, err := filepath.EvalSymlinks(parent)
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			if err := os.Chmod(parent, 0o700); err != nil {
				t.Fatalf("Chmod private parent: %v", err)
			}
			t.Setenv("GC_HOME", filepath.Join(parent, "not-created"))
			t.Setenv(tc.envVar, "1")
			service, err := OpenProduction(ProductionOptions{
				Home:    gchome.ResolveReadOnly(),
				Release: CurrentReleaseIdentity(),
			})
			if err != nil {
				t.Fatalf("OpenProduction() error = %v", err)
			}
			status := service.Status(context.Background())
			if status.State != StateEnvironmentDisabled || status.Reason != tc.reason {
				t.Fatalf("Status with %s=1 in the real environment = (%q, %q), "+
					"want (%q, %q) — the production getenv wiring is not reading "+
					"the real environment", tc.envVar, status.State, status.Reason,
					StateEnvironmentDisabled, tc.reason)
			}
		})
	}
}

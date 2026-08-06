package productmetrics

import (
	"os"
	"testing"
)

// TestMain scrubs the ambient metrics kill switches before any test runs.
//
// Several tests assert Status/RecordingPermit outcomes that presuppose no
// environment-level opt-out — e.g. TestOpenProductionAndPreparationAreLazy-
// AndNonCreating wants (pending-notice, preference-unset). On any machine
// where the operator has set GC_DISABLE_USAGE_METRICS=1 or DO_NOT_TRACK=1
// (a reasonable default for a dev or fleet box), those tests read the
// AMBIENT environment and fail with (environment-disabled,
// gc-disable-usage-metrics) — asserting against the host's preferences
// rather than the code. Tests that exercise the kill switches themselves
// set them explicitly via t.Setenv, which still works after this scrub.
func TestMain(m *testing.M) {
	for _, killSwitch := range []string{envDisableUsageMetrics, "DO_NOT_TRACK"} {
		if err := os.Unsetenv(killSwitch); err != nil {
			panic("productmetrics TestMain: unset " + killSwitch + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

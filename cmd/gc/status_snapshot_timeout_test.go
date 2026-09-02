package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// TestStatusSnapshotTimeoutHonorsDaemonConfig pins the [daemon]
// status_snapshot_timeout knob to an actual reader.
//
// The config field, its StatusSnapshotTimeoutDuration accessor and its
// published schema default all survived the 2026-08-31 resync while the only
// reader did not, leaving a documented operator knob silently inert. A field
// with no reader fails no build and no test, so this asserts the wiring
// directly.
func TestStatusSnapshotTimeoutHonorsDaemonConfig(t *testing.T) {
	t.Parallel()

	if got := statusSnapshotTimeout(nil); got != statusSessionSnapshotTimeout {
		t.Fatalf("statusSnapshotTimeout(nil) = %s, want the package default %s", got, statusSessionSnapshotTimeout)
	}

	unset := &config.City{}
	if got := statusSnapshotTimeout(unset); got != statusSessionSnapshotTimeout {
		t.Fatalf("statusSnapshotTimeout(unset) = %s, want the package default %s", got, statusSessionSnapshotTimeout)
	}

	configured := &config.City{}
	configured.Daemon.StatusSnapshotTimeout = "17s"
	if got := statusSnapshotTimeout(configured); got != 17*time.Second {
		t.Fatalf("statusSnapshotTimeout(status_snapshot_timeout=17s) = %s, want 17s; the documented knob is not wired to a reader", got)
	}
}

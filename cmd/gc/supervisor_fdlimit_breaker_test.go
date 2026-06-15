package main

import (
	"strings"
	"testing"
	"time"
)

// TestSupervisorPlistSetsFDResourceLimits guards the incident-2026-06-15 fix:
// the generated launchd plist must raise NumberOfFiles so the managed dolt
// server (spawned by the supervisor, inheriting its limit) is not capped at the
// 256-default FD limit that co-limited connection saturation.
func TestSupervisorPlistSetsFDResourceLimits(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:       "/usr/local/bin/gc",
		LogPath:      "/home/user/.gc/supervisor.log",
		GCHome:       "/home/user/.gc",
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{
		"<key>SoftResourceLimits</key>",
		"<key>HardResourceLimits</key>",
		"<key>NumberOfFiles</key>",
		"<integer>8192</integer>",
		"<integer>16384</integer>",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd plist missing FD resource limit %q", check)
		}
	}
}

// TestSupervisorSystemdSetsFDLimit guards the Linux parallel of the FD fix:
// the systemd unit must raise LimitNOFILE for the same dolt-saturation reason.
func TestSupervisorSystemdSetsFDLimit(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, &supervisorServiceData{
		GCPath:       "/usr/local/bin/gc",
		LogPath:      "/home/user/.gc/supervisor.log",
		GCHome:       "/home/user/.gc",
		LaunchdLabel: defaultSupervisorLaunchdLabel,
		Path:         "/usr/local/bin:/usr/bin:/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "LimitNOFILE=8192:16384") {
		t.Fatalf("systemd unit missing LimitNOFILE=8192:16384")
	}
}

// TestBeadStoreStartBackoff pins the breaker backoff schedule (capped
// exponential): a transient dolt blip is ridden out across ~45s instead of
// crash-looping the supervisor and re-bouncing dolt.
func TestBeadStoreStartBackoff(t *testing.T) {
	want := []time.Duration{
		3 * time.Second,  // attempt 1
		6 * time.Second,  // attempt 2
		12 * time.Second, // attempt 3
		24 * time.Second, // attempt 4 (cap)
		24 * time.Second, // attempt 5 (still capped)
	}
	for i, w := range want {
		if got := beadStoreStartBackoff(i + 1); got != w {
			t.Errorf("beadStoreStartBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := beadStoreStartBackoff(0); got != 3*time.Second {
		t.Errorf("beadStoreStartBackoff(0) = %v, want 3s (clamped to attempt 1)", got)
	}
}

package supervisordoctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckTickAgeFor(t *testing.T) {
	tests := []struct {
		name    string
		in      TickAgeInput
		wantRed bool
	}{
		{"no heartbeat yet skips", TickAgeInput{City: "c", HasHeartbeat: false, PatrolInterval: 10 * time.Second, LastTickAge: time.Hour}, false},
		{"fresh within 3x", TickAgeInput{City: "c", HasHeartbeat: true, PatrolInterval: 10 * time.Second, LastTickAge: 25 * time.Second}, false},
		{"exactly 3x not red", TickAgeInput{City: "c", HasHeartbeat: true, PatrolInterval: 10 * time.Second, LastTickAge: 30 * time.Second}, false},
		{"past 3x is red", TickAgeInput{City: "c", HasHeartbeat: true, PatrolInterval: 10 * time.Second, LastTickAge: 31 * time.Second}, true},
		{"non-positive patrol skips", TickAgeInput{City: "c", HasHeartbeat: true, PatrolInterval: 0, LastTickAge: time.Hour}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckTickAgeFor(tc.in)
			if (got != nil) != tc.wantRed {
				t.Fatalf("CheckTickAgeFor red=%v, want %v (alert=%+v)", got != nil, tc.wantRed, got)
			}
			if got != nil && got.Check != CheckNameTickAge {
				t.Errorf("alert check = %q, want %q", got.Check, CheckNameTickAge)
			}
		})
	}
}

func TestCheckSlowTicksFor(t *testing.T) {
	tests := []struct {
		name    string
		in      SlowTicksInput
		wantRed bool
	}{
		{"no samples skips", SlowTicksInput{City: "c", BreachCount: 0, SampleCount: 0, Window: 10 * time.Minute}, false},
		{"clean window not red", SlowTicksInput{City: "c", BreachCount: 0, SampleCount: 20, SlowestMs: 4000, Window: 10 * time.Minute}, false},
		{"one breach is red", SlowTicksInput{City: "c", BreachCount: 1, SampleCount: 20, SlowestMs: 443000, Window: 10 * time.Minute}, true},
		{"all breached is red", SlowTicksInput{City: "c", BreachCount: 20, SampleCount: 20, SlowestMs: 55000, Window: 10 * time.Minute}, true},
		// A breach count with zero samples is contradictory gatherer input;
		// skip rather than alert on it.
		{"breaches without samples skips", SlowTicksInput{City: "c", BreachCount: 3, SampleCount: 0, Window: 10 * time.Minute}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckSlowTicksFor(tc.in)
			if (got != nil) != tc.wantRed {
				t.Fatalf("CheckSlowTicksFor red=%v, want %v (alert=%+v)", got != nil, tc.wantRed, got)
			}
			if got != nil && got.Check != CheckNameSlowTicks {
				t.Errorf("alert check = %q, want %q", got.Check, CheckNameSlowTicks)
			}
			if got != nil && got.Subject != tc.in.City {
				t.Errorf("alert subject = %q, want %q", got.Subject, tc.in.City)
			}
		})
	}
}

func TestCheckAgentConfigIsolation(t *testing.T) {
	base := t.TempDir()

	// Root A: clean — only an internal (in-root) symlink, which is allowed.
	cleanRoot := filepath.Join(base, "agent-clean")
	mustMkdir(t, filepath.Join(cleanRoot, "plugins"))
	internalTarget := filepath.Join(cleanRoot, "real")
	mustMkdir(t, internalTarget)
	mustSymlink(t, internalTarget, filepath.Join(cleanRoot, "plugins", "link"))

	// Root B: escaping — a symlink to an out-of-root directory (incident 11).
	outside := filepath.Join(base, "operator-home", "plugins")
	mustMkdir(t, outside)
	escapeRoot := filepath.Join(base, "agent-escape")
	mustMkdir(t, escapeRoot)
	mustSymlink(t, outside, filepath.Join(escapeRoot, "plugins"))

	// Root C: missing — skipped, never red.
	missingRoot := filepath.Join(base, "agent-missing")

	alerts := CheckAgentConfigIsolation([]string{cleanRoot, escapeRoot, missingRoot})
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want exactly 1 (the escaping root)", alerts)
	}
	if alerts[0].Check != CheckNameAgentConfigIsolation {
		t.Errorf("alert check = %q, want %q", alerts[0].Check, CheckNameAgentConfigIsolation)
	}
	wantAbs, _ := filepath.Abs(escapeRoot)
	if alerts[0].Subject != wantAbs {
		t.Errorf("alert subject = %q, want %q", alerts[0].Subject, wantAbs)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

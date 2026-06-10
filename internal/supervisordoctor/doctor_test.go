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

func TestCheckS6ConnectionCeiling(t *testing.T) {
	tests := []struct {
		name    string
		in      S6Input
		wantRed bool
	}{
		// 8 scopes × (pool 2 + 1) = 24 ≤ 0.8×256 = 204.8 → ok.
		{"within budget", S6Input{Scopes: 8, PoolSize: 2, MaxConnections: 256}, false},
		// 8 scopes × (pool 30 + 1) = 248 > 204.8 → red.
		{"over budget", S6Input{Scopes: 8, PoolSize: 30, MaxConnections: 256}, true},
		// exactly at ceiling: 4 × (pool 39 + 1) = 160 = 0.8×200 → not red.
		{"exactly at ceiling", S6Input{Scopes: 4, PoolSize: 39, MaxConnections: 200}, false},
		{"unknown max skips", S6Input{Scopes: 8, PoolSize: 30, MaxConnections: 0}, false},
		{"zero scopes skips", S6Input{Scopes: 0, PoolSize: 30, MaxConnections: 256}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckS6ConnectionCeiling(tc.in)
			if (got != nil) != tc.wantRed {
				t.Fatalf("CheckS6ConnectionCeiling red=%v, want %v (alert=%+v)", got != nil, tc.wantRed, got)
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

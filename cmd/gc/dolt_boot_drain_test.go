package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeBootDrainDB lays a minimal dolt-shaped database dir: .dolt/repo_state.json.
func makeBootDrainDB(t *testing.T, root, name, head string, remotes map[string]any) {
	t.Helper()
	dir := filepath.Join(root, name, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"head": head, "remotes": remotes}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo_state.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

type bootDrainCall struct {
	dir, remote, branch string
}

func stubBootDrainPush(t *testing.T, calls *[]bootDrainCall, fail map[string]error, advance time.Duration) func() {
	t.Helper()
	prevPush := managedDoltBootDrainPushFn
	prevNow := bootDrainNowFn
	// A fake clock instead of wall sleeps: each push "takes" advance. The
	// resource census forbids growing the fixed-sleep ledger, and it is right —
	// a slept test is a flaky test.
	now := time.Unix(1_700_000_000, 0)
	bootDrainNowFn = func() time.Time { return now }
	managedDoltBootDrainPushFn = func(dbDir, remote, branch string, _ time.Duration) error {
		*calls = append(*calls, bootDrainCall{dbDir, remote, branch})
		now = now.Add(advance)
		if err, ok := fail[filepath.Base(dbDir)]; ok {
			return err
		}
		return nil
	}
	return func() { managedDoltBootDrainPushFn = prevPush; bootDrainNowFn = prevNow }
}

// A database with an origin remote is pushed at ITS OWN checked-out branch —
// never a guessed one. A database with no remote is skipped without noise; a
// non-database directory is ignored entirely.
func TestBootDrainPushesEachRemotedDatabaseAtItsOwnBranch(t *testing.T) {
	root := t.TempDir()
	makeBootDrainDB(t, root, "hq", "refs/heads/main", map[string]any{"origin": map[string]any{}})
	makeBootDrainDB(t, root, "vr", "refs/heads/work", map[string]any{"origin": map[string]any{}})
	makeBootDrainDB(t, root, "local-only", "refs/heads/main", map[string]any{})
	if err := os.MkdirAll(filepath.Join(root, "not-a-db"), 0o755); err != nil {
		t.Fatal(err)
	}

	var calls []bootDrainCall
	defer stubBootDrainPush(t, &calls, nil, 0)()
	var out strings.Builder
	report := runManagedDoltBootDrain(root, time.Minute, &out)

	if len(calls) != 2 {
		t.Fatalf("pushes = %d, want 2 (got %+v)", len(calls), calls)
	}
	byBase := map[string]string{}
	for _, c := range calls {
		byBase[filepath.Base(c.dir)] = c.branch
		if c.remote != "origin" {
			t.Errorf("remote = %q, want origin", c.remote)
		}
	}
	if byBase["hq"] != "main" || byBase["vr"] != "work" {
		t.Errorf("branches = %v; a push at a guessed branch would hand the remote a lineage nobody chose", byBase)
	}
	var skipped int
	for _, r := range report.Results {
		if r.Skipped != "" {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the remoteless db)", skipped)
	}
}

// A failing push must NOT block the boot or the remaining databases: five of
// nine fleet stores have hit remote-side archive corruption, and a store with
// a dead remote still needs its local server.
func TestBootDrainFailureIsLoudAndNonBlocking(t *testing.T) {
	root := t.TempDir()
	makeBootDrainDB(t, root, "aa-broken", "refs/heads/main", map[string]any{"origin": map[string]any{}})
	makeBootDrainDB(t, root, "zz-healthy", "refs/heads/main", map[string]any{"origin": map[string]any{}})

	var calls []bootDrainCall
	defer stubBootDrainPush(t, &calls, map[string]error{"aa-broken": fmt.Errorf("Blob not found: abc.darc")}, 0)()
	var out strings.Builder
	report := runManagedDoltBootDrain(root, time.Minute, &out)

	if len(calls) != 2 {
		t.Fatalf("the failure stopped the sweep: pushes = %d, want 2", len(calls))
	}
	if !strings.Contains(out.String(), "FAILED") || !strings.Contains(out.String(), "aa-broken") {
		t.Errorf("failure not reported loudly; out = %q", out.String())
	}
	var failed int
	for _, r := range report.Results {
		if r.Err != "" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

// Budget exhaustion stops the sweep AND names what it did not attempt —
// a truncated sweep that reads as complete is the vp-g2m4 shape.
func TestBootDrainBudgetExhaustionNamesTheUnattempted(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"aa", "bb", "cc"} {
		makeBootDrainDB(t, root, name, "refs/heads/main", map[string]any{"origin": map[string]any{}})
	}
	var calls []bootDrainCall
	defer stubBootDrainPush(t, &calls, nil, 60*time.Millisecond)()
	var out strings.Builder
	report := runManagedDoltBootDrain(root, 90*time.Millisecond, &out)

	if len(calls) >= 3 {
		t.Fatalf("budget did not stop the sweep: %d pushes", len(calls))
	}
	if !report.Exhaust {
		t.Error("report.Exhaust = false, want true")
	}
	if !strings.Contains(out.String(), "NOT attempted") {
		t.Errorf("unattempted stores not named; out = %q", out.String())
	}
}

// An unreadable repo_state is UNKNOWN: skipped with a reason, never pushed at
// a guessed branch (RULE 1 — unknown must not share a path with "fine").
func TestBootDrainUnreadableStateIsSkippedNotGuessed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mangled", ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo_state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []bootDrainCall
	defer stubBootDrainPush(t, &calls, nil, 0)()
	var out strings.Builder
	runManagedDoltBootDrain(root, time.Minute, &out)
	if len(calls) != 0 {
		t.Fatalf("a mangled state was pushed anyway: %+v", calls)
	}
	if !strings.Contains(out.String(), "mangled skipped") {
		t.Errorf("skip not reported; out = %q", out.String())
	}
}

// The kill switch: default ON (an unattended restart must not silently re-arm
// the ratchet), explicit off values honored.
func TestBootDrainKillSwitch(t *testing.T) {
	for value, want := range map[string]bool{"": true, "1": true, "on": true, "0": false, "off": false, "false": false, "no": false} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("GC_DOLT_BOOT_DRAIN", value)
			if got := bootDrainEnabled(); got != want {
				t.Errorf("bootDrainEnabled() with %q = %v, want %v", value, got, want)
			}
		})
	}
}

package dolt_test

// Fork-owned tests for `gc dolt sync` on a bd-owned proxied city. They reuse
// the proxied fake gc from sync_proxied_test.go and live in their own file so
// a resync can take upstream's sync_proxied_test.go without losing them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncProxiedDrainVerifiesThroughGCBD covers the fork's --drain mode on a
// proxied city: there is no managed server to refuse on, and the post-push
// residual re-fetch and re-classification go through gc bd like every other
// query.
func TestSyncProxiedDrainVerifiesThroughGCBD(t *testing.T) {
	t.Run("drained", func(t *testing.T) {
		cityPath, binDir, fake := newProxiedSyncCity(t)
		out, err := runProxiedSync(t, binDir, cityPath, []string{"FAKE_AHEAD_AFTER_PUSH=0"}, "--drain")
		if err != nil {
			t.Fatalf("proxied sync --drain failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "  hq: pushed main -> origin:main (file:///example.invalid/hq) [backlog 0, verified]") {
			t.Fatalf("output missing the verified-drain line:\n%s", out)
		}
		if got := fake.pushes(t, "hq"); len(got) != 1 {
			t.Fatalf("hq pushes = %q, want one", got)
		}
		if n := strings.Count(fake.log(t), "CALL DOLT_FETCH("); n != 2 {
			t.Fatalf("DOLT_FETCH calls = %d, want 2 (pre-push and residual):\n%s", n, fake.log(t))
		}
	})
	t.Run("not drained", func(t *testing.T) {
		cityPath, binDir, fake := newProxiedSyncCity(t)
		out, err := runProxiedSync(t, binDir, cityPath, nil, "--drain")
		if err == nil {
			t.Fatalf("proxied sync --drain succeeded with a residual backlog:\n%s", out)
		}
		if !strings.Contains(out, "BACKLOG NOT DRAINED:") {
			t.Fatalf("output missing the undrained summary:\n%s", out)
		}
		if got := fake.pushes(t, "hq"); len(got) != 3 {
			t.Fatalf("hq pushes = %q, want 3 drain rounds", got)
		}
	})
}

// TestSyncProxiedFetchFailureIsNotAColdWall: a failed fetch on a proxied scope
// is reported as a fetch failure with its stderr, never as a cold-open wall.
// The wall is classified against gc's managed listener deadline, which does
// not describe bd's listener. A 1s deadline makes every failure slow enough
// to have been misclassified before the fix.
func TestSyncProxiedFetchFailureIsNotAColdWall(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	cfg := filepath.Join(t.TempDir(), "dolt-config.yaml")
	if err := os.WriteFile(cfg, []byte("listener:\n  read_timeout_millis: 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProxiedSync(t, binDir, cityPath, []string{
		"FAKE_FETCH_FAIL_DBS=hq",
		"GC_DOLT_CONFIG_FILE=" + cfg,
	})
	if err == nil {
		t.Fatalf("proxied sync succeeded despite a failed fetch:\n%s", out)
	}
	if strings.Contains(out, "COLD-OPEN WALL") || strings.Contains(out, "NO OFF-BOX COPY") {
		t.Fatalf("a failed proxied fetch was reported as a cold-open wall:\n%s", out)
	}
	for _, want := range []string{
		"  hq: fetch failed (exit 1) after ",
		"  hq: Error: query error: Error 1105 (HY000): permission denied fetching hq",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if summary := lines[len(lines)-1]; !strings.Contains(summary, "hq (fetch failed (exit 1))") {
		t.Fatalf("last line does not name the fetch failure: %q\n%s", summary, out)
	}
	if got := fake.pushes(t, "hq"); len(got) != 0 {
		t.Fatalf("hq pushed after a failed fetch: %q", got)
	}
}

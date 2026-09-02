package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// TestReapPhantomSessionBeadsMustScanTheSessionsStore executes the failure
// mode of handing the phantom reaper the WORK store on a city where
// [beads.classes.sessions] relocates session beads: the reaper lists
// type=session beads in whatever store it is given, finds none in the work
// store, and returns 0 with no error — phantom-asleep session beads silently
// accumulate in the relocated store and the reconciler keeps treating dead
// runtimes as live. Its sibling reapStaleSessionBeads was converted to
// cr.sessionsBeadStore().Store; the phantom reaper's two call sites in
// city_runtime.go were not.
func TestReapPhantomSessionBeadsMustScanTheSessionsStore(t *testing.T) {
	t.Parallel()

	// Relocated topology: session beads live ONLY in the sessions store.
	workStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	if _, err := sessionsStore.Create(beads.Bead{
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "phantom-1",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("create phantom: %v", err)
	}
	sp := newPhantomTestProvider(nil, nil) // no live runtimes

	var stderr bytes.Buffer
	if got := reapPhantomSessionBeads(workStore, sp, newDrainTracker(), clock.Real{}, &stderr); got != 0 {
		t.Fatalf("work store unexpectedly reaped %d; fixture is mistargeted", got)
	}
	if got := reapPhantomSessionBeads(sessionsStore, sp, newDrainTracker(), clock.Real{}, &stderr); got != 1 {
		t.Fatalf("sessions store reaped %d, want 1 — the phantom is invisible unless the reaper is handed the SESSIONS store", got)
	}
}

// TestPhantomReaperCallSitesUseSessionsStore is the wiring lock, in the same
// source-lock idiom as TestFrontDoorStoreFreeFilesStayStoreFree: every
// reapPhantomSessionBeads call in city_runtime.go must receive
// cr.sessionsBeadStore().Store — the store that follows a
// [beads.classes.sessions] relocation — matching every sibling
// session-lifecycle call (reapStaleSessionBeads, sweepProcessTableOrphans,
// cleanupDeadRuntimeSessionCorpses). At the default bd backend the two
// accessors are byte-identical, which is exactly why a wrong-store call site
// fails no functional test until a relocated city hits it in production.
func TestPhantomReaperCallSitesUseSessionsStore(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(currentFile), "city_runtime.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	calls := regexp.MustCompile(`reapPhantomSessionBeads\(([^,]+),`).FindAllStringSubmatch(string(data), -1)
	if len(calls) == 0 {
		t.Fatal("no reapPhantomSessionBeads call sites found in city_runtime.go; if the reaper moved, move this lock with it")
	}
	for _, m := range calls {
		arg := strings.TrimSpace(m[1])
		if arg != "cr.sessionsBeadStore().Store" {
			t.Errorf("reapPhantomSessionBeads receives %q in city_runtime.go, want cr.sessionsBeadStore().Store — "+
				"under [beads.classes.sessions] relocation any other store silently reaps nothing", arg)
		}
	}
}

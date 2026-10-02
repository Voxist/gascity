package main

import (
	"path/filepath"
	"testing"
)

// TestWriteDoltRuntimeStateFilePreservesWatchdogForSamePID pins the ga-3bwmf
// review finding: an adoption rewrite for a pid that never restarted must
// not silently clear the durable Watchdog fact recorded at spawn time.
//
// The live symptom this reproduces: `gc dolt-state write-provider` (the
// command every kickstart-adoption rewrite in gc-beads-bd.sh calls) has no
// --watchdog flag, so its state always carries the zero value; without this
// guard, rewriting the SAME already-supervised pid's state clears Watchdog
// back to false and the dolt-watchdog-liveness doctor check goes blind
// exactly after the next kickstart adopts.
func TestWriteDoltRuntimeStateFilePreservesWatchdogForSamePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dolt-provider-state.json")

	first := doltRuntimeState{Running: true, PID: 83926, Port: 48770, DataDir: "/d", StartedAt: "2026-09-13T10:52:11Z", Watchdog: true}
	if err := writeDoltRuntimeStateFile(path, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Same pid, an adoption rewrite carrying no watchdog knowledge (the
	// zero value) — exactly what write-provider always sends.
	adopted := first
	adopted.Watchdog = false
	if err := writeDoltRuntimeStateFile(path, adopted); err != nil {
		t.Fatalf("adoption rewrite: %v", err)
	}
	got, err := readDoltRuntimeStateFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.Watchdog {
		t.Error("Watchdog = false, want true preserved — a pid that never restarted cannot lose its spawn-time watchdog fact")
	}
	if got.Port != 48770 || got.PID != 83926 {
		t.Errorf("other fields must still be written: got pid=%d port=%d", got.PID, got.Port)
	}
}

// TestWriteDoltRuntimeStateFileWatchdogFallThroughs covers every case that
// must NOT preserve, so the guard cannot pin a stale watchdog fact across a
// genuine restart.
func TestWriteDoltRuntimeStateFileWatchdogFallThroughs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priorPID     int
		priorWD      bool
		newPID       int
		incomingWD   bool
		wantWatchdog bool
	}{
		{"different pid is a real restart, incoming false", 83926, true, 99999, false, false},
		{"different pid is a real restart, incoming true", 83926, false, 99999, true, true},
		{"stop write carries PID 0", 83926, true, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: tc.priorPID, Port: 1, DataDir: "/d", StartedAt: "2026-09-13T10:52:11Z", Watchdog: tc.priorWD}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: tc.newPID, Port: 1, DataDir: "/d", StartedAt: "2026-09-13T13:01:53Z", Watchdog: tc.incomingWD}); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := readDoltRuntimeStateFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Watchdog != tc.wantWatchdog {
				t.Errorf("Watchdog = %v, want %v", got.Watchdog, tc.wantWatchdog)
			}
		})
	}

	// No prior file at all: nothing to preserve.
	path := filepath.Join(t.TempDir(), "fresh.json")
	if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: 7, Port: 1, DataDir: "/d", StartedAt: "2026-09-13T13:01:53Z", Watchdog: true}); err != nil {
		t.Fatalf("fresh write: %v", err)
	}
	got, err := readDoltRuntimeStateFile(path)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	if !got.Watchdog {
		t.Error("fresh Watchdog = false, want true (nothing to preserve, incoming value used as-is)")
	}
}

// TestWriteDoltRuntimeStateFileSpawnAuthoritativeWinsOnPIDReuse pins the
// ga-3bwmf review round-3 M2 finding: writeDoltRuntimeStateFile compares
// only the PID NUMBER, not process identity, so a genuine fresh spawn whose
// newly-chosen pid happens to reuse a STALE on-disk pid (left by an
// unrelated, earlier process) must not have its own correctly-computed
// Watchdog silently overwritten by that stale record's value.
// writeDoltRuntimeStateFileSpawnAuthoritative is the spawn path's own write
// and must always win, regardless of what pid is already on disk.
func TestWriteDoltRuntimeStateFileSpawnAuthoritativeWinsOnPIDReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	// A stale record from an earlier, unrelated process: watchdog=false,
	// pid 4242 (long dead, but nothing here proves that -- the whole point
	// of the finding is that pid-number comparison alone cannot).
	stale := doltRuntimeState{Running: false, PID: 4242, Port: 48770, DataDir: "/d", StartedAt: "2026-09-01T00:00:00Z", Watchdog: false}
	if err := writeDoltRuntimeStateFile(path, stale); err != nil {
		t.Fatalf("seed stale record: %v", err)
	}

	// A genuine fresh spawn whose OS-assigned pid happens to reuse 4242,
	// this time correctly supervised (watchdog=true).
	spawned := doltRuntimeState{Running: true, PID: 4242, Port: 48770, DataDir: "/d", StartedAt: "2026-09-30T00:00:00Z", Watchdog: true}
	if err := writeDoltRuntimeStateFileSpawnAuthoritative(path, spawned); err != nil {
		t.Fatalf("spawn-authoritative write: %v", err)
	}

	got, err := readDoltRuntimeStateFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.Watchdog {
		t.Error("Watchdog = false, want true: the spawn path's own freshly-computed value must win over a stale same-pid record, not the ordinary preserve-on-pid-match writer")
	}

	// The mirror-image case: a genuine fresh spawn that is NOT supervised
	// must also win, even against a stale record that WAS.
	path2 := filepath.Join(t.TempDir(), "state2.json")
	staleSupervised := doltRuntimeState{Running: false, PID: 4242, Port: 48770, DataDir: "/d", StartedAt: "2026-09-01T00:00:00Z", Watchdog: true}
	if err := writeDoltRuntimeStateFile(path2, staleSupervised); err != nil {
		t.Fatalf("seed stale supervised record: %v", err)
	}
	spawnedUnsupervised := doltRuntimeState{Running: true, PID: 4242, Port: 48770, DataDir: "/d", StartedAt: "2026-09-30T00:00:00Z", Watchdog: false}
	if err := writeDoltRuntimeStateFileSpawnAuthoritative(path2, spawnedUnsupervised); err != nil {
		t.Fatalf("spawn-authoritative write: %v", err)
	}
	got2, err := readDoltRuntimeStateFile(path2)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got2.Watchdog {
		t.Error("Watchdog = true, want false: the spawn path's own freshly-computed value must win even when a stale same-pid record says the opposite")
	}
}

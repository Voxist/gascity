package main

import (
	"path/filepath"
	"testing"
)

// TestWriteDoltRuntimeStateFilePreservesStartedAtForSamePID pins vc-8mfq: a
// caller that rebuilds the state and stamps a fresh started_at must not move
// the recorded start time of a process that never restarted.
//
// The live symptom this reproduces: pid 83926 ran continuously from 10:52:11Z
// while started_at walked 12:48:13 -> 12:52:02 -> 12:53:54 -> 13:01:53,
// because `gc dolt state` defaults an omitted --started-at to time.Now().
func TestWriteDoltRuntimeStateFilePreservesStartedAtForSamePID(t *testing.T) {
	const (
		realStart = "2026-09-13T10:52:11Z"
		restamp   = "2026-09-13T13:01:53Z"
	)
	path := filepath.Join(t.TempDir(), "dolt-provider-state.json")

	first := doltRuntimeState{Running: true, PID: 83926, Port: 48770, DataDir: "/d", StartedAt: realStart}
	if err := writeDoltRuntimeStateFile(path, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Same pid, fresh stamp — the restamp case.
	again := first
	again.StartedAt = restamp
	if err := writeDoltRuntimeStateFile(path, again); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := readDoltRuntimeStateFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.StartedAt != realStart {
		t.Errorf("StartedAt = %q, want %q preserved — a pid that never restarted cannot have a new start time", got.StartedAt, realStart)
	}
	if got.Port != 48770 || got.PID != 83926 {
		t.Errorf("other fields must still be written: got pid=%d port=%d", got.PID, got.Port)
	}
}

// TestWriteDoltRuntimeStateFileStartedAtFallThroughs covers every case that
// must NOT preserve, so the guard cannot silently pin a stale start time.
func TestWriteDoltRuntimeStateFileStartedAtFallThroughs(t *testing.T) {
	const prior = "2026-09-13T10:52:11Z"
	const incoming = "2026-09-13T13:01:53Z"

	for _, tc := range []struct {
		name     string
		priorPID int
		priorAt  string
		newPID   int
		want     string
	}{
		{"different pid is a real restart", 83926, prior, 99999, incoming},
		{"stop write carries PID 0", 83926, prior, 0, incoming},
		{"prior stamp empty", 83926, "", 83926, incoming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: tc.priorPID, Port: 1, DataDir: "/d", StartedAt: tc.priorAt}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: tc.newPID, Port: 1, DataDir: "/d", StartedAt: incoming}); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := readDoltRuntimeStateFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.StartedAt != tc.want {
				t.Errorf("StartedAt = %q, want %q", got.StartedAt, tc.want)
			}
		})
	}

	// No prior file at all: nothing to preserve.
	path := filepath.Join(t.TempDir(), "fresh.json")
	if err := writeDoltRuntimeStateFile(path, doltRuntimeState{Running: true, PID: 7, Port: 1, DataDir: "/d", StartedAt: incoming}); err != nil {
		t.Fatalf("fresh write: %v", err)
	}
	got, err := readDoltRuntimeStateFile(path)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	if got.StartedAt != incoming {
		t.Errorf("fresh StartedAt = %q, want %q", got.StartedAt, incoming)
	}
}

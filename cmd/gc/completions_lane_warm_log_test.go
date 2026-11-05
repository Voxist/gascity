package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// TestCompletionsSweepLogsWhyTheWarmFailed: the warm-failure line used to say
// only that it failed and when it would retry. A lane that backs off forever
// without naming the cause is indistinguishable from a transient blip; the
// error has to be on the line.
func TestCompletionsSweepLogsWhyTheWarmFailed(t *testing.T) {
	var stderr bytes.Buffer
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityBeadStore: beads.NewMemStore(),
		eventProv:     events.NewFailFake(),
	}
	cr := &CityRuntime{cityName: "test-city", cityPath: t.TempDir(), cfg: cs.cfg, cs: cs, logPrefix: "gc", stderr: &stderr}

	result := cr.runCompletionsSweepChunk(&executionevent.CompletionBackstop{}, cr.completionsLaneOf(), backstopReasonCadence)
	if !result.WarmFailed {
		t.Fatalf("result = %+v, want WarmFailed over an unreadable journal", result)
	}
	got := stderr.String()
	if !strings.Contains(got, "completions sweep: journal warm failed: ") || !strings.Contains(got, result.WarmErr.Error()) {
		t.Fatalf("warm failure logged %q, want the error %q on the line", got, result.WarmErr)
	}
}

// TestCompletionsSweepNamesASkippedArchive: a truncated archive no longer fails
// the warm, so the only trace it leaves is the warning. It must reach the log,
// or the archive is silently missing from the record.
func TestCompletionsSweepNamesASkippedArchive(t *testing.T) {
	dir := t.TempDir()
	name := "events.jsonl.archive-20261024T194703Z-seq-1-300.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for seq := 1; seq <= 300; seq++ {
		fmt.Fprintf(gz, `{"seq":%d,"type":"order.fired","ts":"2026-10-24T19:00:00Z","actor":"test","subject":"%s"}`+"\n", seq, strings.Repeat("y", 64))
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes()[:buf.Len()/2], 0o644); err != nil {
		t.Fatal(err)
	}
	journal, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })

	var stderr bytes.Buffer
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityBeadStore: beads.NewMemStore(),
		eventProv:     journal,
	}
	cr := &CityRuntime{cityName: "test-city", cityPath: t.TempDir(), cfg: cs.cfg, cs: cs, logPrefix: "gc", stderr: &stderr}

	result := cr.runCompletionsSweepChunk(&executionevent.CompletionBackstop{}, cr.completionsLaneOf(), backstopReasonStartup)
	if result.WarmFailed {
		t.Fatalf("warm failed over one truncated archive (err=%v); it must be skipped", result.WarmErr)
	}
	got := stderr.String()
	if !strings.Contains(got, "completions sweep: journal warm: ") || !strings.Contains(got, name) {
		t.Fatalf("sweep logged %q, want a warm warning naming %s", got, name)
	}
	if !strings.Contains(got, "completions sweep: reason=startup converged") {
		t.Fatalf("sweep logged %q, want the completion line: the chunk still ran", got)
	}
}

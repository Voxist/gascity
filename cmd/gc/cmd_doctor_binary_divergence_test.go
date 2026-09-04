package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// unresolvablePID is above every platform's pid ceiling, so resolving a
// process image for it always fails — which is what makes the check's output
// name the pid it was handed.
const unresolvablePID = 2147483646

func doctorCityDir(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityDir
}

// TestBuildDoctorChecks_BinaryDivergenceRegisteredAfterSupervisorHTTP pins the
// check's place in the run: it belongs with the other supervisor-identity
// checks, not scattered among the config checks.
func TestBuildDoctorChecks_BinaryDivergenceRegisteredAfterSupervisorHTTP(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	checks := buildDoctorChecks(doctorCityDir(t), cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	supervisorHTTPIdx, divergenceIdx := -1, -1
	for i, c := range checks {
		switch c.Name() {
		case "supervisor-http-api":
			supervisorHTTPIdx = i
		case "binary-divergence":
			divergenceIdx = i
		}
	}
	if supervisorHTTPIdx < 0 {
		t.Fatal("supervisor-http-api check not registered")
	}
	if divergenceIdx < 0 {
		t.Fatal("binary-divergence check not registered")
	}
	if divergenceIdx != supervisorHTTPIdx+1 {
		t.Errorf("binary-divergence at index %d, want %d (immediately after supervisor-http-api at %d)",
			divergenceIdx, supervisorHTTPIdx+1, supervisorHTTPIdx)
	}
}

// TestBuildDoctorChecks_BinaryDivergenceReceivesSupervisorPID proves the PID
// reaches the check rather than the check being constructed with a zero. A
// check handed 0 reports "supervisor not running" and compares nothing, so
// dropping the plumbing would silently disable it everywhere.
func TestBuildDoctorChecks_BinaryDivergenceReceivesSupervisorPID(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	checks := buildDoctorChecks(doctorCityDir(t), cfg, nil, buildDoctorChecksOpts{
		SupervisorRunning:    true,
		SupervisorPID:        unresolvablePID,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	var found doctor.Check
	for _, c := range checks {
		if c.Name() == "binary-divergence" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("binary-divergence check not registered")
	}

	msg := found.Run(&doctor.CheckContext{}).Message
	if !strings.Contains(msg, strconv.Itoa(unresolvablePID)) {
		t.Errorf("binary-divergence message = %q, want it to name the supervisor pid %d it was handed",
			msg, unresolvablePID)
	}
}

// TestDoDoctorPassesSupervisorPIDToBinaryDivergence closes the last gap: that
// doDoctor forwards the liveness probe's PID, not just its boolean.
func TestDoDoctorPassesSupervisorPIDToBinaryDivergence(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	cityDir := doctorCityDir(t)
	t.Setenv("GC_CITY_PATH", cityDir)

	old := supervisorAliveHook
	supervisorAliveHook = func() int { return unresolvablePID }
	t.Cleanup(func() { supervisorAliveHook = old })

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, 0, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, "binary-divergence") {
		t.Fatalf("doctor output missing the binary-divergence check:\n%s", out)
	}
	if !strings.Contains(out, strconv.Itoa(unresolvablePID)) {
		t.Errorf("doctor output does not carry the probed supervisor pid %d:\n%s", unresolvablePID, out)
	}
}

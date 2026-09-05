package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// probeSupervisorPID is a PID above every platform's ceiling. Nothing here
// probes for it — the check is stubbed — but using an impossible value keeps
// the assertion honest if the stub is ever removed.
const probeSupervisorPID = 2147483646

func doctorCityDir(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	return cityDir
}

// captureBinaryDivergencePID stubs the check's constructor and returns a
// pointer to the PID it was handed. Stubbing the constructor rather than
// running the check keeps this a unit test: the real check would shell out to
// lsof, which is invisible to the resource census and costs seconds in the
// fast lane.
func captureBinaryDivergencePID(t *testing.T) *int {
	t.Helper()
	got := -1
	old := newDoctorBinaryDivergenceCheck
	newDoctorBinaryDivergenceCheck = func(pid int) *doctor.BinaryDivergenceCheck {
		got = pid
		// Constructed with 0 so that if anything does run it, it returns
		// immediately without touching the host.
		return doctor.NewBinaryDivergenceCheck(0)
	}
	t.Cleanup(func() { newDoctorBinaryDivergenceCheck = old })
	return &got
}

// TestBuildDoctorChecks_BinaryDivergenceRegisteredAfterSupervisorHTTP pins the
// check's place in the run: it belongs with the other supervisor-identity
// checks, not scattered among the config checks.
func TestBuildDoctorChecks_BinaryDivergenceRegisteredAfterSupervisorHTTP(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	captureBinaryDivergencePID(t)
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
	got := captureBinaryDivergencePID(t)
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	buildDoctorChecks(doctorCityDir(t), cfg, nil, buildDoctorChecksOpts{
		SupervisorRunning:    true,
		SupervisorPID:        probeSupervisorPID,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	if *got != probeSupervisorPID {
		t.Errorf("binary-divergence constructed with pid %d, want %d", *got, probeSupervisorPID)
	}
}

// TestDoDoctorPassesSupervisorLivenessToBinaryDivergence closes the last gap:
// that doDoctor forwards the liveness probe's whole answer, not just its
// boolean and not just its PID.
//
// All three states matter. A supervisor that is up, executing a stale image
// and wedged on its control socket answers no ping, and a probe that reports
// that as pid 0 hands the check the one value that licenses a green verdict —
// so the check reports healthy for the exact state `gc doctor` is run to find.
// doctor.SupervisorPIDUnknown is what carries the difference across the seam,
// and it has to survive the whole way.
func TestDoDoctorPassesSupervisorLivenessToBinaryDivergence(t *testing.T) {
	oldProbe := supervisorProbePIDHook
	t.Cleanup(func() { supervisorProbePIDHook = oldProbe })

	for name, tc := range map[string]int{
		"a supervisor answered":            probeSupervisorPID,
		"the probe settled that none runs": 0,
		"the probe did not answer":         doctor.SupervisorPIDUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GC_DOLT", "skip")
			t.Setenv("GC_CITY_PATH", doctorCityDir(t))
			got := captureBinaryDivergencePID(t)
			supervisorProbePIDHook = func() int { return tc }

			var stdout, stderr bytes.Buffer
			_ = doDoctor(false, false, false, 0, &stdout, &stderr)

			if *got != tc {
				t.Errorf("doDoctor built binary-divergence with pid %d, want the probed %d", *got, tc)
			}
		})
	}
}

package main

import (
	"bytes"
	"math"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// TestConfigureSupervisorRuntime pins the GC_PPROF gate in both directions: an
// operator who sets GC_PPROF=1 to diagnose a live memory problem must keep a
// usable /debug/pprof/heap, and everyone else must not pay for a profile
// nothing reads.
func TestConfigureSupervisorRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pprof   string
		wantOff bool
	}{
		{"unset disables heap profiling", "", true},
		{"GC_PPROF=1 preserves heap profiling", "1", false},
		{"GC_PPROF=true is not an opt-in", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := goruntime.MemProfileRate
			t.Cleanup(func() { goruntime.MemProfileRate = orig })
			goruntime.MemProfileRate = 512 * 1024
			t.Setenv("GC_PPROF", tc.pprof)

			configureSupervisorRuntime()

			if tc.wantOff && goruntime.MemProfileRate != 0 {
				t.Errorf("MemProfileRate = %d, want 0", goruntime.MemProfileRate)
			}
			if !tc.wantOff && goruntime.MemProfileRate == 0 {
				t.Error("MemProfileRate = 0, want heap profiling left enabled under GC_PPROF=1")
			}
		})
	}
}

// restoreGCSettings snapshots this process's GC percent and memory limit and
// restores them when the test ends, so a GC-tuning test cannot leak a setting
// into the rest of the binary.
func restoreGCSettings(t *testing.T) {
	t.Helper()
	origPercent := debug.SetGCPercent(100)
	debug.SetGCPercent(origPercent)
	origLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() {
		debug.SetGCPercent(origPercent)
		debug.SetMemoryLimit(origLimit)
	})
}

// TestApplySupervisorGCTuningAppliesInProcessOnly pins that [supervisor] gogc
// and memory_limit_mib change only this process's runtime. GOGC and GOMEMLIMIT
// in the environment reach every child the supervisor spawns (Dolt, gc and bd
// CLIs, agent processes), and a GOMEMLIMIT there also disables the default
// limit nudge-poll sidecars install when it is unset, so the knobs must never
// be exported.
func TestApplySupervisorGCTuningAppliesInProcessOnly(t *testing.T) {
	restoreGCSettings(t)
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOGC")       //nolint:errcheck // t.Setenv restores it
	os.Unsetenv("GOMEMLIMIT") //nolint:errcheck // t.Setenv restores it

	gogc := 250
	limitMiB := int64(3072)
	var stderr bytes.Buffer
	if err := applySupervisorGCTuning(supervisor.Section{GOGC: &gogc, MemoryLimitMiB: &limitMiB}, &stderr); err != nil {
		t.Fatalf("applySupervisorGCTuning() = %v", err)
	}

	if got := debug.SetGCPercent(100); got != gogc {
		t.Errorf("GC percent = %d, want %d", got, gogc)
	}
	if got := debug.SetMemoryLimit(-1); got != limitMiB<<20 {
		t.Errorf("memory limit = %d, want %d", got, limitMiB<<20)
	}
	for _, key := range []string{"GOGC", "GOMEMLIMIT"} {
		if v, ok := os.LookupEnv(key); ok {
			t.Errorf("%s exported to the environment as %q; GC tuning must stay in-process", key, v)
		}
	}
	got := stderr.String()
	for _, want := range []string{"gogc=250 (supervisor.toml)", "memory_limit=3072MiB (supervisor.toml)"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	}
}

// TestApplySupervisorGCTuningUnsetLeavesRuntimeAlone pins the default: with
// neither key set the runtime settings are untouched and the effective values
// are still logged.
func TestApplySupervisorGCTuningUnsetLeavesRuntimeAlone(t *testing.T) {
	restoreGCSettings(t)
	debug.SetGCPercent(137)
	debug.SetMemoryLimit(math.MaxInt64)
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOGC")       //nolint:errcheck // t.Setenv restores it
	os.Unsetenv("GOMEMLIMIT") //nolint:errcheck // t.Setenv restores it

	var stderr bytes.Buffer
	if err := applySupervisorGCTuning(supervisor.Section{}, &stderr); err != nil {
		t.Fatalf("applySupervisorGCTuning() = %v", err)
	}
	if got := debug.SetGCPercent(100); got != 137 {
		t.Errorf("GC percent = %d, want unchanged 137", got)
	}
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Errorf("memory limit = %d, want unchanged MaxInt64", got)
	}
	got := stderr.String()
	for _, want := range []string{"gogc=137 (default)", "memory_limit=none (default)"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	}
}

// TestApplySupervisorGCTuningRejectsInvalid pins that an out-of-range value is
// an error, not a silent fallback, and that nothing is applied.
func TestApplySupervisorGCTuningRejectsInvalid(t *testing.T) {
	restoreGCSettings(t)
	debug.SetGCPercent(100)
	zero := 0
	var stderr bytes.Buffer
	err := applySupervisorGCTuning(supervisor.Section{GOGC: &zero}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "gogc") {
		t.Fatalf("applySupervisorGCTuning(gogc=0) = %v, want a gogc error", err)
	}
	if got := debug.SetGCPercent(100); got != 100 {
		t.Errorf("GC percent = %d, want unchanged 100 after a rejected config", got)
	}
}

// TestRunSupervisorRefusesInvalidGCTuning pins that the supervisor refuses to
// start on an invalid GC setting instead of running with a value the operator
// did not ask for.
func TestRunSupervisorRefusesInvalidGCTuning(t *testing.T) {
	neg := int64(-1)
	overflow := 1 << 31
	for _, tc := range []struct {
		name    string
		section supervisor.Section
		want    string
	}{
		{"negative memory limit", supervisor.Section{Port: 41235, MemoryLimitMiB: &neg}, "memory_limit_mib"},
		{"gogc past int32", supervisor.Section{Port: 41235, GOGC: &overflow}, "gogc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreGCSettings(t)
			t.Setenv("GC_HOME", t.TempDir())
			oldLoadConfig := supervisorLoadConfig
			supervisorLoadConfig = func(string) (supervisor.Config, error) {
				return supervisor.Config{Supervisor: tc.section}, nil
			}
			t.Cleanup(func() { supervisorLoadConfig = oldLoadConfig })

			var stdout, stderr bytes.Buffer
			if code := runSupervisor(&stdout, &stderr); code != 1 {
				t.Fatalf("runSupervisor code = %d, want 1; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want a %s error", stderr.String(), tc.want)
			}
		})
	}
}

// TestApplySupervisorGCTuningConfigOverridesEnv pins that a supervisor.toml
// value wins over an inherited GOGC/GOMEMLIMIT for the supervisor itself, and
// that the log line names supervisor.toml as the source.
func TestApplySupervisorGCTuningConfigOverridesEnv(t *testing.T) {
	restoreGCSettings(t)
	t.Setenv("GOGC", "50")
	t.Setenv("GOMEMLIMIT", "1GiB")
	gogc := 250
	limitMiB := int64(2048)
	var stderr bytes.Buffer
	if err := applySupervisorGCTuning(supervisor.Section{GOGC: &gogc, MemoryLimitMiB: &limitMiB}, &stderr); err != nil {
		t.Fatalf("applySupervisorGCTuning() = %v", err)
	}
	if got := debug.SetGCPercent(100); got != gogc {
		t.Errorf("GC percent = %d, want supervisor.toml's %d over GOGC=50", got, gogc)
	}
	if got := debug.SetMemoryLimit(-1); got != limitMiB<<20 {
		t.Errorf("memory limit = %d, want supervisor.toml's %d", got, limitMiB<<20)
	}
	for _, want := range []string{"gogc=250 (supervisor.toml)", "memory_limit=2048MiB (supervisor.toml)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// TestApplySupervisorGCTuningLogsSourceAndOff pins the log line's source
// labels (a set-but-empty env var is not a source) and the GOGC=off branch.
func TestApplySupervisorGCTuningLogsSourceAndOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gogcEnv string
		percent int
		want    string
	}{
		{"inherited env", "50", 50, "gogc=50 (GOGC env)"},
		{"empty env is default", "", 100, "gogc=100 (default)"},
		{"gc off", "off", -1, "gogc=off (GOGC env)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreGCSettings(t)
			t.Setenv("GOGC", tc.gogcEnv)
			t.Setenv("GOMEMLIMIT", "")
			debug.SetGCPercent(tc.percent)
			var stderr bytes.Buffer
			if err := applySupervisorGCTuning(supervisor.Section{}, &stderr); err != nil {
				t.Fatalf("applySupervisorGCTuning() = %v", err)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.want)
			}
			if !strings.Contains(stderr.String(), "(default); supervisor process only") {
				t.Errorf("stderr = %q, want memory_limit source default for an empty GOMEMLIMIT", stderr.String())
			}
		})
	}
}

// writeSupervisorTOML writes body as $GC_HOME/supervisor.toml under a fresh
// GC_HOME, from a working directory outside any city.
func writeSupervisorTOML(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	path := supervisor.ConfigPath()
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestSupervisorGCTuningPreflight pins the operator-facing check `gc supervisor
// start` and `install` run: an invalid or unreadable value is an error naming
// the key, and a missing supervisor.toml is neither an error nor seeded.
func TestSupervisorGCTuningPreflight(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := writeSupervisorTOML(t, "")
		if err := supervisorGCTuningPreflight(); err != nil {
			t.Fatalf("preflight = %v, want nil", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight created %s (err=%v); it must only read", path, err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		writeSupervisorTOML(t, "[supervisor]\nport = 41236\ngogc = 200\nmemory_limit_mib = 3072\n")
		if err := supervisorGCTuningPreflight(); err != nil {
			t.Fatalf("preflight = %v, want nil", err)
		}
	})
	for _, tc := range []struct{ name, body, want string }{
		{"out of range", "[supervisor]\ngogc = 0\n", "gogc = 0"},
		{"wrong type", "[supervisor]\ngogc = \"off\"\n", "gogc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSupervisorTOML(t, tc.body)
			err := supervisorGCTuningPreflight()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("preflight = %v, want an error naming %q and %s", err, tc.want, path)
			}
		})
	}
}

// TestSupervisorStartAndInstallRefuseInvalidGCTuning pins that both commands
// run the preflight before doing anything else.
func TestSupervisorStartAndInstallRefuseInvalidGCTuning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		run    func(stdout, stderr *bytes.Buffer) int
		prefix string
	}{
		{"start", func(o, e *bytes.Buffer) int { return doSupervisorStart(o, e) }, "gc supervisor start: "},
		{"install", func(o, e *bytes.Buffer) int { return doSupervisorInstall(o, e) }, "gc supervisor install: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeSupervisorTOML(t, "[supervisor]\nport = 41237\nmemory_limit_mib = -1\n")
			var stdout, stderr bytes.Buffer
			if code := tc.run(&stdout, &stderr); code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr.String())
			}
			if want := tc.prefix + "[supervisor] memory_limit_mib = -1"; !strings.HasPrefix(stderr.String(), want) {
				t.Fatalf("stderr = %q, want prefix %q", stderr.String(), want)
			}
		})
	}
}

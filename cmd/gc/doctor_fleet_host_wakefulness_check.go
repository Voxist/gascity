package main

import (
	"errors"
	"fmt"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

// errPmsetNotDarwin marks a probe attempted on a non-Darwin host. pmset is
// macOS-only; the check reports StatusOK (not applicable) rather than a
// probe failure in that case.
var errPmsetNotDarwin = errors.New("pmset is darwin-only")

// pmsetACCustomSettings holds the AC-power custom settings this check cares
// about from `pmset -g custom`. Battery settings don't gate the fleet-host
// wakefulness guard — these are always-plugged-in hosts.
type pmsetACCustomSettings struct {
	sleep     int
	disksleep int
	powernap  int
}

// readPmsetACCustom probes the live host. var so tests can stub it without
// invoking real pmset (hermeticity per vc-k7s9). Same pattern as
// providerLifecycleLaunchctlGetenv / sampleHostLoad.
var readPmsetACCustom = readPmsetACCustomReal

func readPmsetACCustomReal() (pmsetACCustomSettings, error) {
	if goruntime.GOOS != "darwin" {
		return pmsetACCustomSettings{}, errPmsetNotDarwin
	}
	out, err := exec.Command("pmset", "-g", "custom").Output() //nolint:gosec // fixed args, no user input
	if err != nil {
		return pmsetACCustomSettings{}, fmt.Errorf("pmset -g custom: %w", err)
	}
	return parsePmsetACCustom(string(out))
}

// parsePmsetACCustom parses `pmset -g custom` output, reading only the "AC
// Power:" section — a header line followed by indented "key value" lines,
// ending at the next unindented line (e.g. "Battery Power:").
func parsePmsetACCustom(out string) (pmsetACCustomSettings, error) {
	var s pmsetACCustomSettings
	found := map[string]bool{}
	inAC := false
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inAC = strings.HasPrefix(strings.TrimSpace(line), "AC Power")
			continue
		}
		if !inAC {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "sleep":
			s.sleep = val
			found["sleep"] = true
		case "disksleep":
			s.disksleep = val
			found["disksleep"] = true
		case "powernap":
			s.powernap = val
			found["powernap"] = true
		}
	}
	if !found["sleep"] || !found["disksleep"] {
		return s, fmt.Errorf("pmset -g custom: AC Power section did not report sleep/disksleep (got: %q)", out)
	}
	return s, nil
}

// fleetHostWakefulnessDoctorCheck asserts the fleet HQ host does not sleep
// on AC power. vc-zbxw (closed, operator-fixed-verified 2026-09-06): a
// sleeping host can't fire launchd probes, and DarkWake orders fetch
// without network, misfiling DNS failures as remote_corrupt CRITICAL pages.
// pmset settings don't survive restores/re-imaging/macOS updates, so this
// check exists to catch a silent re-arm loudly instead of letting
// wakefulness ride ad-hoc caffeinate timers again (vp-bhn24).
type fleetHostWakefulnessDoctorCheck struct{}

func newFleetHostWakefulnessDoctorCheck() *fleetHostWakefulnessDoctorCheck {
	return &fleetHostWakefulnessDoctorCheck{}
}

func (*fleetHostWakefulnessDoctorCheck) Name() string                     { return "fleet-host-wakefulness" }
func (*fleetHostWakefulnessDoctorCheck) CanFix() bool                     { return false }
func (*fleetHostWakefulnessDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false; this check probes host power settings, not
// anything `gc start`'s warm-up scan needs on every boot.
func (*fleetHostWakefulnessDoctorCheck) WarmupEligible() bool { return false }

func (c *fleetHostWakefulnessDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	settings, err := readPmsetACCustom()
	if errors.Is(err, errPmsetNotDarwin) {
		return &doctor.CheckResult{
			Name:    c.Name(),
			Status:  doctor.StatusOK,
			Message: fmt.Sprintf("not applicable on %s (pmset is darwin-only)", goruntime.GOOS),
		}
	}
	if err != nil {
		return &doctor.CheckResult{
			Name:    c.Name(),
			Status:  doctor.StatusWarning,
			Message: fmt.Sprintf("could not read AC power settings: %v", err),
		}
	}
	if settings.sleep > 0 || settings.disksleep > 0 {
		return &doctor.CheckResult{
			Name:   c.Name(),
			Status: doctor.StatusError,
			Message: fmt.Sprintf(
				"AC sleep=%d disksleep=%d (both must be 0): a sleeping fleet host cannot fire launchd probes, "+
					"and DarkWake orders fetch without network and misfile DNS failures as remote_corrupt CRITICAL pages (vc-zbxw)",
				settings.sleep, settings.disksleep),
			FixHint: "sudo pmset -c sleep 0 disksleep 0 powernap 0",
		}
	}
	if settings.powernap != 0 {
		return &doctor.CheckResult{
			Name:    c.Name(),
			Status:  doctor.StatusWarning,
			Message: fmt.Sprintf("AC sleep=0 disksleep=0, powernap=%d (recommend 0; warn-only)", settings.powernap),
			FixHint: "sudo pmset -c powernap 0",
		}
	}
	return &doctor.CheckResult{
		Name:    c.Name(),
		Status:  doctor.StatusOK,
		Message: "AC sleep=0 disksleep=0 powernap=0",
	}
}

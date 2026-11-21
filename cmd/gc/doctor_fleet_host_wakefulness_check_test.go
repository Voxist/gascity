package main

import (
	"errors"
	"strings"
	"testing"

	goruntime "runtime"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestParsePmsetACCustomReadsOnlyACPowerSection(t *testing.T) {
	out := strings.Join([]string{
		"AC Power:",
		" Sleep On Power Button 1",
		" lowpowermode         0",
		" powernap             0",
		" sleep                0",
		" disksleep            0",
		"Battery Power:",
		" sleep                5", // must NOT leak into the AC reading
		" disksleep            3",
		"",
	}, "\n")

	got, err := parsePmsetACCustom(out)
	if err != nil {
		t.Fatalf("parsePmsetACCustom: %v", err)
	}
	want := pmsetACCustomSettings{sleep: 0, disksleep: 0, powernap: 0}
	if got != want {
		t.Fatalf("parsed = %+v, want %+v (Battery Power values must not leak into AC)", got, want)
	}
}

func TestParsePmsetACCustomReportsNonZeroSleep(t *testing.T) {
	out := strings.Join([]string{
		"AC Power:",
		" sleep                10",
		" disksleep            10",
		" powernap             1",
		"",
	}, "\n")

	got, err := parsePmsetACCustom(out)
	if err != nil {
		t.Fatalf("parsePmsetACCustom: %v", err)
	}
	if got.sleep != 10 || got.disksleep != 10 || got.powernap != 1 {
		t.Fatalf("parsed = %+v, want sleep=10 disksleep=10 powernap=1", got)
	}
}

func TestParsePmsetACCustomErrorsWhenACSectionMissingKeys(t *testing.T) {
	out := "AC Power:\n womp 1\n"
	if _, err := parsePmsetACCustom(out); err == nil {
		t.Fatal("expected an error when the AC Power section has no sleep/disksleep keys")
	}
}

func TestFleetHostWakefulnessCheckPassesWhenSleepAndDisksleepAreZero(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{sleep: 0, disksleep: 0, powernap: 0}, nil
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q", res.Status, res.Message)
	}
}

func TestFleetHostWakefulnessCheckFailsWhenACSleepIsNonZero(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{sleep: 1, disksleep: 0, powernap: 0}, nil
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want StatusError; message=%q", res.Status, res.Message)
	}
	if res.FixHint == "" || !strings.Contains(res.FixHint, "sudo pmset") {
		t.Fatalf("FixHint = %q, want the remediation pmset command", res.FixHint)
	}
}

func TestFleetHostWakefulnessCheckFailsWhenDisksleepIsNonZero(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{sleep: 0, disksleep: 1, powernap: 0}, nil
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want StatusError; message=%q", res.Status, res.Message)
	}
}

func TestFleetHostWakefulnessCheckWarnsOnNonZeroPowernap(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{sleep: 0, disksleep: 0, powernap: 1}, nil
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning (powernap is warn-only); message=%q", res.Status, res.Message)
	}
}

func TestFleetHostWakefulnessCheckWarnsOnProbeFailure(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{}, errors.New("pmset -g custom: exit status 1")
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning on probe failure; message=%q", res.Status, res.Message)
	}
}

func TestFleetHostWakefulnessCheckReportsOKOnNonDarwin(t *testing.T) {
	prev := readPmsetACCustom
	readPmsetACCustom = func() (pmsetACCustomSettings, error) {
		return pmsetACCustomSettings{}, errPmsetNotDarwin
	}
	t.Cleanup(func() { readPmsetACCustom = prev })

	c := newFleetHostWakefulnessDoctorCheck()
	res := c.Run(nil)
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (not applicable off darwin); message=%q", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "not applicable") {
		t.Fatalf("message = %q, want it to say not applicable", res.Message)
	}
}

func TestFleetHostWakefulnessCheckNameAndFixability(t *testing.T) {
	c := newFleetHostWakefulnessDoctorCheck()
	if got := c.Name(); got != "fleet-host-wakefulness" {
		t.Fatalf("Name() = %q, want %q", got, "fleet-host-wakefulness")
	}
	if c.CanFix() {
		t.Fatal("CanFix() = true, want false (no automatic remediation — vp-bhn24 explicitly forbids it)")
	}
	if c.WarmupEligible() {
		t.Fatal("WarmupEligible() = true, want false")
	}
}

// TestReadPmsetACCustomRealOnThisHost exercises the live probe end-to-end on
// darwin hosts; it asserts shape, not values, so it stays green regardless
// of the running host's actual pmset settings.
func TestReadPmsetACCustomRealOnThisHost(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("pmset is darwin-only")
	}
	s, err := readPmsetACCustomReal()
	if err != nil {
		t.Fatalf("readPmsetACCustomReal: %v", err)
	}
	if s.sleep < 0 || s.disksleep < 0 || s.powernap < 0 {
		t.Fatalf("negative pmset value: %+v", s)
	}
}

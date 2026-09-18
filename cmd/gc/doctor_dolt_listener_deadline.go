package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doltListenerDeadlineCheck asserts that the Dolt server the swarm actually
// talks to is running at the read_timeout_millis city.toml asks for.
//
// vp-9ex9z. read_timeout_millis is the ONLY idle-connection reaper on this
// dolt version (see the generated config's own header), it is fixed at
// server start, and no SET GLOBAL moves it — so a server that booted with
// the wrong deadline keeps it for its whole lifetime and nothing downstream
// notices. On 2026-09-05 the ADR-0064 delivery window rendered its raised
// 600000 into the SHARED config and the server that ended up serving 48770
// kept it, while city.toml and the process env both said 30000. Every
// artifact anyone would have checked (city.toml, the env, `gc config`)
// agreed and was wrong about the live listener. This check reads the two
// places that can disagree with the config — the rendered YAML the next
// start will use, and the `Starting server` record the RUNNING server wrote
// to dolt.log — and fails when either drifts from the effective configured
// value.
type doltListenerDeadlineCheck struct {
	cityPath string
	cfg      *config.City
}

func newDoltListenerDeadlineCheck(cityPath string, cfg *config.City) *doltListenerDeadlineCheck {
	return &doltListenerDeadlineCheck{cityPath: cityPath, cfg: cfg}
}

// Name implements doctor.Check.
func (*doltListenerDeadlineCheck) Name() string { return "dolt-listener-deadline" }

// CanFix implements doctor.Check. Repair is a config re-render plus a Dolt
// restart, which is an operator-supervised action on a live work ledger, not
// something `gc doctor --fix` may do behind the operator's back.
func (*doltListenerDeadlineCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*doltListenerDeadlineCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check. It tails a log that reaches
// hundreds of MB on a busy city and inspects the port holder, so it stays
// out of `gc start` warm-up.
func (*doltListenerDeadlineCheck) WarmupEligible() bool { return false }

// doltListenerDeadlineLogTailBytes bounds the dolt.log read. The live city's
// dolt.log was 225 MB on 2026-09-05, so the check reads only the tail; a
// `Starting server` line is emitted at every start and is far inside this
// window even after a noisy boot.
const doltListenerDeadlineLogTailBytes = 256 * 1024

// doltStartingServerTimeoutPattern matches the T= field of dolt's
// `Starting server with Config HP="127.0.0.1:48770"|T="30000"|R="false"`
// startup line, with or without the quotes.
var doltStartingServerTimeoutPattern = regexp.MustCompile(`T="?(\d+)"?`)

// doltConfigReadTimeoutPattern matches the rendered YAML's listener key.
var doltConfigReadTimeoutPattern = regexp.MustCompile(`(?m)^\s*read_timeout_millis:\s*(\d+)\s*$`)

// Run implements doctor.Check.
func (c *doltListenerDeadlineCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	name := c.Name()
	if c.cfg == nil || !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		return okCheck(name, "not using managed Dolt topology")
	}
	layout, err := resolveManagedDoltRuntimeLayout(c.cityPath)
	if err != nil {
		return okCheck(name, "no managed Dolt runtime layout for this city")
	}
	doltConfig, err := resolveManagedDoltConfigForStart(c.cityPath, -1)
	if err != nil {
		return errorCheck(name, fmt.Sprintf("resolve managed Dolt config: %v", err),
			"fix the [dolt] section of city.toml", nil)
	}
	want := doltConfig.EffectiveReadTimeoutMillis()

	renderedValue, renderedOK := readDoltConfigReadTimeoutMillis(layout.ConfigFile)
	liveValue, liveOK := readDoltLogStartingServerTimeoutMillis(layout.LogFile)

	var details []string
	mismatch := false
	if liveOK {
		if liveValue != want {
			mismatch = true
			details = append(details, fmt.Sprintf(
				"running listener deadline %d != configured %d (last %q record in %s)",
				liveValue, want, "Starting server", layout.LogFile))
		}
	}
	if renderedOK {
		if renderedValue != want {
			mismatch = true
			details = append(details, fmt.Sprintf(
				"rendered read_timeout_millis %d != configured %d (%s)",
				renderedValue, want, layout.ConfigFile))
		}
	}

	// A delivery-window server on the managed port is a distinct failure: it
	// is by construction running at the raised deadline and must never be
	// the server the swarm reaches (ADR-0064 D1).
	if port := managedDoltResolvablePortFn(c.cityPath); port != "" {
		if windowPID := deliveryWindowServerPIDFn(c.cityPath, port); windowPID > 0 {
			mismatch = true
			details = append(details, fmt.Sprintf(
				"pid %d holding port %s was started from %s — that is the delivery window's nested server, not a publishable one",
				windowPID, port, managedDoltDeliveryWindowConfigFile(layout)))
		}
	}

	if mismatch {
		return errorCheck(name,
			fmt.Sprintf("live Dolt listener deadline disagrees with city.toml (configured %d ms)", want),
			"re-render and restart managed Dolt so the listener binds at the configured deadline: `gc dolt stop && gc dolt start` (read_timeout_millis is fixed at start; SET GLOBAL cannot move it)",
			details)
	}
	if !liveOK && !renderedOK {
		return okCheck(name, fmt.Sprintf("configured %d ms; no rendered config or start record to compare yet", want))
	}
	return okCheck(name, fmt.Sprintf("listener deadline %d ms matches city.toml", want))
}

// readDoltConfigReadTimeoutMillis reads listener.read_timeout_millis out of
// the rendered managed-Dolt YAML. The generated file is written by
// writeManagedDoltConfigFile with one plain integer key, so a line match is
// exact here and avoids pulling a YAML decoder into the doctor path.
func readDoltConfigReadTimeoutMillis(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	m := doltConfigReadTimeoutPattern.FindSubmatch(data)
	if m == nil {
		return 0, false
	}
	value, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return value, true
}

// readDoltLogStartingServerTimeoutMillis returns the T= value of the LAST
// `Starting server` line in dolt.log — the deadline the currently running
// server bound with. Only the tail is read; see
// doltListenerDeadlineLogTailBytes.
func readDoltLogStartingServerTimeoutMillis(path string) (int, bool) {
	tail, err := readFileTail(path, doltListenerDeadlineLogTailBytes)
	if err != nil {
		return 0, false
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, "Starting server") {
			continue
		}
		m := doltStartingServerTimeoutPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			continue
		}
		return value, true
	}
	return 0, false
}

// readFileTail returns at most maxBytes from the end of path. A partial
// first line is acceptable: the caller scans whole lines from the end and
// a truncated head line simply fails the `Starting server` match.
func readFileTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
		size = maxBytes
	}
	buf := make([]byte, size)
	if size == 0 {
		return buf, nil
	}
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

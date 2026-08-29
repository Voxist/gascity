package main

// ADR-0064 D1 steps 1 & 3 + D2 + AC3 (vp-o52ia): the restart-triggered
// delivery window. The drain primitive itself — verified-zero
// `gc dolt sync --drain` — shipped separately in vp-p8tze (PR #130); this
// file only ARMS it on managed-server start, which is the whole of D2: the
// trap re-arms on every restart, so the window must fire on the restart,
// not on a clock. Four supervised hand-run windows (07-30 … 08-06) each
// worked and each was undone by the next restart; hq sat 4d4h / 131
// undelivered mutations with no off-box copy.
//
// THE MECHANISM. Before the swarm-facing server binds, start a NESTED
// managed server on the same data dir with read_timeout_millis raised
// (600000 measured working 2026-08-05) and publish=false — the runtime
// state that admits the swarm is never written for the window server, so
// the city stays quiesced by construction, which is the ONLY thing that
// makes the raised deadline safe (the 2026-06-15 connection pileup that
// took the fleet dark was a 30s read timeout with live swarm traffic).
// With the raised-deadline server up, run `gc dolt sync --drain` against
// it — the verified-zero drain — then stop it (releasing the NBS store
// lock) and let the outer start proceed at the managed 15s default.
//
// TWO LOAD-BEARING CONSTRAINTS (from the bead, not cosmetic):
//
//  1. The window is bounded by an overall deadline. `gc-beads-bd.sh`
//     starts the server on demand when an agent runs `bd`; an unbounded
//     window would stall the first `bd` after every restart for the whole
//     drain (hq measured 242s cold-open), fleet-wide.
//
//  2. A failed window must NOT block the server from starting. D3 makes
//     the drain RUN terminal on undelivered stores; it does not make the
//     server refuse to boot. Refusing to start Dolt because a backup did
//     not drain converts a durability gap into an availability outage —
//     strictly worse, and contrary to the ADR's rollback framing ("the
//     worst case is the status quo").
//
// AC3: a window that does not run — env-disabled, deadline-aborted, or a
// drain that refuses — is reported as a loud stderr record. It must never
// disappear silently: that is the vp-cblo shape ("skipping sweep exits 0,
// so order.completed reads as fresh").

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// defaultDeliveryWindowReadTimeoutMillis is the raised listener deadline
// for the window server only. 600000 (10m) was used successfully in the
// 2026-08-05 supervised window; the largest measured cold push (hq, 242s)
// fits inside it with margin. The swarm-facing lifetime never sees this
// value — only the nested copy does (AC5).
const defaultDeliveryWindowReadTimeoutMillis = 600000

// defaultDeliveryWindowBudget bounds the WHOLE window — nested start,
// drain, and stop — not just one query. It must cover the worst measured
// single-store drain (242s) while keeping the first `bd` call after a
// restart from stalling indefinitely (constraint 1).
const defaultDeliveryWindowBudget = 5 * time.Minute

// deliveryWindowEnabled is the opt-out. Default on: D2's whole point is
// that a start which skips the window is the defect. GC_DOLT_DELIVERY_WINDOW=0
// disables it (and emits the AC3 skip record — an operator disabling
// durability automation must be visible in the logs).
var deliveryWindowEnabled = func() bool {
	switch os.Getenv("GC_DOLT_DELIVERY_WINDOW") {
	case "0", "false", "off":
		return false
	}
	return true
}

// deliveryWindowBudget resolves the overall window deadline.
// GC_DOLT_DELIVERY_WINDOW_BUDGET accepts a positive integer (seconds).
func deliveryWindowBudget() time.Duration {
	raw := os.Getenv("GC_DOLT_DELIVERY_WINDOW_BUDGET")
	if raw == "" {
		return defaultDeliveryWindowBudget
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultDeliveryWindowBudget
	}
	return time.Duration(secs) * time.Second
}

// deliveryWindowReadTimeoutMillis resolves the window server's raised
// read_timeout. GC_DOLT_DELIVERY_WINDOW_READ_TIMEOUT_MILLIS overrides
// (positive integer).
func deliveryWindowReadTimeoutMillis() int {
	raw := os.Getenv("GC_DOLT_DELIVERY_WINDOW_READ_TIMEOUT_MILLIS")
	if raw == "" {
		return defaultDeliveryWindowReadTimeoutMillis
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultDeliveryWindowReadTimeoutMillis
	}
	return v
}

// Seams so tests can drive the window without a real dolt binary, mirroring
// the managed-dolt seam convention (managedDoltStartSQLServerFn et al.).
// defaultDeliveryWindowStart is the production nested-start: raised-deadline
// config copy, publish=false, runWindow=true. A named function (not an
// inline closure) so the seam var's initializer does not participate in the
// package initialization cycle with startManagedDoltProcessWithConfig.
func defaultDeliveryWindowStart(cityPath, host, port, user, logLevel string, timeout time.Duration, cfg config.DoltConfig) (managedDoltStartReport, error) {
	return startManagedDoltProcessWithConfig(cityPath, host, port, user, logLevel, -1, timeout, false, &cfg, true)
}

var deliveryWindowStartFn func(cityPath, host, port, user, logLevel string, timeout time.Duration, cfg config.DoltConfig) (managedDoltStartReport, error)

func init() {
	// Wired in init (not the var initializer) to break the package
	// initialization cycle: startManagedDoltProcessWithConfig →
	// runManagedDoltDeliveryWindow → this seam.
	deliveryWindowStartFn = defaultDeliveryWindowStart
}

var (
	deliveryWindowDrainFn = runDeliveryWindowDrain
	deliveryWindowStopFn  = func(cityPath, port string) error {
		_, err := stopManagedDoltProcessWithOptions(cityPath, port, false)
		return err
	}
	deliveryWindowNowFn = time.Now
)

// runDeliveryWindowDrain shells the gc binary's own pack command
// `dolt sync --drain` against the window server. The drain's env contract
// (examples/bd/dolt/commands/sync/run.sh): GC_CITY_PATH + GC_DOLT_PORT
// required, GC_DOLT_USER default root, GC_DOLT_PASSWORD optional. The
// drain exits non-zero when it cannot PROVE zero backlog (vp-p8tze D3) —
// that is the terminal record the caller reports, never a silent pass.
func runDeliveryWindowDrain(cityPath, port, user string, budget time.Duration) error {
	gcBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve gc binary for drain: %w", err)
	}
	cmd := exec.Command(gcBin, "dolt", "sync", "--drain")
	cmd.Env = append(os.Environ(),
		"GC_CITY_PATH="+cityPath,
		"GC_DOLT_PORT="+port,
		"GC_DOLT_USER="+user,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start gc dolt sync --drain: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("gc dolt sync --drain exceeded the window budget (%s)", budget)
	}
}

// deliveryWindowOutcome is what the outer start reports. Every field is
// set on every path — including the skip paths, per AC3.
type deliveryWindowOutcome struct {
	Ran      bool
	Err      string // non-empty => window ran and failed (drain or stop)
	Skipped  string // non-empty => window did not run, with the reason
	Port     int
	Duration time.Duration
	stopped  bool // guards the at-most-once stop inside one outcome
}

// runManagedDoltDeliveryWindow arms the window (ADR-0064 D1/D2). It ALWAYS
// returns an outcome — never a bare error — because constraint 2 says a
// failed window must not block the boot; the caller turns any non-clean
// outcome into an alertable record and proceeds.
func runManagedDoltDeliveryWindow(cityPath, host, port, user, logLevel string, timeout time.Duration, baseCfg config.DoltConfig) deliveryWindowOutcome {
	start := deliveryWindowNowFn()
	out := deliveryWindowOutcome{}
	budget := deliveryWindowBudget()
	deadline := start.Add(budget)

	windowCfg := baseCfg
	windowCfg.ReadTimeoutMillis = deliveryWindowReadTimeoutMillis()

	// D1 step 1: nested start, raised deadline, publish=false (the inner
	// variant's runWindow=true skips this window block — the recursion
	// guard — and skips the boot drain, which the window's own drain
	// supersedes).
	nested, err := deliveryWindowStartFn(cityPath, host, port, user, logLevel, timeout, windowCfg)
	if err != nil {
		out.Skipped = fmt.Sprintf("window server failed to start: %v", err)
		return out
	}
	out.Ran = true
	out.Port = nested.Port

	// D1 step 2: drive every store to verified zero via the drain.
	remaining := deadline.Sub(deliveryWindowNowFn())
	if remaining <= 0 {
		out.Err = fmt.Sprintf("window budget (%s) exhausted before the drain could run", budget)
		out.recordStopErr(out.stopWindow(cityPath, nested.Port))
		out.Duration = deliveryWindowNowFn().Sub(start)
		return out
	}
	if err := deliveryWindowDrainFn(cityPath, strconv.Itoa(nested.Port), user, remaining); err != nil {
		out.Err = fmt.Sprintf("gc dolt sync --drain failed: %v", err)
	}

	// D1 step 3: stop the window server so the outer start can take the
	// data dir at the managed 15s default. Always attempted — even after a
	// failed drain, leaving a raised-deadline server bound is never an
	// acceptable end state. A failed stop is itself a window failure.
	out.recordStopErr(out.stopWindow(cityPath, nested.Port))
	out.Duration = deliveryWindowNowFn().Sub(start)
	return out
}

// stopWindow stops the nested server at most once per outcome; the first
// error is returned (and recorded by the caller), subsequent calls are
// no-ops.
func (o *deliveryWindowOutcome) stopWindow(cityPath string, port int) string {
	if o.stopped || port <= 0 {
		return ""
	}
	o.stopped = true
	if err := deliveryWindowStopFn(cityPath, strconv.Itoa(port)); err != nil {
		return fmt.Sprintf("window server stop failed (port %d): %v", port, err)
	}
	return ""
}

// recordStopErr folds a stop failure into the outcome's Err, preserving an
// earlier failure if both the drain and the stop went wrong.
func (o *deliveryWindowOutcome) recordStopErr(stopErr string) {
	if stopErr != "" && o.Err == "" {
		o.Err = stopErr
	}
}

// reportDeliveryWindowOutcome emits the AC3 record. Loud, greppable, and
// present on the skip paths too — a start that skips the window is the
// defect D2 exists to prevent, so it may not be quiet.
func reportDeliveryWindowOutcome(out deliveryWindowOutcome, stderr io.Writer) {
	switch {
	case out.Ran && out.Err == "":
		fmt.Fprintf(stderr, "gc dolt: MANAGED DOLT DELIVERY WINDOW: drained to verified zero in %s (port %d)\n", out.Duration, out.Port) //nolint:errcheck
	case out.Ran:
		fmt.Fprintf(stderr, "gc dolt: MANAGED DOLT DELIVERY WINDOW FAILED (ran %s, port %d): %s\n", out.Duration, out.Port, out.Err) //nolint:errcheck
	default:
		fmt.Fprintf(stderr, "gc dolt: MANAGED DOLT DELIVERY WINDOW SKIPPED: %s\n", out.Skipped) //nolint:errcheck
	}
}

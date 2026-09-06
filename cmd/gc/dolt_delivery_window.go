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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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
	At       time.Time // when the outcome was finalized — the durable record's timestamp (AC3/AC5)
	stopped  bool      // guards the at-most-once stop inside one outcome
}

// runManagedDoltDeliveryWindow arms the window (ADR-0064 D1/D2). It ALWAYS
// returns an outcome — never a bare error — because constraint 2 says a
// failed window must not block the boot; the caller turns any non-clean
// outcome into an alertable record and proceeds.
func runManagedDoltDeliveryWindow(cityPath, host, port, user, logLevel string, timeout time.Duration, baseCfg config.DoltConfig) (out deliveryWindowOutcome) {
	start := deliveryWindowNowFn()
	// At is stamped on every exit path (including the early skip returns
	// below) via defer, so the durable record always carries a finalize
	// timestamp without duplicating the stamp at each return site.
	defer func() { out.At = deliveryWindowNowFn() }()

	// AC3: env-disabled is a skip like any other, not silence. The opt-out
	// is resolved HERE rather than at the call site so that disabling
	// durability automation — by an operator, or by the reclaim retry in
	// ensureBeadsProvider — still produces the loud record and the durable
	// outcome file.
	if !deliveryWindowEnabled() {
		out.Skipped = "GC_DOLT_DELIVERY_WINDOW disables the window; this start did not drain"
		return
	}

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
		return
	}
	out.Ran = true
	out.Port = nested.Port

	// D1 step 2: drive every store to verified zero via the drain.
	remaining := deadline.Sub(deliveryWindowNowFn())
	if remaining <= 0 {
		out.Err = fmt.Sprintf("window budget (%s) exhausted before the drain could run", budget)
		out.recordStopErr(out.stopWindow(cityPath, nested.Port))
		out.Duration = deliveryWindowNowFn().Sub(start)
		return
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
	return
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

// reportDeliveryWindowOutcome emits the AC3 record to stderr. Loud,
// greppable, and present on the skip paths too — a start that skips the
// window is the defect D2 exists to prevent, so it may not be quiet. Stderr
// alone is not the durable record, though: production captures that stream
// nowhere (measured on the sibling boot-drain path, vp-5mc4p: grep -c
// boot-drain supervisor.log = 0 on a live 7.1MB log). See
// writeDeliveryWindowOutcomeFile for the sink that survives the starting
// process exiting.
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

// managedDoltDeliveryWindowConfigSuffix names the window server's OWN
// rendered config, a sibling of the published server's. vp-9ex9z: the
// nested start used to render its raised read_timeout into the SHARED
// layout.ConfigFile — the file the comment at the top of this file already
// called a "config copy" but which the start path never actually copied.
// On 2026-09-05 (city 23:00Z and 23:52Z) that left
// .gc/runtime/packs/dolt/dolt-config.yaml holding read_timeout_millis
// 600000 while city.toml said 30000, and the server that ended up serving
// the swarm on 48770 was running with the 10-minute reaper deadline — the
// exact condition this file's header calls unsafe with live traffic.
// Separating the paths makes the leak unrepresentable: the swarm-facing
// config is only ever written by a start whose config came from city.toml.
const managedDoltDeliveryWindowConfigSuffix = ".window"

// managedDoltDeliveryWindowConfigFile derives the window server's config
// path from the published one, so it honors GC_DOLT_CONFIG_FILE and every
// other layout override exactly as layout.ConfigFile does
// (dolt-config.yaml -> dolt-config.window.yaml).
func managedDoltDeliveryWindowConfigFile(layout managedDoltRuntimeLayout) string {
	dir, base := filepath.Split(layout.ConfigFile)
	ext := filepath.Ext(base)
	return filepath.Join(dir, strings.TrimSuffix(base, ext)+managedDoltDeliveryWindowConfigSuffix+ext)
}

// deliveryWindowProcessArgsFn is the seam over the process-argv read used by
// managedDoltDeliveryWindowServerPID, so tests can describe a live server
// without spawning one.
var deliveryWindowProcessArgsFn = processArgs

// deliveryWindowInspectFn is the seam over the port/pid inspection, so the
// window-server guard is testable without a live dolt on a real port.
var deliveryWindowInspectFn = inspectManagedDoltProcess

// deliveryWindowServerPIDFn is the seam ensureBeadsProvider's reclaim
// resolves through, so it is testable without a live dolt on a real port.
var deliveryWindowServerPIDFn = managedDoltDeliveryWindowServerPID

// managedDoltResolvablePortFn is the seam over the managed-port lookup used
// by the stranded-window reclaim and by the dolt-listener-deadline doctor
// check.
var managedDoltResolvablePortFn = currentResolvableManagedDoltPort

// deliveryWindowReclaimStopFn is the seam over the stop used to reclaim a
// stranded window server. clearPublishedState is true: a published record
// pointing at a window server is itself wrong and must go.
var deliveryWindowReclaimStopFn = func(cityPath, port string) error {
	_, err := stopManagedDoltProcessWithOptions(cityPath, port, true)
	return err
}

// reclaimStrandedDeliveryWindowServer stops a delivery-window server that is
// still holding the managed port after the start that armed it died, and
// reports whether there was one.
//
// vp-9ex9z, second half. ensureBeadsProvider treats "the start reported an
// error but the health probe passes" as "the server was already live,
// publish it". That is right for a racing start and wrong for a window
// server: the window binds the managed port itself, so a start killed while
// the window is up (a slow cold drain against the provider's context
// deadline is enough) leaves a healthy Dolt answering the probe at the
// RAISED deadline. Publishing it is how 48770 came to serve the swarm at
// read_timeout_millis 600000 on 2026-09-05 while city.toml said 30000.
//
// Refusing outright would satisfy the safety rule and break ADR-0064
// constraint 2 — "refusing to start Dolt because a backup did not drain
// converts a durability gap into an availability outage, strictly worse".
// So the stranded server is STOPPED instead, which the ownership arm in
// managedDoltProcessArgsNameOwnedConfig makes possible, and the caller runs
// an ordinary start into the freed data dir.
func reclaimStrandedDeliveryWindowServer(cityPath string, stderr io.Writer) (bool, error) {
	port := managedDoltResolvablePortFn(cityPath)
	if port == "" {
		return false, nil
	}
	pid := deliveryWindowServerPIDFn(cityPath, port)
	if pid <= 0 {
		return false, nil
	}
	fmt.Fprintf(stderr, "gc dolt: MANAGED DOLT DELIVERY WINDOW STRANDED: pid %d still holds port %s at the raised read timeout; stopping it so an ordinary start can bind\n", pid, port) //nolint:errcheck
	if err := deliveryWindowReclaimStopFn(cityPath, port); err != nil {
		return true, fmt.Errorf("stop stranded delivery window server (pid %d, port %s): %w", pid, port, err)
	}
	return true, nil
}

// managedDoltDeliveryWindowServerPID reports the pid of a live server that
// was started from the delivery window's own config and is currently
// holding the managed dolt port, or 0 when the port's occupant is not a
// window server.
//
// This is the guard for the SECOND half of vp-9ex9z. The window server binds
// the same port the published server will, so once the outer start fails
// (its lock gate refuses while the window server still owns the data dir)
// anything that probes the port finds a healthy Dolt answering and is
// entitled to treat it as the managed server —
// ensureBeadsProvider's start-error recovery did exactly that and published
// it. A window server is never a publishable server: its whole safety
// argument (ADR-0064 D1) is that the swarm is not admitted to it.
func managedDoltDeliveryWindowServerPID(cityPath, port string) int {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return 0
	}
	windowConfig := managedDoltDeliveryWindowConfigFile(layout)
	info, err := deliveryWindowInspectFn(cityPath, port)
	if err != nil {
		return 0
	}
	for _, pid := range []int{info.ManagedPID, info.PortHolderPID} {
		if pid <= 0 {
			continue
		}
		args, argsErr := deliveryWindowProcessArgsFn(pid)
		if argsErr != nil {
			continue
		}
		if containsProcessConfig(args, windowConfig) {
			return pid
		}
	}
	return 0
}

// deliveryWindowOutcomeFileName is the durable AC3/AC5 record's basename,
// written under the dolt pack's state dir (same directory the provider
// already publishes runtime state into — see dolt_runtime_publication.go).
const deliveryWindowOutcomeFileName = "dolt-delivery-window-outcome.json"

// deliveryWindowOutcomeStatePath resolves the durable record's path from a
// managedDoltRuntimeLayout.PackStateDir, so it honors the same
// GC_PACK_STATE_DIR / GC_CITY_RUNTIME_DIR overrides as the rest of the
// managed-dolt runtime files.
func deliveryWindowOutcomeStatePath(packStateDir string) string {
	return filepath.Join(packStateDir, deliveryWindowOutcomeFileName)
}

// writeDeliveryWindowOutcomeFile persists the outcome record so it survives
// the starting process exiting — the architect's explicit AC3/AC5 ask
// (bead comment 2026-08-12): "armed and drained N", "armed but push
// failed", and "never armed" must be three distinguishable, machine-readable
// states, not three ways of printing to a stream nothing captures. Mirrors
// writeDoltRuntimeStateFile's convention exactly: atomic temp-file + rename
// via internal/fsys, one JSON object overwritten per publishing start (the
// record is "the last window's outcome," not an append-only log — At is
// what disambiguates a stale record from a fresh one).
func writeDeliveryWindowOutcomeFile(packStateDir string, outcome deliveryWindowOutcome) error {
	path := deliveryWindowOutcomeStatePath(packStateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

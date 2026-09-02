package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Boot-time backlog drain for the managed Dolt store (vp-6hb8).
//
// THE TRAP THIS CLOSES. Pushing over the sql-server pays a cold-open cost once
// per server lifetime per database: the git-blobstore transport has no
// server-side range reads, so the first push of a lifetime spools the store's
// whole remote blobset (measured: 822 MB / 311 git calls for a 1 GB store)
// while the listener's read_timeout — 15s in production, and deliberately so:
// it is the only working idle-connection reaper, wait_timeout being accepted
// but inert (verified 2026-08-11) — kills the query mid-open. A store that
// misses one push accumulates backlog, which makes the next attempt larger,
// which makes it miss again: measured on the live fleet, one store ratcheted
// from 0 to 8,514 unpushed commits in five days, unbackable the whole time,
// and every server restart re-arms the trap for every large store at once.
//
// THE MECHANISM. Drain each database by CLI `dolt push` BEFORE the sql-server
// starts. At that moment the caller has already waited for the data-dir LOCK
// to be free, so no server owns the store and CLI access is safe — and a CLI
// push has no listener in front of it, so no read_timeout applies and no
// config needs to be raised and restored. The server then boots cold but with
// ZERO backlog, and a cold push of a near-empty delta fits the production
// window (measured 2026-08-06: every store passed at 15s immediately after a
// drain, largest 13.2s), after which the store is warm and stays current.
//
// FAILURE POSTURE. The drain must never block the boot: a store whose remote
// is unreachable or corrupted (five of nine fleet stores have hit remote-side
// "Blob not found" archive corruption) still needs its LOCAL server. Every
// per-database failure is reported loudly and boot proceeds. The one thing the
// drain never does is force-push: a diverged store needs an ownership decision
// this code cannot make (vp-ukvx), so it is reported and left alone.

// bootDrainResult is one database's outcome.
type bootDrainResult struct {
	DB       string
	Skipped  string // non-empty reason => not attempted
	Err      string // non-empty => attempted and failed
	Duration time.Duration
}

// bootDrainReport is the whole pass.
type bootDrainReport struct {
	Enabled  bool
	Results  []bootDrainResult
	Exhaust  bool // budget ran out before all databases were attempted
	Duration time.Duration
}

// managedDoltBootDrainPushFn runs one CLI push; a test seam like the other
// managed-dolt seams in this file's siblings. The production implementation
// shells `dolt push <remote> <branch>` with the database directory as cwd.
var managedDoltBootDrainPushFn = runBootDrainPush

// bootDrainNowFn is the drain's clock; a seam so the budget logic is testable
// without wall-clock sleeps (the resource census forbids growing the
// fixed-sleep ledger, and it is right: a slept test is a flaky test).
var bootDrainNowFn = time.Now

func runBootDrainPush(dbDir, remote, branch string, timeout time.Duration) error {
	cmd := exec.Command("dolt", "push", remote, branch)
	cmd.Dir = dbDir
	// The CLI must not inherit a half-configured server env; it operates on
	// the files directly.
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	done := make(chan error, 1)
	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, truncateForLog(out.String(), 300))
		}
		return nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s: %s", timeout, truncateForLog(out.String(), 300))
	}
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// bootDrainHeadBranch reads the database's checked-out branch from
// repo_state.json. An unreadable or unexpected head is UNKNOWN — the database
// is skipped with a reason, never pushed at a guessed branch (a push aimed at
// the wrong branch is how a remote ends up holding a lineage nobody chose).
func bootDrainHeadBranch(dbDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dbDir, ".dolt", "repo_state.json"))
	if err != nil {
		return "", err
	}
	var state struct {
		Head    string                     `json:"head"`
		Remotes map[string]json.RawMessage `json:"remotes"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return "", err
	}
	const prefix = "refs/heads/"
	if !strings.HasPrefix(state.Head, prefix) {
		return "", fmt.Errorf("head %q is not a local branch ref", state.Head)
	}
	if len(state.Remotes) == 0 {
		return "", errNoBootDrainRemote
	}
	if _, ok := state.Remotes["origin"]; !ok {
		return "", fmt.Errorf("no 'origin' remote (has: %s)", strings.Join(sortedRawMessageKeys(state.Remotes), ","))
	}
	return strings.TrimPrefix(state.Head, prefix), nil
}

var errNoBootDrainRemote = fmt.Errorf("no remotes configured")

func sortedRawMessageKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runManagedDoltBootDrain drains every database under dataDir that has an
// origin remote, within an overall wall-clock budget. It NEVER returns an
// error: the report says what happened, the boot proceeds regardless, and the
// caller prints the report where the operator will see it. RULE 3: it reports
// what it DID (pushed N, skipped M, failed K), not that it finished.
func runManagedDoltBootDrain(dataDir string, budget time.Duration, out io.Writer) bootDrainReport {
	report := bootDrainReport{Enabled: true}
	started := bootDrainNowFn()
	defer func() { report.Duration = bootDrainNowFn().Sub(started) }()

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: cannot enumerate %s: %v — drain skipped, boot continues\n", dataDir, err)
		report.Results = append(report.Results, bootDrainResult{DB: dataDir, Skipped: "enumerate: " + err.Error()})
		return report
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(dataDir, e.Name(), ".dolt")); statErr != nil {
			continue // not a dolt database
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		remaining := budget - bootDrainNowFn().Sub(started)
		if remaining <= 0 {
			report.Exhaust = true
			_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: budget %s exhausted; NOT attempted: %s — their backlog remains unpushed\n",
				budget, strings.Join(names[indexOf(names, name):], ", "))
			for _, rest := range names[indexOf(names, name):] {
				report.Results = append(report.Results, bootDrainResult{DB: rest, Skipped: "budget exhausted"})
			}
			break
		}
		dbDir := filepath.Join(dataDir, name)
		branch, err := bootDrainHeadBranch(dbDir)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, errNoBootDrainRemote) {
				reason = "no remote (nothing to drain)"
			}
			report.Results = append(report.Results, bootDrainResult{DB: name, Skipped: reason})
			if !errors.Is(err, errNoBootDrainRemote) {
				_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: %s skipped: %s\n", name, reason)
			}
			continue
		}
		pushStart := bootDrainNowFn()
		pushErr := managedDoltBootDrainPushFn(dbDir, "origin", branch, remaining)
		elapsed := bootDrainNowFn().Sub(pushStart)
		if pushErr != nil {
			report.Results = append(report.Results, bootDrainResult{DB: name, Err: pushErr.Error(), Duration: elapsed})
			_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: %s push FAILED after %s (%v) — boot continues, backlog remains; a diverged or corrupted remote needs a human (vp-ukvx)\n",
				name, elapsed.Round(time.Second), truncateForLog(pushErr.Error(), 200))
			continue
		}
		report.Results = append(report.Results, bootDrainResult{DB: name, Duration: elapsed})
		_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: %s pushed in %s\n", name, elapsed.Round(time.Millisecond))
	}
	pushed, failed, skipped := 0, 0, 0
	for _, r := range report.Results {
		switch {
		case r.Err != "":
			failed++
		case r.Skipped != "":
			skipped++
		default:
			pushed++
		}
	}
	_, _ = fmt.Fprintf(out, "gc: dolt boot-drain: pushed %d, failed %d, skipped %d in %s\n",
		pushed, failed, skipped, bootDrainNowFn().Sub(started).Round(time.Second))
	return report
}

func indexOf(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return len(names)
}

// bootDrainEnabled resolves the kill switch. Default ON: the drain exists
// precisely so that an UNATTENDED restart cannot silently re-arm the backlog
// ratchet, so it must not depend on anyone remembering to enable it. The env
// switch exists for maintenance windows that manage the drain themselves.
func bootDrainEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_DOLT_BOOT_DRAIN"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// bootDrainBudget bounds the whole pass. A full-fleet drain after a long
// outage measured ~6 minutes worst case (nine stores, one at 8.5k commits);
// the default leaves headroom without letting a wedged remote hold the boot
// hostage indefinitely.
func bootDrainBudget() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("GC_DOLT_BOOT_DRAIN_BUDGET")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Minute
}

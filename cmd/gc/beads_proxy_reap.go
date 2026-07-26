package main

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Proxied-server proxy reaping (gastownhall/gascity #1978, Option 2 lifecycle).
//
// When [beads] proxied is on, gascity owns the db-proxy lifecycle: the proxy is
// spawned with a zero idle timeout so it stays warm for the city's lifetime
// (eliminating the spawn/serve/idle-die/respawn churn that sparse controller
// probes otherwise cause). Because the proxy no longer self-terminates on idle,
// gc must reap it when the city stops — otherwise a never-idle db-proxy-child
// would outlive the city. Discovery is by live process table, not pidfiles: a
// stale pidfile from a crash must never make us signal an unrelated PID, and the
// process table is the single source of truth for "what is running".

// proxiedScopeBeadsDirs returns the .beads directory for the city and every
// configured rig. Each is the parent of a db-proxy rootDir (<.beads>/proxieddb)
// when proxied mode is active.
func proxiedScopeBeadsDirs(cityPath string, cfg *config.City) []string {
	dirs := []string{filepath.Join(cityPath, ".beads")}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			dirs = append(dirs, filepath.Join(rig.Path, ".beads"))
		}
	}
	return dirs
}

// proxyChildPIDsFromPS parses `ps -axww -o pid=,command=` output and returns the
// PIDs of `bd db-proxy-child` processes whose --root lives under one of the
// given .beads directories. Matching on the .beads prefix (rather than the exact
// proxieddb path) tolerates a non-default proxied-server root. Exported-style
// pure function so the matching is unit-testable without a live process table.
func proxyChildPIDsFromPS(psOutput string, beadsDirs []string) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "db-proxy-child") {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || pid <= 0 {
			continue
		}
		root := proxyChildRootArg(fields[1])
		if root == "" {
			continue
		}
		for _, beadsDir := range beadsDirs {
			if root == beadsDir || strings.HasPrefix(root, beadsDir+string(filepath.Separator)) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// proxyChildRootArg extracts the value of the --root flag from a db-proxy-child
// command line. Returns "" when absent.
func proxyChildRootArg(command string) string {
	toks := strings.Fields(command)
	for i, tok := range toks {
		if tok == "--root" && i+1 < len(toks) {
			return toks[i+1]
		}
		if strings.HasPrefix(tok, "--root=") {
			return strings.TrimPrefix(tok, "--root=")
		}
	}
	return ""
}

// reapProxiedChildrenForCityPath loads the city config and reaps its proxied
// db-proxy children. Best-effort: a config-load failure is treated as "nothing
// to reap" so a stop path is never blocked by it.
func reapProxiedChildrenForCityPath(cityPath string) int {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return 0
	}
	return reapProxiedChildrenForCity(cityPath, cfg, io.Discard)
}

// reapProxiedChildrenForCity stops every db-proxy-child gc owns for the city
// (SIGTERM, then SIGKILL after a short grace). Best-effort and idempotent: it is
// safe to call when proxied mode is off (no matching processes) or when the
// proxies have already exited. Returns the number of PIDs signaled.
func reapProxiedChildrenForCity(cityPath string, cfg *config.City, warn io.Writer) int {
	if cfg == nil || !cfg.Beads.ProxiedEnabled() {
		return 0
	}
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output() //nolint:gosec // fixed argv, no user input
	if err != nil {
		if warn != nil {
			fmt.Fprintf(warn, "gc: proxy reap: ps failed: %v\n", err) //nolint:errcheck
		}
		return 0
	}
	pids := proxyChildPIDsFromPS(string(out), proxiedScopeBeadsDirs(cityPath, cfg))
	for _, pid := range pids {
		signalProxyChild(pid)
	}
	if len(pids) > 0 && warn != nil {
		fmt.Fprintf(warn, "gc: proxy reap: stopped %d db-proxy-child process(es)\n", len(pids)) //nolint:errcheck
	}
	return len(pids)
}

// signalProxyChild stops one db-proxy-child: SIGTERM, wait a short grace, then
// SIGKILL if still alive. ESRCH (already gone) is treated as success.
func signalProxyChild(pid int) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

//go:build integration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// DIAG ONLY (ga-ynsmt): dumps dolt process and pack-state evidence around
// the managed-bd test city teardown. Never merged.
func diagLeakDump(t *testing.T, cityPath, label string) {
	t.Helper()
	if os.Getenv("DIAGLEAK") == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== DIAG %s city=%s at=%s env GC_DOLT_DELIVERY_WINDOW=%q pid=%d\n", label, cityPath, time.Now().Format(time.RFC3339Nano), os.Getenv("GC_DOLT_DELIVERY_WINDOW"), os.Getpid())
	out, _ := exec.Command("ps", "-eo", "pid,ppid,pgid,sid,lstart,args").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "dolt sql-server") || strings.Contains(line, "gc-beads-bd") || strings.Contains(line, "sync --drain") || strings.Contains(line, " dolt start") {
			fmt.Fprintf(&b, "  ps: %s\n", line)
		}
	}
	packDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	entries, _ := os.ReadDir(packDir)
	for _, e := range entries {
		p := filepath.Join(packDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(data)
		if strings.HasSuffix(e.Name(), ".log") && len(s) > 6000 {
			s = "...\n" + s[len(s)-6000:]
		}
		fmt.Fprintf(&b, "  --- %s ---\n%s\n", e.Name(), s)
	}
	fmt.Fprint(os.Stderr, b.String())
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// hostLoadSample is one host-load observation. Field semantics match
// events.HostLoadSamplePayload.
type hostLoadSample struct {
	load1, load5, load15 float64
	runnableProcs        int
	totalCPUPercent      float64
}

// sampleHostLoad probes the live host. Package-level so tests can stub
// the probe without spawning subprocesses.
var sampleHostLoad = sampleHostLoadReal

// hostLoadSampler emits a host.load_sample event every interval until ctx
// is done. It runs on its own goroutine — never on the reconcile loop —
// so the series keeps flowing while a tick is wedged: attributing a tick
// excursion to host load is needed exactly when the tick loop cannot be
// the emitter (vp-qvqk; without the series every load excursion cost a
// manual ps/uptime forensic and left nothing retrospective). Best-effort:
// a sampler failure warns to stderr once and the loop keeps trying.
func (cr *CityRuntime) hostLoadSampler(ctx context.Context, interval time.Duration) {
	if cr.rec == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s, err := sampleHostLoad()
		if err != nil {
			if !warned {
				warned = true
				fmt.Fprintf(cr.stderr, "%s: host-load sampler: %v (suppressing further warnings; sampling continues)\n", //nolint:errcheck // best-effort stderr
					cr.logPrefix, err)
			}
			continue
		}
		payload, err := json.Marshal(events.HostLoadSamplePayload{
			Load1:           s.load1,
			Load5:           s.load5,
			Load15:          s.load15,
			Cores:           goruntime.NumCPU(),
			RunnableProcs:   s.runnableProcs,
			TotalCPUPercent: s.totalCPUPercent,
		})
		if err != nil {
			continue
		}
		cr.rec.Record(events.Event{
			Type:    events.HostLoadSample,
			Actor:   eventActor(),
			Subject: cr.cityName,
			Payload: payload,
		})
	}
}

// sampleHostLoadReal probes the live host: load averages from the
// platform's loadavg source, runnable count and summed %CPU from one ps
// pass. Load average alone cannot answer "oversubscribed or blocked?" —
// on Darwin it also counts uninterruptible waits — which is why runnable
// and %CPU ride in the same sample.
func sampleHostLoadReal() (hostLoadSample, error) {
	var s hostLoadSample
	l1, l5, l15, err := readLoadAverages()
	if err != nil {
		return s, err
	}
	runnable, cpu, err := readProcessTable()
	if err != nil {
		return s, err
	}
	return hostLoadSample{
		load1:           l1,
		load5:           l5,
		load15:          l15,
		runnableProcs:   runnable,
		totalCPUPercent: cpu,
	}, nil
}

// readLoadAverages reads the 1/5/15-minute load averages from
// /proc/loadavg where it exists (Linux) and falls back to
// `sysctl -n vm.loadavg` (Darwin/BSD).
func readLoadAverages() (l1, l5, l15 float64, err error) {
	if data, rerr := os.ReadFile("/proc/loadavg"); rerr == nil {
		return parseLoadAvgFields(string(data))
	}
	out, serr := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if serr != nil {
		return 0, 0, 0, fmt.Errorf("no loadavg source: /proc/loadavg unavailable and sysctl -n vm.loadavg failed: %w", serr)
	}
	return parseLoadAvgFields(string(out))
}

// parseLoadAvgFields extracts the first three float fields from a loadavg
// line, tolerating both the Darwin sysctl braces ("{ 1.86 2.02 2.05 }")
// and the /proc/loadavg shape ("1.86 2.02 2.05 2/345 6789" — the
// non-float runnable/total field is skipped before three values land).
func parseLoadAvgFields(raw string) (float64, float64, float64, error) {
	fields := strings.Fields(strings.NewReplacer("{", " ", "}", " ").Replace(raw))
	vals := make([]float64, 0, 3)
	for _, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			continue
		}
		vals = append(vals, v)
		if len(vals) == 3 {
			return vals[0], vals[1], vals[2], nil
		}
	}
	return 0, 0, 0, fmt.Errorf("unparseable loadavg %q", strings.TrimSpace(raw))
}

// readProcessTable runs one ps pass over every process and returns the
// count in runnable state plus the summed %CPU. ps is the portable
// (Darwin+Linux) source for per-process state without a /proc walk.
func readProcessTable() (runnable int, totalCPUPercent float64, err error) {
	out, err := exec.Command("ps", "-A", "-o", "state=,pcpu=").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps -A -o state=,pcpu=: %w", err)
	}
	runnable, totalCPUPercent = parseProcessTable(string(out))
	return runnable, totalCPUPercent, nil
}

// parseProcessTable parses `ps -A -o state=,pcpu=` output: one process
// per line, a state token (leading R = runnable/on-CPU, further modifier
// characters may follow) and a %CPU float. Unparseable lines are skipped
// — a shorter table is a degraded sample, not an error.
func parseProcessTable(out string) (runnable int, totalCPUPercent float64) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[0], "R") {
			runnable++
		}
		if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
			totalCPUPercent += v
		}
	}
	return runnable, totalCPUPercent
}

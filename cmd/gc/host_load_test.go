package main

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// syncRecorder is a race-safe events.Recorder for goroutine-emitter tests.
type syncRecorder struct {
	mu       sync.Mutex
	events   []events.Event
	recorded chan struct{}
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{recorded: make(chan struct{}, 64)}
}

func (r *syncRecorder) Record(e events.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	select {
	case r.recorded <- struct{}{}:
	default:
	}
}

func (r *syncRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

// syncBuffer is a race-safe io.Writer for goroutine-emitter tests.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestParseLoadAvgFields(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    [3]float64
		wantErr bool
	}{
		{"darwin sysctl braces", "{ 1.86 2.02 2.05 }\n", [3]float64{1.86, 2.02, 2.05}, false},
		// /proc/loadavg's 4th field (runnable/total) is not a float and must
		// not be swallowed into the triple.
		{"linux proc line", "0.52 0.58 0.59 2/345 6789\n", [3]float64{0.52, 0.58, 0.59}, false},
		{"garbage", "no loads here\n", [3]float64{}, true},
		{"too few fields", "1.5 2.5\n", [3]float64{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l1, l5, l15, err := parseLoadAvgFields(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got := [3]float64{l1, l5, l15}; got != tc.want {
				t.Fatalf("loads = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseProcessTable(t *testing.T) {
	// Shapes seen from `ps -A -o state=,pcpu=`: bare states, states with
	// modifier suffixes (R+, Ss), and whitespace-padded columns. Only a
	// leading R counts as runnable; every parseable %CPU sums.
	out := strings.Join([]string{
		"S     0.0",
		"R    12.5",
		"R+   50.0",
		"Ss    0.3",
		"U     2.2",
		"Z     0.0",
		"garbage-line",
		"",
	}, "\n")
	runnable, cpu := parseProcessTable(out)
	if runnable != 2 {
		t.Fatalf("runnable = %d, want 2 (R and R+)", runnable)
	}
	if want := 65.0; cpu != want {
		t.Fatalf("total %%CPU = %v, want %v", cpu, want)
	}
}

// TestSampleHostLoadRealOnThisHost exercises the live probe end-to-end;
// it asserts shape, not values, so it stays green on any load.
func TestSampleHostLoadRealOnThisHost(t *testing.T) {
	s, err := sampleHostLoadReal()
	if err != nil {
		t.Fatalf("sampleHostLoadReal: %v", err)
	}
	if s.load1 < 0 || s.load5 < 0 || s.load15 < 0 {
		t.Fatalf("negative load average: %+v", s)
	}
	if s.runnableProcs < 0 || s.totalCPUPercent < 0 {
		t.Fatalf("negative process-table values: %+v", s)
	}
}

// TestHostLoadSamplerEmitsTypedEvents asserts the sampler goroutine emits
// host.load_sample events with the sampled values and stops on ctx
// cancellation.
func TestHostLoadSamplerEmitsTypedEvents(t *testing.T) {
	prev := sampleHostLoad
	sampleHostLoad = func() (hostLoadSample, error) {
		return hostLoadSample{load1: 36.5, load5: 20.1, load15: 10.2, runnableProcs: 18, totalCPUPercent: 412.7}, nil
	}
	defer func() { sampleHostLoad = prev }()

	rec := newSyncRecorder()
	cr := &CityRuntime{cityName: "testcity", rec: rec, stderr: &syncBuffer{}, logPrefix: "gc test"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cr.hostLoadSampler(ctx, 2*time.Millisecond); close(done) }()

	select {
	case <-rec.recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("no host.load_sample event within 5s")
	}
	cancel()
	<-done

	evts := rec.snapshot()
	if len(evts) == 0 {
		t.Fatal("no events recorded")
	}
	e := evts[0]
	if e.Type != events.HostLoadSample {
		t.Fatalf("event type = %q, want %q", e.Type, events.HostLoadSample)
	}
	if e.Subject != "testcity" {
		t.Errorf("subject = %q, want testcity", e.Subject)
	}
	// host.load_sample is deliberately not payload-registered (SSE
	// projection deferred — see hostload_payloads.go), so decode the
	// typed struct directly rather than via events.DecodePayload.
	var p events.HostLoadSamplePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode host.load_sample payload: %v", err)
	}
	if p.Load1 != 36.5 || p.Load5 != 20.1 || p.Load15 != 10.2 {
		t.Errorf("loads = %v/%v/%v, want 36.5/20.1/10.2", p.Load1, p.Load5, p.Load15)
	}
	if p.RunnableProcs != 18 {
		t.Errorf("runnable_procs = %d, want 18", p.RunnableProcs)
	}
	if p.TotalCPUPercent != 412.7 {
		t.Errorf("total_cpu_percent = %v, want 412.7", p.TotalCPUPercent)
	}
	if p.Cores != goruntime.NumCPU() {
		t.Errorf("cores = %d, want %d", p.Cores, goruntime.NumCPU())
	}
}

// TestHostLoadSamplerWarnsOnceOnProbeFailure asserts a failing probe
// warns exactly once (fail-loud, then quiet) and emits no events.
func TestHostLoadSamplerWarnsOnceOnProbeFailure(t *testing.T) {
	prev := sampleHostLoad
	calls := make(chan struct{}, 64)
	sampleHostLoad = func() (hostLoadSample, error) {
		select {
		case calls <- struct{}{}:
		default:
		}
		return hostLoadSample{}, fmt.Errorf("probe exploded")
	}
	defer func() { sampleHostLoad = prev }()

	rec := newSyncRecorder()
	buf := &syncBuffer{}
	cr := &CityRuntime{cityName: "testcity", rec: rec, stderr: buf, logPrefix: "gc test"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cr.hostLoadSampler(ctx, 2*time.Millisecond); close(done) }()

	// Wait for at least three probe attempts so a per-attempt warning
	// would have repeated.
	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("sampler stopped probing after a failure")
		}
	}
	cancel()
	<-done

	if evts := rec.snapshot(); len(evts) != 0 {
		t.Fatalf("events recorded = %d, want 0 on probe failure", len(evts))
	}
	if got := strings.Count(buf.String(), "host-load sampler"); got != 1 {
		t.Fatalf("stderr warnings = %d, want exactly 1:\n%s", got, buf.String())
	}
}

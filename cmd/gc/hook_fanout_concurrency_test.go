package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// withHookFanOutConcurrency pins hookFanOutConcurrency for the duration of
// the test, restoring the original value on cleanup.
func withHookFanOutConcurrency(t *testing.T, n int) {
	t.Helper()
	orig := hookFanOutConcurrency
	hookFanOutConcurrency = n
	t.Cleanup(func() { hookFanOutConcurrency = orig })
}

// TestConcurrentHookStoreRunnerHotResumePathQueriesOnlyPrimary is the
// invariant concurrentHookStoreRunner exists to preserve (vp-34cpm doc
// comment, mirroring bestStoreWithWorkRanked's "nothing may preempt it, and
// short-circuiting keeps the hot resume path at one query"): when the
// primary store alone already carries this session's own in-progress work,
// no other store is queried at all, so prefetching never adds fan-out load
// to the one call every resume session makes.
func TestConcurrentHookStoreRunnerHotResumePathQueriesOnlyPrimary(t *testing.T) {
	stores := []hookStore{{dir: "own"}, {dir: "riga"}, {dir: "rigb"}, {dir: "rigc"}}
	var mu sync.Mutex
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		mu.Lock()
		calls = append(calls, dir)
		mu.Unlock()
		if dir == "own" {
			return `[{"id":"resume-1","status":"in_progress"}]`, nil
		}
		t.Fatalf("queried %q after primary resolved to in_progress work — the hot resume path must stop at one query", dir)
		return "[]", nil
	}

	fanOut := concurrentHookStoreRunner("q", stores, stores[0], run, time.Now())
	out, selected, err := bestStoreWithWorkRanked("q", stores, stores[0], fanOut, hookStoreRankOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if selected.dir != "own" {
		t.Fatalf("selected.dir = %q, want own", selected.dir)
	}
	if out != `[{"id":"resume-1","status":"in_progress"}]` {
		t.Fatalf("out = %q, want the resume row", out)
	}
	if len(calls) != 1 || calls[0] != "own" {
		t.Fatalf("calls = %v, want exactly [own]", calls)
	}
}

// TestConcurrentHookStoreRunnerMatchesSequentialAnswer proves the prefetch
// wrapper changes nothing about WHICH store bestStoreWithWorkRanked selects —
// only when the underlying queries run. Same stores, same per-store answers,
// fed through the plain sequential run and through
// concurrentHookStoreRunner, must produce byte-identical results.
func TestConcurrentHookStoreRunnerMatchesSequentialAnswer(t *testing.T) {
	cases := []struct {
		name    string
		answers map[string]string
	}{
		{
			name: "rig-routed P0 beats own-store P2",
			answers: map[string]string{
				"own":  `[{"id":"ci-1","priority":2}]`,
				"riga": `[{"id":"va-1","priority":0}]`,
				"rigb": `[]`,
			},
		},
		{
			name: "own store P1 beats rig P3",
			answers: map[string]string{
				"own":  `[{"id":"ci-1","priority":1}]`,
				"riga": `[{"id":"va-1","priority":3}]`,
				"rigb": `[]`,
			},
		},
		{
			name: "no store has ready work",
			answers: map[string]string{
				"own":  `[]`,
				"riga": `[]`,
				"rigb": `[]`,
			},
		},
		{
			name: "assigned tier in a secondary store beats routed in primary",
			answers: map[string]string{
				"own":  `[{"id":"ci-1","priority":0}]`,
				"riga": `[{"id":"va-1","priority":3,"assignee":"me"}]`,
				"rigb": `[]`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stores := []hookStore{{dir: "own"}, {dir: "riga"}, {dir: "rigb"}}
			run := func(_, dir string, _ []string) (string, error) {
				out, ok := tc.answers[dir]
				if !ok {
					t.Fatalf("unexpected store %q queried", dir)
				}
				return out, nil
			}

			wantOut, wantStore, wantErr := bestStoreWithWorkRanked("q", stores, stores[0], run, hookStoreRankOptions{})

			fanOut := concurrentHookStoreRunner("q", stores, stores[0], run, time.Now())
			gotOut, gotStore, gotErr := bestStoreWithWorkRanked("q", stores, stores[0], fanOut, hookStoreRankOptions{})

			if gotErr != wantErr {
				t.Fatalf("err = %v, want %v", gotErr, wantErr)
			}
			if gotOut != wantOut {
				t.Fatalf("out = %q, want %q", gotOut, wantOut)
			}
			if gotStore.dir != wantStore.dir {
				t.Fatalf("selected.dir = %q, want %q", gotStore.dir, wantStore.dir)
			}
		})
	}
}

// TestConcurrentHookStoreRunnerRunsStoresConcurrently is the direct
// regression test for the bead: a federated fan-out across several stores
// must cost roughly its SLOWEST single store, not their sum. Nine secondary
// stores each take simulatedLatency; sequentially that is 9x the latency,
// concurrently (bounded to 3 at a time) it is roughly 3 batches.
func TestConcurrentHookStoreRunnerRunsStoresConcurrently(t *testing.T) {
	withHookFanOutConcurrency(t, 3)

	const simulatedLatency = 40 * time.Millisecond
	const secondaryCount = 9

	stores := []hookStore{{dir: "own"}}
	for i := 0; i < secondaryCount; i++ {
		stores = append(stores, hookStore{dir: fmt.Sprintf("rig%d", i)})
	}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	run := func(_, dir string, _ []string) (string, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(simulatedLatency)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return "[]", nil
	}

	start := time.Now()
	fanOut := concurrentHookStoreRunner("q", stores, stores[0], run, time.Now())
	_, _, err := bestStoreWithWorkRanked("q", stores, stores[0], fanOut, hookStoreRankOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Sequential would be (secondaryCount+1)*simulatedLatency = 400ms.
	// Bounded-concurrent-at-3 is ceil(10/3)=4 batches = 160ms. Generous
	// margin for scheduler jitter: anything under 300ms proves real overlap.
	if elapsed >= (secondaryCount+1)*simulatedLatency*3/4 {
		t.Fatalf("elapsed = %v, want well under the sequential sum (%v) — the fan-out did not overlap", elapsed, (secondaryCount+1)*simulatedLatency)
	}
	if maxInFlight > 3 {
		t.Fatalf("maxInFlight = %d, want <= hookFanOutConcurrency (3) — the bound was not respected", maxInFlight)
	}
	if maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want > 1 — queries never actually overlapped", maxInFlight)
	}
}

// TestConcurrentHookStoreRunnerFallsBackOnUnrecognizedShape guards the
// explicit fallback: a single-store call (nothing to parallelize) and a call
// whose primary is not stores[0] (a shape no production caller builds) must
// both decline the optimization and behave exactly like the raw run.
func TestConcurrentHookStoreRunnerFallsBackOnUnrecognizedShape(t *testing.T) {
	t.Run("single store", func(t *testing.T) {
		stores := []hookStore{{dir: "own"}}
		calls := 0
		run := func(_, dir string, _ []string) (string, error) {
			calls++
			return `[{"id":"x"}]`, nil
		}
		fanOut := concurrentHookStoreRunner("q", stores, stores[0], run, time.Now())
		out, err := fanOut("q", "own", nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if out != `[{"id":"x"}]` {
			t.Fatalf("out = %q", out)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (no prefetch for a single-store shape)", calls)
		}
	})

	t.Run("primary not stores[0]", func(t *testing.T) {
		stores := []hookStore{{dir: "own"}, {dir: "riga"}}
		notFirst := hookStore{dir: "riga"}
		calls := 0
		run := func(_, dir string, _ []string) (string, error) {
			calls++
			return "[]", nil
		}
		fanOut := concurrentHookStoreRunner("q", stores, notFirst, run, time.Now())
		if _, err := fanOut("q", "own", nil); err != nil {
			t.Fatalf("err: %v", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (fallback must not prefetch)", calls)
		}
	})
}

// TestConcurrentHookStoreRunnerServesProbeCallsLive guards the one call
// bestStoreWithWorkRanked makes that the prefetch does not cover:
// hookProbeStoreRank's D1 priority probe runs a DIFFERENT command against an
// already-fetched store, so it must fall through to a live run rather than
// serve (or miss) a cached answer meant for the shared command.
func TestConcurrentHookStoreRunnerServesProbeCallsLive(t *testing.T) {
	stores := []hookStore{{dir: "own"}, {dir: "riga"}}
	var mu sync.Mutex
	probeCalls := 0
	run := func(command, dir string, _ []string) (string, error) {
		if command == "probe" {
			mu.Lock()
			probeCalls++
			mu.Unlock()
			return `[{"id":"va-1","priority":0}]`, nil
		}
		if dir == "own" {
			return `[]`, nil
		}
		// Window answer for riga has a worse (truncation-artifact) priority
		// than the probe's — the D1 refinement this test is pinning.
		return `[{"id":"va-1","priority":2}]`, nil
	}

	fanOut := concurrentHookStoreRunner("q", stores, stores[0], run, time.Now())
	out, selected, err := bestStoreWithWorkRanked("q", stores, stores[0], fanOut, hookStoreRankOptions{RankProbeCommand: "probe"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if selected.dir != "riga" {
		t.Fatalf("selected.dir = %q, want riga", selected.dir)
	}
	if out != `[{"id":"va-1","priority":2}]` {
		t.Fatalf("out = %q, want riga's window answer (the probe only refines rank, not the served row)", out)
	}
	if probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1 — the probe command must reach a live run, not the prefetch cache", probeCalls)
	}
}

// TestHookWorkQueryRunnerConcurrentSafe guards hookWorkQueryRunner's shared
// seen map and diag writer under the concurrent callers
// concurrentHookStoreRunner now makes (vp-34cpm): run under `go test -race`,
// many goroutines hitting the same runner must neither race nor garble the
// forwarded diagnostic lines.
func TestHookWorkQueryRunnerConcurrentSafe(t *testing.T) {
	var diag bytes.Buffer
	run := hookWorkQueryRunner(&diag)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := run(fmt.Sprintf(`printf 'line-%d\n' >&2; printf '[]'`, i), "", nil)
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("line-%d", i)
		if !strings.Contains(diag.String(), want) {
			t.Fatalf("diag missing %q; concurrent writes may have garbled the stream:\n%s", want, diag.String())
		}
	}
}

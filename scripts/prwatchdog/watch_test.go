package prwatchdog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type fakeSleeper struct {
	clock *fakeClock
	calls []time.Duration
}

func (s *fakeSleeper) Sleep(_ context.Context, d time.Duration) {
	s.calls = append(s.calls, d)
	s.clock.now = s.clock.now.Add(d)
}

type fetchResponse struct {
	runs []CheckRun
	err  error
}

type scriptedFetcher struct {
	// responses scripts the fetches for the head SHA; baseResponses, when
	// baseSHA is set, scripts the fetches for that SHA. The final entry of
	// each script repeats once exhausted.
	responses     []fetchResponse
	baseResponses []fetchResponse
	baseSHA       string
	headIdx       int
	baseIdx       int
	calls         []string
}

func (f *scriptedFetcher) FetchCheckRuns(_ context.Context, sha string) ([]CheckRun, error) {
	f.calls = append(f.calls, sha)
	script, idx := f.responses, &f.headIdx
	if f.baseSHA != "" && sha == f.baseSHA {
		script, idx = f.baseResponses, &f.baseIdx
	}
	i := *idx
	if i >= len(script) {
		i = len(script) - 1
	}
	*idx++
	r := script[i]
	return r.runs, r.err
}

func TestWatch_PollsUntilTerminalThenStops(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: nil},
		{runs: []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusInProgress, StartedAt: base}}},
		{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if !eval.Pass || !eval.Terminal {
		t.Fatalf("expected an eventual pass, got %+v", eval)
	}
	if len(fetcher.calls) != 3 {
		t.Fatalf("expected exactly 3 fetch attempts (stop as soon as terminal), got %d: %v", len(fetcher.calls), fetcher.calls)
	}
	if len(sleeper.calls) != 2 {
		t.Fatalf("expected exactly 2 sleeps between 3 attempts, got %d", len(sleeper.calls))
	}
	for _, headSHA := range fetcher.calls {
		if headSHA != testHeadSHA {
			t.Fatalf("fetcher must always be called with the explicit head SHA, got %q", headSHA)
		}
	}
}

func TestWatch_StopsAtDeadlineWithoutUnboundedPolling(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	// Every attempt reports "still queued" -- this never resolves on its own.
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusQueued, StartedAt: base}}},
	}}

	deadline := 25 * time.Minute
	interval := 5 * time.Minute
	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: deadline,
		Interval: interval,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected a fail-closed terminal result at the deadline, got %+v", eval)
	}
	// Bounded: with a 25m deadline and a 5m interval, only a handful of
	// attempts should occur before the deadline check stops the loop --
	// never an unbounded number.
	if len(fetcher.calls) == 0 || len(fetcher.calls) > 10 {
		t.Fatalf("expected a small bounded number of fetch attempts, got %d", len(fetcher.calls))
	}
}

func TestWatch_APIErrorStopsImmediatelyWithoutRetry(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{err: errors.New("simulated rate limit / auth / timeout error")},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected an immediate fail-closed result on API error, got %+v", eval)
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("expected exactly one fetch attempt (no retry on API error), got %d", len(fetcher.calls))
	}
	if len(sleeper.calls) != 0 {
		t.Fatalf("expected no sleep/retry after an API error, got %d sleeps", len(sleeper.calls))
	}
}

func TestWatch_FetchesBaseRunsAndPassesThroughOnBaseRed(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	const baseSHA = "base0123456"
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	// Poll 1 sees a red head with a base run still in flight -- inconclusive,
	// so the watchdog keeps observing. Poll 2 sees base red and terminates.
	headRed := []CheckRun{
		{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base},
	}
	fetcher := &scriptedFetcher{
		responses: []fetchResponse{{runs: headRed}},
		baseSHA:   baseSHA,
		baseResponses: []fetchResponse{
			{runs: []CheckRun{{Name: CIRequiredName, HeadSHA: baseSHA, Status: StatusInProgress, StartedAt: base}}},
			{runs: []CheckRun{{Name: CIRequiredName, HeadSHA: baseSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base}}},
		},
	}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		BaseSHA:  baseSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if !eval.Pass || !eval.Terminal || !eval.BaseInheritedFailure {
		t.Fatalf("expected a base-red pass-through, got %+v", eval)
	}
	if len(fetcher.calls) != 4 {
		t.Fatalf("expected 4 fetches (head+base per poll, 2 polls), got %d: %v", len(fetcher.calls), fetcher.calls)
	}
	wantOrder := []string{testHeadSHA, baseSHA, testHeadSHA, baseSHA}
	for i, want := range wantOrder {
		if fetcher.calls[i] != want {
			t.Fatalf("fetch call %d = %q, want %q", i, fetcher.calls[i], want)
		}
	}
}

func TestWatch_BaseFetchErrorKeepsObservingThenFailsClosedAtDeadline(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	const baseSHA = "base0123456"
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{
		responses: []fetchResponse{{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base},
		}}},
		baseSHA:       baseSHA,
		baseResponses: []fetchResponse{{err: errors.New("base lookup exploded")}},
	}

	// A short deadline keeps the unit test bounded: the base lookup fails on
	// every poll, so the observation loop must run until the deadline and
	// only then fail closed with the base verdict explicitly unavailable.
	deadline := 2 * time.Minute
	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		BaseSHA:  baseSHA,
		Deadline: deadline,
		Interval: 30 * time.Second,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("a permanently unavailable base verdict must fail closed on a red head, got %+v", eval)
	}
	if !strings.Contains(eval.Reason, "base verdict unavailable") {
		t.Fatalf("Reason = %q, want it to note the base verdict was unavailable", eval.Reason)
	}
	if len(sleeper.calls) != 4 {
		t.Fatalf("expected 4 sleeps across 5 polls before the 2m deadline, got %d", len(sleeper.calls))
	}
}

func TestWatch_WithoutBaseSHAFetchesHeadOnly(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if !eval.Pass || !eval.Terminal {
		t.Fatalf("expected a pass, got %+v", eval)
	}
	if len(fetcher.calls) != 1 || fetcher.calls[0] != testHeadSHA {
		t.Fatalf("BaseSHA unset must leave the fetch pattern untouched (single head fetch), got %v", fetcher.calls)
	}
}

func TestWatch_PassesThroughOptInLabels(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			// Mac opt-in run omitted deliberately: with NeedsMacLabel true
			// below, this must fail closed at the deadline instead of
			// passing on core evidence alone.
		}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:       testHeadSHA,
		NeedsMacLabel: true,
		Deadline:      10 * time.Minute,
		Interval:      10 * time.Minute,
	})

	if eval.Pass {
		t.Fatalf("expected fail because the requested Mac run never appeared, got %+v", eval)
	}
}

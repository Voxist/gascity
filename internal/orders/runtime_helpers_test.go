package orders

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type rowsErrorStore struct {
	*beads.MemStore
	rows []beads.Bead
	err  error
}

func (s *rowsErrorStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, s.err
}

func ordersStoreOver(store beads.Store) *Store {
	return NewStore(beads.OrdersStore{Store: store})
}

func TestLastRunReturnsLatestRun(t *testing.T) {
	store := beads.NewMemStore()

	first, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest", "order-tracking"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Millisecond)

	second, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest", "order-tracking", "wisp-failed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.Equal(second.CreatedAt) {
		t.Fatalf("LastRun() = %s, want %s (latest run should remain authoritative)", got, second.CreatedAt)
	}
	if !second.CreatedAt.After(first.CreatedAt) {
		t.Fatalf("test setup invalid: second.CreatedAt=%s, first.CreatedAt=%s", second.CreatedAt, first.CreatedAt)
	}
}

func TestLastRunReturnsZeroWhenNoRunsExist(t *testing.T) {
	store := beads.NewMemStore()

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("LastRun() = %s, want zero time", got)
	}
}

func TestLastRunUsesRowsFromPartialTierError(t *testing.T) {
	want := time.Date(2026, 5, 15, 7, 0, 0, 0, time.UTC)
	store := &rowsErrorStore{
		MemStore: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:        "run-1",
			Title:     "digest",
			CreatedAt: want,
			Labels:    []string{"order-run:digest", "order-tracking"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("LastRun() = %s, want %s from surviving rows", got, want)
	}
}

func TestLastRunFuncWithEventFallback_UsesStoreResult(t *testing.T) {
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	storeFn := func(string) (time.Time, error) { return want, nil }
	ep := events.NewFake()
	// Event with a later timestamp — must NOT be chosen when store succeeds.
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "digest-generate",
		Ts:      want.Add(time.Hour),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want store result %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_FallsBackToEvents(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "digest-generate",
		Ts:      want,
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want event timestamp %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_FallsBackToLatestEvent(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	older := time.Date(2026, 6, 8, 0, 23, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	ep.Record(events.Event{Type: events.OrderFired, Subject: "digest-generate", Ts: older})
	ep.Record(events.Event{Type: events.OrderFired, Subject: "digest-generate", Ts: newer})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(newer) {
		t.Fatalf("got %s, want newest event %s", got, newer)
	}
}

func TestLastRunFuncWithEventFallback_IgnoresOtherOrderEvents(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "other-order",
		Ts:      time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (event is for a different order)", got)
	}
}

func TestLastRunFuncWithEventFallback_NilProviderReturnsStoreResult(t *testing.T) {
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	storeFn := func(string) (time.Time, error) { return want, nil }

	got, err := LastRunFuncWithEventFallback(storeFn, nil)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_NilProviderZeroStore(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }

	got, err := LastRunFuncWithEventFallback(storeFn, nil)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (nil provider, no store result)", got)
	}
}

func TestLastRunFuncWithEventFallback_StoreErrorPropagated(t *testing.T) {
	wantErr := errors.New("store error")
	storeFn := func(string) (time.Time, error) { return time.Time{}, wantErr }
	ep := events.NewFake()

	_, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

func TestLastRunFuncWithEventFallback_EventErrorFailsOpen(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFailFake()

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("expected nil error (fail-open), got: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero time (fail-open on broken provider)", got)
	}
}

func TestCursorUsesRowsAndLogsPartialTierError(t *testing.T) {
	oldLogf := runtimeHelpersLogf
	var logs []string
	runtimeHelpersLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		runtimeHelpersLogf = oldLogf
	})
	store := &rowsErrorStore{
		MemStore: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:     "run-1",
			Labels: []string{"order-run:digest", "order-tracking", "seq:42"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got := ordersStoreOver(store).Cursor("digest")
	if got != 42 {
		t.Fatalf("Cursor() = %d, want 42 from surviving rows", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "partially failed") {
		t.Fatalf("logs = %#v, want partial failure log", logs)
	}
}

// TestLastRunAcrossReturnsMaxScope proves the federation helper takes the most
// recent run across scopes.
func TestLastRunAcrossReturnsMaxScope(t *testing.T) {
	early := beads.NewMemStore()
	if _, err := early.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	late := beads.NewMemStore()
	lateRun, err := late.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LastRunAcross([]*Store{ordersStoreOver(early), ordersStoreOver(late)})("digest")
	if err != nil {
		t.Fatalf("LastRunAcross(): %v", err)
	}
	if !got.Equal(lateRun.CreatedAt) {
		t.Fatalf("LastRunAcross() = %s, want %s (max across scopes)", got, lateRun.CreatedAt)
	}
}

// closedHistoryFailsStore serves every active-row read and fails, hard, every
// read that includes closed history: the shape of a backing whose closed rows
// are unreachable while the controller's cache still holds the open ones.
type closedHistoryFailsStore struct{ *beads.MemStore }

func (s closedHistoryFailsStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.IncludeClosed {
		return nil, errors.New("closed history unavailable")
	}
	return s.MemStore.List(q)
}

// Over a CachingStore, a hard backing error on the order-run history read must
// stay an error. Kills: a cached read that turns it into a partial result of
// the open rows the cache holds, which LastRun and Cursor would trust as
// surviving rows — a cooldown clock from an open run, a cursor that replays
// consumed events.
func TestLastRunAndCursorKeepAHardBackingErrorThroughACache(t *testing.T) {
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking", "seq:3"}}); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStore(closedHistoryFailsStore{backing}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	store := ordersStoreOver(cache)
	if got, err := store.LastRun("digest"); err == nil {
		t.Fatalf("LastRun() = %s, nil; a hard backing error was answered from the cache's open rows", got)
	}
	if got := store.Cursor("digest"); got != 0 {
		t.Fatalf("Cursor() = %d, want 0 (unread); a hard backing error was answered from the cache's open rows", got)
	}
}

// writeOrderFiredArchive writes a rotated events archive holding one
// order.fired event per ts, named with the seq window the reader orders on.
func writeOrderFiredArchive(t *testing.T, dir, stamp string, firstSeq, lastSeq uint64, subject string, ts ...time.Time) {
	t.Helper()
	var b strings.Builder
	for i, at := range ts {
		fmt.Fprintf(&b, `{"seq":%d,"type":%q,"ts":%q,"actor":"test","subject":%q}`+"\n",
			firstSeq+uint64(i), events.OrderFired, at.UTC().Format(time.RFC3339Nano), subject)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, firstSeq, lastSeq)
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLastRunFuncWithEventFallback_NewestFirstStopsAtNewestArchive pins
// vp-d7vcu on a real file recorder. The two oldest archives are not gzip, so
// any read of them leaves a "skipped unreadable archive" warning in the log
// (ReadLatestMatch) or fails the read (List's oldest-first walk). The middle
// archive holds an older match. The fallback must return the newest archive's
// last match and never open the archives behind it.
func TestLastRunFuncWithEventFallback_NewestFirstStopsAtNewestArchive(t *testing.T) {
	oldLogf := runtimeHelpersLogf
	var logs []string
	runtimeHelpersLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { runtimeHelpersLogf = oldLogf })

	dir := t.TempDir()
	for _, w := range []struct {
		stamp             string
		firstSeq, lastSeq uint64
	}{
		{"20260701T000000Z", 1, 100},
		{"20260702T000000Z", 101, 200},
	} {
		name := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", w.stamp, w.firstSeq, w.lastSeq)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not gzip at all"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	older := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC)
	writeOrderFiredArchive(t, dir, "20260703T000000Z", 201, 300, "digest-generate", older)
	writeOrderFiredArchive(t, dir, "20260705T000000Z", 301, 400, "digest-generate", newer, newest)

	fr, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { fr.Close() }) //nolint:errcheck
	// The active log holds only another order's event, so the answer must
	// come from the archives.
	fr.Record(events.Event{Type: events.OrderFired, Subject: "other-order", Actor: "test"})

	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	got, err := LastRunFuncWithEventFallback(storeFn, fr)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(newest) {
		t.Fatalf("got %s, want the newest archive's last match %s", got, newest)
	}
	if len(logs) != 0 {
		t.Fatalf("fallback logged %q; it opened an archive older than the newest match", logs)
	}
}

// latestOnlyProvider answers LatestMatch and fails the test on List, so a
// fallback that still walks the whole history is caught.
type latestOnlyProvider struct {
	*events.Fake
	t        *testing.T
	e        events.Event
	found    bool
	warnings []string
	err      error
	got      events.Filter
}

func (p *latestOnlyProvider) List(events.Filter) ([]events.Event, error) {
	p.t.Error("List called; the fallback must ask a LatestMatchProvider for the newest match only")
	return nil, nil
}

func (p *latestOnlyProvider) LatestMatch(filter events.Filter) (events.Event, bool, []string, error) {
	p.got = filter
	return p.e, p.found, p.warnings, p.err
}

func TestLastRunFuncWithEventFallback_UsesLatestMatchProvider(t *testing.T) {
	oldLogf := runtimeHelpersLogf
	var logs []string
	runtimeHelpersLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { runtimeHelpersLogf = oldLogf })
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	want := time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		p        latestOnlyProvider
		want     time.Time
		wantLogs int
	}{
		{name: "match", p: latestOnlyProvider{e: events.Event{Ts: want}, found: true}, want: want},
		{name: "match after a skipped archive", p: latestOnlyProvider{e: events.Event{Ts: want}, found: true, warnings: []string{"skipped unreadable archive \"a.gz\""}}, want: want, wantLogs: 1},
		{name: "no match", p: latestOnlyProvider{}},
		{name: "error fails open", p: latestOnlyProvider{err: errors.New("read failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs = nil
			p := tc.p
			p.Fake, p.t = events.NewFake(), t
			got, err := LastRunFuncWithEventFallback(storeFn, &p)("digest-generate")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if p.got.Type != events.OrderFired || p.got.Subject != "digest-generate" {
				t.Fatalf("LatestMatch filter = %+v, want order.fired for digest-generate", p.got)
			}
			if len(logs) != tc.wantLogs {
				t.Fatalf("logs = %q, want %d line(s)", logs, tc.wantLogs)
			}
		})
	}
}

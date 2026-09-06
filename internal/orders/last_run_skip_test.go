package orders

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// seedRun creates a tracking bead for scoped at the given time.
func seedRun(t *testing.T, store beads.Store, scoped string, at time.Time) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:     trackingTitle(scoped),
		Labels:    baseLabels(scoped, RunOutcomeExec),
		CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return b
}

// seedSkip creates a run_on skip record for scoped at the given time.
func seedSkip(t *testing.T, store beads.Store, scoped string, at time.Time) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:     trackingTitle(scoped),
		Labels:    append(baseLabels(scoped, RunOutcomeNone), labelOrderSkipRunOn),
		Status:    "closed",
		CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("seed skip: %v", err)
	}
	return b
}

// A run_on skip must never advance the cooldown clock. If it did, a city that
// STOPPED running an order would report the freshest possible last-run to every
// liveness reader, and the outage would look like health.
func TestLastRunIgnoresRunOnSkipRecords(t *testing.T) {
	store := beads.NewMemStore()
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	realRun := seedRun(t, store, "merge-sweep", base)
	seedSkip(t, store, "merge-sweep", base.Add(3*time.Hour))
	seedSkip(t, store, "merge-sweep", base.Add(4*time.Hour))

	st := NewStore(beads.OrdersStore{Store: store})
	got, err := st.LastRun("merge-sweep")
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !got.Equal(realRun.CreatedAt) {
		t.Fatalf("LastRun = %s, want the real run at %s — a skip advanced the clock", got, realRun.CreatedAt)
	}
}

// With nothing but skips, LastRun reports no run at all. That pushes a liveness
// reader toward reporting staleness, which is the correct direction to fail.
func TestLastRunAllSkipsReportsNoRun(t *testing.T) {
	store := beads.NewMemStore()
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	seedSkip(t, store, "merge-sweep", base)
	seedSkip(t, store, "merge-sweep", base.Add(time.Hour))

	got, err := NewStore(beads.OrdersStore{Store: store}).LastRun("merge-sweep")
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("LastRun = %s, want zero when every record is a skip", got)
	}
}

// The hot path must be unchanged: when the newest row is a real run, LastRun
// still costs exactly one bounded row read.
func TestLastRunHotPathReadsOneRow(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	seedRun(t, spy, "digest", time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC))
	spy.queries = nil

	if _, err := NewStore(beads.OrdersStore{Store: spy}).LastRun("digest"); err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if len(spy.queries) != 1 {
		t.Fatalf("queries = %d, want exactly 1 on the hot path: %+v", len(spy.queries), spy.queries)
	}
	if spy.queries[0].Limit != 1 {
		t.Fatalf("hot-path limit = %d, want 1", spy.queries[0].Limit)
	}
}

// The widened re-read happens only when the newest row IS a skip.
func TestLastRunWidensOnlyWhenNewestRowIsASkip(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	seedRun(t, spy, "digest", base)
	seedSkip(t, spy, "digest", base.Add(time.Hour))
	spy.queries = nil

	if _, err := NewStore(beads.OrdersStore{Store: spy}).LastRun("digest"); err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if len(spy.queries) != 2 {
		t.Fatalf("queries = %d, want 2 (probe then widened read)", len(spy.queries))
	}
	if spy.queries[1].Limit != lastRunSkipWindow {
		t.Fatalf("widened limit = %d, want %d", spy.queries[1].Limit, lastRunSkipWindow)
	}
}

// The skip record still carries the order's run label, so `gc order history`
// shows it. Excluding it from LastRun must not make it invisible.
func TestRunOnSkipRecordStaysInHistory(t *testing.T) {
	store := beads.NewMemStore()
	st := NewStore(beads.OrdersStore{Store: store})
	run, err := st.CreateRunSkippedRunOn("merge-sweep")
	if err != nil {
		t.Fatalf("CreateRunSkippedRunOn: %v", err)
	}
	if run.ID == "" {
		t.Fatal("skip record has no id")
	}

	recent, err := st.RecentRuns("merge-sweep", 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("RecentRuns = %d, want the skip record to remain visible", len(recent))
	}

	got, err := store.Get(run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !IsRunOnSkipBead(got) {
		t.Errorf("skip record missing its skip label: %v", got.Labels)
	}
	if got.Metadata["close_reason"] != SkipReasonRunOn {
		t.Errorf("close_reason = %q, want %q", got.Metadata["close_reason"], SkipReasonRunOn)
	}
	if got.Status != "closed" {
		t.Errorf("skip record status = %q, want closed", got.Status)
	}
	if last, err := st.LastRun("merge-sweep"); err != nil || !last.IsZero() {
		t.Errorf("LastRun after a skip = %s (err %v), want zero", last, err)
	}
}

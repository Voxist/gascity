package readywork

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func intRef(v int) *int { return &v }

func readyRow(id string, priority int, created time.Time, mutate ...func(*beads.Bead)) beads.Bead {
	b := beads.Bead{ID: id, Title: id, Status: "open", Type: "task", Priority: intRef(priority), CreatedAt: created}
	for _, m := range mutate {
		m(&b)
	}
	return b
}

func readIDs(t *testing.T, legs []Leg, q Query) []string {
	t.Helper()
	rows, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read(%+v): %v", q, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestReadAssigneeMatchesTheStoredAssigneeExactly pins --assignee and
// --unassigned to `bd ready`'s comparison: the stored assignee exactly. A
// stored assignee with surrounding whitespace is a different identity, and a
// whitespace-only one is an assignee, not the absence of one. Exactness is also
// what lets a store narrow by these predicates and still return every row they
// keep (the advisory pushdown of beads.ReadyQuery).
func TestReadAssigneeMatchesTheStoredAssigneeExactly(t *testing.T) {
	now := time.Now().UTC()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		readyRow("a-1", 1, now, func(b *beads.Bead) { b.Assignee = "worker" }),
		readyRow("a-2", 2, now.Add(time.Second), func(b *beads.Bead) { b.Assignee = " worker" }),
		readyRow("a-3", 3, now.Add(2*time.Second), func(b *beads.Bead) { b.Assignee = "  " }),
		readyRow("a-4", 2, now.Add(3*time.Second)),
	}, nil)
	legs := []Leg{testLeg("city", store)}
	if got := readIDs(t, legs, Query{Assignee: " worker "}); !slices.Equal(got, []string{"a-1"}) {
		t.Fatalf("--assignee worker = %v, want only the exact match a-1", got)
	}
	if got := readIDs(t, legs, Query{Unassigned: true}); !slices.Equal(got, []string{"a-4"}) {
		t.Fatalf("--unassigned = %v, want only the empty assignee a-4 (a whitespace assignee is an assignee, as bd matches it)", got)
	}
}

// TestReadRefusesANegativeLimitBeforeReadingAnyStore pins the limit's floor at
// the reader, where the CLI and the API meet: zero is unlimited and a negative
// limit is refused, as the API refuses it, so the two doors cannot disagree on
// it. The refusal costs no store read.
func TestReadRefusesANegativeLimitBeforeReadingAnyStore(t *testing.T) {
	store := newNarrowingStore(readyRow("a-1", 1, time.Now().UTC()))
	_, err := Read([]Leg{testLeg("city", store)}, Query{Limit: -1})
	if err == nil || !strings.Contains(err.Error(), "--limit -1") {
		t.Fatalf("Read(--limit -1) error = %v, want a refusal naming the limit", err)
	}
	if got := len(store.readyQueries()) + len(store.orderedCalls()); got != 0 {
		t.Fatalf("a refused query read the store %d times, want none", got)
	}
	if got := readIDs(t, []Leg{testLeg("city", store)}, Query{Limit: 0}); !slices.Equal(got, []string{"a-1"}) {
		t.Fatalf("--limit 0 = %v, want every row (0 is unlimited)", got)
	}
}

// TestReadAcceptsEverySortBDReadyTakes pins --sort to bd ready's vocabulary.
// The generated routed tier reads `--sort priority` (its best-by-priority leg)
// and `--sort hybrid` (its claim window), and on a federated city that read is
// this reader: an order bd takes and this reader refuses fails every federated
// probe that asks for it (vp-bzeg). newest is this reader's own addition.
func TestReadAcceptsEverySortBDReadyTakes(t *testing.T) {
	for _, order := range []string{SortPriority, SortHybrid, SortOldest, SortNewest} {
		if err := (Query{Sort: order}).Validate(); err != nil {
			t.Errorf("--sort %s: %v, want it accepted", order, err)
		}
	}
	err := (Query{Sort: "newest-first"}).Validate()
	if err == nil {
		t.Fatal("--sort newest-first was accepted; a mistyped order must be refused, not served in some other order")
	}
	for _, order := range []string{SortPriority, SortHybrid, SortOldest, SortNewest} {
		if !strings.Contains(err.Error(), order) {
			t.Errorf("refusal %q does not name the accepted order %s", err, order)
		}
	}
}

// TestReadSortPriorityIsTheCanonicalOrder pins that bd's flag default and this
// reader's default are one law spelled two ways: --sort priority serves the
// canonical ready order (priority, created_at, id), bounded or not, and a
// bounded read is still cut by the leg in that order.
func TestReadSortPriorityIsTheCanonicalOrder(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rows := []beads.Bead{
		readyRow("p3-old", 3, base),
		readyRow("p1-new", 1, base.Add(2*time.Minute)),
		readyRow("p1-old", 1, base.Add(time.Minute)),
		readyRow("nil-priority", 0, base.Add(3*time.Minute), func(b *beads.Bead) { b.Priority = nil }),
	}
	plain := []Leg{testLeg("city", beads.NewMemStoreFrom(0, rows, nil))}
	for _, limit := range []int{0, 2} {
		want := readIDs(t, plain, Query{Limit: limit})
		if got := readIDs(t, plain, Query{Sort: SortPriority, Limit: limit}); !slices.Equal(got, want) {
			t.Errorf("--sort priority --limit %d = %v, want the default order %v", limit, got, want)
		}
	}
	store := newNarrowingStore(rows...)
	if _, err := Read([]Leg{testLeg("city", store)}, Query{Sort: SortPriority, Limit: 2}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if calls := store.orderedCalls(); len(calls) != 1 || calls[0].order != beads.ReadyOrderCanonical {
		t.Fatalf("--sort priority --limit 2 asked %+v, want one bounded read cut in the canonical order", calls)
	}
}

// TestReadSortHybridServesBDsRecencySeam pins --sort hybrid to bd's SQL
// rendering (sqlbuild.BuildReadyWorkOrder): rows created within the last 48h
// first, by (priority, created_at, id); rows past the seam after them, strictly
// (created_at, id). The routed claim window reads under it, so the merged
// answer of a federated city must come out in the order the single-store bd
// read serves. The rows are split across two legs, so the order is imposed on
// the merged set, not inherited from one store. Margins stay a minute clear of
// the seam so the case cannot flake on execution time.
func TestReadSortHybridServesBDsRecencySeam(t *testing.T) {
	now := time.Now().UTC()
	nilPriority := func(b *beads.Bead) { b.Priority = nil }
	city := beads.NewMemStoreFrom(0, []beads.Bead{
		readyRow("aged-p1", 1, now.Add(-72*time.Hour)),
		readyRow("aged-p4", 4, now.Add(-96*time.Hour)),
		readyRow("fresh-p3", 3, now.Add(-30*time.Minute)),
		readyRow("seam-aged", 0, now.Add(-48*time.Hour-time.Minute), nilPriority),
	}, nil)
	rig := beads.NewMemStoreFrom(0, []beads.Bead{
		readyRow("aged-p2", 2, now.Add(-50*time.Hour)),
		readyRow("aged-p0", 0, now.Add(-60*time.Hour)),
		readyRow("fresh-p1", 1, now.Add(-time.Hour)),
		readyRow("fresh-nil", 0, now.Add(-2*time.Hour), nilPriority),
		readyRow("seam-fresh", 3, now.Add(-48*time.Hour+time.Minute)),
	}, nil)
	legs := []Leg{testLeg("city", city), testLeg("rig r", rig)}

	// Fresh tier: priority first (a nil priority sorts as 2), then created_at
	// ascending, so the p3 row a minute inside the seam precedes the half-hour
	// old one. Aged tier: created_at ascending whatever the priority, so the p4
	// row 96h old precedes the p0 row 60h old; an aged tier that still sorted
	// by priority would put aged-p0 first.
	want := []string{"fresh-p1", "fresh-nil", "seam-fresh", "fresh-p3", "aged-p4", "aged-p1", "aged-p0", "aged-p2", "seam-aged"}
	if got := readIDs(t, legs, Query{Sort: SortHybrid}); !slices.Equal(got, want) {
		t.Fatalf("--sort hybrid = %v, want %v", got, want)
	}
	if got := readIDs(t, legs, Query{Sort: SortHybrid, Limit: 3}); !slices.Equal(got, want[:3]) {
		t.Fatalf("--sort hybrid --limit 3 = %v, want the hybrid top 3 %v", got, want[:3])
	}
}

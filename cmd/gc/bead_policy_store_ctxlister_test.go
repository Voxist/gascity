package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type ctxKeyType struct{}

// ctxRecordingStore is a backing whose ListCtx records the context it was
// handed and honors its cancellation, so a test can prove the CALLER's ctx
// (not a fresh Background) reached the backend read.
type ctxRecordingStore struct {
	*beads.MemStore
	got   context.Context
	block bool
}

func (s *ctxRecordingStore) ListCtx(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.got = ctx
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.List(query)
}

// TestBeadPolicyStoreForwardsListCtxToInnerStore pins that the policy wrapper
// State.BeadStore() hands to the status handlers satisfies beads.CtxLister and
// routes ListCtx to the inner store with the caller's context. Without the
// forwarder the wrapper embeds the ctx-less Store interface only, so the
// handler's `store.(beads.CtxLister)` assertion fails on the wrapper and a
// wedged backend read keeps its connection past the handler's deadline.
func TestBeadPolicyStoreForwardsListCtxToInnerStore(t *testing.T) {
	backing := &ctxRecordingStore{MemStore: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{Title: "row", Status: "open", Type: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, tc := range []struct {
		name  string
		inner beads.Store
	}{
		{name: "over CachingStore", inner: beads.NewCachingStoreForTest(backing, nil)},
		{name: "over backing directly", inner: backing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing.got = nil
			wrapped := wrapStoreWithBeadPolicies(tc.inner, &config.City{})
			lister, ok := wrapped.(beads.CtxLister)
			if !ok {
				t.Fatalf("%T does not implement beads.CtxLister; status reads fall back to the uncancellable List", wrapped)
			}
			ctx := context.WithValue(context.Background(), ctxKeyType{}, "caller")
			rows, err := lister.ListCtx(ctx, beads.ListQuery{Status: "open", AllowScan: true, TierMode: beads.TierIssues})
			if err != nil {
				t.Fatalf("ListCtx: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("ListCtx returned %d rows; want 1", len(rows))
			}
			if backing.got == nil {
				t.Fatal("backing ListCtx was never called; the wrapper did not forward to the inner store")
			}
			if v, _ := backing.got.Value(ctxKeyType{}).(string); v != "caller" {
				t.Fatalf("backing received a different context (value %q); want the caller's", v)
			}
		})
	}
}

// TestBeadPolicyStoreListCtxCancellationAbortsBackingRead pins the reason the
// forwarder exists: a canceled caller context must abort the in-flight backing
// read through the wrapper instead of leaving it holding its connection.
func TestBeadPolicyStoreListCtxCancellationAbortsBackingRead(t *testing.T) {
	backing := &ctxRecordingStore{MemStore: beads.NewMemStore(), block: true}
	wrapped := wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(backing, nil), &config.City{})
	lister, ok := wrapped.(beads.CtxLister)
	if !ok {
		t.Fatalf("%T does not implement beads.CtxLister", wrapped)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := lister.ListCtx(ctx, beads.ListQuery{AllowScan: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want the caller's deadline surfaced from the backing read", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ListCtx took %s; the backing read did not observe the caller's cancellation", elapsed)
	}
}

// TestBeadPolicyStoreListCtxFallsBackToListWithoutInnerCapability pins the
// no-capability shape: an inner store with no CtxLister is read through the
// policy-expanded List (mirroring CachingStore.backingListCtx), so the wrapper
// never reports a capability error the status handler would not fall back on.
func TestBeadPolicyStoreListCtxFallsBackToListWithoutInnerCapability(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "row", Status: "open", Type: "task"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(mem, &config.City{})
	lister, ok := wrapped.(beads.CtxLister)
	if !ok {
		t.Fatalf("%T does not implement beads.CtxLister", wrapped)
	}
	rows, err := lister.ListCtx(context.Background(), beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("ListCtx over a non-CtxLister inner store: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListCtx returned %d rows; want 1", len(rows))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lister.ListCtx(canceled, beads.ListQuery{Status: "open", AllowScan: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListCtx with an already-canceled ctx = %v; want context.Canceled pre-flight", err)
	}
}

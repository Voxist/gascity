package beads

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCachingStoreSatisfiesCtxLister pins the capability itself. Dropping
// ListCtx does not fail to compile and breaks nothing loudly — every
// store.(CtxLister) assertion just starts returning false and the caller
// silently takes the uncancellable path. That is how the 2026-08-31 resync
// lost it. This assertion is the tripwire.
func TestCachingStoreSatisfiesCtxLister(t *testing.T) {
	t.Parallel()
	var s any = &CachingStore{}
	if _, ok := s.(CtxLister); !ok {
		t.Fatal("*CachingStore no longer satisfies CtxLister; context-cancellable reads silently degrade to the uncancellable fallback")
	}
}

// TestCachingStoreListCtxHonorsCanceledContext proves the ctx is actually
// consulted rather than merely accepted.
func TestCachingStoreListCtxHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	c := NewCachingStore(NewMemStore(), func(_, _, _, _, _ string, _ *[]string, _ json.RawMessage) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListCtx(ctx, ListQuery{AllowScan: true}); err == nil {
		t.Fatal("ListCtx with a canceled context returned nil error; the ctx is not consulted")
	}
}

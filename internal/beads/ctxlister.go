package beads

import "context"

// CtxLister is an optional Store capability for context-cancellable reads.
// Stores that hold an external connection (Dolt, bd-CLI subprocess) should
// implement it so a canceled context aborts the in-flight read and releases
// the connection, instead of the caller abandoning a goroutine that keeps
// holding it (the statusListStoreWithTimeout goroutine+time.After pattern).
//
// Implementations make List(query) a context.Background() shim to ListCtx,
// mirroring how Counter's optional-capability pattern coexists with the
// ctx-less Store interface.
type CtxLister interface {
	ListCtx(ctx context.Context, query ListQuery) ([]Bead, error)
}
